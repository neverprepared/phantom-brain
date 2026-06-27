package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/neverprepared/phantom-brain/internal/brain"
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

// handleRecall executes brain_recall. Phase D1: recall is ONLINE-ONLY.
// It embeds the query and POSTs to the daemon's live pb_records recall
// endpoint (Postgres projection, hybrid BM25+kNN, always fresh). There is
// no local-snapshot fallback anymore — the snapshot is no longer the
// authoritative search surface (signed-off cutover). A nil RecallClient or
// a daemon failure returns a clear tool error rather than serving stale
// local data.
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

	if s.deps.RecallClient == nil {
		return mcp.NewToolResultError(
			"brain_recall is online-only and no daemon recall client is configured (legacy snapshot recall was removed in the Postgres cutover)"), nil
	}

	// Embed the query for the vector half of the daemon's hybrid query.
	embs, err := s.deps.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("embed query: %v", err)), nil
	}
	if len(embs) != 1 {
		return mcp.NewToolResultError(fmt.Sprintf("embedder returned %d vectors, want 1", len(embs))), nil
	}

	resp, oErr := s.deps.RecallClient.Recall(ctx, brain.RecallRequest{
		Query:     query,
		Embedding: embs[0],
		Limit:     limit,
	})
	if oErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("recall failed (%s)", onlineRecallReason(oErr))), nil
	}
	return mcp.NewToolResultText(renderOnlineRecallHits(query, resp.Hits)), nil
}

// onlineRecallReason renders a short, human reason for an online-recall
// failure to embed in the fallback note. Daemon-unreachable and the
// 503 "not enabled" envelope are the common cases; everything else
// falls through to the raw error string.
func onlineRecallReason(err error) string {
	if errors.Is(err, brain.ErrDaemonUnreachable) {
		return "daemon unreachable"
	}
	var apiErr *brain.APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == http.StatusServiceUnavailable {
			return "not enabled for this binding"
		}
		return fmt.Sprintf("daemon error %d", apiErr.StatusCode)
	}
	return err.Error()
}

// renderOnlineRecallHits formats live daemon recall results, mirroring
// renderRecallHits' style (title + kind indicator + snippet + score).
// Attachment hits surface a fetch hint built from mime_type /
// original_filename. The footer announces the results are live (always
// fresh), distinct from the snapshot-staleness footer of the local path.
func renderOnlineRecallHits(query string, hits []brain.RecallHitDTO) string {
	const liveFooter = "\n_— live results from daemon (always fresh)_\n"
	if len(hits) == 0 {
		return fmt.Sprintf("No results for %q.%s", query, liveFooter)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d result(s) for %q:\n\n", len(hits), query)
	for i, h := range hits {
		heading := h.Title
		if heading == "" {
			heading = h.SHA
		}
		fmt.Fprintf(&b, "## %d. %s %s\n", i+1, heading, onlineKindIndicator(h))
		fmt.Fprintf(&b, "- SHA: `%s`\n", h.SHA)
		fmt.Fprintf(&b, "- Score: %.4f\n", h.Score)
		if h.Kind == "attachment_stub" {
			fmt.Fprintf(&b, "- Fetch via `GET /api/brain/attach/%s`", h.SHA)
			if h.OriginalFilename != "" {
				fmt.Fprintf(&b, " (%s)", h.OriginalFilename)
			}
			b.WriteString("\n")
		}
		if h.Snippet != "" {
			fmt.Fprintf(&b, "- Snippet: %s\n", h.Snippet)
		}
		b.WriteString("\n")
	}
	b.WriteString(liveFooter)
	return b.String()
}

// onlineKindIndicator renders the short bracketed label for an online
// hit. Mirrors kindIndicator, but the MIME for attachments comes from
// the structured mime_type field (the projection carries it directly)
// rather than the "mime:" tag the snapshot path parses out of tags.
func onlineKindIndicator(h brain.RecallHitDTO) string {
	switch h.Kind {
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
		mime := h.MimeType
		if mime == "" {
			return "[attachment]"
		}
		if slash := strings.LastIndex(mime, "/"); slash >= 0 && slash < len(mime)-1 {
			return "[attachment " + mime[slash+1:] + "]"
		}
		return "[attachment " + mime + "]"
	case "":
		return "[unknown]"
	default:
		return "[" + h.Kind + "]"
	}
}
