package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/neverprepared/phantom-brain/internal/index"
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

	// Issue #61: append a staleness footer when the parent snapshot
	// is more than an hour old. Helps the agent reason about why a
	// just-written perceive wouldn't show up yet (snapshot rebuild
	// + sync are async). Lifecycle is nil in legacy mode — skip.
	var snapAge time.Duration
	var snapGen uint64
	if lc := s.deps.Lifecycle; lc != nil {
		snapAge = lc.SnapshotAge(time.Now())
		if pg := lc.Snapshot().ParentGen; pg != nil {
			snapGen = *pg
		}
	}
	return mcp.NewToolResultText(renderRecallHits(query, hits, snapGen, snapAge)), nil
}

// renderRecallHits formats search results for the agent. The output is
// markdown-ish so a downstream agent can copy paths or quote scores
// readably.
//
// Each hit is rendered with title + kind indicator + body snippet so
// the calling agent can decide whether the hit is worth fetching in
// full without a second tool call. Attachment hits include a hint that
// the body lives behind GET /api/brain/attach/<sha> (presigned MinIO
// URL), since the snippet alone won't convey the binary content.
func renderRecallHits(query string, hits []index.Hit, snapGen uint64, snapAge time.Duration) string {
	if len(hits) == 0 {
		return fmt.Sprintf("No results for %q.%s", query, snapshotFooter(snapGen, snapAge))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d result(s) for %q:\n\n", len(hits), query)
	for i, h := range hits {
		heading := h.Title
		if heading == "" {
			heading = h.SourcePath
		}
		indicator := kindIndicator(h.Kind, h.Tags)
		fmt.Fprintf(&b, "## %d. %s %s\n", i+1, heading, indicator)
		fmt.Fprintf(&b, "- SHA: `%s`\n", h.SHA)
		fmt.Fprintf(&b, "- Path: `%s`\n", h.SourcePath)
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
		b.WriteString("\n")
		if h.Kind == "attachment_stub" {
			fmt.Fprintf(&b, "- Fetch via `GET /api/brain/attach/%s`\n", h.SHA)
		}
		if h.Snippet != "" {
			fmt.Fprintf(&b, "- Snippet: %s\n", h.Snippet)
		}
		b.WriteString("\n")
	}
	b.WriteString(snapshotFooter(snapGen, snapAge))
	return b.String()
}

// snapshotFooter returns a one-line staleness disclosure when the
// parent snapshot is more than an hour old, otherwise empty.
func snapshotFooter(gen uint64, age time.Duration) string {
	if age <= time.Hour {
		return ""
	}
	if gen == 0 {
		return fmt.Sprintf("\n_Snapshot built %s ago._\n", humanizeAge(age))
	}
	return fmt.Sprintf("\n_Snapshot gen %d, built %s ago._\n", gen, humanizeAge(age))
}

// kindIndicator renders the short bracketed label that follows the
// hit title. Attachment stubs surface the MIME extracted from the
// "mime:<type>" tag the daemon writes at attach time, formatted as
// "[attachment pdf]" / "[attachment png]" / "[attachment]" when MIME
// is unknown.
func kindIndicator(kind, tags string) string {
	switch kind {
	case "note":
		return "[note]"
	case "web_scrape":
		return "[web]"
	case "task_summary":
		return "[task]"
	case "email_import":
		return "[email]"
	case "manual_curate":
		return "[curated]"
	case "attachment_stub":
		mime := extractMIMETag(tags)
		if mime == "" {
			return "[attachment]"
		}
		// Prefer the subtype ("pdf" from "application/pdf"); fall back
		// to the full MIME when no slash.
		if slash := strings.LastIndex(mime, "/"); slash >= 0 && slash < len(mime)-1 {
			return "[attachment " + mime[slash+1:] + "]"
		}
		return "[attachment " + mime + "]"
	case "":
		return "[unknown]"
	default:
		return "[" + kind + "]"
	}
}

// extractMIMETag pulls the value of the first "mime:..." token from a
// space-joined tag blob. Returns "" when no such tag is present.
func extractMIMETag(tags string) string {
	for _, t := range strings.Fields(tags) {
		if strings.HasPrefix(t, "mime:") {
			return strings.TrimPrefix(t, "mime:")
		}
	}
	return ""
}

// ftsPhrase rewrites a free-text query into FTS5 syntax that does an
// implicit OR over its tokens. Each token is wrapped in double quotes
// (preventing FTS5 from treating user punctuation as a column-scope
// or operator) and joined with " OR ".
//
//	"loop engineering AI coding agents"
//	  → `"loop" OR "engineering" OR "AI" OR "coding" OR "agents"`
//
// The previous implementation wrapped the WHOLE query in one set of
// quotes, which made FTS5 treat it as a strict ordered phrase match.
// That meant a query like "loop engineering AI coding agents" missed
// a doc titled "What Is Loop Engineering? The New Meta for AI Coding
// Agents" because the `? The New Meta for` between Engineering and AI
// broke the consecutive-token requirement. Tokenize + OR-join gives
// the BM25 path a chance to score docs by individual term hits, which
// is what hybrid recall expects from its text half.
func ftsPhrase(s string) string {
	tokens := strings.Fields(s)
	if len(tokens) == 0 {
		return ""
	}
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		// Skip lone FTS5 reserved words that have no content
		// ("AND"/"OR"/"NOT" by themselves). Folded to lowercase
		// because tokenizer is case-insensitive anyway.
		switch strings.ToLower(t) {
		case "and", "or", "not", "near":
			continue
		}
		out = append(out, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, " OR ")
}

