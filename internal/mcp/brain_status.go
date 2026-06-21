package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/neverprepared/mcp-phantom-brain/internal/brain"
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
	BrainID            string         `json:"brain_id"`
	BrainDir           string         `json:"brain_dir"`
	Manifest           brain.Manifest `json:"manifest"`
	HeartbeatAgeSecs   int64          `json:"heartbeat_age_secs"` // -1 if unparseable
	ShipQueueCount     int            `json:"ship_queue_count"`
	ShipQueueBytes     int64          `json:"ship_queue_bytes"`
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

	// Ship-queue depth is best-effort — a failing read should not
	// keep brain_status from returning the rest of the snapshot.
	items, _ := brain.ListShipQueue(s.deps.Lifecycle.Agent())
	var bytes int64
	for _, it := range items {
		bytes += it.SizeBytes
	}

	resp := brainStatusResponse{
		BrainID:          m.BrainID,
		BrainDir:         lc.BrainDir(),
		Manifest:         m,
		HeartbeatAgeSecs: ageSecs,
		ShipQueueCount:   len(items),
		ShipQueueBytes:   bytes,
	}
	body, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("brain_status: marshal: %v", err)), nil
	}
	return mcp.NewToolResultText(string(body)), nil
}
