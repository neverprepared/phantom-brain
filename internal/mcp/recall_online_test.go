package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

// fakeRecallClient is the test seam for the daemon online-recall call.
// resp/err drive the handler down either the online-success or the
// online-failure→tool-error branch; called records invocation.
type fakeRecallClient struct {
	resp   *brain.RecallResponse
	err    error
	called bool
	gotReq brain.RecallRequest
}

func (f *fakeRecallClient) Recall(_ context.Context, req brain.RecallRequest) (*brain.RecallResponse, error) {
	f.called = true
	f.gotReq = req
	return f.resp, f.err
}

// TestRecallOnline_Success: a daemon that returns hits → output renders
// the online hits + the live footer. Phase D1: recall is online-only, so
// the daemon is ALWAYS consulted.
func TestRecallOnline_Success(t *testing.T) {
	query := "loop engineering"
	plan := map[string][]float32{query: {1, 0, 0}}
	_, deps := setup(t, 3, plan)

	fake := &fakeRecallClient{
		resp: &brain.RecallResponse{Hits: []brain.RecallHitDTO{
			{SHA: "abc123", Title: "Live Note", Kind: "note", Snippet: "fresh from daemon", Score: 0.9},
		}},
	}
	deps.RecallClient = fake
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleRecall, map[string]any{"query": query})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if !fake.called {
		t.Fatal("expected online Recall to be called")
	}
	if !strings.Contains(text, "Live Note") {
		t.Errorf("output missing online hit title: %q", text)
	}
	if !strings.Contains(text, "abc123") {
		t.Errorf("output missing online hit SHA: %q", text)
	}
	if !strings.Contains(text, "live results from daemon") {
		t.Errorf("output missing live footer: %q", text)
	}
	// The query embedding must have been forwarded to the daemon.
	if len(fake.gotReq.Embedding) != 3 {
		t.Errorf("expected query embedding forwarded, got %v", fake.gotReq.Embedding)
	}
}

// TestRecallOnline_DaemonError: the daemon is unreachable → Phase D1 has
// no local fallback, so brain_recall returns a clear tool error naming the
// reason. (Pre-D1 this fell back to the local snapshot; that path was
// removed in the cutover.)
func TestRecallOnline_DaemonError(t *testing.T) {
	query := "loop engineering"
	plan := map[string][]float32{query: {1, 0, 0}}
	_, deps := setup(t, 3, plan)

	fake := &fakeRecallClient{err: brain.ErrDaemonUnreachable}
	deps.RecallClient = fake
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleRecall, map[string]any{"query": query})
	if !isErr {
		t.Fatalf("expected a tool error on daemon failure, got success: %s", text)
	}
	if !fake.called {
		t.Fatal("expected online Recall to be attempted")
	}
	if !strings.Contains(text, "daemon unreachable") {
		t.Errorf("error should name the reason: %q", text)
	}
	if strings.Contains(text, "live results from daemon") {
		t.Errorf("error path must not show the live footer: %q", text)
	}
}

// TestRecallOnline_NoClient: no RecallClient wired → brain_recall is
// online-only and returns a clear "not configured" tool error rather than
// serving stale local data.
func TestRecallOnline_NoClient(t *testing.T) {
	query := "loop engineering"
	plan := map[string][]float32{query: {1, 0, 0}}
	_, deps := setup(t, 3, plan)
	deps.RecallClient = nil
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleRecall, map[string]any{"query": query})
	if !isErr {
		t.Fatalf("expected a tool error with no recall client, got success: %s", text)
	}
	if !strings.Contains(text, "online-only") {
		t.Errorf("error should explain online-only requirement: %q", text)
	}
}

// TestOnlineKindIndicator covers the pure label helper (the online
// successor to the snapshot path's kindIndicator, which went away with the
// snapshot-recall code). Asserts the closed-set kinds plus the
// MIME-derived attachment label and the empty/unknown fallbacks.
func TestOnlineKindIndicator(t *testing.T) {
	cases := []struct {
		kind, mime string
		want       string
	}{
		{"note", "", "[note]"},
		{"web_scrape", "", "[web]"},
		{"task_summary", "", "[task]"},
		{"email_import", "", "[email]"},
		{"manual_curate", "", "[curated]"},
		{"attachment_stub", "application/pdf", "[attachment pdf]"},
		{"attachment_stub", "", "[attachment]"},
		{"attachment_stub", "weird", "[attachment weird]"},
		{"", "", "[unknown]"},
		{"future_kind", "", "[future_kind]"},
	}
	for _, c := range cases {
		got := onlineKindIndicator(brain.RecallHitDTO{Kind: c.kind, MimeType: c.mime})
		if got != c.want {
			t.Errorf("onlineKindIndicator(kind=%q,mime=%q) = %q, want %q", c.kind, c.mime, got, c.want)
		}
	}
}

// TestRenderOnlineRecallHits checks the rendered output carries title,
// SHA, the live footer, and — for attachments — the fetch hint with the
// original filename. The empty-hit path still shows the live footer.
func TestRenderOnlineRecallHits(t *testing.T) {
	out := renderOnlineRecallHits("q", []brain.RecallHitDTO{
		{SHA: "deadbeef", Title: "A Note", Kind: "note", Snippet: "snip", Score: 0.5},
		{SHA: "att1", Title: "Invoice", Kind: "attachment_stub", MimeType: "application/pdf", OriginalFilename: "inv.pdf", Score: 0.4},
	})
	for _, want := range []string{"A Note", "deadbeef", "[note]", "snip",
		"Invoice", "[attachment pdf]", "GET /api/brain/attach/att1", "inv.pdf",
		"live results from daemon"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q:\n%s", want, out)
		}
	}

	empty := renderOnlineRecallHits("nothing", nil)
	if !strings.Contains(empty, "No results") || !strings.Contains(empty, "live results from daemon") {
		t.Errorf("empty render missing no-results + footer: %q", empty)
	}
}
