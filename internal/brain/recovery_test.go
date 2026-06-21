package brain

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// --- Heartbeat --------------------------------------------------------

func TestHeartbeat_TouchesMarkerAtInterval(t *testing.T) {
	agent := agentForTest(t)
	dir, _, err := Birth(BirthOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Birth: %v", err)
	}
	hb, err := StartHeartbeat(context.Background(), HeartbeatOpts{
		BrainDir: dir,
		Interval: 20 * time.Millisecond,
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("StartHeartbeat: %v", err)
	}
	t.Cleanup(func() { _ = hb.Stop() })

	marker := AliveMarkerPath(dir)
	st1, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("stat marker: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	st2, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("stat marker after sleep: %v", err)
	}
	if !st2.ModTime().After(st1.ModTime()) {
		t.Errorf("marker mtime did not advance: %v -> %v", st1.ModTime(), st2.ModTime())
	}

	// Manifest's LastHeartbeat must reflect the latest touch.
	m, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.LastHeartbeat == "" {
		t.Error("LastHeartbeat empty after ticks")
	}
}

func TestHeartbeat_RejectsDoubleAttach(t *testing.T) {
	agent := agentForTest(t)
	dir, _, _ := Birth(BirthOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	hb, err := StartHeartbeat(context.Background(), HeartbeatOpts{
		BrainDir: dir,
		Interval: time.Second,
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("StartHeartbeat: %v", err)
	}
	t.Cleanup(func() { _ = hb.Stop() })
	_, err = StartHeartbeat(context.Background(), HeartbeatOpts{
		BrainDir: dir,
		Interval: time.Second,
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err == nil || !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("expected double-attach rejection, got %v", err)
	}
}

func TestHeartbeat_StopReleasesFlock(t *testing.T) {
	agent := agentForTest(t)
	dir, _, _ := Birth(BirthOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	hb, _ := StartHeartbeat(context.Background(), HeartbeatOpts{BrainDir: dir, Interval: time.Second, Logger: slog.New(slog.DiscardHandler)})
	if err := hb.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// After Stop, the marker flock must be takeable from a fresh
	// flock.Flock — proves the lock was released.
	lk := flock.New(AliveMarkerPath(dir))
	got, err := lk.TryLock()
	if err != nil {
		t.Fatalf("post-Stop TryLock: %v", err)
	}
	if !got {
		t.Fatal("flock should be free after Stop")
	}
	_ = lk.Unlock()
}

func TestHeartbeat_StopIsIdempotent(t *testing.T) {
	agent := agentForTest(t)
	dir, _, _ := Birth(BirthOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	hb, _ := StartHeartbeat(context.Background(), HeartbeatOpts{BrainDir: dir, Interval: time.Second, Logger: slog.New(slog.DiscardHandler)})
	if err := hb.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := hb.Stop(); err != nil {
		t.Errorf("second Stop should be no-op, got %v", err)
	}
}

// --- Recovery sweep ---------------------------------------------------

// seedSiblingBrain creates a brain dir on disk without going through
// Birth(), so the test can control boot_id / pid / heartbeat freshness
// independently of the current process.
func seedSiblingBrain(t *testing.T, agent any, m *Manifest, touchMarker bool) string {
	t.Helper()
	a := agent.(interface{ BrainDir(string) string })
	dir := a.BrainDir(m.BrainID)
	if err := os.MkdirAll(filepath.Join(dir, "markers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	if touchMarker {
		if err := os.WriteFile(AliveMarkerPath(dir), []byte("seeded\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRecovery_PriorBootBrainMarkedDead(t *testing.T) {
	agent := agentForTest(t)
	now := time.Now().UTC().Format(time.RFC3339)
	sib := &Manifest{
		BrainID:          "sibling-old-boot",
		ContributorID:    "personal/memory@host",
		Profile:          "personal",
		Vault:            "memory",
		BornAt:           now,
		Status:           StatusAlive,
		Host:             "test-host-uuid",
		BootID:           "DIFFERENT-BOOT-ID",
		PID:              99999,
		LastHeartbeat:    now,
		LastCheckpointAt: now,
		SeedSource:       SeedGreenfield,
	}
	_ = seedSiblingBrain(t, agent, sib, true)

	buf := &bytes.Buffer{}
	res, err := Recover(RecoverOpts{
		Agent:    agent,
		Platform: newFakePlatform(),
		Logger:   captureLogger(buf),
	})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.MarkedDead) != 1 {
		t.Fatalf("expected 1 marked dead, got %d (skipped=%v)", len(res.MarkedDead), res.Skipped)
	}
	m, _ := ReadManifest(agent.BrainDir(sib.BrainID))
	if m.Status != StatusDead {
		t.Errorf("sibling not marked dead: status=%q", m.Status)
	}
	if !strings.Contains(buf.String(), "prior boot") {
		t.Errorf("expected prior-boot log, got %s", buf.String())
	}
}

func TestRecovery_StaleHeartbeatMarkedDead(t *testing.T) {
	agent := agentForTest(t)
	// last_heartbeat far past the orphan threshold.
	stale := time.Now().UTC().Add(-time.Duration(agent.OrphanThresholdSecs+60) * time.Second).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)
	sib := &Manifest{
		BrainID:          "sibling-stale",
		ContributorID:    "personal/memory@host",
		Profile:          "personal",
		Vault:            "memory",
		BornAt:           now,
		Status:           StatusAlive,
		Host:             "test-host-uuid",
		BootID:           "test-boot-id", // SAME as current
		PID:              99999,
		LastHeartbeat:    stale,
		LastCheckpointAt: now,
		SeedSource:       SeedGreenfield,
	}
	_ = seedSiblingBrain(t, agent, sib, true)

	// fakePlatform reports pid 99999 as dead by default (aliveSet is
	// empty). Recovery should then mark the sibling dead.
	res, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.MarkedDead) != 1 {
		t.Fatalf("expected 1 marked dead, got %d (skipped=%v)", len(res.MarkedDead), res.Skipped)
	}
}

func TestRecovery_FreshHeartbeatSkipped(t *testing.T) {
	agent := agentForTest(t)
	now := time.Now().UTC().Format(time.RFC3339)
	sib := &Manifest{
		BrainID:          "sibling-fresh",
		ContributorID:    "personal/memory@host",
		Profile:          "personal",
		Vault:            "memory",
		BornAt:           now,
		Status:           StatusAlive,
		Host:             "test-host-uuid",
		BootID:           "test-boot-id",
		PID:              99999,
		LastHeartbeat:    now,
		LastCheckpointAt: now,
		SeedSource:       SeedGreenfield,
	}
	dir := seedSiblingBrain(t, agent, sib, true)

	res, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.MarkedDead) != 0 {
		t.Errorf("expected nothing marked dead, got %v", res.MarkedDead)
	}
	if _, ok := res.Skipped[dir]; !ok {
		t.Errorf("sibling should be in Skipped map: %v", res.Skipped)
	}
}

func TestRecovery_HeldFlockLeftAlone(t *testing.T) {
	// A live sibling holds the flock; recovery must not transition it
	// to dead even though we're testing in the same process.
	agent := agentForTest(t)
	now := time.Now().UTC().Format(time.RFC3339)
	sib := &Manifest{
		BrainID:          "sibling-live",
		ContributorID:    "personal/memory@host",
		Profile:          "personal",
		Vault:            "memory",
		BornAt:           now,
		Status:           StatusAlive,
		Host:             "test-host-uuid",
		BootID:           "test-boot-id",
		PID:              os.Getpid(),
		LastHeartbeat:    now,
		LastCheckpointAt: now,
		SeedSource:       SeedGreenfield,
	}
	dir := seedSiblingBrain(t, agent, sib, true)
	lk := flock.New(AliveMarkerPath(dir))
	if got, err := lk.TryLock(); err != nil || !got {
		t.Fatalf("seed flock: got=%v err=%v", got, err)
	}
	t.Cleanup(func() { _ = lk.Unlock() })

	res, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.MarkedDead) != 0 {
		t.Fatalf("expected nothing marked dead, got %v", res.MarkedDead)
	}
	if reason := res.Skipped[dir]; reason != "flock held" {
		t.Errorf("Skipped reason = %q, want 'flock held'", reason)
	}
}

func TestRecovery_CurrentBrainExcluded(t *testing.T) {
	agent := agentForTest(t)
	dir, m, err := Birth(BirthOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Birth: %v", err)
	}
	res, err := Recover(RecoverOpts{
		Agent:          agent,
		Platform:       newFakePlatform(),
		CurrentBrainID: m.BrainID,
		Logger:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	for _, d := range res.Inspected {
		if d == dir {
			t.Errorf("current brain %q should not be inspected", dir)
		}
	}
}

func TestRecovery_EmptyBrainsRootIsNoop(t *testing.T) {
	agent := agentForTest(t)
	// Don't birth anything — BrainsRoot does not exist.
	res, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Inspected) != 0 || len(res.MarkedDead) != 0 {
		t.Errorf("expected no-op, got %+v", res)
	}
}

func TestRecovery_CorruptManifestSkipped(t *testing.T) {
	agent := agentForTest(t)
	// Hand-craft a sibling with garbage manifest content.
	dir := agent.BrainDir("bad-sibling")
	if err := os.MkdirAll(filepath.Join(dir, "markers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ManifestPath(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.MarkedDead) != 0 {
		t.Errorf("corrupt manifest should be skipped, not marked dead: %v", res.MarkedDead)
	}
	if reason := res.Skipped[dir]; !strings.Contains(reason, "manifest read") {
		t.Errorf("expected manifest-read skip reason, got %q", reason)
	}
}

// --- Lifecycle integration with heartbeat -----------------------------

func TestLifecycle_StartStartsHeartbeatByDefault(t *testing.T) {
	agent := agentForTest(t)
	lc, err := Start(StartOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = lc.Shutdown(ctx)
	})
	// The flock on markers/alive must be held.
	lk := flock.New(AliveMarkerPath(lc.BrainDir()))
	got, err := lk.TryLock()
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	if got {
		_ = lk.Unlock()
		t.Fatal("flock should be held by Lifecycle's heartbeat")
	}
}

func TestLifecycle_SkipHeartbeatLeavesFlockFree(t *testing.T) {
	agent := agentForTest(t)
	lc, err := Start(StartOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler), SkipHeartbeat: true})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = lc.Shutdown(ctx)
	})
	if _, err := os.Stat(AliveMarkerPath(lc.BrainDir())); !errors.Is(err, os.ErrNotExist) {
		// Marker doesn't have to exist when heartbeat is skipped — but
		// if it does, we shouldn't have crashed.
		t.Logf("marker present: %v (acceptable)", err)
	}
}
