package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/neverprepared/mcp-phantom-brain/internal/brain"
)

// brainDeathTool exposes Lifecycle.Shutdown via MCP. Calling it
// transitions the brain to dead and packs the trimmed payload into
// the local ship queue. After it returns, other brain_* / task_*
// tools that touch live state will return errors — the MCP server
// itself stays up so the operator can read the response, then SIGTERM
// the process at their leisure.
//
// In Phase 1 the payload sits locally forever; Phase 2 daemon picks
// it up.
func brainDeathTool() mcp.Tool {
	return mcp.NewTool("brain_death",
		mcp.WithDescription(
			`Transition this brain to dead and pack the trimmed death payload (manifest + `+
				`vault/Raw/) into the local ship queue under XDG_DATA_HOME/phantom-brain/`+
				`{profile}/{vault}/_pending/<brain_id>/death-<unix>.tar. In Phase 1 the payload `+
				`stays local until the Phase 2 daemon ships.`,
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
		"brain_death: payload %d bytes at %s (brain_id=%s)",
		res.PayloadSize, res.PayloadPath, res.BrainID,
	)), nil
}
