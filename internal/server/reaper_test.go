package server

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pbbrain "github.com/neverprepared/mcp-phantom-brain/internal/brain"
)

// --- SafeExtract ------------------------------------------------------

// tarOf builds an in-memory tar from a sequence of entries.
type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

func tarOf(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o644,
			Size:     int64(len(e.body)),
			Typeflag: e.typeflag,
			Linkname: e.linkname,
		}
		if e.typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if e.typeflag == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSafeExtract_ValidArchiveExtracts(t *testing.T) {
	dest := t.TempDir()
	body := tarOf(t,
		tarEntry{name: "manifest.json", body: `{"x":1}`},
		tarEntry{name: "vault/Raw/curated/hello.md", body: "# hi\n"},
	)
	if err := SafeExtract(bytes.NewReader(body), dest, SafeTarLimits{MaxUncompressedBytes: 1 << 20}); err != nil {
		t.Fatalf("SafeExtract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "vault/Raw/curated/hello.md")); err != nil {
		t.Errorf("expected extracted file: %v", err)
	}
}

func TestSafeExtract_RejectsTraversal(t *testing.T) {
	body := tarOf(t, tarEntry{name: "../escape.md", body: "x"})
	err := SafeExtract(bytes.NewReader(body), t.TempDir(), SafeTarLimits{MaxUncompressedBytes: 1 << 20})
	if !IsSafeTarErrorKind(err, SafeTarErrTraversal) {
		t.Fatalf("expected traversal err, got %v", err)
	}
}

func TestSafeExtract_RejectsAbsolutePath(t *testing.T) {
	body := tarOf(t, tarEntry{name: "/etc/passwd", body: "x"})
	err := SafeExtract(bytes.NewReader(body), t.TempDir(), SafeTarLimits{MaxUncompressedBytes: 1 << 20})
	if !IsSafeTarErrorKind(err, SafeTarErrTraversal) {
		t.Fatalf("expected traversal err, got %v", err)
	}
}

func TestSafeExtract_RejectsSymlinkEscape(t *testing.T) {
	body := tarOf(t, tarEntry{name: "link", typeflag: tar.TypeSymlink, linkname: "../../../etc/passwd"})
	err := SafeExtract(bytes.NewReader(body), t.TempDir(), SafeTarLimits{MaxUncompressedBytes: 1 << 20})
	if !IsSafeTarErrorKind(err, SafeTarErrSymlinkEscape) {
		t.Fatalf("expected symlink-escape err, got %v", err)
	}
}

func TestSafeExtract_EnforcesSizeCap(t *testing.T) {
	body := tarOf(t, tarEntry{name: "big.bin", body: strings.Repeat("A", 1000)})
	err := SafeExtract(bytes.NewReader(body), t.TempDir(), SafeTarLimits{MaxUncompressedBytes: 500})
	if !IsSafeTarErrorKind(err, SafeTarErrSizeCap) {
		t.Fatalf("expected size-cap err, got %v", err)
	}
}

func TestSafeExtract_EnforcesFileCap(t *testing.T) {
	body := tarOf(t,
		tarEntry{name: "a.txt", body: "a"},
		tarEntry{name: "b.txt", body: "b"},
		tarEntry{name: "c.txt", body: "c"},
	)
	err := SafeExtract(bytes.NewReader(body), t.TempDir(), SafeTarLimits{MaxUncompressedBytes: 1 << 20, MaxFiles: 2})
	if !IsSafeTarErrorKind(err, SafeTarErrFileCap) {
		t.Fatalf("expected file-cap err, got %v", err)
	}
}

// --- Queue ------------------------------------------------------------

func TestQueue_EnqueueAndClaim(t *testing.T) {
	d := DataDir(t.TempDir())
	if err := EnsureCollectiveSkeleton(d, "personal", "memory"); err != nil {
		t.Fatal(err)
	}
	path, err := EnqueueItem(d, "personal", "memory", QueueItem{
		RawPath: "Raw/curated/hello.md",
		Source:  "curated",
		Title:   "hello",
	}, "deadbeef0000")
	if err != nil {
		t.Fatalf("EnqueueItem: %v", err)
	}
	if !strings.Contains(path, "queue/pending") {
		t.Errorf("enqueue path = %q", path)
	}
	claimed, item, err := ClaimNextItem(d, "personal", "memory")
	if err != nil {
		t.Fatalf("ClaimNextItem: %v", err)
	}
	if !strings.Contains(claimed, "queue/claimed") {
		t.Errorf("claimed path = %q", claimed)
	}
	if item == nil || item.Title != "hello" {
		t.Errorf("item = %+v", item)
	}
	// Second claim sees the queue empty.
	c2, _, err := ClaimNextItem(d, "personal", "memory")
	if err != nil {
		t.Fatal(err)
	}
	if c2 != "" {
		t.Errorf("second claim should be empty, got %q", c2)
	}
	// Mark done.
	if err := MarkDone(d, "personal", "memory", claimed); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	if _, err := os.Stat(claimed); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("claimed file should be gone after MarkDone")
	}
}

