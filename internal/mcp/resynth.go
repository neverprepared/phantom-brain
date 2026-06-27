package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// brain_resynth (issue #82). The fix-it apply-companion to brain_reflect:
// docs can get stuck at Synthesised=false when a bulk ingest outruns the
// single CLI-bound synth worker and overflows its in-memory queue (the
// drop is logged, not retried). brain_reflect surfaces these as
// "stale-gate" candidates; brain_resynth re-processes them WITHOUT
// deleting anything — they get re-synthesised (kept), not forgotten.
//
// The backfill runs daemon-side in a background goroutine, serialized
// with the live worker, and bypasses the lossy enqueue channel so it
// can't drop the backlog a second time.
//
// dry_run defaults TRUE — report before mutating. Re-run with
// dry_run=false to apply.
func resynthTool() mcp.Tool {
	return mcp.NewTool("brain_resynth",
		mcp.WithDescription(
			`Re-synthesize long-term-memory docs stuck at Synthesised=false (dropped synth `+
				`jobs, issue #82). Defaults to a dry run: reports the backlog count + a sample. `+
				`Re-run with dry_run=false to start a background backfill that re-processes them. `+
				`Unlike brain_forget these docs are KEPT and re-synthesized, not deleted.`,
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("Report the backlog without mutating. Default true — re-run with false to apply."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Cap how many stuck docs the apply pass processes. Optional; 0 (default) means all. The reported backlog_count is always the true total."),
		),
	)
}

func (s *Server) handleResynth(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.deps.Client == nil {
		return mcp.NewToolResultError("brain_resynth requires the daemon (agent-contract mode); not available in legacy BRAIN_VAULT_PATH mode"), nil
	}

	// dry_run defaults TRUE — safe default: report before mutating.
	dryRun := true
	if v, err := req.RequireBool("dry_run"); err == nil {
		dryRun = v
	}
	limit := 0
	if v, err := req.RequireFloat("limit"); err == nil {
		limit = int(v)
	}

	resp, err := s.deps.Client.Resynth(ctx, dryRun, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("brain_resynth: %v", err)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "brain_resynth: %d doc(s) stuck at Synthesised=false (dropped synth jobs).\n", resp.BacklogCount)
	if len(resp.Sample) > 0 {
		fmt.Fprintf(&b, "\nSample (up to %d):\n", len(resp.Sample))
		for _, item := range resp.Sample {
			title := item.Title
			if strings.TrimSpace(title) == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&b, "- %s\n  %s\n", item.SHA, title)
		}
	}

	if dryRun {
		b.WriteString("\nDry run — nothing changed. Re-run with dry_run=false to re-synthesize. " +
			"(count drops as docs synthesize; re-run dry_run to watch progress.)")
	} else if resp.Started {
		fmt.Fprintf(&b, "\nStarted re-synthesis of %d doc(s) in the background. They synthesize "+
			"over time (CLI-bound); re-run brain_resynth with dry_run=true to watch backlog_count "+
			"fall. Results appear in online brain_recall as each doc finishes.", resp.Pending)
	} else {
		b.WriteString("\nNothing to re-synthesize — long-term memory looks fully synthesized.")
	}
	b.WriteString("\nNote: these docs are re-synthesized (kept), not deleted — that's brain_forget.")
	return mcp.NewToolResultText(b.String()), nil
}
