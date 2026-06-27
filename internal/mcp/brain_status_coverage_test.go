package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"net/http"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

// TestBrainStatus_OnlineAfterSuccess: a prior successful daemon contact
// flips connectivity online and yields a non-negative
// last_daemon_contact_secs (the !LastContactAt.IsZero() branch). The
// existing integration test only covers the offline-after-failure case.
func TestBrainStatus_OnlineAfterSuccess(t *testing.T) {
	s, _, lc, cleanup := setupWithQueue(t, 3, map[string][]float32{}, http.HandlerFunc(always200))
	defer cleanup()

	lc.Connectivity().NoteSuccess(time.Now())

	text, isErr := callTool(t, s.handleBrainStatus, map[string]any{})
	if isErr {
		t.Fatalf("brain_status error: %s", text)
	}
	var got brainStatusResponse
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode brain_status: %v", err)
	}
	if got.Connectivity != string(brain.ConnOnline) {
		t.Errorf("connectivity = %q, want online", got.Connectivity)
	}
	if got.LastDaemonContactSec < 0 {
		t.Errorf("last_daemon_contact_secs = %d, want >= 0 after a success", got.LastDaemonContactSec)
	}
	// No queued writes — nothing failed.
	if got.QueuedWrites != 0 {
		t.Errorf("queued_writes = %d, want 0", got.QueuedWrites)
	}
	// HeartbeatAgeSecs is -1 for the test lifecycle (empty manifest, no
	// heartbeat string to parse).
	if got.HeartbeatAgeSecs != -1 {
		t.Errorf("heartbeat_age_secs = %d, want -1 (no heartbeat)", got.HeartbeatAgeSecs)
	}
}

// TestBrainStatus_LegacyModeErrors: with no Lifecycle wired (legacy
// BRAIN_VAULT_PATH mode) brain_status returns a clear tool error rather
// than a half-populated manifest.
func TestBrainStatus_LegacyModeErrors(t *testing.T) {
	s, _ := setup(t, 3, map[string][]float32{})
	text, isErr := callTool(t, s.handleBrainStatus, map[string]any{})
	if !isErr {
		t.Fatalf("expected error in legacy mode, got: %s", text)
	}
}
