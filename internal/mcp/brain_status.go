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
// + connectivity / queued-writes. Used by operators (and the agent
// itself) to introspect "am I healthy right now?" without reading
// manifest.json off disk.
func brainStatusTool() mcp.Tool {
	return mcp.NewTool("brain_status",
		mcp.WithDescription(
			`Return the running brain's manifest, heartbeat age, daemon connectivity, `+
				`and queued-write depth (ages in seconds). Returns an error in `+
				`legacy BRAIN_VAULT_PATH mode where no Lifecycle has been started.`,
		),
	)
}

// brainStatusResponse is the JSON shape returned to operators.
type brainStatusResponse struct {
	BrainID          string         `json:"brain_id"`
	BrainDir         string         `json:"brain_dir"`
	Manifest         brain.Manifest `json:"manifest"`
	HeartbeatAgeSecs int64          `json:"heartbeat_age_secs"` // -1 if unparseable

	// Issue #61: degraded-mode visibility.
	Connectivity         string `json:"connectivity"`             // online|degraded|offline
	LastDaemonContactSec int64  `json:"last_daemon_contact_secs"` // -1 if never contacted
	QueuedWrites         int    `json:"queued_writes"`
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

	// Default to offline + -1 + 0 so legacy or partly-wired lifecycles
	// (no daemon client, no queue) emit a sensible shape.
	connState := string(brain.ConnOffline)
	lastContact := int64(-1)
	queued := 0
	if conn := lc.Connectivity(); conn != nil {
		snap := conn.Snapshot()
		connState = string(snap.State)
		if !snap.LastContactAt.IsZero() {
			lastContact = int64(time.Since(snap.LastContactAt).Seconds())
		}
	}
	if q := lc.Queue(); q != nil {
		if n, qerr := q.Depth(ctx); qerr == nil {
			queued = n
		}
	}

	resp := brainStatusResponse{
		BrainID:              m.BrainID,
		BrainDir:             lc.BrainDir(),
		Manifest:             m,
		HeartbeatAgeSecs:     ageSecs,
		Connectivity:         connState,
		LastDaemonContactSec: lastContact,
		QueuedWrites:         queued,
	}
	body, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("brain_status: marshal: %v", err)), nil
	}
	return mcp.NewToolResultText(string(body)), nil
}
