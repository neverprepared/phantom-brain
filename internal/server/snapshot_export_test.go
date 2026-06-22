package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/neverprepared/mcp-phantom-brain/internal/index"
	"github.com/neverprepared/mcp-phantom-brain/internal/osearch"
)

// --- fakeExporter -------------------------------------------------

// fakeExporter writes a deterministic vectors.db tarball without
// touching OpenSearch. The DB it produces is opened in-test with
// internal/index.Open to verify the wired sidecars match what's on
// disk. Tracks call count + last opts so debouncer tests can assert
// "rebuilt exactly once" or "not yet".
type fakeExporter struct {
	mu       sync.Mutex
	calls    int
	lastOpts osearch.ExportOptions
	docs     []indexDoc // what to seed into the exported DB
	failNext bool
}

type indexDoc struct {
	SHA        string
	SourcePath string
	Title      string
	Tags       string
	Body       string
	Embedding  []float32
}

func (f *fakeExporter) Export(_ context.Context, opts osearch.ExportOptions) (osearch.ExportManifest, error) {
	f.mu.Lock()
	f.calls++
	f.lastOpts = opts
	failed := f.failNext
	f.failNext = false
	docs := append([]indexDoc(nil), f.docs...)
	f.mu.Unlock()
	if failed {
		return osearch.ExportManifest{}, errors.New("fake: export forced failure")
	}

	stage, err := os.MkdirTemp("", "fake-export-*")
	if err != nil {
		return osearch.ExportManifest{}, err
	}
	defer os.RemoveAll(stage)

	// Mirror the real Export layout — vectors.db lives at _index/
	// inside the tarball.
	indexStage := filepath.Join(stage, "_index")
	if err := os.MkdirAll(indexStage, 0o755); err != nil {
		return osearch.ExportManifest{}, err
	}
	idx, err := index.Open(indexStage, osearch.EmbeddingDim)
	if err != nil {
		return osearch.ExportManifest{}, err
	}
	for _, d := range docs {
		if err := idx.Upsert(context.Background(), index.Record{
			SHA: d.SHA, SourcePath: d.SourcePath,
			Title: d.Title, Tags: d.Tags, Body: d.Body,
			Embedding: d.Embedding,
		}); err != nil {
			idx.Close()
			return osearch.ExportManifest{}, err
		}
	}
	if err := idx.Close(); err != nil {
		return osearch.ExportManifest{}, err
	}

	// Tar+zst via the same helper Day 3 used.
	sum, size, err := writeFakeTarZst(stage, opts.OutputPath)
	if err != nil {
		return osearch.ExportManifest{}, err
	}
	return osearch.ExportManifest{
		Profile: opts.Profile, Vault: opts.Vault,
		NumDocs: len(docs), SHA256: sum, SizeBytes: size,
		GeneratedAt: time.Now().UTC(), EmbeddingDim: osearch.EmbeddingDim,
	}, nil
}

func (f *fakeExporter) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// writeFakeTarZst delegates to internal/osearch.WriteTarZst so the
// produced tarball is bit-for-bit identical to a real Export call.
func writeFakeTarZst(stage, outPath string) (string, int64, error) {
	return osearch.WriteTarZst(stage, outPath)
}

// extractTarZstTest delegates to internal/osearch.ExtractTarZst.
func extractTarZstTest(t *testing.T, tarPath, dest string) error {
	t.Helper()
	return osearch.ExtractTarZst(tarPath, dest)
}

func nonZeroEmbedding(seed int) []float32 {
	v := make([]float32, osearch.EmbeddingDim)
	v[seed%osearch.EmbeddingDim] = 1.0
	v[(seed+1)%osearch.EmbeddingDim] = 0.5
	return v
}

// --- BuildSnapshotFromOS tests ------------------------------------

