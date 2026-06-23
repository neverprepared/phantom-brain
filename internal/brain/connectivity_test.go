package brain

import (
	"errors"
	"testing"
	"time"
)

func TestConnectivityInitialState(t *testing.T) {
	c := NewConnectivity()
	s := c.Snapshot()
	if s.State != ConnOffline {
		t.Fatalf("initial state = %s, want offline", s.State)
	}
	if !s.LastSuccessAt.IsZero() || !s.LastContactAt.IsZero() {
		t.Fatalf("timestamps non-zero on fresh state: %+v", s)
	}
}

func TestConnectivitySuccessFlipsOnline(t *testing.T) {
	c := NewConnectivity()
	now := time.Now()
	c.NoteSuccess(now)
	s := c.Snapshot()
	if s.State != ConnOnline {
		t.Fatalf("state = %s, want online", s.State)
	}
	if !s.LastSuccessAt.Equal(now) || !s.LastContactAt.Equal(now) {
		t.Fatalf("timestamps not stamped: %+v", s)
	}
	if s.LastError != "" {
		t.Fatalf("LastError = %q, want empty", s.LastError)
	}
}

func TestConnectivityFailureFromOnlineGoesDegraded(t *testing.T) {
	c := NewConnectivity()
	c.NoteSuccess(time.Now())
	c.NoteFailure(time.Now(), errors.New("boom"))
	s := c.Snapshot()
	if s.State != ConnDegraded {
		t.Fatalf("state = %s, want degraded", s.State)
	}
	if s.LastError != "boom" {
		t.Fatalf("LastError = %q", s.LastError)
	}
}

func TestConnectivityFailureFromOfflineStaysOffline(t *testing.T) {
	c := NewConnectivity()
	c.NoteFailure(time.Now(), errors.New("nope"))
	if c.Snapshot().State != ConnOffline {
		t.Fatalf("state = %s, want offline", c.Snapshot().State)
	}
}

func TestConnectivitySuccessFromDegradedReturnsOnline(t *testing.T) {
	c := NewConnectivity()
	c.NoteSuccess(time.Now())
	c.NoteFailure(time.Now(), errors.New("x"))
	c.NoteSuccess(time.Now())
	if c.Snapshot().State != ConnOnline {
		t.Fatalf("did not return to online")
	}
}

func TestConnectivitySnapshotIsDefensive(t *testing.T) {
	c := NewConnectivity()
	c.NoteSuccess(time.Now())
	s := c.Snapshot()
	s.State = "tampered"
	if c.Snapshot().State != ConnOnline {
		t.Fatal("snapshot mutation leaked back to holder")
	}
}
