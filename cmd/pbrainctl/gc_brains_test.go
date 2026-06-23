package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neverprepared/mcp-phantom-brain/internal/brain"
)

// fakeGCPlatform reports a fixed set of pids as alive. Only
// ProcessAlive is exercised by gcBrainsCmd; the other Platform methods
// are stubbed to satisfy the interface.
type fakeGCPlatform struct {
	alive map[int]bool
}

func (f *fakeGCPlatform) HostUUID() (string, error) { return "host", nil }
func (f *fakeGCPlatform) BootID() (string, error)   { return "boot", nil }
func (f *fakeGCPlatform) Hostname() (string, error) { return "h", nil }
func (f *fakeGCPlatform) InContainer() bool         { return false }
func (f *fakeGCPlatform) ProcessAlive(pid int) bool { return f.alive[pid] }

// writeManifestAt fabricates a brain dir at <root>/<brainID> with a
// manifest matching the supplied fields. Mtime of the manifest is
// rewound by ageBack so age math works without waiting in real time.
func writeManifestAt(t *testing.T, root, brainID string, status brain.Status, lastHeartbeat time.Time, ageBack time.Duration, pid int) string {
	t.Helper()
	dir := filepath.Join(root, brainID)
	if err := os.MkdirAll(filepath.Join(dir, "markers"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	m := brain.Manifest{
		SchemaVersion: brain.ManifestSchemaVersion,
		BrainID:       brainID,
		Profile:       "personal",
		Vault:         "memory",
		Status:        status,
		PID:           pid,
		BornAt:        time.Now().Add(-ageBack).UTC().Format(time.RFC3339),
	}
	if !lastHeartbeat.IsZero() {
		m.LastHeartbeat = lastHeartbeat.UTC().Format(time.RFC3339)
	}
	buf, _ := json.MarshalIndent(&m, "", "  ")
	buf = append(buf, '\n')
	mpath := filepath.Join(dir, brain.ManifestFilename)
	if err := os.WriteFile(mpath, buf, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	// Rewind mtime so the fallback ladder in IsGCEligible sees a real age
	// when LastHeartbeat is empty.
	past := time.Now().Add(-ageBack)
	if err := os.Chtimes(mpath, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return dir
}

func runGC(t *testing.T, opts gcBrainsOpts) string {
	t.Helper()
	buf := &bytes.Buffer{}
	if err := runGCBrains(buf, opts); err != nil {
		t.Fatalf("runGCBrains: %v", err)
	}
	return buf.String()
}

func TestGCBrains_DryRunDoesNotDelete(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	dir := writeManifestAt(t, root, "b-old", brain.StatusDead, now.Add(-48*time.Hour), 48*time.Hour, 0)

	out := runGC(t, gcBrainsOpts{
		Root: root, Retention: 24 * time.Hour, DryRun: true,
		Now: func() time.Time { return now }, Platform: &fakeGCPlatform{},
	})

	if !strings.Contains(out, "delete") {
		t.Errorf("expected delete row in table, got:\n%s", out)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dry-run should have left the dir intact: %v", err)
	}
}

func TestGCBrains_DeletesEligibleDeadBrain(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	dir := writeManifestAt(t, root, "b-dead", brain.StatusDead, now.Add(-48*time.Hour), 48*time.Hour, 0)

	_ = runGC(t, gcBrainsOpts{
		Root: root, Retention: 24 * time.Hour,
		Now: func() time.Time { return now }, Platform: &fakeGCPlatform{},
	})

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("expected dir to be removed; stat err=%v", err)
	}
}

func TestGCBrains_KeepsYoungDeadBrain(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	dir := writeManifestAt(t, root, "b-young", brain.StatusDead, now.Add(-1*time.Hour), 1*time.Hour, 0)

	out := runGC(t, gcBrainsOpts{
		Root: root, Retention: 24 * time.Hour,
		Now: func() time.Time { return now }, Platform: &fakeGCPlatform{},
	})

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("young dead brain should survive: %v", err)
	}
	if !strings.Contains(out, "keep:too young") {
		t.Errorf("expected keep:too young in output, got:\n%s", out)
	}
}

func TestGCBrains_DefaultExcludesAliveBrains(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	dir := writeManifestAt(t, root, "b-alive", brain.StatusAlive, now.Add(-48*time.Hour), 48*time.Hour, 9999)

	out := runGC(t, gcBrainsOpts{
		Root: root, Retention: 24 * time.Hour,
		Now: func() time.Time { return now }, Platform: &fakeGCPlatform{},
	})

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("alive brain should not be touched: %v", err)
	}
	if !strings.Contains(out, "keep:status=alive") {
		t.Errorf("expected keep:status=alive in output, got:\n%s", out)
	}
}

func TestGCBrains_IncludeAliveWithDeadPIDDeletes(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	dir := writeManifestAt(t, root, "b-stale", brain.StatusAlive, now.Add(-48*time.Hour), 48*time.Hour, 4242)

	_ = runGC(t, gcBrainsOpts{
		Root: root, Retention: 24 * time.Hour, IncludeAlive: true,
		Now: func() time.Time { return now },
		// PID 4242 not in alive set -> treated as dead process.
		Platform: &fakeGCPlatform{},
	})

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("stale alive brain with dead pid should be removed; stat err=%v", err)
	}
}

