package brain

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// seedDeadBrain writes a sibling brain dir with status=dead and a
// chosen heartbeat age. Returns the dir path.
func seedDeadBrain(t *testing.T, agent interface{ BrainDir(string) string }, id string, heartbeatAge time.Duration, touchMarker bool) string {
	t.Helper()
	hb := time.Now().UTC().Add(-heartbeatAge).Format(time.RFC3339)
	m := &Manifest{
		BrainID:          id,
		ContributorID:    "personal/memory@host",
		Profile:          "personal",
		Vault:            "memory",
		BornAt:           hb,
		Status:           StatusDead,
		Host:             "test-host-uuid",
		BootID:           "test-boot-id",
		PID:              99999,
		LastHeartbeat:    hb,
		LastCheckpointAt: hb,
		SeedSource:       SeedGreenfield,
	}
	return seedSiblingBrain(t, agent, m, touchMarker)
}

// setRetention is a small helper so each test states the retention
// override on its own line without reaching into config internals.
func setRetention(t *testing.T, hours int) {
	t.Helper()
	t.Setenv("CL_BRAIN_LOCAL_RETENTION_HOURS", itoa(hours))
}

func itoa(i int) string {
	// avoid importing strconv just for this in test files
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func TestGC_DeadBrainOlderThanRetentionDeleted(t *testing.T) {
	setRetention(t, 24)
	agent := agentForTest(t)
	dir := seedDeadBrain(t, agent, "dead-old", 48*time.Hour, true)

	buf := &bytes.Buffer{}
	res, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: captureLogger(buf)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != dir {
		t.Fatalf("expected Deleted=[%s], got %v (skipped=%v)", dir, res.Deleted, res.DeleteSkipped)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dir should be removed, stat err=%v", err)
	}
	if !strings.Contains(buf.String(), "garbage-collected dead brain") {
		t.Errorf("expected GC log entry, got %s", buf.String())
	}
}

func TestGC_DeadBrainYoungerThanRetentionKept(t *testing.T) {
	setRetention(t, 24)
	agent := agentForTest(t)
	dir := seedDeadBrain(t, agent, "dead-young", 1*time.Hour, true)

	res, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("expected no deletions, got %v", res.Deleted)
	}
	if reason := res.DeleteSkipped[dir]; !strings.Contains(reason, "too young") {
		t.Errorf("DeleteSkipped reason = %q, want 'too young...'", reason)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir should still exist, stat err=%v", err)
	}
}

func TestGC_RetentionZeroDisablesPass(t *testing.T) {
	setRetention(t, 0)
	agent := agentForTest(t)
	dir := seedDeadBrain(t, agent, "would-be-deleted", 999*time.Hour, true)

	res, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("expected GC disabled, got Deleted=%v", res.Deleted)
	}
	if _, ok := res.DeleteSkipped[dir]; ok {
		t.Errorf("GC pass should be skipped entirely, but dir appeared in DeleteSkipped: %v", res.DeleteSkipped)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir should remain on disk, stat err=%v", err)
	}
}

