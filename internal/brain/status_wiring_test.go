package brain

// Issue #130: brain_status misreported a healthy daemon connection.
// These tests pin the two wiring fixes:
//
//  1. Client.do() feeds the Connectivity tracker on every daemon
//     round-trip (reads included), not just the wqueue/drainer path.
//  2. Heartbeat touches propagate to Lifecycle.Snapshot() via the
//     OnTouch hook + hbLast overlay, so heartbeat_age doesn't grow
//     monotonically from born_at.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"log/slog"
)

func TestClientDo_NotesConnectivitySuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer ts.Close()

	conn := NewConnectivity()
	c, err := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok", Connectivity: conn})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}

	snap := conn.Snapshot()
	if snap.State != ConnOnline {
		t.Errorf("state = %s, want online", snap.State)
	}
	if snap.LastContactAt.IsZero() {
		t.Error("LastContactAt not set after successful read call")
	}
	if snap.LastSuccessAt.IsZero() {
		t.Error("LastSuccessAt not set after successful read call")
	}
}

func TestClientDo_NotesConnectivityAPIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"code":"BOOM","message":"nope"}}`)
	}))
	defer ts.Close()

	conn := NewConnectivity()
	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok", Connectivity: conn})
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("expected error")
	}

	snap := conn.Snapshot()
	// Never succeeded → stays offline, but the attempt IS recorded so
	// brain_status's last_daemon_contact_secs leaves -1.
	if snap.State != ConnOffline {
		t.Errorf("state = %s, want offline (no prior success)", snap.State)
	}
	if snap.LastContactAt.IsZero() {
		t.Error("LastContactAt not set after failed attempt")
	}
	if snap.LastError == "" {
		t.Error("LastError empty after API error")
	}
}

func TestClientDo_TransportFailureDegradesFromOnline(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))

	conn := NewConnectivity()
	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok", Connectivity: conn})
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	ts.Close() // daemon goes away

	err := c.Health(context.Background())
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("err = %v, want ErrDaemonUnreachable", err)
	}
	if got := conn.Snapshot().State; got != ConnDegraded {
		t.Errorf("state = %s, want degraded (success then failure)", got)
	}
}

func TestClientDo_NilConnectivityIsSafe(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer ts.Close()

	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok"})
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("health without connectivity: %v", err)
	}
}

func TestHeartbeat_OnTouchFires(t *testing.T) {
	dir := t.TempDir()
	touched := make(chan string, 16)
	hb, err := StartHeartbeat(context.Background(), HeartbeatOpts{
		BrainDir: dir,
		Interval: 10 * time.Millisecond,
		Logger:   slog.New(slog.DiscardHandler),
		OnTouch:  func(now string) { touched <- now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hb.Stop() }()

	// The first touch is synchronous inside StartHeartbeat.
	select {
	case now := <-touched:
		if _, perr := time.Parse(time.RFC3339, now); perr != nil {
			t.Errorf("OnTouch timestamp %q not RFC3339: %v", now, perr)
		}
	default:
		t.Fatal("OnTouch did not fire on the synchronous initial touch")
	}

	// And it keeps firing from the ticker goroutine.
	select {
	case <-touched:
	case <-time.After(2 * time.Second):
		t.Fatal("OnTouch did not fire from the ticker")
	}
}

func TestSnapshot_OverlaysHeartbeat(t *testing.T) {
	born := "2026-07-07T00:00:00Z"
	lc := &Lifecycle{manifest: &Manifest{BrainID: "b1", LastHeartbeat: born}}

	// No touches yet → manifest value passes through untouched.
	if got := lc.Snapshot().LastHeartbeat; got != born {
		t.Errorf("LastHeartbeat = %q, want manifest value %q", got, born)
	}

	fresh := "2026-07-07T00:05:00Z"
	lc.hbLast.Store(fresh)
	if got := lc.Snapshot().LastHeartbeat; got != fresh {
		t.Errorf("LastHeartbeat = %q, want overlaid %q", got, fresh)
	}
	// The overlay must not mutate the underlying manifest.
	if lc.manifest.LastHeartbeat != born {
		t.Errorf("manifest mutated: %q", lc.manifest.LastHeartbeat)
	}
}
