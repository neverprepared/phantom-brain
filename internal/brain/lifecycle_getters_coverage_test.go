package brain

import (
	"testing"

	"github.com/neverprepared/phantom-brain/internal/config"
)

// TestLifecycleAccessors covers the thin getters MCP handlers branch on
// (Client/Queue/Connectivity/Agent/BrainDir/VaultDir) plus the
// nil-receiver tolerance the doc comments promise.
func TestLifecycleAccessors(t *testing.T) {
	c, err := NewClient(ClientOpts{BaseURL: "https://x.invalid", Token: "t"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	lc := NewLifecycleForTest(nil, c)
	// NewLifecycleForTest only wires client/queue/conn/manifest; set the
	// path + agent fields directly (same package) to cover those getters.
	lc.brainDir = "/tmp/brains/abc"
	lc.agent = &config.Agent{Profile: "personal", Vault: "memory"}

	if lc.Client() != c {
		t.Error("Client() did not return the injected client")
	}
	if lc.Queue() != nil {
		t.Error("Queue() should be nil when constructed with nil queue")
	}
	if lc.Connectivity() == nil {
		t.Error("Connectivity() should be non-nil after NewLifecycleForTest")
	}
	if lc.Agent() == nil || lc.Agent().Profile != "personal" {
		t.Errorf("Agent() = %+v", lc.Agent())
	}
	if lc.BrainDir() != "/tmp/brains/abc" {
		t.Errorf("BrainDir() = %q", lc.BrainDir())
	}
	if lc.VaultDir() != "/tmp/brains/abc/vault" {
		t.Errorf("VaultDir() = %q", lc.VaultDir())
	}
}

// TestLifecycleNilReceivers asserts the documented nil-receiver guards
// on Queue() and Connectivity() — call sites may hold a nil *Lifecycle
// in legacy mode and must not panic.
func TestLifecycleNilReceivers(t *testing.T) {
	var lc *Lifecycle
	if lc.Queue() != nil {
		t.Error("nil-Lifecycle Queue() should return nil")
	}
	if lc.Connectivity() != nil {
		t.Error("nil-Lifecycle Connectivity() should return nil")
	}
}

// TestLifecycleRecordWriteAndSnapshot covers the write counter hook and
// the defensive manifest Snapshot copy.
func TestLifecycleRecordWriteAndSnapshot(t *testing.T) {
	lc := NewLifecycleForTest(nil, nil)
	lc.manifest = &Manifest{BrainID: "wm", Status: StatusAlive}
	if lc.WriteCount() != 0 {
		t.Fatalf("fresh WriteCount = %d, want 0", lc.WriteCount())
	}
	lc.RecordWrite()
	lc.RecordWrite()
	if lc.WriteCount() != 2 {
		t.Errorf("WriteCount = %d, want 2", lc.WriteCount())
	}
	// Snapshot returns a value copy — mutating it must not leak back.
	snap := lc.Snapshot()
	if snap.BrainID != "wm" {
		t.Errorf("Snapshot BrainID = %q", snap.BrainID)
	}
	snap.BrainID = "tampered"
	if lc.Snapshot().BrainID != "wm" {
		t.Error("Snapshot is not a defensive copy")
	}
}
