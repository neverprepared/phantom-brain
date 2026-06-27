package osearch

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// writeTarZst builds a .tar.zst at path from the given entries. Each
// entry is either a regular file (content non-nil) or a directory
// (content nil and name ending in "/"). Used to exercise ExtractTarZst
// without depending on the (daemon-side) production tar writer.
func writeTarZst(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	tw := tar.NewWriter(zw)
	for name, content := range entries {
		if content == nil {
			hdr := &tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatalf("write dir header %q: %v", name, err)
			}
			continue
		}
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write file header %q: %v", name, err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("write file body %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write tar.zst: %v", err)
	}
}

func TestExtractTarZst_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "snap.tar.zst")
	writeTarZst(t, src, map[string][]byte{
		"_index/":           nil,
		"_index/vectors.db": []byte("vector-bytes"),
		"manifest.json":     []byte(`{"gen":7}`),
		"nested/a/b.txt":    []byte("deep"),
	})

	dest := filepath.Join(tmp, "out")
	if err := ExtractTarZst(src, dest); err != nil {
		t.Fatalf("ExtractTarZst: %v", err)
	}

	// File written through an explicit dir entry.
	got, err := os.ReadFile(filepath.Join(dest, "_index", "vectors.db"))
	if err != nil {
		t.Fatalf("read vectors.db: %v", err)
	}
	if string(got) != "vector-bytes" {
		t.Errorf("vectors.db = %q, want vector-bytes", got)
	}
	// File whose parent dirs only exist implicitly (no dir header) —
	// ExtractTarZst must MkdirAll the parent.
	deep, err := os.ReadFile(filepath.Join(dest, "nested", "a", "b.txt"))
	if err != nil {
		t.Fatalf("read nested file: %v", err)
	}
	if string(deep) != "deep" {
		t.Errorf("nested file = %q, want deep", deep)
	}
	// Top-level file.
	man, err := os.ReadFile(filepath.Join(dest, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if string(man) != `{"gen":7}` {
		t.Errorf("manifest = %q", man)
	}
}

func TestExtractTarZst_PathTraversalBlocked(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "evil.tar.zst")
	writeTarZst(t, src, map[string][]byte{
		"../escape.txt": []byte("pwned"),
	})

	dest := filepath.Join(tmp, "out")
	err := ExtractTarZst(src, dest)
	if err == nil {
		t.Fatal("expected path traversal to be blocked, got nil error")
	}
	// The escaping file must NOT have been written to the parent.
	if _, statErr := os.Stat(filepath.Join(tmp, "escape.txt")); statErr == nil {
		t.Fatal("traversal file escaped to parent dir — guard failed")
	}
}

func TestExtractTarZst_MissingSource(t *testing.T) {
	tmp := t.TempDir()
	err := ExtractTarZst(filepath.Join(tmp, "does-not-exist.tar.zst"), filepath.Join(tmp, "out"))
	if err == nil {
		t.Fatal("expected error opening missing tar, got nil")
	}
}

