package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

// brainStatusTool exposes the brain's manifest + heartbeat freshness
// + ship-queue depth as a JSON blob. Used by operators (and the agent
// itself) to introspect "am I a healthy brain right now?" without
// having to read manifest.json off disk.
func brainStatusTool() mcp.Tool {
	return mcp.NewTool("brain_status",
		mcp.WithDescription(
			`Return the running brain's manifest, heartbeat age in seconds, and ship-queue `+
				`depth (count + bytes). Returns an error in legacy BRAIN_VAULT_PATH mode where `+
				`no Lifecycle has been started.`,
		),
	)
}

// brainStatusResponse is the JSON shape returned to operators.
// Documented inline so the schema is discoverable from the source —
// MCP clients don't currently get a typed output schema.
type brainStatusResponse struct {
	BrainID          string         `json:"brain_id"`
	BrainDir         string         `json:"brain_dir"`
	Manifest         brain.Manifest `json:"manifest"`
	HeartbeatAgeSecs int64          `json:"heartbeat_age_secs"` // -1 if unparseable
}

func (s *Server) handleBrainStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.deps.Lifecycle == nil {
		return mcp.NewToolResultError("brain_status: lifecycle not initialised (legacy BRAIN_VAULT_PATH mode)"), nil
	}
	lc := s.deps.Lifecycle
	m := lc.Snapshot()

	ageSecs := int64(-1)
	if m.LastHeartbeat != "" {
		if t, err := time.Parse(time.RFC3339, m.LastHeartbeat); err == nil {
			ageSecs = int64(time.Since(t).Seconds())
		}
	}

	resp := brainStatusResponse{
		BrainID:          m.BrainID,
		BrainDir:         lc.BrainDir(),
		Manifest:         m,
		HeartbeatAgeSecs: ageSecs,
	}
	body, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("brain_status: marshal: %v", err)), nil
	}
	return mcp.NewToolResultText(string(body)), nil
}
