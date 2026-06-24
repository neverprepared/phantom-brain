package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// brain_fetch returns the full stored body for a SHA — the retrieval
// step that pairs with brain_recall's discovery. recall surfaces a
// 150-char snippet to find the right doc; brain_fetch reads the whole
// document from the SAME local snapshot, so a SHA recall returned is
// always fetchable. Use it deliberately on a SHA you identified, not as
// a browsing habit — full bodies are large, which is why recall
// truncates in the first place.
func fetchTool() mcp.Tool {
	return mcp.NewTool("brain_fetch",
		mcp.WithDescription(
			`Return the full body of one long-term-memory doc by SHA. Pairs with brain_recall: `+
				`recall finds the SHA (snippet view), brain_fetch reads the whole document. `+
				`Reads the local snapshot, so a doc written this session isn't fetchable until a new `+
				`snapshot publishes (same lag as recall). Use deliberately — bodies can be large.`,
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

	res, err := s.deps.Index.FetchBySHA(ctx, sha)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("brain_fetch: %v", err)), nil
	}
	if res == nil {
		return mcp.NewToolResultText(fmt.Sprintf(
			"brain_fetch: no document with SHA %s in the current snapshot. If you just wrote it "+
				"this session, it won't appear until a new snapshot publishes.",
			sha,
		)), nil
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
	}
	if strings.TrimSpace(res.Tags) != "" {
		fmt.Fprintf(&b, " · tags: %s", strings.TrimSpace(res.Tags))
	}
	fmt.Fprintf(&b, " · sha: %s_\n\n", res.SHA)
	b.WriteString(res.Body)

	return mcp.NewToolResultText(b.String()), nil
}
