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

// isNotFound reports whether err is a daemon 404 (unknown SHA). Lets
// brain_fetch render a friendly "no such doc" text result rather than a
// raw tool error for the common not-found case.
func isNotFound(err error) bool {
	var apiErr *brain.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// brain_fetch returns the full stored body for a SHA — the retrieval
// step that pairs with brain_recall's discovery. recall surfaces a
// 150-char snippet to find the right doc; brain_fetch reads the whole
// document. Phase D2a: ONLINE-ONLY — it reads the daemon's Postgres SoR
// by SHA (same authoritative store recall queries), so any SHA recall
// returned is fetchable and fresh. Use it deliberately on a SHA you
// identified, not as a browsing habit — full bodies are large, which is
// why recall truncates in the first place.
func fetchTool() mcp.Tool {
	return mcp.NewTool("brain_fetch",
		mcp.WithDescription(
			`Return the full body of one long-term-memory doc by SHA. Pairs with brain_recall: `+
				`recall finds the SHA (snippet view), brain_fetch reads the whole document. `+
				`Reads the daemon's live store (same source as recall), so a doc is fetchable as `+
				`soon as it is synthesised. Use deliberately — bodies can be large.`,
		),
		mcp.WithString("sha",
			mcp.Required(),
			mcp.Description("Content SHA of the doc to fetch (as shown in brain_recall results)."),
		),
	)
}

func (s *Server) handleFetch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sha, err := req.RequireString("sha")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return mcp.NewToolResultError("brain_fetch requires a non-empty sha"), nil
	}

	if s.deps.FetchClient == nil {
		return mcp.NewToolResultError(
			"brain_fetch is online-only and no daemon fetch client is configured (legacy snapshot fetch was removed in the Postgres cutover)"), nil
	}

	res, err := s.deps.FetchClient.Fetch(ctx, sha)
	if err != nil {
		if isNotFound(err) {
			return mcp.NewToolResultText(fmt.Sprintf(
				"brain_fetch: no document with SHA %s in long-term memory. If you just wrote it "+
					"this session, it won't be fetchable until the daemon synthesises it.",
				sha,
			)), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("brain_fetch: %v", err)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", res.Title)
	kind := res.Kind
	if kind == "" {
		kind = "note"
	}
	fmt.Fprintf(&b, "_kind: %s", kind)
	if res.SourcePath != "" {
		fmt.Fprintf(&b, " · source: %s", res.SourcePath)
	} else if res.SourceURL != "" {
		fmt.Fprintf(&b, " · source: %s", res.SourceURL)
	}
	if tags := strings.TrimSpace(strings.Join(res.Tags, ", ")); tags != "" {
		fmt.Fprintf(&b, " · tags: %s", tags)
	}
	fmt.Fprintf(&b, " · sha: %s_\n\n", res.SHA)
	b.WriteString(res.Body)

	return mcp.NewToolResultText(b.String()), nil
}
