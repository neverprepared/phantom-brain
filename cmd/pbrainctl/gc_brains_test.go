package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neverprepared/phantom-brain/internal/brain"
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

// runGCBrainsCmd executes the cobra command end-to-end with the supplied
// args against a captured stdout buffer. Lets us exercise the binding
// resolution that lives inside RunE rather than just the interior loop.
func runGCBrainsCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := gcBrainsCmd()
	buf := &bytes.Buffer{}
	c.SetOut(buf)
	c.SetErr(buf)
	c.SetArgs(args)
	err := c.Execute()
	return buf.String(), err
}

// scrubAgentEnv unsets every CL_BRAIN_* and CL_WORKSPACE_* var so
// LoadAgent's fast-path fails and we fall into the bindless walk.
// XDG_DATA_HOME is pointed at the test tempdir.
func scrubAgentEnv(t *testing.T, dataHome string) {
	t.Helper()
	for _, k := range []string{
		"CL_BRAIN_API", "CL_BRAIN_API_TOKEN", "CL_WORKSPACE_PROFILE", "CL_BRAIN_VAULT",
		"CL_BRAIN_LOCAL_RETENTION_HOURS", "CL_BRAIN_ID",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("XDG_DATA_HOME", dataHome)
}

func TestGCBrains_BindlessWalkDiscoversAllBindings(t *testing.T) {
	dataHome := t.TempDir()
	scrubAgentEnv(t, dataHome)

	// Two synthetic bindings, each with one dead, eligible brain.
	r1 := filepath.Join(dataHome, "phantom-brain", "p1", "v1", "brains")
	r2 := filepath.Join(dataHome, "phantom-brain", "p2", "v2", "brains")
	now := time.Now()
	writeManifestAt(t, r1, "b-1", brain.StatusDead, now.Add(-48*time.Hour), 48*time.Hour, 0)
	writeManifestAt(t, r2, "b-2", brain.StatusDead, now.Add(-48*time.Hour), 48*time.Hour, 0)

	out, err := runGCBrainsCmd(t, "--dry-run")
	if err != nil {
		t.Fatalf("cmd: %v\n%s", err, out)
	}
	if !strings.Contains(out, "# p1/v1") || !strings.Contains(out, "# p2/v2") {
		t.Errorf("expected per-binding headers, got:\n%s", out)
	}
	if !strings.Contains(out, "b-1") || !strings.Contains(out, "b-2") {
		t.Errorf("expected both brain rows, got:\n%s", out)
	}
}

func TestGCBrains_ProfileVaultFlagsScope(t *testing.T) {
	dataHome := t.TempDir()
	scrubAgentEnv(t, dataHome)

	r1 := filepath.Join(dataHome, "phantom-brain", "p1", "v1", "brains")
	r2 := filepath.Join(dataHome, "phantom-brain", "p2", "v2", "brains")
	now := time.Now()
	writeManifestAt(t, r1, "b-1", brain.StatusDead, now.Add(-48*time.Hour), 48*time.Hour, 0)
	writeManifestAt(t, r2, "b-2", brain.StatusDead, now.Add(-48*time.Hour), 48*time.Hour, 0)

	out, err := runGCBrainsCmd(t, "--dry-run", "--profile", "p1", "--vault", "v1")
	if err != nil {
		t.Fatalf("cmd: %v\n%s", err, out)
	}
	if !strings.Contains(out, "b-1") {
		t.Errorf("expected b-1 in scoped output, got:\n%s", out)
	}
	if strings.Contains(out, "b-2") {
		t.Errorf("--profile/--vault must not touch other bindings, got:\n%s", out)
	}
}

func TestGCBrains_ProfileVaultMustPairUp(t *testing.T) {
	dataHome := t.TempDir()
	scrubAgentEnv(t, dataHome)

	_, err := runGCBrainsCmd(t, "--dry-run", "--profile", "p1")
	if err == nil {
		t.Fatal("expected error when --profile passed without --vault")
	}
	if !strings.Contains(err.Error(), "must be set together") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGCBrains_RejectsPathTraversalInProfile(t *testing.T) {
	dataHome := t.TempDir()
	scrubAgentEnv(t, dataHome)

	_, err := runGCBrainsCmd(t, "--dry-run", "--profile", "../etc", "--vault", "v1")
	if err == nil {
		t.Fatal("expected rejection of traversal in --profile")
	}
	if !strings.Contains(err.Error(), "path separators") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGCBrains_BrainsRootFlagWinsOverEverything(t *testing.T) {
	dataHome := t.TempDir()
	scrubAgentEnv(t, dataHome)

	// Decoy binding the walk would pick up if --brains-root were ignored.
	decoy := filepath.Join(dataHome, "phantom-brain", "decoy", "vault", "brains")
	now := time.Now()
	writeManifestAt(t, decoy, "b-decoy", brain.StatusDead, now.Add(-48*time.Hour), 48*time.Hour, 0)

	// Explicit root with one brain in it.
	explicit := t.TempDir()
	writeManifestAt(t, explicit, "b-explicit", brain.StatusDead, now.Add(-48*time.Hour), 48*time.Hour, 0)

	out, err := runGCBrainsCmd(t, "--dry-run", "--brains-root", explicit)
	if err != nil {
		t.Fatalf("cmd: %v\n%s", err, out)
	}
	if !strings.Contains(out, "b-explicit") {
		t.Errorf("expected b-explicit, got:\n%s", out)
	}
	if strings.Contains(out, "b-decoy") || strings.Contains(out, "# decoy/") {
		t.Errorf("--brains-root must short-circuit the walk, got:\n%s", out)
	}
}

func TestGCBrains_BindlessWalkEmptyIsNoop(t *testing.T) {
	dataHome := t.TempDir()
	scrubAgentEnv(t, dataHome)

	out, err := runGCBrainsCmd(t, "--dry-run")
	if err != nil {
		t.Fatalf("cmd: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no bindings found") {
		t.Errorf("expected no-bindings notice, got:\n%s", out)
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
