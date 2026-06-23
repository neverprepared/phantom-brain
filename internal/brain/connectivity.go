package brain

import (
	"sync"
	"time"
)

// ConnState is the agent's current view of daemon reachability.
type ConnState string

const (
	// ConnOffline: no successful daemon contact this process lifetime.
	// Initial state at Lifecycle.Start().
	ConnOffline ConnState = "offline"
	// ConnDegraded: at least one prior success, most recent attempt failed.
	ConnDegraded ConnState = "degraded"
	// ConnOnline: most recent attempt succeeded.
	ConnOnline ConnState = "online"
)

// Connectivity is the per-Lifecycle state holder. Concurrency-safe.
// Owned by Lifecycle; mutated by both the MCP-thread immediate-POST
// in Enqueue paths AND the drainer goroutine.
type Connectivity struct {
	mu            sync.Mutex
	state         ConnState
	lastContactAt time.Time
	lastSuccessAt time.Time
	lastError     string
}

// NewConnectivity returns a Connectivity in ConnOffline.
func NewConnectivity() *Connectivity {
	return &Connectivity{state: ConnOffline}
}

// ConnectivitySnapshot is the read-side defensive copy.
type ConnectivitySnapshot struct {
	State         ConnState
	LastContactAt time.Time
	LastSuccessAt time.Time
	LastError     string
}

// NoteSuccess records a successful daemon POST. Flips state to
// ConnOnline regardless of prior state. Clears LastError.
func (c *Connectivity) NoteSuccess(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = ConnOnline
	c.lastContactAt = now
	c.lastSuccessAt = now
	c.lastError = ""
}

// NoteFailure records a failed daemon attempt. Flips ConnOnline ->
// ConnDegraded; leaves ConnOffline alone (no prior success means we
// stay offline).
func (c *Connectivity) NoteFailure(now time.Time, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastContactAt = now
	if err != nil {
		c.lastError = err.Error()
	}
	if c.state == ConnOnline {
		c.state = ConnDegraded
	}
}

// Snapshot returns a defensive copy.
func (c *Connectivity) Snapshot() ConnectivitySnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ConnectivitySnapshot{
		State:         c.state,
		LastContactAt: c.lastContactAt,
		LastSuccessAt: c.lastSuccessAt,
		LastError:     c.lastError,
	}
}
