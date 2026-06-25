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
		"Raw/curated/legal-doc.txt":      kindLearn,     // #87: .txt now ingested
		"Raw/curated/scrape.html":        kindLearn,     // #87: .html now ingested
		"Raw/curated/scrape.HTM":         kindLearn,     // #87: case-insensitive
		"Raw/gathered/web.md":            kindPerceive,
		"Raw/gathered/sub/web.md":        kindPerceive,
		"Raw/gathered/page.txt":          kindPerceive,  // #87
		"Raw/gathered/page.html":         kindPerceive,  // #87
		"Raw/attachments/abc.pdf":        kindAttach,
		"Raw/attachments/abc.md":         kindSkip, // sidecar stub
		"Raw/attachments/scan.txt":       kindAttach, // text under attachments is still a blob
		"Raw/curated/data.json":          kindSkip,  // non-text extension stays skipped
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

func TestResolveAttachmentTitle(t *testing.T) {
	type sidecar struct {
		name string
		body string
	}
	cases := []struct {
		name    string
		attach  string
		side    *sidecar
		want    string
		fallbck string
	}{
		{
			name:    "frontmatter title (basename.md variant)",
			attach:  "Raw/attachments/hand_muscles.jpg",
			side:    &sidecar{name: "Raw/attachments/hand_muscles.md", body: "---\ntitle: Hand Muscles\n---\nbody"},
			want:    "Hand Muscles",
			fallbck: "hand_muscles",
		},
		{
			name:    "frontmatter title (basename.ext.md variant)",
			attach:  "Raw/attachments/hand_muscles.jpg",
			side:    &sidecar{name: "Raw/attachments/hand_muscles.jpg.md", body: "---\ntitle: Hand Muscles Stub\n---\nbody"},
			want:    "Hand Muscles Stub",
			fallbck: "hand_muscles",
		},
		{
			name:    "first H1 fallback when no frontmatter title",
			attach:  "Raw/attachments/anatomy.jpg",
			side:    &sidecar{name: "Raw/attachments/anatomy.md", body: "# Anatomy Reference\n\nbla"},
			want:    "Anatomy Reference",
			fallbck: "anatomy",
		},
		{
			name:    "no sidecar -> fallback",
			attach:  "Raw/attachments/lonely.jpg",
			side:    nil,
			want:    "lonely",
			fallbck: "lonely",
		},
		{
			name:    "sidecar without title or H1 -> fallback",
			attach:  "Raw/attachments/blank.jpg",
			side:    &sidecar{name: "Raw/attachments/blank.md", body: "just some body text"},
			want:    "blank",
			fallbck: "blank",
		},
		{
			name:    "malformed frontmatter -> fallback",
			attach:  "Raw/attachments/broken.jpg",
			side:    &sidecar{name: "Raw/attachments/broken.md", body: "---\ntitle: [unterminated\n---\nbody"},
			want:    "broken",
			fallbck: "broken",
		},
		{
			name:    "## h2 is not treated as title",
			attach:  "Raw/attachments/h2only.jpg",
			side:    &sidecar{name: "Raw/attachments/h2only.md", body: "## Not A Title\n\nbody"},
			want:    "h2only",
			fallbck: "h2only",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			attachAbs := filepath.Join(root, tc.attach)
			if err := os.MkdirAll(filepath.Dir(attachAbs), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(attachAbs, []byte("blob"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.side != nil {
				sidePath := filepath.Join(root, tc.side.name)
				if err := os.WriteFile(sidePath, []byte(tc.side.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := resolveAttachmentTitle(root, tc.attach, tc.fallbck, nil)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
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
