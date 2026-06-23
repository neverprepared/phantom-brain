package brain

import (
	"time"

	"github.com/neverprepared/phantom-brain/internal/brain/wqueue"
)

// NewLifecycleForTest constructs a minimal Lifecycle wired only with
// the fields the MCP write-path tests need (queue, connectivity,
// snapshotBuiltAt, client). Bypasses Birth/Heartbeat/Checkpointer.
//
// Test-only — production code must use Start.
func NewLifecycleForTest(q *wqueue.Queue, client *Client, builtAt time.Time) *Lifecycle {
	return &Lifecycle{
		client:          client,
		queue:           q,
		conn:            NewConnectivity(),
		snapshotBuiltAt: builtAt,
		manifest:        &Manifest{},
	}
}
