package brain

import (
	"github.com/neverprepared/phantom-brain/internal/brain/wqueue"
)

// NewLifecycleForTest constructs a minimal Lifecycle wired only with
// the fields the MCP write-path tests need (queue, connectivity,
// client). Bypasses Birth/Heartbeat/Checkpointer.
//
// Test-only — production code must use Start.
func NewLifecycleForTest(q *wqueue.Queue, client *Client) *Lifecycle {
	return &Lifecycle{
		client:   client,
		queue:    q,
		conn:     NewConnectivity(),
		manifest: &Manifest{},
	}
}
