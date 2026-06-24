package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// brain_reflect (issue #72, Phase 1). Read-only maintenance report:
// asks the daemon for forget-candidate SHAs and surfaces them so the
// operator/agent can decide what to brain_forget. Nothing is deleted
// here — this is the "propose" half of propose-then-apply.
//
// Phase 1 detector: stale-gate only (summaries the synth gate never
// enriched). Orphan-entity + near-dup detectors are Phase 2.
func reflectTool() mcp.Tool {
	return mcp.NewTool("brain_reflect",
		mcp.WithDescription(
			`Report long-term-memory forget-candidates (read-only). Phase 1 surfaces `+
				`"stale-gate" docs: summaries the synthesis gate never enriched. Review the `+
				`list, then call brain_forget on the SHAs you approve. Deletes nothing itself.`,
		),
	)
}

func (s *Server) handleReflect(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.deps.Client == nil {
		return mcp.NewToolResultError("brain_reflect requires the daemon (agent-contract mode); not available in legacy BRAIN_VAULT_PATH mode"), nil
	}
	resp, err := s.deps.Client.Reflect(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("brain_reflect: %v", err)), nil
	}
	if len(resp.Candidates) == 0 {
		return mcp.NewToolResultText("brain_reflect: no stale-gate candidates. Long-term memory looks clean."), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "brain_reflect: %d forget-candidate(s)\n\n", len(resp.Candidates))
	for _, c := range resp.Candidates {
		title := c.Title
		if strings.TrimSpace(title) == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&b, "- %s\n  %s\n  reason: %s\n", c.SHA, title, c.Reason)
	}
	b.WriteString("\nReview these, then call brain_forget(sha) on the ones you approve. " +
		"reflect only proposes — nothing is deleted until you forget.")
	return mcp.NewToolResultText(b.String()), nil
}
