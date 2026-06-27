package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	pbbrain "github.com/neverprepared/phantom-brain/internal/brain"
)

// recordingDaemon stands in for the Phase 6 daemon. It echoes the
// request body back as a 202 and records the parsed JSON so the test
// can assert on what the agent actually sent.
type recordingDaemon struct {
	mu       sync.Mutex
	requests map[string][]map[string]any
}

func newRecordingDaemon() *recordingDaemon {
	return &recordingDaemon{requests: map[string][]map[string]any{}}
}

func (d *recordingDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
		http.Error(w, "unauthorized: "+got, http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	d.mu.Lock()
	d.requests[r.URL.Path] = append(d.requests[r.URL.Path], parsed)
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"sha":"x","indexed_at":1,"synth_enqueued":true}`))
}

func (d *recordingDaemon) calls(path string) []map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]map[string]any, len(d.requests[path]))
	copy(out, d.requests[path])
	return out
}

// setupWithDaemon adds a daemon Client to the base setup() rig so
// perceive/learn/attach take the POST path.
func setupWithDaemon(t *testing.T, dims int, plan map[string][]float32) (*Server, ServerDeps, *recordingDaemon, func()) {
	t.Helper()
	daemon := newRecordingDaemon()
	ts := httptest.NewServer(daemon)
	client, err := pbbrain.NewClient(pbbrain.ClientOpts{
		BaseURL: ts.URL,
		Token:   "test-token",
	})
	if err != nil {
		ts.Close()
		t.Fatalf("NewClient: %v", err)
	}

	_, deps := setup(t, dims, plan)
	deps.Client = client
	s := NewServer(deps)
	return s, deps, daemon, ts.Close
}

// --- perceive over daemon ----------------------------------------

func TestPerceive_PostsToDaemon_AndSkipsLocalWrite(t *testing.T) {
	plan := map[string][]float32{
		"My Page\n\nbody contents here": {1, 0, 0},
	}
	s, deps, daemon, cleanup := setupWithDaemon(t, 3, plan)
	defer cleanup()

	text, isErr := callTool(t, s.handlePerceive, map[string]any{
		"content":    "body contents here",
		"title":      "My Page",
		"source_url": "https://example.com/page",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if !strings.Contains(text, "Stored to Raw/gathered/my-page.md") {
		t.Errorf("status missing path: %q", text)
	}

	// Daemon received exactly one POST /api/brain/perceive with the
	// right shape (SHA non-empty, title + body present, URL preserved,
	// embedding length matches dims).
	calls := daemon.calls("/api/brain/perceive")
	if len(calls) != 1 {
		t.Fatalf("daemon call count = %d, want 1", len(calls))
	}
	body := calls[0]
	if body["title"] != "My Page" {
		t.Errorf("title = %v", body["title"])
	}
	if body["body"] != "body contents here" {
		t.Errorf("body = %v", body["body"])
	}
	if body["url"] != "https://example.com/page" {
		t.Errorf("url = %v", body["url"])
	}
	if sha, _ := body["sha"].(string); len(sha) != 64 {
		t.Errorf("sha = %v, want 64-char hex", body["sha"])
	}
	emb, _ := body["embedding"].([]any)
	if len(emb) != 3 {
		t.Errorf("embedding len = %d, want 3", len(emb))
	}

	// No local file written — Phase D2b writes are daemon-only.
	if _, err := os.Stat(filepath.Join(deps.VaultDir, "Raw", "gathered", "my-page.md")); !os.IsNotExist(err) {
		t.Errorf("local file unexpectedly exists (writes are daemon-only): err=%v", err)
	}
}

// --- learn over daemon -------------------------------------------

func TestLearn_PostsToDaemon(t *testing.T) {
	plan := map[string][]float32{
		"My Curated Note\n\nthis is curated content": {0, 1, 0},
	}
	s, _, daemon, cleanup := setupWithDaemon(t, 3, plan)
	defer cleanup()

	text, isErr := callTool(t, s.handleLearn, map[string]any{
		"content": "this is curated content",
		"title":   "My Curated Note",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	calls := daemon.calls("/api/brain/learn")
	if len(calls) != 1 {
		t.Fatalf("learn calls = %d", len(calls))
	}
	if calls[0]["title"] != "My Curated Note" {
		t.Errorf("title = %v", calls[0]["title"])
	}
	// Perceive endpoint should NOT have been hit — routing is per-tool.
	if got := daemon.calls("/api/brain/perceive"); len(got) != 0 {
		t.Errorf("perceive calls = %d, want 0", len(got))
	}
}

// --- attach over daemon ------------------------------------------

func TestAttach_PostsToDaemon(t *testing.T) {
	plan := map[string][]float32{
		"Manual\n\nthe operator manual": {0, 0, 1},
	}
	s, _, daemon, cleanup := setupWithDaemon(t, 3, plan)
	defer cleanup()

	// Make a fake "PDF" the handler can read + base64.
	tmp := t.TempDir()
	pdfPath := filepath.Join(tmp, "manual.pdf")
	bodyBytes := []byte("%PDF-fake-bytes")
	if err := os.WriteFile(pdfPath, bodyBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	text, isErr := callTool(t, s.handleAttach, map[string]any{
		"file_path":   pdfPath,
		"title":       "Manual",
		"description": "the operator manual",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}

	calls := daemon.calls("/api/brain/attach")
	if len(calls) != 1 {
		t.Fatalf("attach calls = %d, want 1", len(calls))
	}
	body := calls[0]
	if body["original_filename"] != "manual.pdf" {
		t.Errorf("original_filename = %v", body["original_filename"])
	}
	if body["mime_type"] != "application/pdf" {
		t.Errorf("mime_type = %v, want application/pdf", body["mime_type"])
	}
	if !strings.Contains(text, "via daemon") {
		t.Errorf("expected daemon-path success string, got: %q", text)
	}
}

// Phase D2b: TestPerceive_DedupShortCircuitsBeforeDaemon was removed.
// There is no local read cache to dedup against — the daemon SHA-dedups
// every write idempotently, so the agent always POSTs and a re-paste is a
// benign no-op upsert daemon-side.
