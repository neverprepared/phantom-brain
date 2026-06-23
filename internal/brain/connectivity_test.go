package brain

import (
	"errors"
	"testing"
	"time"
)

func TestConnectivityInitialState(t *testing.T) {
	c := NewConnectivity()
	snap := c.Snapshot()
	if snap.State != ConnOffline {
		t.Errorf("initial state = %q, want %q", snap.State, ConnOffline)
	}
	if !snap.LastSuccessAt.IsZero() {
		t.Errorf("LastSuccessAt not zero")
	}
}

func TestConnectivitySuccessFlipsOnline(t *testing.T) {
	c := NewConnectivity()
	c.NoteSuccess(time.Now())
	if c.Snapshot().State != ConnOnline {
		t.Errorf("after first success, want %q got %q", ConnOnline, c.Snapshot().State)
	}
}

func TestConnectivityFailureFromOnlineGoesDegraded(t *testing.T) {
	c := NewConnectivity()
	c.NoteSuccess(time.Now())
	c.NoteFailure(time.Now(), errors.New("boom"))
	snap := c.Snapshot()
	if snap.State != ConnDegraded {
		t.Errorf("state = %q, want %q", snap.State, ConnDegraded)
	}
	if snap.LastError != "boom" {
		t.Errorf("LastError = %q", snap.LastError)
	}
}

func TestConnectivityFailureFromOfflineStaysOffline(t *testing.T) {
	c := NewConnectivity()
	c.NoteFailure(time.Now(), errors.New("net"))
	if c.Snapshot().State != ConnOffline {
		t.Errorf("offline should stay offline on failure")
	}
}

func TestConnectivityNilReceiverIsSafe(t *testing.T) {
	var c *Connectivity
	c.NoteSuccess(time.Now())
	c.NoteFailure(time.Now(), nil)
	snap := c.Snapshot()
	if snap.State != ConnOffline {
		t.Errorf("nil snapshot state = %q, want %q", snap.State, ConnOffline)
	}
}
