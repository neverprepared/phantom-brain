package brain

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neverprepared/phantom-brain/internal/config"
)

// --- Manifest ---------------------------------------------------------

func TestManifest_WriteThenRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	// Phase D2b: snapshots are gone, so the parent_gen / parent_snapshot_*
	// manifest fields were removed; births are always greenfield.
	want := &Manifest{
		BrainID:              "brain-1",
		ContributorID:        "personal/memory@host1",
		Profile:              "personal",
		Vault:                "memory",
		BornAt:               "2026-01-01T00:00:00Z",
		Status:               StatusAlive,
		Host:                 "host1",
		Hostname:             "laptop",
		BootID:               "boot1",
		PID:                  os.Getpid(),
		LastHeartbeat:        "2026-01-01T00:00:00Z",
		LastCheckpointAt:     "2026-01-01T00:00:00Z",
		SeedSource:           SeedGreenfield,
	}
	if err := WriteManifest(dir, want); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.SchemaVersion != ManifestSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, ManifestSchemaVersion)
	}
	if got.BrainID != want.BrainID || got.ContributorID != want.ContributorID {
		t.Errorf("identity mismatch: got %+v", got)
	}
	if got.SeedSource != SeedGreenfield {
		t.Errorf("SeedSource round-trip lost: got %v", got.SeedSource)
	}
}

func TestManifest_ReadMissing_ReturnsErrNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadManifest(dir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ErrNotExist, got %v", err)
	}
}

func TestManifest_RejectsNewerSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	// Hand-craft a manifest claiming a future schema version.
	body, _ := json.MarshalIndent(map[string]any{
		"schema_version": ManifestSchemaVersion + 99,
		"brain_id":       "b",
	}, "", "  ")
	if err := os.WriteFile(ManifestPath(dir), body, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadManifest(dir)
	if err == nil || !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Fatalf("expected schema rejection, got %v", err)
	}
}

func TestManifest_NilWriteFails(t *testing.T) {
	if err := WriteManifest(t.TempDir(), nil); err == nil {
		t.Fatal("expected error on nil manifest")
	}
}

// --- Platform helpers -------------------------------------------------

func TestExtractIOPlatformUUID(t *testing.T) {
	sample := `+-o IOPlatformExpertDevice  <class IOPlatformExpertDevice>
{
  "IOPlatformSerialNumber" = "FVFXXXXXX"
  "IOPlatformUUID" = "DEADBEEF-1234-5678-9ABC-DEF012345678"
}`
	got := extractIOPlatformUUID(sample)
	if got != "DEADBEEF-1234-5678-9ABC-DEF012345678" {
		t.Errorf("got %q", got)
	}
	if extractIOPlatformUUID("nothing here") != "" {
		t.Error("missing key should return empty string")
	}
}

func TestContributorID_FormatWithAndWithoutNonce(t *testing.T) {
	got := ContributorID("personal", "memory", "abc123", "")
	if got != "personal/memory@abc123" {
		t.Errorf("got %q", got)
	}
	got = ContributorID("work", "core", "abc123", "nonce42")
	if got != "work/core@abc123+nonce42" {
		t.Errorf("got %q", got)
	}
}

// --- Birth ------------------------------------------------------------

// fakePlatform is a deterministic Platform for tests. Real one probes
// the OS; we don't want that variability in unit tests.
type fakePlatform struct {
	hostUUID    string
	bootID      string
	hostname    string
	inContainer bool
	aliveSet    map[int]bool
}

func (f *fakePlatform) HostUUID() (string, error) { return f.hostUUID, nil }
func (f *fakePlatform) BootID() (string, error)   { return f.bootID, nil }
func (f *fakePlatform) Hostname() (string, error) { return f.hostname, nil }
func (f *fakePlatform) InContainer() bool          { return f.inContainer }
func (f *fakePlatform) ProcessAlive(pid int) bool  { return f.aliveSet[pid] }

func newFakePlatform() *fakePlatform {
	return &fakePlatform{
		hostUUID: "test-host-uuid",
		bootID:   "test-boot-id",
		hostname: "test-host",
		aliveSet: map[int]bool{},
	}
}

// agentForTest sets all required env vars and points XDG_DATA_HOME at
// a tempdir, then loads a real config.Agent. Using the production
// loader rather than a hand-built struct keeps the tests honest about
// the env contract.
func agentForTest(t *testing.T) *config.Agent {
	t.Helper()
	t.Setenv("CL_BRAIN_API", "https://example.invalid")
	t.Setenv("CL_BRAIN_API_TOKEN", "token")
	t.Setenv("CL_WORKSPACE_PROFILE", "personal")
	t.Setenv("CL_BRAIN_VAULT", "memory")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	a, err := config.LoadAgent()
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	return a
}