func TestQueue_MarkDeadMovesToDead(t *testing.T) {
	d := DataDir(t.TempDir())
	_ = EnsureCollectiveSkeleton(d, "personal", "memory")
	_, _ = EnqueueItem(d, "personal", "memory", QueueItem{RawPath: "r.md", Source: "curated"}, "abc")
	claimed, _, _ := ClaimNextItem(d, "personal", "memory")
	if err := MarkDead(d, "personal", "memory", claimed); err != nil {
		t.Fatalf("MarkDead: %v", err)
	}
	deadDir := filepath.Join(d.VaultDir("personal", "memory"), "queue", "dead")
	entries, _ := os.ReadDir(deadDir)
	if len(entries) != 1 {
		t.Errorf("dead dir has %d entries", len(entries))
	}
}

// --- Reaper -----------------------------------------------------------

// makeDeathPayload builds the same shape internal/brain/death.go's
// packDeathPayload writes: tarball at the root containing manifest.json
// and vault/Raw/{curated,gathered,attachments}/* files.
func makeDeathPayload(t *testing.T, m pbbrain.Manifest, rawFiles map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mb, _ := json.MarshalIndent(m, "", "  ")
	_ = tw.WriteHeader(&tar.Header{Name: pbbrain.ManifestFilename, Mode: 0o644, Size: int64(len(mb)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(mb)
	for relpath, body := range rawFiles {
		_ = tw.WriteHeader(&tar.Header{
			Name:     relpath,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	return buf.Bytes()
}

// dropPayload places a payload tar in brains/_pending/ for the reaper.
func dropPayload(t *testing.T, d DataDir, profile, vaultName string, payload []byte) string {
	t.Helper()
	pendingDir := filepath.Join(d.BrainsDir(profile, vaultName), "_pending")
	if err := os.MkdirAll(pendingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(pendingDir, "death-"+time.Now().UTC().Format("20060102150405.000")+".tar")
	if err := os.WriteFile(p, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReaper_HappyPathMergesAndEnqueues(t *testing.T) {
	d := DataDir(t.TempDir())
	binding := VaultBinding{
		Key:      VaultKey{Profile: "personal", Vault: "memory"},
		Auth:     VaultAuth{BearerToken: "t"},
		Defaults: VaultDefaults{MaxUncompressedBytes: 1 << 20},
	}
	if err := EnsureCollectiveSkeleton(d, binding.Key.Profile, binding.Key.Vault); err != nil {
		t.Fatal(err)
	}
	payload := makeDeathPayload(t, pbbrain.Manifest{
		BrainID:       "brain-A",
		ContributorID: "personal/memory@host",
		Profile:       "personal",
		Vault:         "memory",
		Status:        pbbrain.StatusDead,
	}, map[string]string{
		"vault/Raw/curated/hello.md":              "# hello world\n",
		"vault/Raw/gathered/2026-06-20-stuff.html": "<p>stuff</p>",
	})
	tarPath := dropPayload(t, d, binding.Key.Profile, binding.Key.Vault, payload)

	res, err := ReapOnce(d, binding, slog.New(slog.DiscardHandler), &sync.Mutex{})
	if err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	if len(res.Merged) != 1 || res.Merged[0] != "brain-A" {
		t.Fatalf("Merged = %v", res.Merged)
	}
	// Tarball should be gone.
	if _, err := os.Stat(tarPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("original tar should be gone")
	}
	// Raw files landed in the collective.
	for _, p := range []string{"Raw/curated/hello.md", "Raw/gathered/2026-06-20-stuff.html"} {
		if _, err := os.Stat(filepath.Join(d.VaultDir(binding.Key.Profile, binding.Key.Vault), p)); err != nil {
			t.Errorf("expected %s, err=%v", p, err)
		}
	}
	// Queue has two pending items.
	pending, _ := ListPending(d, binding.Key.Profile, binding.Key.Vault)
	if len(pending) != 2 {
		t.Errorf("expected 2 pending queue items, got %d", len(pending))
	}
	// Ledger row exists.
	l, _ := OpenLedger(d, binding.Key.Profile, binding.Key.Vault)
	defer l.Close()
	rec, err := l.Get("brain-A")
	if err != nil {
		t.Fatalf("ledger.Get: %v", err)
	}
	if rec.RawCount != 2 {
		t.Errorf("RawCount = %d, want 2", rec.RawCount)
	}
	// Merge record JSON written.
	if _, err := os.Stat(filepath.Join(d.BrainsDir(binding.Key.Profile, binding.Key.Vault), "_merged", "brain-A.json")); err != nil {
		t.Errorf("merge record missing: %v", err)
	}
}

func TestReaper_DedupSkipsDuplicateContent(t *testing.T) {
	d := DataDir(t.TempDir())
	binding := VaultBinding{
		Key:      VaultKey{Profile: "personal", Vault: "memory"},
		Defaults: VaultDefaults{MaxUncompressedBytes: 1 << 20},
	}
	_ = EnsureCollectiveSkeleton(d, binding.Key.Profile, binding.Key.Vault)

	body := "# same content\n"
	payload1 := makeDeathPayload(t, pbbrain.Manifest{BrainID: "b1", Profile: "personal", Vault: "memory"},
		map[string]string{"vault/Raw/curated/dup.md": body})
	payload2 := makeDeathPayload(t, pbbrain.Manifest{BrainID: "b2", Profile: "personal", Vault: "memory"},
		map[string]string{"vault/Raw/curated/dup.md": body})

	_ = dropPayload(t, d, binding.Key.Profile, binding.Key.Vault, payload1)
	if _, err := ReapOnce(d, binding, slog.New(slog.DiscardHandler), &sync.Mutex{}); err != nil {
		t.Fatal(err)
	}
	_ = dropPayload(t, d, binding.Key.Profile, binding.Key.Vault, payload2)
	if _, err := ReapOnce(d, binding, slog.New(slog.DiscardHandler), &sync.Mutex{}); err != nil {
		t.Fatal(err)
	}
	pending, _ := ListPending(d, binding.Key.Profile, binding.Key.Vault)
	if len(pending) != 1 {
		t.Errorf("dup should not re-enqueue; pending = %d", len(pending))
	}
}

func TestReaper_VaultMismatchRejected(t *testing.T) {
	d := DataDir(t.TempDir())
	binding := VaultBinding{
		Key:      VaultKey{Profile: "personal", Vault: "memory"},
		Defaults: VaultDefaults{MaxUncompressedBytes: 1 << 20},
	}
	_ = EnsureCollectiveSkeleton(d, binding.Key.Profile, binding.Key.Vault)
	payload := makeDeathPayload(t, pbbrain.Manifest{
		BrainID: "wrong", Profile: "work", Vault: "core",
	}, map[string]string{"vault/Raw/curated/x.md": "x"})
	_ = dropPayload(t, d, binding.Key.Profile, binding.Key.Vault, payload)

	res, err := ReapOnce(d, binding, slog.New(slog.DiscardHandler), &sync.Mutex{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) == 0 {
		t.Fatalf("expected vault-mismatch error in result; got %+v", res)
	}
}

func TestReaper_QuarantinesTraversalPayload(t *testing.T) {
	d := DataDir(t.TempDir())
	binding := VaultBinding{
		Key:      VaultKey{Profile: "personal", Vault: "memory"},
		Defaults: VaultDefaults{MaxUncompressedBytes: 1 << 20},
	}
	_ = EnsureCollectiveSkeleton(d, binding.Key.Profile, binding.Key.Vault)
	bad := tarOf(t, tarEntry{name: "../escape.md", body: "x"})
	_ = dropPayload(t, d, binding.Key.Profile, binding.Key.Vault, bad)
	res, err := ReapOnce(d, binding, slog.New(slog.DiscardHandler), &sync.Mutex{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Quarantine) != 1 {
		t.Errorf("expected 1 quarantine, got %v", res.Quarantine)
	}
}

// Ensure the reaper helper sha256 path is correct (silences unused-
// imports nag if io is otherwise unreferenced in this file).
func TestReaper_SHA256FileMatchesStdlib(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x")
	body := "hello sha"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sha256File(p)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256New()
	_, _ = io.Copy(h, bytes.NewReader([]byte(body)))
	want := hexEncode(h.Sum(nil))
	if got != want {
		t.Errorf("sha mismatch: %q vs %q", got, want)
	}
}

// hexEncode mirrors encoding/hex without importing it again in tests.
func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0x0f]
	}
	return string(out)
}