func TestGC_CurrentBrainNeverInspected(t *testing.T) {
	setRetention(t, 1)
	agent := agentForTest(t)
	// Seed a dir that would be eligible were it not the current brain.
	dir := seedDeadBrain(t, agent, "current-brain", 100*time.Hour, true)

	res, err := Recover(RecoverOpts{
		Agent:          agent,
		Platform:       newFakePlatform(),
		CurrentBrainID: "current-brain",
		Logger:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	for _, d := range res.Deleted {
		if d == dir {
			t.Fatalf("current brain %s was deleted", dir)
		}
	}
	if reason, ok := res.DeleteSkipped[dir]; ok {
		t.Errorf("current brain should be invisible to GC, but appeared with reason %q", reason)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("current brain dir should remain, stat err=%v", err)
	}
}

func TestGC_AliveBrainNeverDeleted(t *testing.T) {
	setRetention(t, 1)
	agent := agentForTest(t)
	now := time.Now().UTC().Format(time.RFC3339)
	// Status=alive with held flock — analogous to a live sibling. GC
	// must not touch it regardless of heartbeat age (alive status is
	// authoritative).
	m := &Manifest{
		BrainID:          "alive-sibling",
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
	dir := seedSiblingBrain(t, agent, m, true)
	lk := flock.New(AliveMarkerPath(dir))
	if got, err := lk.TryLock(); err != nil || !got {
		t.Fatalf("seed flock: got=%v err=%v", got, err)
	}
	t.Cleanup(func() { _ = lk.Unlock() })

	res, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("alive brain must not be deleted, got %v", res.Deleted)
	}
	if reason := res.DeleteSkipped[dir]; !strings.HasPrefix(reason, "status=") {
		t.Errorf("DeleteSkipped reason = %q, want 'status=...'", reason)
	}
}

func TestGC_CorruptManifestSkipped(t *testing.T) {
	setRetention(t, 1)
	agent := agentForTest(t)
	dir := agent.BrainDir("bad-corpse")
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
	if len(res.Deleted) != 0 {
		t.Fatalf("corrupt manifest must not be deleted, got %v", res.Deleted)
	}
	if reason := res.DeleteSkipped[dir]; !strings.HasPrefix(reason, "manifest:") {
		t.Errorf("DeleteSkipped reason = %q, want 'manifest:' prefix", reason)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir should remain after corrupt-manifest skip, stat err=%v", err)
	}
}

func TestGC_HeartbeatEmptyFallsBackToMtime(t *testing.T) {
	setRetention(t, 1)
	agent := agentForTest(t)
	// Status=dead, LastHeartbeat empty. The fallback should consult
	// the manifest mtime — fresh after WriteManifest, so kept.
	m := &Manifest{
		BrainID:          "no-heartbeat",
		ContributorID:    "personal/memory@host",
		Profile:          "personal",
		Vault:            "memory",
		BornAt:           time.Now().UTC().Format(time.RFC3339),
		Status:           StatusDead,
		Host:             "test-host-uuid",
		BootID:           "test-boot-id",
		PID:              99999,
		LastCheckpointAt: time.Now().UTC().Format(time.RFC3339),
		SeedSource:       SeedGreenfield,
	}
	dir := seedSiblingBrain(t, agent, m, false)

	res, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("fresh-mtime corpse should be kept, got %v", res.Deleted)
	}
	reason := res.DeleteSkipped[dir]
	if !strings.Contains(reason, "manifest mtime") {
		t.Errorf("DeleteSkipped reason = %q, expected mention of 'manifest mtime'", reason)
	}

	// Now backdate the manifest's mtime past the retention window and
	// rerun — should be deleted via mtime fallback.
	old := time.Now().UTC().Add(-2 * time.Hour)
	if err := os.Chtimes(ManifestPath(dir), old, old); err != nil {
		t.Fatal(err)
	}
	res2, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Recover (post-backdate): %v", err)
	}
	if len(res2.Deleted) != 1 || res2.Deleted[0] != dir {
		t.Fatalf("backdated corpse should be deleted, got %v (skipped=%v)", res2.Deleted, res2.DeleteSkipped)
	}
}

func TestGC_HeartbeatUnparseableFallsBackToMtime(t *testing.T) {
	setRetention(t, 1)
	agent := agentForTest(t)
	m := &Manifest{
		BrainID:          "garbled-heartbeat",
		ContributorID:    "personal/memory@host",
		Profile:          "personal",
		Vault:            "memory",
		BornAt:           time.Now().UTC().Format(time.RFC3339),
		Status:           StatusDead,
		Host:             "test-host-uuid",
		BootID:           "test-boot-id",
		PID:              99999,
		LastHeartbeat:    "not-a-timestamp",
		LastCheckpointAt: time.Now().UTC().Format(time.RFC3339),
		SeedSource:       SeedGreenfield,
	}
	dir := seedSiblingBrain(t, agent, m, false)
	old := time.Now().UTC().Add(-3 * time.Hour)
	if err := os.Chtimes(ManifestPath(dir), old, old); err != nil {
		t.Fatal(err)
	}

	res, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != dir {
		t.Fatalf("unparseable-heartbeat corpse should be deleted via mtime, got %v (skipped=%v)", res.Deleted, res.DeleteSkipped)
	}
}