// captureLogger returns a slog.Logger whose output is captured in buf
// so tests can assert on warning emission.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestBirth_Greenfield_AllocatesUUIDAndWritesManifest(t *testing.T) {
	agent := agentForTest(t)
	buf := &bytes.Buffer{}
	dir, m, err := Birth(BirthOpts{
		Agent:    agent,
		Platform: newFakePlatform(),
		Logger:   captureLogger(buf),
		Now:      func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("Birth: %v", err)
	}
	if m.BrainID == "" {
		t.Fatal("expected non-empty brain_id")
	}
	if m.Status != StatusAlive {
		t.Errorf("status=%q, want alive", m.Status)
	}
	if m.SeedSource != SeedGreenfield {
		t.Errorf("seed_source=%q, want greenfield", m.SeedSource)
	}
	if m.Host != "test-host-uuid" || m.BootID != "test-boot-id" || m.Hostname != "test-host" {
		t.Errorf("platform fields not propagated: %+v", m)
	}
	if m.ContributorID != "personal/memory@test-host-uuid" {
		t.Errorf("contributor_id = %q", m.ContributorID)
	}
	if m.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d", m.PID, os.Getpid())
	}
	// Manifest must be on disk and readable.
	if _, err := ReadManifest(dir); err != nil {
		t.Fatalf("ReadManifest after Birth: %v", err)
	}
	// Vault skeleton must exist.
	for _, sub := range []string{"vault/Raw/curated", "vault/Wiki/summaries", "markers", "_index"} {
		st, err := os.Stat(filepath.Join(dir, sub))
		if err != nil || !st.IsDir() {
			t.Errorf("expected directory %s, err=%v", sub, err)
		}
	}
	// Phase D2b: birth no longer contacts the daemon for a snapshot —
	// it is always greenfield, so there is no snapshot-fetch warning.
	if strings.Contains(buf.String(), "snapshot") {
		t.Errorf("birth should not mention snapshots anymore, got: %s", buf.String())
	}
}

func TestBirth_ContainerAllocatesNonceInContributorID(t *testing.T) {
	agent := agentForTest(t)
	p := newFakePlatform()
	p.inContainer = true
	_, m, err := Birth(BirthOpts{Agent: agent, Platform: p, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Birth: %v", err)
	}
	if m.ContainerNonce == "" {
		t.Fatal("expected non-empty container_nonce inside container")
	}
	if !strings.HasPrefix(m.ContributorID, "personal/memory@test-host-uuid+") {
		t.Errorf("contributor_id missing container suffix: %q", m.ContributorID)
	}
}

func TestBirth_HonorsCallerSuppliedBrainID(t *testing.T) {
	agent := agentForTest(t)
	agent.BrainID = "my-specific-id"
	_, m, err := Birth(BirthOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Birth: %v", err)
	}
	if m.BrainID != "my-specific-id" {
		t.Errorf("brain_id = %q, want my-specific-id", m.BrainID)
	}
}

func TestBirth_RebindToExistingBrain(t *testing.T) {
	agent := agentForTest(t)
	dir, first, err := Birth(BirthOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("first Birth: %v", err)
	}
	agent.BrainID = first.BrainID
	dir2, second, err := Birth(BirthOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("rebind Birth: %v", err)
	}
	if dir2 != dir {
		t.Errorf("rebind moved dirs: %q vs %q", dir2, dir)
	}
	if second.BrainID != first.BrainID {
		t.Errorf("rebind allocated new id %q (was %q)", second.BrainID, first.BrainID)
	}
	if second.BornAt != first.BornAt {
		t.Errorf("rebind clobbered born_at")
	}
}

func TestBirth_RebindFailsOnDeadManifest(t *testing.T) {
	agent := agentForTest(t)
	dir, m, err := Birth(BirthOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Birth: %v", err)
	}
	m.Status = StatusDead
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	agent.BrainID = m.BrainID
	_, _, err = Birth(BirthOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err == nil || !strings.Contains(err.Error(), "dead") {
		t.Fatalf("expected dead-rebind rejection, got %v", err)
	}
}

func TestBirth_MissingAgentOrPlatformErrors(t *testing.T) {
	if _, _, err := Birth(BirthOpts{}); err == nil {
		t.Error("expected error for nil Agent")
	}
	agent := agentForTest(t)
	if _, _, err := Birth(BirthOpts{Agent: agent}); err == nil {
		t.Error("expected error for nil Platform")
	}
}

func TestConcurrent_TwoBirthsAllocateDistinctIDs(t *testing.T) {
	agent := agentForTest(t)
	const N = 8
	var wg sync.WaitGroup
	ids := make([]string, N)
	var fails atomic.Int32
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, m, err := Birth(BirthOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
			if err != nil {
				fails.Add(1)
				return
			}
			ids[idx] = m.BrainID
		}(i)
	}
	wg.Wait()
	if fails.Load() != 0 {
		t.Fatalf("%d births failed", fails.Load())
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate brain_id %q", id)
		}
		seen[id] = true
	}
}
