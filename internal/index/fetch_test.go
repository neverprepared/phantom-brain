package index

import (
	"context"
	"strings"
	"testing"
)

// TestFetchBySHA covers the exact gap brain_fetch exists to close: a
// value (here a URL) that sits past recall's 150-char snippet cutoff is
// invisible to recall but returned in full by FetchBySHA.
func TestFetchBySHA(t *testing.T) {
	idx := openTest(t, 4)
	ctx := context.Background()

	// The URL is deliberately past char 150 so a snippet would clip it.
	prefix := strings.Repeat("Lakeview maintains an internal DNS and IP inventory tool. ", 4)
	url := "https://lakeviewloanservicing.github.io/dns-inventory/"
	body := prefix + "It is accessible at a GitHub Pages URL: " + url + " (read-only)."

	if err := idx.Upsert(ctx, Record{
		SHA:        "c471d33e",
		SourcePath: "Raw/curated/dns-inventory.md",
		Title:      "Lakeview internal DNS / IP inventory tool",
		Tags:       "vendor:lakeview type:tool",
		Kind:       "note",
		Body:       body,
		Embedding:  []float32{0.1, 0.2, 0.3, 0.4},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	res, err := idx.FetchBySHA(ctx, "c471d33e")
	if err != nil {
		t.Fatalf("FetchBySHA: %v", err)
	}
	if res == nil {
		t.Fatal("expected a result, got nil")
	}
	if res.Body != body {
		t.Errorf("body truncated: got %d chars, want %d", len(res.Body), len(body))
	}
	if !strings.Contains(res.Body, url) {
		t.Errorf("URL past the snippet cutoff missing from fetched body")
	}
	if res.Title != "Lakeview internal DNS / IP inventory tool" || res.Kind != "note" ||
		res.SourcePath != "Raw/curated/dns-inventory.md" {
		t.Errorf("metadata wrong: %+v", res)
	}

	// Unknown SHA → (nil, nil), not an error.
	miss, err := idx.FetchBySHA(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("FetchBySHA miss: %v", err)
	}
	if miss != nil {
		t.Errorf("expected nil for unknown sha, got %+v", miss)
	}

	// Empty SHA → error.
	if _, err := idx.FetchBySHA(ctx, ""); err == nil {
		t.Error("expected error for empty sha")
	}
}
