package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyIngestPath(t *testing.T) {
	cases := map[string]ingestKind{
		"Raw/curated/notes.md":           kindLearn,
		"Raw/curated/sub/notes.md":       kindLearn,
		"Raw/gathered/web.md":            kindPerceive,
		"Raw/gathered/sub/web.md":        kindPerceive,
		"Raw/attachments/abc.pdf":        kindAttach,
		"Raw/attachments/abc.md":         kindSkip, // sidecar stub
		"Wiki/summaries/foo.md":          kindSkip,
		"_index/provenance.json":         kindSkip,
		"random.md":                      kindSkip,
		"Raw/other/x.md":                 kindSkip,
	}
	for in, want := range cases {
		if got := classifyIngestPath(in); got != want {
			t.Errorf("classifyIngestPath(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestScanVaultForIngest_CountsByKind(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel string, body []byte) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("Raw/curated/a.md", []byte("---\ntitle: A\n---\nbody"))
	mustWrite("Raw/curated/b.md", []byte("---\ntitle: B\n---\nbody"))
	mustWrite("Raw/gathered/c.md", []byte("---\ntitle: C\n---\nbody"))
	mustWrite("Raw/attachments/file.pdf", []byte("%PDF-1.4"))
	mustWrite("Raw/attachments/file.pdf.md", []byte("stub"))
	mustWrite("Wiki/summaries/derived.md", []byte("synth"))
	mustWrite(".git/HEAD", []byte("ref"))
	mustWrite("README.md", []byte("ignored"))

	plan, err := scanVaultForIngest(root, 10*1024*1024)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if plan.learn != 2 {
		t.Errorf("learn = %d, want 2", plan.learn)
	}
	if plan.perceive != 1 {
		t.Errorf("perceive = %d, want 1", plan.perceive)
	}
	if plan.attach != 1 {
		t.Errorf("attach = %d, want 1 (binary), got %d", plan.attach, plan.attach)
	}
	// .md sidecar + README.md both classified as skip. Wiki/ and
	// .git/ are pruned at dir level so their contents never reach
	// the classifier.
	if plan.skipped < 2 {
		t.Errorf("skipped = %d, want >= 2", plan.skipped)
	}
}

func TestScanVaultForIngest_RejectsOversizedFiles(t *testing.T) {
	root := t.TempDir()
	bigPath := filepath.Join(root, "Raw", "attachments", "big.bin")
	if err := os.MkdirAll(filepath.Dir(bigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bigPath, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := scanVaultForIngest(root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if plan.attach != 0 {
		t.Errorf("attach = %d, want 0 (file > max)", plan.attach)
	}
	if plan.skipped < 1 {
		t.Error("expected oversized file to be skipped")
	}
}

func TestGuessAttachmentMIME(t *testing.T) {
	if guessAttachmentMIME(".pdf") != "application/pdf" {
		t.Error(".pdf mismatch")
	}
	if !strings.HasPrefix(guessAttachmentMIME(".png"), "image/") {
		t.Error(".png should be image/*")
	}
	if guessAttachmentMIME(".unknown") != "application/octet-stream" {
		t.Error("unknown ext should default to octet-stream")
	}
}
