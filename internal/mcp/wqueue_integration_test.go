package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pbbrain "github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/brain/wqueue"
)

// setupWithQueue stands up the base rig with a daemon client + a real
// wqueue + a real Connectivity, plus a Lifecycle that exposes both to
// the MCP handlers. handler is the daemon's HTTP responder; pass a
// 5xx handler to drive the queued-write path, 2xx for the happy path.
func setupWithQueue(t *testing.T, dims int, plan map[string][]float32, handler http.Handler) (*Server, *wqueue.Queue, *pbbrain.Lifecycle, func()) {
	t.Helper()
	ts := httptest.NewServer(handler)

	client, err := pbbrain.NewClient(pbbrain.ClientOpts{
		BaseURL: ts.URL, Token: "test-token",
	})
	if err != nil {
		ts.Close()
		t.Fatalf("NewClient: %v", err)
	}

	qDir := t.TempDir()
	q, err := wqueue.Open(qDir)
	if err != nil {
		ts.Close()
		t.Fatalf("wqueue.Open: %v", err)
	}

	lc := pbbrain.NewLifecycleForTest(q, client, time.Time{})

	_, deps := setup(t, dims, plan)
	deps.Client = client
	deps.Lifecycle = lc
	s := NewServer(deps)

	cleanup := func() {
		_ = q.Close()
		ts.Close()
	}
	return s, q, lc, cleanup
}

func always5xx(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "unauth", http.StatusUnauthorized)
		return
	}
	http.Error(w, "daemon down", http.StatusServiceUnavailable)
}

func always200(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "unauth", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"sha":"x","indexed_at":1,"synth_enqueued":true}`))
}

// --- queued perceive on daemon failure ---------------------------

func TestPerceive_QueuesOnDaemonFailure(t *testing.T) {
	plan := map[string][]float32{
		"Page\n\nbody contents": {1, 0, 0},
	}
	s, q, lc, cleanup := setupWithQueue(t, 3, plan, http.HandlerFunc(always5xx))
	defer cleanup()

	text, isErr := callTool(t, s.handlePerceive, map[string]any{
		"content": "body contents", "title": "Page",
	})
	if isErr {
		t.Fatalf("expected success-with-notice, got error: %s", text)
	}
	if !strings.Contains(text, "Queued (daemon") || !strings.Contains(text, "pending") {
		t.Errorf("expected queued notice in result; got: %q", text)
	}
	depth, _ := q.Depth(t.Context())
	if depth != 1 {
		t.Errorf("queue depth = %d, want 1", depth)
	}
	// Connectivity: no prior success means it stays offline after a
	// failure (locked decision).
	state := lc.Connectivity().Snapshot().State
	if state != pbbrain.ConnOffline {
		t.Errorf("connectivity = %q, want %q (no prior success)", state, pbbrain.ConnOffline)
	}
}

func TestPerceive_PostsDirectlyOnDaemonSuccess(t *testing.T) {
	plan := map[string][]float32{
		"Page\n\nbody contents": {1, 0, 0},
	}
	s, q, lc, cleanup := setupWithQueue(t, 3, plan, http.HandlerFunc(always200))
	defer cleanup()

	text, isErr := callTool(t, s.handlePerceive, map[string]any{
		"content": "body contents", "title": "Page",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if strings.Contains(text, "Queued (daemon") {
		t.Errorf("did not expect queued notice on happy path; got: %q", text)
	}
	depth, _ := q.Depth(t.Context())
	if depth != 0 {
		t.Errorf("queue depth = %d, want 0 (item should have been deleted after success)", depth)
	}
	if lc.Connectivity().Snapshot().State != pbbrain.ConnOnline {
		t.Errorf("expected ConnOnline after first success")
	}
}

// --- attach: staging file written then drained on success --------

func TestAttach_QueuesBytesAndDrainsOnSuccess(t *testing.T) {
	plan := map[string][]float32{
		"Manual\n\nthe operator manual": {0, 0, 1},
	}
	s, q, _, cleanup := setupWithQueue(t, 3, plan, http.HandlerFunc(always200))
	defer cleanup()

	tmp := t.TempDir()
	pdfPath := filepath.Join(tmp, "manual.pdf")
	if err := os.WriteFile(pdfPath, []byte("%PDF-fake"), 0o644); err != nil {
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
	// Success path: staging dir should be empty post-drain.
	entries, _ := os.ReadDir(q.StageDir())
	if len(entries) != 0 {
		t.Errorf("staging dir has %d entries after success drain, want 0", len(entries))
	}
}

func TestAttach_StagesBytesWhenDaemonDown(t *testing.T) {
	plan := map[string][]float32{
		"Manual\n\nthe operator manual": {0, 0, 1},
	}
	s, q, _, cleanup := setupWithQueue(t, 3, plan, http.HandlerFunc(always5xx))
	defer cleanup()

	tmp := t.TempDir()
	pdfPath := filepath.Join(tmp, "manual.pdf")
	body := []byte("%PDF-fake-bytes-here")
	if err := os.WriteFile(pdfPath, body, 0o644); err != nil {
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
	if !strings.Contains(text, "Queued (daemon") {
		t.Errorf("expected Queued notice; got: %q", text)
	}
	entries, _ := os.ReadDir(q.StageDir())
	if len(entries) != 1 {
		t.Fatalf("staging dir has %d entries, want 1", len(entries))
	}
	got, _ := os.ReadFile(filepath.Join(q.StageDir(), entries[0].Name()))
	if string(got) != string(body) {
		t.Errorf("staged bytes mismatch")
	}
}

// --- brain_status reports degraded fields ------------------------

func TestBrainStatus_ReportsConnectivityAndQueueDepth(t *testing.T) {
	plan := map[string][]float32{
		"Page\n\nbody contents": {1, 0, 0},
	}
	s, _, _, cleanup := setupWithQueue(t, 3, plan, http.HandlerFunc(always5xx))
	defer cleanup()

	// One failed perceive to populate queue depth + flip connectivity.
	if _, isErr := callTool(t, s.handlePerceive, map[string]any{
		"content": "body contents", "title": "Page",
	}); isErr {
		t.Fatal("unexpected error from perceive")
	}

	text, isErr := callTool(t, s.handleBrainStatus, map[string]any{})
	if isErr {
		t.Fatalf("brain_status error: %s", text)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode brain_status: %v", err)
	}
	if got["connectivity"] != "offline" {
		t.Errorf("connectivity = %v, want offline", got["connectivity"])
	}
	if qw, _ := got["queued_writes"].(float64); int(qw) != 1 {
		t.Errorf("queued_writes = %v, want 1", got["queued_writes"])
	}
	if _, ok := got["last_daemon_contact_secs"]; !ok {
		t.Errorf("missing last_daemon_contact_secs field")
	}
	if _, ok := got["snapshot_age_secs"]; !ok {
		t.Errorf("missing snapshot_age_secs field")
	}
}

// --- recall footer for stale snapshot ---------------------------
//
// Phase D1: brain_recall is online-only now; the local-snapshot recall
// path (and its snapshot-staleness footer) was removed in the Postgres
// cutover. The footer the online path emits is a "live / always fresh"
// note, not a snapshot-age one, so these two tests assert behaviour that
// no longer exists. Removed (the online recall footer is covered by
// recall_online_test.go):
//
//   - TestRecall_AppendsSnapshotFooterWhenStale (asserted the stale footer
//     — impossible online-only; errored without a recall client)
//   - TestRecall_NoFooterWhenFresh (passed only vacuously: recall errored
//     and the error text happened not to contain "_Snapshot")
