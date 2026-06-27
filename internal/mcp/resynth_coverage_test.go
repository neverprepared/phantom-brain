package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pbbrain "github.com/neverprepared/phantom-brain/internal/brain"
)

// resynthDaemon stands up a stub serving /api/brain/resynth. respFn maps
// the decoded request body (dry_run, limit) to the JSON response so a
// single test can assert dry-run vs apply behaviour, and captures the last
// request for forwarding assertions.
type resynthDaemon struct {
	lastBody map[string]any
	respFn   func(req map[string]any) string
	status   int
}

func (d *resynthDaemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/brain/resynth", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		d.lastBody = parsed
		w.Header().Set("Content-Type", "application/json")
		if d.status != 0 && d.status != http.StatusOK {
			w.WriteHeader(d.status)
			_, _ = w.Write([]byte(`{"code":"BOOM","message":"resynth blew up"}`))
			return
		}
		_, _ = w.Write([]byte(d.respFn(parsed)))
	})
	return mux
}

func newResynthServer(t *testing.T, d *resynthDaemon) (*Server, func()) {
	t.Helper()
	ts := httptest.NewServer(d.handler())
	client, err := pbbrain.NewClient(pbbrain.ClientOpts{BaseURL: ts.URL, Token: "tok"})
	if err != nil {
		ts.Close()
		t.Fatalf("NewClient: %v", err)
	}
	_, deps := setup(t, 3, map[string][]float32{})
	deps.Client = client
	return NewServer(deps), ts.Close
}

// TestResynth_DryRunDefault: dry_run defaults TRUE when the arg is absent.
// The daemon must receive dry_run=true, and the output reports the backlog
// + sample + the "Dry run — nothing changed" guidance.
func TestResynth_DryRunDefault(t *testing.T) {
	d := &resynthDaemon{respFn: func(_ map[string]any) string {
		return `{"backlog_count":2,"sample":[{"sha":"s1","title":"First"},{"sha":"s2","title":""}]}`
	}}
	s, cleanup := newResynthServer(t, d)
	defer cleanup()

	text, isErr := callTool(t, s.handleResynth, map[string]any{})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if d.lastBody["dry_run"] != true {
		t.Errorf("expected dry_run defaulted true, daemon got %v", d.lastBody["dry_run"])
	}
	if !strings.Contains(text, "2 doc(s) stuck") {
		t.Errorf("missing backlog count: %s", text)
	}
	if !strings.Contains(text, "s1") || !strings.Contains(text, "First") {
		t.Errorf("missing sample row: %s", text)
	}
	// Blank-title sample row renders "(untitled)".
	if !strings.Contains(text, "(untitled)") {
		t.Errorf("blank title should render (untitled): %s", text)
	}
	if !strings.Contains(text, "Dry run — nothing changed") {
		t.Errorf("missing dry-run guidance: %s", text)
	}
	// Re-synthesised-not-deleted reminder always present.
	if !strings.Contains(text, "not deleted") {
		t.Errorf("missing keep-not-delete note: %s", text)
	}
}

// TestResynth_ApplyStartsBackfill: dry_run=false with a non-empty backlog
// the daemon reports Started=true → output announces the background work.
func TestResynth_ApplyStartsBackfill(t *testing.T) {
	d := &resynthDaemon{respFn: func(_ map[string]any) string {
		return `{"backlog_count":5,"started":true,"pending":5}`
	}}
	s, cleanup := newResynthServer(t, d)
	defer cleanup()

	text, isErr := callTool(t, s.handleResynth, map[string]any{"dry_run": false, "limit": float64(10)})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if d.lastBody["dry_run"] != false {
		t.Errorf("expected dry_run=false forwarded, got %v", d.lastBody["dry_run"])
	}
	if got, _ := d.lastBody["limit"].(float64); int(got) != 10 {
		t.Errorf("expected limit=10 forwarded, got %v", d.lastBody["limit"])
	}
	if !strings.Contains(text, "Started re-synthesis of 5 doc(s)") {
		t.Errorf("missing started announcement: %s", text)
	}
	if strings.Contains(text, "Dry run — nothing changed") {
		t.Errorf("apply path should not print dry-run guidance: %s", text)
	}
}

// TestResynth_ApplyNothingToDo: dry_run=false but the daemon reports
// Started=false (backlog already clear) → "Nothing to re-synthesize".
func TestResynth_ApplyNothingToDo(t *testing.T) {
	d := &resynthDaemon{respFn: func(_ map[string]any) string {
		return `{"backlog_count":0,"started":false,"pending":0}`
	}}
	s, cleanup := newResynthServer(t, d)
	defer cleanup()

	text, isErr := callTool(t, s.handleResynth, map[string]any{"dry_run": false})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if !strings.Contains(text, "Nothing to re-synthesize") {
		t.Errorf("missing nothing-to-do branch: %s", text)
	}
	// No sample section when the sample is empty.
	if strings.Contains(text, "Sample (up to") {
		t.Errorf("empty sample should omit the Sample header: %s", text)
	}
}

// TestResynth_DaemonError: a non-2xx daemon response surfaces as a tool
// error naming the tool.
func TestResynth_DaemonError(t *testing.T) {
	d := &resynthDaemon{status: http.StatusInternalServerError, respFn: func(_ map[string]any) string { return "" }}
	s, cleanup := newResynthServer(t, d)
	defer cleanup()

	text, isErr := callTool(t, s.handleResynth, map[string]any{})
	if !isErr {
		t.Fatalf("expected tool error on daemon failure, got: %s", text)
	}
	if !strings.Contains(text, "brain_resynth") {
		t.Errorf("error should name the tool: %s", text)
	}
}

// TestResynth_LegacyModeErrors: with no daemon client (legacy
// BRAIN_VAULT_PATH mode) brain_resynth is unavailable.
func TestResynth_LegacyModeErrors(t *testing.T) {
	_, deps := setup(t, 3, map[string][]float32{})
	deps.Client = nil
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleResynth, map[string]any{})
	if !isErr {
		t.Fatalf("expected error in legacy mode, got: %s", text)
	}
	if !strings.Contains(text, "legacy BRAIN_VAULT_PATH mode") {
		t.Errorf("error should explain legacy mode: %s", text)
	}
}

// TestResynthTool_Schema asserts the tool advertises its name and the
// optional dry_run / limit knobs.
func TestResynthTool_Schema(t *testing.T) {
	tool := resynthTool()
	if tool.Name != "brain_resynth" {
		t.Errorf("tool name = %q, want brain_resynth", tool.Name)
	}
	if _, ok := tool.InputSchema.Properties["dry_run"]; !ok {
		t.Errorf("schema missing dry_run property")
	}
	if _, ok := tool.InputSchema.Properties["limit"]; !ok {
		t.Errorf("schema missing limit property")
	}
}
