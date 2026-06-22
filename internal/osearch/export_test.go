package osearch

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/neverprepared/mcp-phantom-brain/internal/index"
)

// nonZeroEmbedding builds a unit-ish vector that varies by seed so
// each test doc has a distinct (and non-zero) embedding — both
// requirements of the cosinesimil knn_vector field.
func nonZeroEmbedding(seed int) []float32 {
	v := make([]float32, EmbeddingDim)
	// A handful of non-zero entries are enough; the rest stay zero.
	v[seed%EmbeddingDim] = 1.0
	v[(seed+1)%EmbeddingDim] = 0.5
	return v
}

func TestLive_Export_RoundTripsThroughIndex(t *testing.T) {
	c, ctx, cleanup := testClient(t)
	defer cleanup()

	seed := []SummaryDoc{
		{
			Profile: "p", Vault: "v", SHA: "a1",
			Title: "Kubernetes pods overview", Body: "Pods are the smallest deployable unit.",
			Tags: []string{"k8s", "container"}, Topic: "infrastructure",
			SourcePath: "Wiki/summaries/k8s-pods.md",
			Synthesised: true, Embedding: nonZeroEmbedding(1),
		},
		{
			Profile: "p", Vault: "v", SHA: "a2",
			Title: "Helm charts intro", Body: "Helm templates Kubernetes manifests with values files.",
			Tags: []string{"k8s", "helm"}, Topic: "infrastructure",
			SourcePath: "Wiki/summaries/helm-intro.md",
			Synthesised: true, Embedding: nonZeroEmbedding(2),
		},
		// Raw-only — should be skipped by default
		{
			Profile: "p", Vault: "v", SHA: "raw1",
			Title: "Raw doc", RawBody: "Not yet synthesised.",
			SourcePath: "Raw/gathered/raw1.md",
			Synthesised: false, Embedding: nonZeroEmbedding(3),
		},
		// Different vault — must not appear
		{
			Profile: "p", Vault: "other", SHA: "x1",
			Title: "Other vault", Body: "Should not be exported into p/v's snapshot.",
			Synthesised: true, Embedding: nonZeroEmbedding(4),
		},
	}
	for _, d := range seed {
		if err := c.UpsertSummary(ctx, d, false); err != nil {
			t.Fatalf("seed %s: %v", d.SHA, err)
		}
	}
	if err := c.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	outDir := t.TempDir()
	tarPath := filepath.Join(outDir, "snapshot.tar.zst")

	manifest, err := c.Export(ctx, ExportOptions{
		Profile:    "p",
		Vault:      "v",
		OutputPath: tarPath,
		BatchSize:  100,
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if manifest.NumDocs != 2 {
		t.Errorf("manifest.NumDocs = %d, want 2 (the two synthesised p/v docs)", manifest.NumDocs)
	}
	if manifest.NumSkipped < 1 {
		t.Errorf("manifest.NumSkipped = %d, want >= 1 (raw-only doc)", manifest.NumSkipped)
	}
	if manifest.SHA256 == "" || manifest.SizeBytes == 0 {
		t.Errorf("manifest missing hash/size: %+v", manifest)
	}
	if manifest.GeneratedAt.IsZero() || time.Since(manifest.GeneratedAt) > time.Minute {
		t.Errorf("manifest.GeneratedAt unreasonable: %v", manifest.GeneratedAt)
	}

	// Extract the tarball and verify the SQLite is readable by the
	// agent's index package + the seeded docs come back via SearchText.
	extractDir := filepath.Join(outDir, "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		t.Fatalf("mkdir extract: %v", err)
	}
	if err := extractTarZst(tarPath, extractDir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	dbPath := filepath.Join(extractDir, "_index", "vectors.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected _index/vectors.db in tarball: %v", err)
	}

	idx, err := index.Open(filepath.Join(extractDir, "_index"), EmbeddingDim)
	if err != nil {
		t.Fatalf("index.Open exported db: %v", err)
	}
	defer idx.Close()

	all, err := idx.AllSHAs()
	if err != nil {
		t.Fatalf("AllSHAs: %v", err)
	}
	if _, ok := all["a1"]; !ok {
		t.Errorf("AllSHAs missing a1; got %v", all)
	}
	if _, ok := all["a2"]; !ok {
		t.Errorf("AllSHAs missing a2; got %v", all)
	}
	if _, ok := all["raw1"]; ok {
		t.Errorf("AllSHAs unexpectedly contains raw1 (raw-only doc should have been skipped)")
	}
	if _, ok := all["x1"]; ok {
		t.Errorf("AllSHAs leaked cross-vault doc x1: %v", all)
	}

	hits, err := idx.SearchText(ctx, "kubernetes", 10)
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(hits) == 0 {
		t.Error("SearchText('kubernetes') returned 0 hits in the exported snapshot")
	}
	foundA1 := false
	for _, h := range hits {
		if h.SHA == "a1" {
			foundA1 = true
		}
	}
	if !foundA1 {
		t.Errorf("SearchText hits %v missing a1", hits)
	}
}

func TestLive_Export_IncludeRawOnly(t *testing.T) {
	c, ctx, cleanup := testClient(t)
	defer cleanup()

	docs := []SummaryDoc{
		{Profile: "p", Vault: "v", SHA: "synth", Title: "synth", Body: "synthesised", Synthesised: true, Embedding: nonZeroEmbedding(1), SourcePath: "Wiki/x.md"},
		{Profile: "p", Vault: "v", SHA: "raw", Title: "raw", RawBody: "raw only", Synthesised: false, Embedding: nonZeroEmbedding(2), SourcePath: "Raw/y.md"},
	}
	for _, d := range docs {
		if err := c.UpsertSummary(ctx, d, false); err != nil {
			t.Fatalf("seed %s: %v", d.SHA, err)
		}
	}
	if err := c.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	tarPath := filepath.Join(t.TempDir(), "snap.tar.zst")
	manifest, err := c.Export(ctx, ExportOptions{
		Profile: "p", Vault: "v", OutputPath: tarPath,
		IncludeRawOnly: true,
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if manifest.NumDocs != 2 {
		t.Errorf("IncludeRawOnly=true: NumDocs = %d, want 2", manifest.NumDocs)
	}
}

func TestExportRequiresProfileVaultOutput(t *testing.T) {
	c := &Client{}
	if _, err := c.Export(context.Background(), ExportOptions{}); err == nil {
		t.Error("expected error for empty options; got nil")
	}
}

// extractTarZst is the test helper that mirrors what the agent's
// birth.go does on the receive side. Kept in-package so the test
// stays self-contained.
func extractTarZst(tarPath, dest string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	zr, err := zstd.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Resist path traversal in test artefacts.
		if strings.Contains(hdr.Name, "..") {
			continue
		}
		target := filepath.Join(dest, hdr.Name)
		if hdr.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		out.Close()
	}
}

// TestManifestJSONShape guards the wire format for Day 6's sidecar
// persistence — renaming a field silently breaks the agent's parse.
func TestManifestJSONShape(t *testing.T) {
	m := ExportManifest{
		Profile: "p", Vault: "v", NumDocs: 1, SHA256: "deadbeef",
		SizeBytes: 42, GeneratedAt: time.Now().UTC(), EmbeddingDim: EmbeddingDim,
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"profile", "vault", "num_docs", "num_skipped", "sha256", "size_bytes", "generated_at", "embedding_dim"} {
		if _, ok := back[k]; !ok {
			t.Errorf("ExportManifest missing JSON field %q", k)
		}
	}
}
