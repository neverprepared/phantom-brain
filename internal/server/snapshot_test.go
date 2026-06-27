package server

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// --- Ledger -----------------------------------------------------------

func TestLedger_OpenInsertGetList(t *testing.T) {
	d := DataDir(t.TempDir())
	if err := EnsureCollectiveSkeleton(d, "personal", "memory"); err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(d, "personal", "memory")
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	rec := MergeRecord{
		BrainID:         "brain-1",
		ContributorID:   "personal/memory@host",
		Profile:         "personal",
		Vault:           "memory",
		MergedAt:        time.Now().UTC().Truncate(time.Second),
		RawCount:        3,
		AttachmentCount: 1,
		PayloadBytes:    12345,
	}
	if err := l.Insert(rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := l.Get("brain-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RawCount != 3 || got.PayloadBytes != 12345 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if !got.MergedAt.Equal(rec.MergedAt) {
		t.Errorf("MergedAt round-trip lost: %v vs %v", got.MergedAt, rec.MergedAt)
	}

	list, err := l.List(10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("List returned %d, want 1", len(list))
	}
}

func TestLedger_DuplicateBrainIDRejected(t *testing.T) {
	d := DataDir(t.TempDir())
	_ = EnsureCollectiveSkeleton(d, "personal", "memory")
	l, _ := OpenLedger(d, "personal", "memory")
	t.Cleanup(func() { _ = l.Close() })

	rec := MergeRecord{BrainID: "dup", Profile: "personal", Vault: "memory"}
	if err := l.Insert(rec); err != nil {
		t.Fatal(err)
	}
	err := l.Insert(rec)
	if !errors.Is(err, ErrDuplicateMerge) {
		t.Fatalf("expected ErrDuplicateMerge, got %v", err)
	}
}

func TestLedger_OpenIdempotent(t *testing.T) {
	d := DataDir(t.TempDir())
	_ = EnsureCollectiveSkeleton(d, "personal", "memory")
	l1, _ := OpenLedger(d, "personal", "memory")
	_ = l1.Insert(MergeRecord{BrainID: "x", Profile: "personal", Vault: "memory"})
	_ = l1.Close()

	l2, err := OpenLedger(d, "personal", "memory")
	if err != nil {
		t.Fatalf("second OpenLedger: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	got, err := l2.Get("x")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.BrainID != "x" {
		t.Errorf("data lost across reopen: %+v", got)
	}
}

// --- Snapshot builder -------------------------------------------------

// seedCollective builds a minimal vault tree with a Wiki summary so
// BuildSnapshot has something to tar.
func seedCollective(t *testing.T, d DataDir, profile, vaultName, body string) {
	t.Helper()
	if err := EnsureCollectiveSkeleton(d, profile, vaultName); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(d.VaultDir(profile, vaultName), "Wiki", "summaries", "hello.md")
	if err := os.WriteFile(page, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshot_BuildBumpsGen(t *testing.T) {
	d := DataDir(t.TempDir())
	seedCollective(t, d, "personal", "memory", "# hello\n")

	if g, _ := ReadGenCounter(d, "personal", "memory"); g != 0 {
		t.Fatalf("initial gen counter = %d, want 0", g)
	}

	info, err := BuildSnapshot(d, "personal", "memory", 30)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	if info.Manifest.Gen != 1 {
		t.Errorf("first gen = %d, want 1", info.Manifest.Gen)
	}
	if info.Manifest.SHA256 == "" || info.Manifest.SizeBytes <= 0 {
		t.Errorf("manifest incomplete: %+v", info.Manifest)
	}

	got, _ := ReadGenCounter(d, "personal", "memory")
	if got != 1 {
		t.Errorf("counter after build = %d, want 1", got)
	}

	info2, err := BuildSnapshot(d, "personal", "memory", 30)
	if err != nil {
		t.Fatalf("second BuildSnapshot: %v", err)
	}
	if info2.Manifest.Gen != 2 {
		t.Errorf("second gen = %d, want 2", info2.Manifest.Gen)
	}
}

func TestSnapshot_TarballContainsWikiAndOmitsClaims(t *testing.T) {
	d := DataDir(t.TempDir())
	seedCollective(t, d, "personal", "memory", "# hi\n")

	info, err := BuildSnapshot(d, "personal", "memory", 30)
	if err != nil {
		t.Fatal(err)
	}
	// Plant a .claims/ marker AFTER the build to exercise the prune
	// path; the tarball was already written without it.
	if err := os.WriteFile(filepath.Join(d.StagedDir("personal", "memory"),
		"snapshot-1", ".claims", "brain-X"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	got := tarZstFileSet(t, info.TarballPath)
	if !got["vault/Wiki/summaries/hello.md"] {
		t.Errorf("tarball missing Wiki page; entries: %v", got)
	}
	for name := range got {
		if strings.HasPrefix(name, ".claims") {
			t.Errorf("tarball should not contain .claims entries: %s", name)
		}
	}
}

func TestSnapshot_LoadAndCurrent(t *testing.T) {
	d := DataDir(t.TempDir())
	seedCollective(t, d, "personal", "memory", "# x\n")
	info, _ := BuildSnapshot(d, "personal", "memory", 30)

	cur, err := CurrentSnapshot(d, "personal", "memory")
	if err != nil || cur == nil {
		t.Fatalf("CurrentSnapshot: %v / %v", err, cur)
	}
	if cur.Manifest.Gen != info.Manifest.Gen || cur.Manifest.SHA256 != info.Manifest.SHA256 {
		t.Errorf("Current mismatch: %+v vs %+v", cur.Manifest, info.Manifest)
	}
	loaded, err := LoadSnapshotManifest(d, "personal", "memory", info.Manifest.Gen)
	if err != nil {
		t.Fatalf("LoadSnapshotManifest: %v", err)
	}
	if loaded.SHA256 != info.Manifest.SHA256 {
		t.Errorf("loaded sha mismatch")
	}
}

func TestSnapshot_CurrentReturnsNilForFreshVault(t *testing.T) {
	d := DataDir(t.TempDir())
	if err := EnsureCollectiveSkeleton(d, "personal", "memory"); err != nil {
		t.Fatal(err)
	}
	cur, err := CurrentSnapshot(d, "personal", "memory")
	if err != nil {
		t.Fatalf("CurrentSnapshot: %v", err)
	}
	if cur != nil {
		t.Errorf("expected nil for fresh vault, got %+v", cur)
	}
}

func TestSnapshot_RetentionPrunesOldestButRespectsClaims(t *testing.T) {
	d := DataDir(t.TempDir())
	seedCollective(t, d, "personal", "memory", "# 1\n")

	// Build 5 snapshots with retention=3. The oldest two SHOULD prune
	// unless pinned by a .claims/ marker.
	for i := 0; i < 5; i++ {
		// Tweak the wiki page so the contents differ and the SHAs
		// don't accidentally match (catches a hash-stability bug).
		if err := os.WriteFile(filepath.Join(d.VaultDir("personal", "memory"),
			"Wiki", "summaries", "hello.md"),
			[]byte("# version "+string('A'+rune(i))+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Pin gen=1 so the retention path must skip it. Write the
		// claim BEFORE the next build so the staged dir exists.
		if i == 1 {
			pinDir := filepath.Join(d.StagedDir("personal", "memory"), "snapshot-1", ".claims")
			_ = os.MkdirAll(pinDir, 0o755)
			_ = os.WriteFile(filepath.Join(pinDir, "brain-pin"), []byte{}, 0o644)
		}
		if _, err := BuildSnapshot(d, "personal", "memory", 3); err != nil {
			t.Fatalf("build %d: %v", i+1, err)
		}
	}

	gens, err := listPublishedGens(d, "personal", "memory")
	if err != nil {
		t.Fatalf("listPublishedGens: %v", err)
	}
	// gen=1 pinned + last 3 (3, 4, 5) retained = {1, 3, 4, 5}
	got := map[uint64]bool{}
	for _, g := range gens {
		got[g] = true
	}
	for _, want := range []uint64{1, 3, 4, 5} {
		if !got[want] {
			t.Errorf("expected gen %d in retained set, got %v", want, gens)
		}
	}
	if got[2] {
		t.Errorf("gen 2 should have been pruned, got %v", gens)
	}
}

// --- HTTP snapshot handlers --------------------------------------------
//
// Phase D1: these drove a fully started daemon (startDaemonWithSeed →
// Start), which now mandates a live Postgres SoR. But the snapshot HTTP
// handlers (current / by-gen / tarball) are purely DataDir-backed — they
// never touch Postgres — so they're restored here against the no-Start
// router rig (newRouterRig in handlers_test.go) rather than dropped to
// integration.

func TestHandler_SnapshotCurrent_FreshVaultReturnsGenZero(t *testing.T) {
	r := newRouterRig(t)
	resp := r.do(t, http.MethodGet, "/api/brain/snapshot/current", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var m SnapshotManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.Gen != 0 || m.Profile != "personal" {
		t.Errorf("unexpected manifest: %+v", m)
	}
}

func TestHandler_SnapshotByGen_AndTarballDownload(t *testing.T) {
	r := newRouterRig(t)
	seedCollective(t, r.d.DataDir, "personal", "memory", "# served\n")
	if _, err := BuildSnapshot(r.d.DataDir, "personal", "memory", 30); err != nil {
		t.Fatal(err)
	}

	// Manifest by gen.
	resp := r.do(t, http.MethodGet, "/api/brain/snapshot/1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest gen=1 status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Tarball download.
	resp = r.do(t, http.MethodGet, "/api/brain/snapshot/1/tarball", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tarball status=%d", resp.StatusCode)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "application/zstd" {
		t.Errorf("Content-Type = %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) < 50 {
		t.Errorf("tarball suspiciously small: %d bytes", len(body))
	}
}

func TestHandler_Snapshot_UnknownGenReturns404(t *testing.T) {
	r := newRouterRig(t)
	resp := r.do(t, http.MethodGet, "/api/brain/snapshot/999", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// --- helpers ----------------------------------------------------------

// tarZstFileSet reads path as tar+zstd and returns the set of entry
// names present.
func tarZstFileSet(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	out := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		out[hdr.Name] = true
	}
}
