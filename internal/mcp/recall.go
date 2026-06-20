package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mindmorass/mcp-phantom-brain/internal/index"
)

const defaultRecallLimit = 10

// recallTool defines the brain_recall MCP tool schema.
//
// brain_recall is the search-side primary entry point. It runs a
// hybrid (vector + FTS5) query against the brain's vectors.db and
// returns matched documents formatted as MCP text content for the
// agent to consume.
func recallTool() mcp.Tool {
	return mcp.NewTool("brain_recall",
		mcp.WithDescription(
			`Hybrid (vector + full-text) search over the brain's Wiki pages. `+
				`Returns the top-K matching pages with scores, paths, and snippets. `+
				`Use whenever you need to find prior knowledge stored in the brain.`,
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Natural-language query string. Matched against page titles, tags, and body via FTS5 AND against page embeddings via vector similarity."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of results (default 10, max 50)."),
		),
	)
}

// handleRecall executes brain_recall. It embeds the query, runs the
// hybrid search, and renders results as MCP text content.
func (s *Server) handleRecall(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return mcp.NewToolResultError("query must be non-empty"), nil
	}

	limit := defaultRecallLimit
	if got, err := req.RequireFloat("limit"); err == nil {
		limit = int(got)
	}
	if limit <= 0 {
		limit = defaultRecallLimit
	}
	if limit > 50 {
		limit = 50
	}

	// Embed the query. The embedder's Dims must match the index's.
	if s.deps.Embedder.Dims() != s.deps.Index.Dims() {
		return mcp.NewToolResultError(fmt.Sprintf(
			"embedder/index dim mismatch: embedder=%d index=%d",
			s.deps.Embedder.Dims(), s.deps.Index.Dims(),
		)), nil
	}
	embs, err := s.deps.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("embed query: %v", err)), nil
	}
	if len(embs) != 1 {
		return mcp.NewToolResultError(fmt.Sprintf("embedder returned %d vectors, want 1", len(embs))), nil
	}

	// Phrase-quote so FTS5 treats user input literally. Without this,
	// punctuation like ':' or '-' is parsed as FTS5 syntax (column
	// scope, operators) and produces "no such column" errors for
	// arbitrary natural-language input.
	hits, err := s.deps.Index.SearchHybrid(ctx, ftsPhrase(query), embs[0], limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("hybrid search: %v", err)), nil
	}

	return mcp.NewToolResultText(renderRecallHits(query, hits)), nil
}

// renderRecallHits formats search results for the agent. The output is
// markdown-ish so a downstream agent can copy paths or quote scores
// readably.
func renderRecallHits(query string, hits []index.Hit) string {
	if len(hits) == 0 {
		return fmt.Sprintf("No results for %q.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d result(s) for %q:\n\n", len(hits), query)
	for i, h := range hits {
		fmt.Fprintf(&b, "## %d. %s\n", i+1, h.SourcePath)
		fmt.Fprintf(&b, "- SHA: `%s`\n", h.SHA)
		fmt.Fprintf(&b, "- Score: %.4f", h.Score)
		if h.VectorRank > 0 {
			fmt.Fprintf(&b, "  (vector rank %d", h.VectorRank)
			if h.TextRank > 0 {
				fmt.Fprintf(&b, ", text rank %d", h.TextRank)
			}
			b.WriteString(")")
		} else if h.TextRank > 0 {
			fmt.Fprintf(&b, "  (text rank %d)", h.TextRank)
		}
		b.WriteString("\n\n")
	}
	return b.String()
}

// ftsPhrase wraps a free-text query in FTS5 phrase-literal syntax:
// double quotes around the string with any embedded quotes doubled.
// The tokenizer still splits the phrase on its own rules (whitespace,
// punctuation) so multi-word queries still match; the wrapper just
// prevents the FTS5 parser from interpreting user punctuation as
// column scopes or operators.
func ftsPhrase(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