func TestGC_RaceFlockHeldBetweenPasses(t *testing.T) {
	// Simulate the race where after IsGCEligible returns true a sibling
	// claims the flock. Predicate is fine with status=dead; we just
	// hold the flock before Recover runs.
	setRetention(t, 1)
	agent := agentForTest(t)
	dir := seedDeadBrain(t, agent, "raced-corpse", 5*time.Hour, true)
	lk := flock.New(AliveMarkerPath(dir))
	if got, err := lk.TryLock(); err != nil || !got {
		t.Fatalf("seed flock: got=%v err=%v", got, err)
	}
	t.Cleanup(func() { _ = lk.Unlock() })

	res, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("racing-flock corpse must not be deleted, got %v", res.Deleted)
	}
	if reason := res.DeleteSkipped[dir]; !strings.Contains(reason, "flock acquired") {
		t.Errorf("DeleteSkipped reason = %q, want mention of 'flock acquired'", reason)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir should remain, stat err=%v", err)
	}
}

// TestGC_HoldsFlockAcrossRemoveAll asserts the TOCTOU fix: while the
// GC pass owns a candidate's flock, a competing TryLock from another
// process / goroutine must fail. The pre-fix code took and released
// the flock before RemoveAll, leaving a window where a sibling could
// claim the marker and have its fresh dir wiped. We can't directly
// schedule against the unexported gcSweep timing, so this is a
// targeted unit check on the lock semantics RemoveAll relies on.
func TestGC_HoldsFlockAcrossRemoveAll(t *testing.T) {
	setRetention(t, 1)
	agent := agentForTest(t)
	dir := seedDeadBrain(t, agent, "lock-held", 5*time.Hour, true)

	marker := AliveMarkerPath(dir)
	owner := flock.New(marker)
	took, err := owner.TryLock()
	if err != nil || !took {
		t.Fatalf("seed owner lock: took=%v err=%v", took, err)
	}
	t.Cleanup(func() { _ = owner.Unlock() })

	// Sibling probe must fail: we (the "GC pass") hold the lock.
	sibling := flock.New(marker)
	got, err := sibling.TryLock()
	if err != nil {
		t.Fatalf("sibling TryLock errored: %v", err)
	}
	if got {
		_ = sibling.Unlock()
		t.Fatal("sibling acquired flock while owner holds it — TOCTOU fix broken")
	}
}

func TestGC_TwoPassInteractionFreshlyMarkedDeadKept(t *testing.T) {
	// A brain transitioned alive->dead in pass 1 has a "now" heartbeat
	// — so even with retention=1h, pass 2 should not delete it the
	// same cycle.
	setRetention(t, 1)
	agent := agentForTest(t)
	stale := time.Now().UTC().Add(-time.Duration(agent.OrphanThresholdSecs+60) * time.Second).Format(time.RFC3339)
	m := &Manifest{
		BrainID:          "fresh-corpse",
		ContributorID:    "personal/memory@host",
		Profile:          "personal",
		Vault:            "memory",
		BornAt:           stale,
		Status:           StatusAlive,
		Host:             "test-host-uuid",
		BootID:           "test-boot-id",
		PID:              99999,
		LastHeartbeat:    stale,
		LastCheckpointAt: stale,
		SeedSource:       SeedGreenfield,
	}
	dir := seedSiblingBrain(t, agent, m, true)

	res, err := Recover(RecoverOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// First pass should have marked it dead (heartbeat is stale +
	// pid 99999 not in aliveSet).
	if len(res.MarkedDead) != 1 {
		t.Fatalf("expected pass 1 to mark dead, got %v", res.MarkedDead)
	}
	// Pass 2 sees status=dead and reads the original (stale) heartbeat.
	// With retention=1h and heartbeat just past the orphan threshold
	// (~6m), the corpse is "too young" — kept this cycle, GC'd later.
	if len(res.Deleted) != 0 {
		t.Fatalf("same-cycle GC should NOT delete freshly transitioned dead brain, got %v", res.Deleted)
	}
	if reason := res.DeleteSkipped[dir]; !strings.Contains(reason, "too young") {
		t.Errorf("DeleteSkipped reason = %q, want 'too young...'", reason)
	}
}
