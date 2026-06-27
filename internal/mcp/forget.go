package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// brain_forget (issue #72, Phase 1). The apply half of
// propose-then-apply: delete one long-term summary by SHA. The SHA is
// the handle — typically one the operator copied out of brain_reflect.
// The daemon deletes the doc; the removal takes effect on the next
// online recall (Phase D2b: recall reads the daemon's Postgres SoR).
//
// Forget is a delete, not an ingest — it does NOT go through the
// write-ahead queue. (If the daemon is unreachable the call simply
// errors; re-run when it's back.)
func forgetTool() mcp.Tool {
	return mcp.NewTool("brain_forget",
		mcp.WithDescription(
			`Delete one long-term-memory summary by SHA (the apply step of brain_reflect). `+
				`Pass the SHA of an approved forget-candidate. The delete takes effect on the `+
				`next online brain_recall against the daemon.`,
		),
		mcp.WithString("sha",
			mcp.Required(),
			mcp.Description("Content SHA of the summary doc to forget (64-char lowercase hex)."),
		),
	)
}

func (s *Server) handleForget(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.deps.Client == nil {
		return mcp.NewToolResultError("brain_forget requires the daemon (agent-contract mode); not available in legacy BRAIN_VAULT_PATH mode"), nil
	}
	sha, _ := req.RequireString("sha")
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return mcp.NewToolResultError("brain_forget requires a non-empty sha"), nil
	}

	resp, err := s.deps.Client.Forget(ctx, sha)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("brain_forget: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf(
		"brain_forget: forgot %s.\nNote: the removal takes effect on the next online "+
			"brain_recall against the daemon's Postgres store.",
		resp.SHA,
	)), nil
}
