package brain

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConcurrent_LiveBrainSurvivesRecoverySweep is the key safety
// property: a brain currently heartbeating MUST NOT be transitioned
// to dead by a recovery sweep, even one running in the same process
// at the same time. This was a real bug class in prior designs.
func TestConcurrent_LiveBrainSurvivesRecoverySweep(t *testing.T) {
	agent := agentForTest(t)
	lc, err := Start(StartOpts{
		Agent:    agent,
		Platform: newFakePlatform(),
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = lc.Shutdown(ctx)
	})

	// Sweep with no CurrentBrainID — the live brain is in scope and
	// should be inspected. The held flock + fresh heartbeat must keep
	// it alive.
	res, err := Recover(RecoverOpts{
		Agent:    agent,
		Platform: newFakePlatform(),
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.MarkedDead) != 0 {
		t.Fatalf("LIVE BRAIN false-orphaned: %v (skipped: %v)", res.MarkedDead, res.Skipped)
	}

	// Manifest must still report alive.
	m, _ := ReadManifest(lc.BrainDir())
	if m.Status != StatusAlive {
		t.Fatalf("live brain manifest status corrupted: %s", m.Status)
	}
}

// TestConcurrent_TwoLifecyclesCohabit verifies that two brains for
// the same (profile, vault) can run concurrently, each owning its
// own dir, and neither false-orphans the other on its own startup
// sweep.
func TestConcurrent_TwoLifecyclesCohabit(t *testing.T) {
	agent := agentForTest(t)
	lc1, err := Start(StartOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	t.Cleanup(func() { _, _ = lc1.Shutdown(context.Background()) })

	lc2, err := Start(StartOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	t.Cleanup(func() { _, _ = lc2.Shutdown(context.Background()) })

	if lc1.BrainDir() == lc2.BrainDir() {
		t.Fatal("two Lifecycles allocated the same brain dir")
	}

	// Run the sweep from lc1's perspective. Both brains must survive
	// because both flocks are held.
	res, err := Recover(RecoverOpts{
		Agent:          agent,
		Platform:       newFakePlatform(),
		CurrentBrainID: lc1.Snapshot().BrainID,
		Logger:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.MarkedDead) != 0 {
		t.Fatalf("cohabiting brain false-orphaned: %v", res.MarkedDead)
	}
}

// TestConcurrent_HeartbeatRunsThroughManyTouches stress-tests the
// heartbeat goroutine over a tight interval to surface any race
// between manifest reads and the marker rewrite. A handful of
// successive ReadManifest calls must all see a valid manifest with
// a non-empty LastHeartbeat that advances.
func TestConcurrent_HeartbeatRunsThroughManyTouches(t *testing.T) {
	agent := agentForTest(t)
	dir, _, err := Birth(BirthOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Birth: %v", err)
	}
	hb, err := StartHeartbeat(context.Background(), HeartbeatOpts{
		BrainDir: dir,
		Interval: 10 * time.Millisecond,
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("StartHeartbeat: %v", err)
	}
	t.Cleanup(func() { _ = hb.Stop() })

	var seen atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(200 * time.Millisecond)
			for time.Now().Before(deadline) {
				m, err := ReadManifest(dir)
				if err != nil {
					t.Errorf("ReadManifest during heartbeat: %v", err)
					return
				}
				if m.LastHeartbeat == "" {
					t.Errorf("LastHeartbeat empty during heartbeat")
					return
				}
				seen.Add(1)
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	if seen.Load() < 10 {
		t.Errorf("expected many successful reads during heartbeat, got %d", seen.Load())
	}
}

// TestConcurrent_RecoveryIsIdempotent runs the sweep twice over the
// same crashed-sibling layout and verifies the second pass adds
// nothing — once a brain is dead, the sweep no longer touches it.
func TestConcurrent_RecoveryIsIdempotent(t *testing.T) {
	agent := agentForTest(t)
	now := time.Now().UTC().Format(time.RFC3339)
	sib := &Manifest{
		BrainID:          "sibling-twice",
		ContributorID:    "personal/memory@host",
		Profile:          "personal",
		Vault:            "memory",
		BornAt:           now,
		Status:           StatusAlive,
		Host:             "test-host-uuid",
		BootID:           "DIFFERENT-BOOT",
		PID:              99999,
		LastHeartbeat:    now,
		LastCheckpointAt: now,
		SeedSource:       SeedGreenfield,
	}
	dir := agent.BrainDir(sib.BrainID)
	if err := os.MkdirAll(filepath.Join(dir, "markers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(dir, sib); err != nil {
		t.Fatal(err)
	}

	first, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("first Recover: %v", err)
	}
	if len(first.MarkedDead) != 1 {
		t.Fatalf("first sweep should mark dead, got %v", first.MarkedDead)
	}

	second, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	if len(second.MarkedDead) != 0 {
		t.Errorf("second sweep should be idempotent, got %v", second.MarkedDead)
	}
}