func TestGCBrains_IncludeAliveWithLivePIDKeeps(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	dir := writeManifestAt(t, root, "b-live", brain.StatusAlive, now.Add(-48*time.Hour), 48*time.Hour, 1234)

	out := runGC(t, gcBrainsOpts{
		Root: root, Retention: 24 * time.Hour, IncludeAlive: true,
		Now:      func() time.Time { return now },
		Platform: &fakeGCPlatform{alive: map[int]bool{1234: true}},
	})

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("live alive brain must not be deleted: %v", err)
	}
	if !strings.Contains(out, "pid 1234 alive") {
		t.Errorf("expected pid-alive reason, got:\n%s", out)
	}
}

func TestGCBrains_CurrentBrainAlwaysExempt(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	dir := writeManifestAt(t, root, "b-self", brain.StatusDead, now.Add(-72*time.Hour), 72*time.Hour, 0)

	out := runGC(t, gcBrainsOpts{
		Root: root, Retention: 24 * time.Hour, CurrentBrainID: "b-self",
		Now: func() time.Time { return now }, Platform: &fakeGCPlatform{},
	})

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("current brain must never be deleted: %v", err)
	}
	if !strings.Contains(out, "keep:current brain") {
		t.Errorf("expected keep:current brain reason, got:\n%s", out)
	}
}

func TestGCBrains_CorruptManifestKept(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "b-corrupt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, brain.ManifestFilename), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runGC(t, gcBrainsOpts{
		Root: root, Retention: 24 * time.Hour,
		Now: time.Now, Platform: &fakeGCPlatform{},
	})

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("corrupt brain must be kept (no manifest = no decision): %v", err)
	}
	if !strings.Contains(out, "keep:manifest:") {
		t.Errorf("expected keep:manifest:... reason, got:\n%s", out)
	}
}

func TestGCBrains_HeartbeatFallsBackToMtime(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	// No LastHeartbeat — mtime rewind of 48h is what gates eligibility.
	dir := writeManifestAt(t, root, "b-mtime", brain.StatusDead, time.Time{}, 48*time.Hour, 0)

	_ = runGC(t, gcBrainsOpts{
		Root: root, Retention: 24 * time.Hour,
		Now: func() time.Time { return now }, Platform: &fakeGCPlatform{},
	})

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("mtime-aged dead brain should be removed; stat err=%v", err)
	}
}

func TestGCBrains_RetentionZeroDisablesDeletion(t *testing.T) {
	// Retention=0 propagated into runGCBrains; the cobra wrapper would
	// have refused this, but the interior must also be defensive.
	root := t.TempDir()
	now := time.Now()
	dir := writeManifestAt(t, root, "b-zero", brain.StatusDead, now.Add(-100*time.Hour), 100*time.Hour, 0)

	out := runGC(t, gcBrainsOpts{
		Root: root, Retention: 0,
		Now: func() time.Time { return now }, Platform: &fakeGCPlatform{},
	})

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("retention=0 must not delete: %v", err)
	}
	if !strings.Contains(out, "gc disabled") {
		t.Errorf("expected gc disabled reason, got:\n%s", out)
	}
}

func TestGCBrains_BrainsRootMissingIsNoop(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	out := runGC(t, gcBrainsOpts{
		Root: missing, Retention: 24 * time.Hour,
		Now: time.Now, Platform: &fakeGCPlatform{},
	})
	if !strings.Contains(out, "no brains root") {
		t.Errorf("expected no-root message, got:\n%s", out)
	}
}

func TestIsGCEligible_RespectsRules(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	deadOld := &brain.Manifest{Status: brain.StatusDead, LastHeartbeat: now.Add(-48 * time.Hour).UTC().Format(time.RFC3339)}
	if ok, _ := brain.IsGCEligible(deadOld, dir, now, 24*time.Hour); !ok {
		t.Error("dead+old should be eligible")
	}
	deadYoung := &brain.Manifest{Status: brain.StatusDead, LastHeartbeat: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339)}
	if ok, _ := brain.IsGCEligible(deadYoung, dir, now, 24*time.Hour); ok {
		t.Error("dead+young must not be eligible")
	}
	alive := &brain.Manifest{Status: brain.StatusAlive, LastHeartbeat: now.Add(-48 * time.Hour).UTC().Format(time.RFC3339)}
	if ok, _ := brain.IsGCEligible(alive, dir, now, 24*time.Hour); ok {
		t.Error("alive must not be eligible")
	}
	if ok, _ := brain.IsGCEligible(deadOld, dir, now, 0); ok {
		t.Error("retention=0 disables eligibility")
	}
	if ok, _ := brain.IsGCEligible(nil, dir, now, 24*time.Hour); ok {
		t.Error("nil manifest must not be eligible")
	}
}