func TestExtractTarZst_NotZstd(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "plain.bin")
	// Random non-zstd bytes — the zstd reader should reject the magic.
	if err := os.WriteFile(src, []byte("this is definitely not a zstd stream"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := ExtractTarZst(src, filepath.Join(tmp, "out")); err == nil {
		t.Fatal("expected zstd decode error on non-zstd input, got nil")
	}
}

func TestExtractTarZst_DestEqualsEntry(t *testing.T) {
	// An entry named "." resolves to dest itself (cleanAbs == destAbs),
	// which the guard explicitly permits as a directory. It must not be
	// treated as traversal.
	tmp := t.TempDir()
	src := filepath.Join(tmp, "dot.tar.zst")
	writeTarZst(t, src, map[string][]byte{
		"./":     nil,
		"ok.txt": []byte("fine"),
	})
	dest := filepath.Join(tmp, "out")
	if err := ExtractTarZst(src, dest); err != nil {
		t.Fatalf("ExtractTarZst with '.' dir entry: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "ok.txt")); err != nil || string(b) != "fine" {
		t.Fatalf("ok.txt = %q err=%v", b, err)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if len(cfg.Addresses) != 1 || cfg.Addresses[0] != "http://localhost:9200" {
		t.Errorf("Addresses = %v, want [http://localhost:9200]", cfg.Addresses)
	}
	if cfg.RequestTimeout != 10*time.Second {
		t.Errorf("RequestTimeout = %v, want 10s", cfg.RequestTimeout)
	}
	// Defaults must leave the prefix empty so the canonical index names
	// are used unless an operator opts into a sandbox prefix.
	if cfg.IndexPrefix != "" {
		t.Errorf("IndexPrefix = %q, want empty", cfg.IndexPrefix)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true, want false by default")
	}
}

func TestClient_WithPrefix(t *testing.T) {
	orig := &Client{prefix: "base_"}
	derived := orig.WithPrefix("client_x_")

	if derived == orig {
		t.Fatal("WithPrefix returned the same pointer; must be a shallow copy")
	}
	if got := derived.Prefix(); got != "client_x_" {
		t.Errorf("derived.Prefix() = %q, want client_x_", got)
	}
	// Original must be untouched — views are derived, not mutated.
	if got := orig.Prefix(); got != "base_" {
		t.Errorf("orig.Prefix() = %q, want base_ (mutated!)", got)
	}
	// IndexName on the derived view routes through the new prefix.
	if got := derived.IndexName(IndexSummaries); got != "client_x_pb_summaries" {
		t.Errorf("derived.IndexName = %q, want client_x_pb_summaries", got)
	}
	// Empty prefix derives a view that uses canonical names.
	bare := orig.WithPrefix("")
	if got := bare.IndexName(IndexEntities); got != "pb_entities" {
		t.Errorf("bare.IndexName = %q, want pb_entities", got)
	}
}

func TestClient_WithPrefix_NilReceiver(t *testing.T) {
	var c *Client
	if got := c.WithPrefix("anything_"); got != nil {
		t.Errorf("nil.WithPrefix() = %v, want nil", got)
	}
}

func TestClient_Prefix(t *testing.T) {
	c := &Client{prefix: "report_me_"}
	if got := c.Prefix(); got != "report_me_" {
		t.Errorf("Prefix() = %q, want report_me_", got)
	}
}

// TestEntityDocRoundTrip guards the EntityDoc JSON tags against drift
// from the entities index mapping. The slug + mentioned_by fields are
// the load-bearing ones (doc ID base + inverse-index growth).
func TestEntityDocRoundTrip(t *testing.T) {
	in := EntityDoc{
		Profile:     "p",
		Vault:       "v",
		Slug:        "kubernetes",
		Name:        "Kubernetes",
		Aliases:     []string{"k8s"},
		Body:        "container orchestrator",
		Tags:        []string{"infra"},
		Topic:       "infrastructure",
		MentionedBy: []string{"sha1", "sha2"},
		Embedding:   []float32{0.5, 0.25},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"profile", "vault", "slug", "name", "aliases", "body", "tags", "topic", "mentioned_by", "embedding"} {
		if _, ok := out[k]; !ok {
			t.Errorf("EntityDoc JSON missing field %q", k)
		}
	}
	// created_at/updated_at are non-omitempty time.Time — always present.
	if _, ok := out["created_at"]; !ok {
		t.Error("EntityDoc JSON missing created_at")
	}
}

// TestAttachmentDocRoundTrip guards the MinIO-key + size fields that the
// retrieval path depends on, plus the omitempty behaviour of optional
// CapturedAt.
func TestAttachmentDocRoundTrip(t *testing.T) {
	in := AttachmentDoc{
		Profile:          "p",
		Vault:            "v",
		SHA:              "deadbeef",
		OriginalFilename: "invoice.pdf",
		MIMEType:         "application/pdf",
		SizeBytes:        2048,
		MinIOKey:         "p/v/attachments/deadbeef.pdf",
		ExtractedText:    "total due 0",
		SummarySHA:       "summarysha",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"sha", "original_filename", "mime_type", "size_bytes", "minio_key", "extracted_text", "summary_sha"} {
		if _, ok := out[k]; !ok {
			t.Errorf("AttachmentDoc JSON missing field %q", k)
		}
	}
	// CapturedAt was nil → omitempty must drop it.
	if _, ok := out["captured_at"]; ok {
		t.Error("captured_at present despite nil CapturedAt; omitempty broken")
	}
}
