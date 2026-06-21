package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// brainCheckpointTool exposes Lifecycle.Checkpoint via MCP. Operators
// (and the agent) call it to advance the brain's checkpoint state on
// demand — handy for incident response ("force a checkpoint NOW") or
// as part of a manual cadence before automatic triggering lands in
// Phase 4.
func brainCheckpointTool() mcp.Tool {
	return mcp.NewTool("brain_checkpoint",
		mcp.WithDescription(
			`Run the checkpoint flow. Skips if the v4.4 mtime-cutoff predicate says no `+
				`unless force=true. Returns the checkpoint directory path or a "skipped" `+
				`message with the threshold reason.`,
		),
		mcp.WithNumber("writes",
			mcp.Description("Number of mutations since the last checkpoint. Optional; default 0. Participates in the threshold check unless force=true."),
		),
		mcp.WithBoolean("force",
			mcp.Description("Bypass the cadence thresholds and checkpoint unconditionally. Use sparingly — checkpoints are not free."),
		),
	)
}

func (s *Server) handleBrainCheckpoint(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.deps.Lifecycle == nil {
		return mcp.NewToolResultError("brain_checkpoint: lifecycle not initialised (legacy BRAIN_VAULT_PATH mode)"), nil
	}
	writes := 0
	if v, err := req.RequireFloat("writes"); err == nil {
		writes = int(v)
	}
	force := false
	if v, err := req.RequireBool("force"); err == nil {
		force = v
	}
	res, err := s.deps.Lifecycle.Checkpoint(writes, force)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("brain_checkpoint: %v", err)), nil
	}
	if res.Skipped {
		return mcp.NewToolResultText(fmt.Sprintf("brain_checkpoint: skipped (%s)", res.Reason)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("brain_checkpoint: written to %s", res.CheckpointDir)), nil
}
