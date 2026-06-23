package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// learnTool defines the brain_learn MCP tool schema.
//
// brain_learn ingests operator-curated content into Raw/curated/.
// Same flow as brain_perceive but the destination subdir tells the
// synthesizer to treat it as higher-reliability source material.
//
// Supports batch ingest via the items[] array; up to 100 per call.
// On batch ingest the response lists per-item statuses (stored vs
// duplicate vs error) so the caller can act on partial failure.
func learnTool() mcp.Tool {
	return mcp.NewTool("brain_learn",
		mcp.WithDescription(
			`Ingest operator-curated content into Raw/curated/. Single mode: pass `+
				`content + title + filename. Batch mode: pass items[] (up to 100; each item `+
				`is {content, title, filename, source_url?}). Duplicates are detected by `+
				`canonical SHA256 and skipped.`,
		),
		mcp.WithString("content",
			mcp.Description("Single-item mode: raw markdown body."),
		),
		mcp.WithString("title",
			mcp.Description("Single-item mode: human-readable title."),
		),
		mcp.WithString("filename",
			mcp.Description("Single-item mode: optional filename hint. Slug-derived from title if omitted."),
		),
		mcp.WithString("source_url",
			mcp.Description("Single-item mode: optional source URL."),
		),
		mcp.WithArray("items",
			mcp.Description("Batch mode: array of items, each shaped like the single-item args. Up to 100 entries. If provided, single-mode fields are ignored."),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content":    map[string]any{"type": "string"},
					"title":      map[string]any{"type": "string"},
					"filename":   map[string]any{"type": "string"},
					"source_url": map[string]any{"type": "string"},
				},
				"required": []string{"content", "title"},
			}),
		),
	)
}

// learnItem mirrors one entry of the items[] batch.
type learnItem struct {
	Content   string
	Title     string
	Filename  string
	SourceURL string
}

const learnBatchMax = 100

// queueNoticeCap bounds how many per-item "Queued ..." suffixes
// appear in the batch output before the loop drops to a single-line
// summary. Prevents a 100-item offline batch from emitting 100 copies
// of the same outage notice.
const queueNoticeCap = 3

func (s *Server) handleLearn(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	items, ok := extractLearnItems(req)
	if !ok {
		return mcp.NewToolResultError("brain_learn requires either single-item args (content + title) or items[]"), nil
	}
	if len(items) == 0 {
		return mcp.NewToolResultError("items[] is empty"), nil
	}
	if len(items) > learnBatchMax {
		return mcp.NewToolResultError(fmt.Sprintf("items[] exceeds max of %d (got %d)", learnBatchMax, len(items))), nil
	}

	var b strings.Builder
	stored, dups, failed := 0, 0, 0
	queuedItems := 0
	for i, it := range items {
		res, errMsg, success := s.ingestMarkdown(ctx, ingestParams{
			Subdir:    "curated",
			StampKey:  "learned_at",
			Content:   it.Content,
			Title:     it.Title,
			Filename:  it.Filename,
			SourceURL: it.SourceURL,
		})
		if !success {
			failed++
			fmt.Fprintf(&b, "[%d] %s — ERROR: %s\n", i+1, it.Title, errMsg)
			continue
		}
		switch res.Status {
		case "duplicate":
			dups++
			fmt.Fprintf(&b, "[%d] %s — duplicate (SHA %s)\n", i+1, it.Title, res.SHA[:12])
		default:
			stored++
			notice := res.Notice
			if notice != "" {
				queuedItems++
			}
			// Cap per-item notice output: after queueNoticeCap items
			// have emitted a notice, drop the per-item suffix and let
			// the batch-level summary line carry the count.
			if queuedItems > queueNoticeCap {
				notice = ""
			}
			fmt.Fprintf(&b, "[%d] %s — stored to %s (SHA %s)%s\n", i+1, it.Title, res.RelativePath, res.SHA[:12], notice)
		}
	}
	if queuedItems > queueNoticeCap {
		fmt.Fprintf(&b, "... and %d more pending (%d total queued).\n", queuedItems-queueNoticeCap, queuedItems)
	}

	header := fmt.Sprintf("brain_learn: %d stored, %d duplicate, %d failed (of %d)\n\n",
		stored, dups, failed, len(items))
	return mcp.NewToolResultText(header + b.String()), nil
}

// extractLearnItems pulls a normalized []learnItem from the request,
// resolving the single-item vs batch shape. Returns (items, true) on
// any recognized shape (including a single-item set with empty
// optional fields); returns (nil, false) when neither shape is
// present (no items[] AND no content) so the caller can emit the
// usage error.
func extractLearnItems(req mcp.CallToolRequest) ([]learnItem, bool) {
	// Batch first — items[] dominates per the v4.x TS behavior.
	if raw, err := req.RequireStringSlice("items"); err == nil && raw != nil {
		// RequireStringSlice would return []string. items[] elements
		// are objects, not strings, so we go a layer deeper.
		_ = raw
	}
	if rawAny := optionalRawSlice(req, "items"); rawAny != nil {
		out := make([]learnItem, 0, len(rawAny))
		for _, elem := range rawAny {
			m, ok := elem.(map[string]any)
			if !ok {
				return nil, false
			}
			it := learnItem{
				Content:   strFromMap(m, "content"),
				Title:     strFromMap(m, "title"),
				Filename:  strFromMap(m, "filename"),
				SourceURL: strFromMap(m, "source_url"),
			}
			out = append(out, it)
		}
		return out, true
	}

	// Single-item shape.
	content, _ := req.RequireString("content")
	title, _ := req.RequireString("title")
	if content == "" && title == "" {
		return nil, false
	}
	filename, _ := req.RequireString("filename")
	sourceURL, _ := req.RequireString("source_url")
	return []learnItem{{
		Content:   content,
		Title:     title,
		Filename:  filename,
		SourceURL: sourceURL,
	}}, true
}

// optionalRawSlice returns the raw []any for a JSON array argument, or
// nil if the key is absent. mcp-go doesn't expose typed object-array
// access directly; we go through the underlying argument map.
func optionalRawSlice(req mcp.CallToolRequest, key string) []any {
	args := req.GetArguments()
	if args == nil {
		return nil
	}
	v, ok := args[key]
	if !ok {
		return nil
	}
	slice, ok := v.([]any)
	if !ok {
		return nil
	}
	return slice
}

func strFromMap(m map[string]any, k string) string {
	v, ok := m[k]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
