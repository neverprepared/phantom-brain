package brain

import (
	"sync"
	"time"
)

// ConnState is the agent's current view of daemon reachability.
type ConnState string

const (
	// ConnOffline: no successful daemon contact this process lifetime.
	ConnOffline ConnState = "offline"
	// ConnDegraded: at least one prior success, most recent attempt failed.
	ConnDegraded ConnState = "degraded"
	// ConnOnline: most recent attempt succeeded.
	ConnOnline ConnState = "online"
)

// Connectivity is the per-Lifecycle state holder. Concurrency-safe.
// Mutated by every code path that touches the daemon (write tools'
// immediate-POST, drainer goroutine, snapshot fetcher).
type Connectivity struct {
	mu            sync.Mutex
	state         ConnState
	lastContact   time.Time
	lastSuccess   time.Time
	lastError     string
}

// NewConnectivity returns a fresh ConnOffline holder.
func NewConnectivity() *Connectivity {
	return &Connectivity{state: ConnOffline}
}

// ConnectivitySnapshot is the defensive-copy read shape.
type ConnectivitySnapshot struct {
	State         ConnState
	LastContactAt time.Time
	LastSuccessAt time.Time
	LastError     string
}

// NoteSuccess flips state to ConnOnline regardless of prior state and
// stamps last_contact + last_success.
func (c *Connectivity) NoteSuccess(now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = ConnOnline
	c.lastContact = now
	c.lastSuccess = now
	c.lastError = ""
}

// NoteFailure flips ConnOnline -> ConnDegraded (and leaves ConnOffline
// at ConnOffline since no prior success means we never were online).
func (c *Connectivity) NoteFailure(now time.Time, err error) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastContact = now
	if err != nil {
		c.lastError = err.Error()
	}
	if c.state == ConnOnline {
		c.state = ConnDegraded
	}
	// ConnOffline -> stays ConnOffline; ConnDegraded -> stays ConnDegraded.
}

// Snapshot returns a defensive copy.
func (c *Connectivity) Snapshot() ConnectivitySnapshot {
	if c == nil {
		return ConnectivitySnapshot{State: ConnOffline}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return ConnectivitySnapshot{
		State:         c.state,
		LastContactAt: c.lastContact,
		LastSuccessAt: c.lastSuccess,
		LastError:     c.lastError,
	}
}
