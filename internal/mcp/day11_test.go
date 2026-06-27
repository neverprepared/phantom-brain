package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- brain_learn ---
//
// Phase D2b: writes are daemon-only. Single-item learn over the daemon is
// covered by daemon_post_test.go's TestLearn_PostsToDaemon. The batch path
// is exercised below against the recording daemon. Removed as
// testing-removed-behavior:
//
//   - TestLearnSingleItemWritesToCurated (redundant w/ TestLearn_PostsToDaemon)
//   - TestLearnBatchReportsDuplicates    (local dedup removed; daemon SHA-dedups)
//   - TestAttachWritesBlobAndStub        (redundant w/ TestAttach_PostsToDaemon)
//   - TestAttachDuplicateOnSameInput     (local dedup removed)

func TestLearnBatchMode(t *testing.T) {
	plan := map[string][]float32{
		"A\n\na body": {1, 0, 0},
		"B\n\nb body": {0, 1, 0},
		"C\n\nc body": {0, 0, 1},
	}
	s, _, daemon, cleanup := setupWithDaemon(t, 3, plan)
	defer cleanup()

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
	if got := len(daemon.calls("/api/brain/learn")); got != 3 {
		t.Errorf("daemon learn calls = %d, want 3", got)
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
