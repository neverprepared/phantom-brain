package brain

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// TestRecordWrite_BumpsCounter is the trivial atomic-add property.
// Worth one assertion so a future refactor that swaps the storage
// can't silently regress.
func TestRecordWrite_BumpsCounter(t *testing.T) {
	lc, err := Start(StartOpts{
		Agent:            agentForTest(t),
		Platform:         newFakePlatform(),
		Logger:           slog.New(slog.DiscardHandler),
		SkipCheckpointer: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _, _ = lc.Shutdown(context.Background()) })

	if lc.WriteCount() != 0 {
		t.Fatalf("initial count = %d, want 0", lc.WriteCount())
	}
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lc.RecordWrite()
		}()
	}
	wg.Wait()
	if lc.WriteCount() != 25 {
		t.Errorf("after 25 concurrent RecordWrite, count = %d, want 25", lc.WriteCount())
	}
}

// TestCheckpointer_FiresWhenWritesThresholdMet stress-tests the
// automatic cadence: feed the counter past CheckpointWrites within a
// short tick window and verify Checkpoint() runs (manifest's
// LastCheckpointWrites advances + WriteCount resets).
//
// The default Agent config sets CheckpointMinIntervalSecs to 300s,
// which would block ShouldCheckpoint forever in test time. We work
// around it by setting the manifest's LastCheckpointAt to a value
// far enough in the past that the min-interval has already elapsed.
func TestCheckpointer_FiresWhenWritesThresholdMet(t *testing.T) {
	agent := agentForTest(t)
	// Tiny threshold so we don't have to bump 50 times in a test.
	agent.CheckpointWrites = 3

	lc, err := Start(StartOpts{
		Agent:                  agent,
		Platform:               newFakePlatform(),
		Logger:                 slog.New(slog.DiscardHandler),
		CheckpointTickInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _, _ = lc.Shutdown(context.Background()) })

	// Rewind the manifest's last-checkpoint so ShouldCheckpoint's
	// min-interval predicate clears immediately.
	m, _ := ReadManifest(lc.BrainDir())
	m.LastCheckpointAt = time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	if err := WriteManifest(lc.BrainDir(), m); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		lc.RecordWrite()
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lc.WriteCount() == 0 {
			// Counter reset → a checkpoint ran.
			m2, _ := ReadManifest(lc.BrainDir())
			if m2.LastCheckpointWrites < 3 {
				t.Errorf("LastCheckpointWrites=%d, want >=3", m2.LastCheckpointWrites)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("checkpointer did not fire within deadline; WriteCount=%d", lc.WriteCount())
}

// TestCheckpointer_StopsOnShutdown verifies the ticker goroutine
// exits when Shutdown is called. Without this, the goroutine would
// outlive the process and the deferred fixture cleanup would race.
func TestCheckpointer_StopsOnShutdown(t *testing.T) {
	lc, err := Start(StartOpts{
		Agent:                  agentForTest(t),
		Platform:               newFakePlatform(),
		Logger:                 slog.New(slog.DiscardHandler),
		CheckpointTickInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := lc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// ckptDone must be closed; reading from it should not block.
	select {
	case <-lc.ckptDone:
	case <-time.After(2 * time.Second):
		t.Fatal("checkpointer did not exit after Shutdown")
	}
}

// TestCheckpointer_SkippedWhenThresholdNotMet verifies the loop
// doesn't fire spurious checkpoints when neither write threshold
// nor idle/age gaps have been met. Negative test for the cadence.
func TestCheckpointer_SkippedWhenThresholdNotMet(t *testing.T) {
	lc, err := Start(StartOpts{
		Agent:                  agentForTest(t),
		Platform:               newFakePlatform(),
		Logger:                 slog.New(slog.DiscardHandler),
		CheckpointTickInterval: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _, _ = lc.Shutdown(context.Background()) })

	before, _ := ReadManifest(lc.BrainDir())
	time.Sleep(150 * time.Millisecond) // multiple tick cycles
	after, _ := ReadManifest(lc.BrainDir())
	if after.LastCheckpointAt != before.LastCheckpointAt {
		t.Errorf("checkpoint fired without threshold met: %q -> %q",
			before.LastCheckpointAt, after.LastCheckpointAt)
	}
}
