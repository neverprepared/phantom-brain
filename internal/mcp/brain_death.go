package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

// brainDeathTool exposes Lifecycle.Shutdown via MCP. Calling it
// transitions the brain to dead and marks the manifest + log. After it
// returns, other brain_* / task_* tools that touch live state will
// return errors — the MCP server itself stays up so the operator can
// read the response, then SIGTERM the process at their leisure.
//
// Post-cutover there is no death payload tarball: every write went to
// the daemon as it happened, so shutdown is just a status flip.
func brainDeathTool() mcp.Tool {
	return mcp.NewTool("brain_death",
		mcp.WithDescription(
			`Mark this brain dead: flip its manifest status and log the shutdown. Writes `+
				`already reached the daemon as they happened, so there is nothing to ship — `+
				`this is a clean teardown, not a flush. Live brain_* / task_* tools error afterward.`,
		),
	)
}

func (s *Server) handleBrainDeath(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.deps.Lifecycle == nil {
		return mcp.NewToolResultError("brain_death: lifecycle not initialised (legacy BRAIN_VAULT_PATH mode)"), nil
	}
	res, err := s.deps.Lifecycle.Shutdown(ctx)
	if err != nil {
		if brain.IsAlreadyShutDown(err) {
			return mcp.NewToolResultText("brain_death: already shut down (no-op)"), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("brain_death: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf(
		"brain_death: brain %s marked dead (writes already persisted to daemon; nothing to ship)",
		res.BrainID,
	)), nil
}
