package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- brain_learn ---

func TestLearnSingleItemWritesToCurated(t *testing.T) {
	plan := map[string][]float32{
		"Curated Note\n\nbody contents": {1, 0, 0},
	}
	s, deps := setup(t, 3, plan)

	text, isErr := callTool(t, s.handleLearn, map[string]any{
		"content": "body contents",
		"title":   "Curated Note",
	})
	if isErr {
		t.Fatalf("err: %s", text)
	}
	if !strings.Contains(text, "1 stored") {
		t.Errorf("expected '1 stored' header; got %q", text)
	}
	if _, err := os.Stat(filepath.Join(deps.VaultDir, "Raw", "curated", "curated-note.md")); err != nil {
		t.Errorf("expected Raw/curated/curated-note.md: %v", err)
	}
}

func TestLearnBatchMode(t *testing.T) {
	plan := map[string][]float32{
		"A\n\na body": {1, 0, 0},
		"B\n\nb body": {0, 1, 0},
		"C\n\nc body": {0, 0, 1},
	}
	s, deps := setup(t, 3, plan)

	items := []any{
		map[string]any{"content": "a body", "title": "A"},
		map[string]any{"content": "b body", "title": "B"},
		map[string]any{"content": "c body", "title": "C"},
	}
	text, isErr := callTool(t, s.handleLearn, map[string]any{"items": items})
	if isErr {
		t.Fatalf("err: %s", text)
	}
	if !strings.Contains(text, "3 stored") {
		t.Errorf("header should report 3 stored: %q", text)
	}
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if _, err := os.Stat(filepath.Join(deps.VaultDir, "Raw", "curated", name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}

func TestLearnBatchReportsDuplicates(t *testing.T) {
	plan := map[string][]float32{
		"Solo\n\nbody": {1, 0, 0},
	}
	s, _ := setup(t, 3, plan)

	items := []any{
		map[string]any{"content": "body", "title": "Solo"},
		map[string]any{"content": "body", "title": "Solo"}, // exact dup
	}
	text, isErr := callTool(t, s.handleLearn, map[string]any{"items": items})
	if isErr {
		t.Fatalf("err: %s", text)
	}
	if !strings.Contains(text, "1 stored") || !strings.Contains(text, "1 duplicate") {
		t.Errorf("header should report 1 stored + 1 duplicate: %q", text)
	}
}

func TestLearnBatchTooLarge(t *testing.T) {
	s, _ := setup(t, 3, nil)
	items := make([]any, learnBatchMax+1)
	for i := range items {
		items[i] = map[string]any{"content": "x", "title": "y"}
	}
	text, isErr := callTool(t, s.handleLearn, map[string]any{"items": items})
	if !isErr {
		t.Errorf("oversized batch should error; got %q", text)
	}
}

func TestLearnRejectsEmptyInvocation(t *testing.T) {
	s, _ := setup(t, 3, nil)
	text, isErr := callTool(t, s.handleLearn, map[string]any{})
	if !isErr {
		t.Errorf("no args should error; got %q", text)
	}
}

// --- brain_attach ---

func TestAttachWritesBlobAndStub(t *testing.T) {
	plan := map[string][]float32{
		"My PDF\n\na useful research paper": {1, 0, 0},
	}
	s, deps := setup(t, 3, plan)

	// Make a small "binary" file. Content doesn't matter; the test
	// only checks SHA stability and write semantics.
	src := filepath.Join(t.TempDir(), "paper.pdf")
	if err := os.WriteFile(src, []byte("%PDF-1.4 not really a pdf"), 0o644); err != nil {
		t.Fatal(err)
	}

	text, isErr := callTool(t, s.handleAttach, map[string]any{
		"file_path":   src,
		"title":       "My PDF",
		"description": "a useful research paper",
		"source_url":  "https://example.com/paper.pdf",
	})
	if isErr {
		t.Fatalf("err: %s", text)
	}
	if !strings.Contains(text, "Attached") {
		t.Errorf("status should start with Attached; got %q", text)
	}

	// Find the stub: Raw/attachments/<sha>.md
	entries, err := os.ReadDir(filepath.Join(deps.VaultDir, "Raw", "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	var stub, blob string
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".md"):
			stub = e.Name()
		case strings.HasSuffix(e.Name(), ".pdf"):
			blob = e.Name()
		}
	}
	if stub == "" {
		t.Error("expected a .md stub")
	}
	if blob == "" {
		t.Error("expected a .pdf blob")
	}

	// FTS hit on description.
	hits, err := deps.Index.SearchText(context.Background(), "research", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Errorf("FTS hits = %d, want 1", len(hits))
	}
}

func TestAttachDuplicateOnSameInput(t *testing.T) {
	plan := map[string][]float32{
		"Dup\n\nsame description": {1, 0, 0},
	}
	s, _ := setup(t, 3, plan)

	src := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(src, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{
		"file_path":   src,
		"title":       "Dup",
		"description": "same description",
	}
	if _, isErr := callTool(t, s.handleAttach, args); isErr {
		t.Fatal("first attach should succeed")
	}
	text, isErr := callTool(t, s.handleAttach, args)
	if isErr {
		t.Fatalf("duplicate should not be error: %s", text)
	}
	if !strings.Contains(text, "Duplicate") {
		t.Errorf("expected Duplicate; got %q", text)
	}
}

func TestAttachRejectsMissingFile(t *testing.T) {
	s, _ := setup(t, 3, nil)
	text, isErr := callTool(t, s.handleAttach, map[string]any{
		"file_path": "/nonexistent/path/xyz.bin",
		"title":     "x",
	})
	if !isErr {
		t.Errorf("missing file should error; got %q", text)
	}
}

func TestAttachRejectsDirectory(t *testing.T) {
	s, _ := setup(t, 3, nil)
	dir := t.TempDir()
	text, isErr := callTool(t, s.handleAttach, map[string]any{
		"file_path": dir,
		"title":     "x",
	})
	if !isErr {
		t.Errorf("directory path should error; got %q", text)
	}
}

// --- brain_trace ---

func TestTraceReadsLastEntries(t *testing.T) {
	s, deps := setup(t, 3, nil)

	log := strings.Join([]string{
		"# Brain Synthesis Log",
		"",
		"## 2026-06-19T10:00:00Z — first batch",
		"- gathered/a.md -> summaries/a.md",
		"",
		"## 2026-06-19T12:00:00Z — second batch",
		"- gathered/b.md -> summaries/b.md",
		"",
		"## 2026-06-20T09:00:00Z — third batch",
		"- gathered/c.md -> summaries/c.md",
		"",
	}, "\n")

	logPath := filepath.Join(deps.VaultDir, "Wiki", "_log.md")
	if err := os.WriteFile(logPath, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}

	text, isErr := callTool(t, s.handleTrace, map[string]any{
		"limit": float64(2),
	})
	if isErr {
		t.Fatalf("err: %s", text)
	}
	if !strings.Contains(text, "third batch") {
		t.Errorf("most-recent entry should appear: %q", text)
	}
	if !strings.Contains(text, "second batch") {
		t.Errorf("limit=2 should include 2nd-most-recent: %q", text)
	}
	if strings.Contains(text, "first batch") {
		t.Errorf("limit=2 should NOT include 3rd-most-recent: %q", text)
	}
}

func TestTraceContainsFilter(t *testing.T) {
	s, deps := setup(t, 3, nil)

	log := strings.Join([]string{
		"## entry one — topic agents",
		"body 1",
		"## entry two — topic memory",
		"body 2",
		"## entry three — topic agents",
		"body 3",
	}, "\n")
	if err := os.WriteFile(filepath.Join(deps.VaultDir, "Wiki", "_log.md"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}

	text, isErr := callTool(t, s.handleTrace, map[string]any{
		"contains": "agents",
		"limit":    float64(10),
	})
	if isErr {
		t.Fatalf("err: %s", text)
	}
	if !strings.Contains(text, "entry one") || !strings.Contains(text, "entry three") {
		t.Errorf("filter should keep agents entries: %q", text)
	}
	if strings.Contains(text, "entry two") {
		t.Errorf("filter should exclude non-agents entries: %q", text)
	}
}

func TestTraceMissingLogFileFriendly(t *testing.T) {
	s, _ := setup(t, 3, nil)
	text, isErr := callTool(t, s.handleTrace, map[string]any{})
	if isErr {
		t.Errorf("missing log file should not error; got %q", text)
	}
	if !strings.Contains(text, "empty") {
		t.Errorf("expected friendly empty message; got %q", text)
	}
}

func TestTraceNoMatchesFriendly(t *testing.T) {
	s, deps := setup(t, 3, nil)
	if err := os.WriteFile(filepath.Join(deps.VaultDir, "Wiki", "_log.md"),
		[]byte("## entry alpha\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, isErr := callTool(t, s.handleTrace, map[string]any{
		"contains": "no-such-token-here",
	})
	if isErr {
		t.Fatalf("err: %s", text)
	}
	if !strings.Contains(text, "No log entries match") {
		t.Errorf("expected friendly no-match message; got %q", text)
	}
}