func TestBuildSnapshotFromOS_ProducesReadableTarball(t *testing.T) {
	dataDir := DataDir(t.TempDir())
	if err := EnsureCollectiveSkeleton(dataDir, "p", "v"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	fx := &fakeExporter{
		docs: []indexDoc{
			{SHA: "a", SourcePath: "Wiki/summaries/a.md", Title: "Kubernetes basics", Body: "Pods are units.", Embedding: nonZeroEmbedding(1)},
			{SHA: "b", SourcePath: "Wiki/summaries/b.md", Title: "Helm intro", Body: "Helm packages charts.", Embedding: nonZeroEmbedding(2)},
		},
	}

	info, err := BuildSnapshotFromOS(context.Background(), dataDir, fx, "p", "v", 30)
	if err != nil {
		t.Fatalf("BuildSnapshotFromOS: %v", err)
	}
	if info == nil {
		t.Fatal("info nil with NumDocs > 0")
	}
	if info.Manifest.Gen != 1 {
		t.Errorf("gen = %d, want 1 on first build", info.Manifest.Gen)
	}
	if info.Manifest.SHA256 == "" || info.Manifest.SizeBytes == 0 {
		t.Errorf("sidecars missing: %+v", info.Manifest)
	}

	// Sidecar files exist + match the in-memory manifest.
	shaPath := filepath.Join(dataDir.PublishedDir("p", "v"), "snapshot-1.tar.zst.sha256")
	if _, err := os.Stat(shaPath); err != nil {
		t.Errorf(".sha256 sidecar missing: %v", err)
	}
	manifestPath := filepath.Join(dataDir.PublishedDir("p", "v"), "snapshot-1.manifest.json")
	mraw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest read: %v", err)
	}
	var got SnapshotManifest
	if err := json.Unmarshal(mraw, &got); err != nil {
		t.Fatalf("manifest decode: %v", err)
	}
	if got.SHA256 != info.Manifest.SHA256 {
		t.Errorf("sidecar sha256 mismatch: got %q want %q", got.SHA256, info.Manifest.SHA256)
	}

	// gen counter bumped.
	gen, _ := ReadGenCounter(dataDir, "p", "v")
	if gen != 1 {
		t.Errorf("gen counter = %d, want 1", gen)
	}

	// Extract the tarball + verify the agent's index opens it.
	extract := t.TempDir()
	if err := extractTarZstTest(t, info.TarballPath, extract); err != nil {
		t.Fatalf("extract: %v", err)
	}
	idx, err := index.Open(filepath.Join(extract, "_index"), osearch.EmbeddingDim)
	if err != nil {
		t.Fatalf("index.Open exported tarball: %v", err)
	}
	defer idx.Close()
	all, _ := idx.AllSHAs()
	if _, ok := all["a"]; !ok {
		t.Errorf("AllSHAs missing 'a' after extract; got %v", all)
	}
}

func TestBuildSnapshotFromOS_ZeroDocsIsNoop(t *testing.T) {
	dataDir := DataDir(t.TempDir())
	if err := EnsureCollectiveSkeleton(dataDir, "p", "v"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	fx := &fakeExporter{} // no docs
	info, err := BuildSnapshotFromOS(context.Background(), dataDir, fx, "p", "v", 30)
	if err != nil {
		t.Fatalf("err = %v, want nil for zero-docs", err)
	}
	if info != nil {
		t.Errorf("info = %+v, want nil (no publish on empty export)", info)
	}
	gen, _ := ReadGenCounter(dataDir, "p", "v")
	if gen != 0 {
		t.Errorf("gen counter bumped to %d on empty export; want 0", gen)
	}
}

func TestBuildSnapshotFromOS_ExportFailureBubbles(t *testing.T) {
	dataDir := DataDir(t.TempDir())
	if err := EnsureCollectiveSkeleton(dataDir, "p", "v"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	fx := &fakeExporter{failNext: true}
	if _, err := BuildSnapshotFromOS(context.Background(), dataDir, fx, "p", "v", 30); err == nil {
		t.Error("expected error from failed export")
	}
}

// --- SnapshotDebouncer tests --------------------------------------

func TestSnapshotDebouncer_CollapsesBursts(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	dbn := NewSnapshotDebouncer(func(_ context.Context, _, _ string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	}, 50*time.Millisecond, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dbn.Start(ctx)

	// 5 triggers within the debounce window → 1 rebuild
	for i := 0; i < 5; i++ {
		dbn.Trigger("p", "v")
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("calls = %d, want 1 (debounce should collapse burst)", got)
	}
}

func TestSnapshotDebouncer_PerVaultIsolation(t *testing.T) {
	var (
		mu  sync.Mutex
		got = map[VaultKey]int{}
	)
	dbn := NewSnapshotDebouncer(func(_ context.Context, profile, vaultName string) error {
		mu.Lock()
		got[VaultKey{Profile: profile, Vault: vaultName}]++
		mu.Unlock()
		return nil
	}, 50*time.Millisecond, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dbn.Start(ctx)

	dbn.Trigger("a", "x")
	dbn.Trigger("b", "y")
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if got[VaultKey{Profile: "a", Vault: "x"}] != 1 {
		t.Errorf("a/x rebuild count = %d, want 1", got[VaultKey{Profile: "a", Vault: "x"}])
	}
	if got[VaultKey{Profile: "b", Vault: "y"}] != 1 {
		t.Errorf("b/y rebuild count = %d, want 1", got[VaultKey{Profile: "b", Vault: "y"}])
	}
}

func TestSnapshotDebouncer_SurvivesBuildError(t *testing.T) {
	var calls int32 = 0
	var mu sync.Mutex
	dbn := NewSnapshotDebouncer(func(_ context.Context, _, _ string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return errors.New("synthetic build failure")
	}, 30*time.Millisecond, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dbn.Start(ctx)

	dbn.Trigger("p", "v")
	time.Sleep(120 * time.Millisecond)
	// New trigger after the failure should still produce a build.
	dbn.Trigger("p", "v")
	time.Sleep(120 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Errorf("calls = %d; want >=2 (errors should not stall debouncer)", calls)
	}
}
