package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

// fakeRecallClient is the test seam for the daemon online-recall call.
// resp/err drive the handler down either the online-success or the
// online-failure→snapshot-fallback branch; called records invocation so
// the OnlineRecall=false case can assert the daemon was NOT consulted.
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

// seedSnapshotDoc ingests one doc into the local index so the snapshot
// (fallback / OnlineRecall-off) path has something to return.
func seedSnapshotDoc(t *testing.T, s *Server, title, body string, vec []float32) {
	t.Helper()
	// brain_perceive writes to the local index in the legacy/no-Client
	// test setup, giving SearchHybrid a hit to find.
	text, isErr := callTool(t, s.handlePerceive, map[string]any{
		"content": body,
		"title":   title,
	})
	if isErr {
		t.Fatalf("seed perceive failed: %s", text)
	}
}

// TestRecallOnline_Success: OnlineRecall=true + a daemon that returns
// hits → output renders the online hits + the live footer, and the
// local snapshot search is NOT consulted.
func TestRecallOnline_Success(t *testing.T) {
	query := "loop engineering"
	plan := map[string][]float32{query: {1, 0, 0}}
	s, deps := setup(t, 3, plan)

	fake := &fakeRecallClient{
		resp: &brain.RecallResponse{Hits: []brain.RecallHitDTO{
			{SHA: "abc123", Title: "Live Note", Kind: "note", Snippet: "fresh from daemon", Score: 0.9},
		}},
	}
	deps.RecallClient = fake
	deps.OnlineRecall = true
	s = NewServer(deps)

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
	if strings.Contains(text, "snapshot") {
		t.Errorf("online success should not mention the snapshot: %q", text)
	}
	// The query embedding must have been forwarded to the daemon.
	if len(fake.gotReq.Embedding) != 3 {
		t.Errorf("expected query embedding forwarded, got %v", fake.gotReq.Embedding)
	}
}

// TestRecallOnline_FallbackOnDaemonUnreachable: OnlineRecall=true but the
// daemon is unreachable → the handler falls back to the local snapshot
// search and notes the degradation. Recall NEVER errors out.
func TestRecallOnline_FallbackOnDaemonUnreachable(t *testing.T) {
	query := "loop engineering"
	plan := map[string][]float32{query: {1, 0, 0}}
	s, deps := setup(t, 3, plan)

	// Seed a doc the snapshot fallback can return.
	plan["Loop Note\n\nloop engineering body"] = []float32{1, 0, 0}
	deps.Embedder = &fakeEmbedder{dims: 3, plan: plan}
	s = NewServer(deps)
	seedSnapshotDoc(t, s, "Loop Note", "loop engineering body", []float32{1, 0, 0})

	fake := &fakeRecallClient{err: brain.ErrDaemonUnreachable}
	deps.RecallClient = fake
	deps.OnlineRecall = true
	s = NewServer(deps)

	text, isErr := callTool(t, s.handleRecall, map[string]any{"query": query})
	if isErr {
		t.Fatalf("recall must not error on daemon failure; got: %s", text)
	}
	if !fake.called {
		t.Fatal("expected online Recall to be attempted")
	}
	if !strings.Contains(text, "online recall unavailable") {
		t.Errorf("output missing fallback note: %q", text)
	}
	if !strings.Contains(text, "daemon unreachable") {
		t.Errorf("fallback note should name the reason: %q", text)
	}
	if !strings.Contains(text, "local snapshot") {
		t.Errorf("fallback note should mention local snapshot: %q", text)
	}
	if strings.Contains(text, "live results from daemon") {
		t.Errorf("fallback path must not show the live footer: %q", text)
	}
}

// TestRecallOnline_Disabled: OnlineRecall=false → the daemon is never
// consulted; the existing local-snapshot path runs unchanged.
func TestRecallOnline_Disabled(t *testing.T) {
	query := "loop engineering"
	plan := map[string][]float32{
		query:                            {1, 0, 0},
		"Loop Note\n\nloop engineering body": {1, 0, 0},
	}
	s, deps := setup(t, 3, plan)
	s = NewServer(deps)
	seedSnapshotDoc(t, s, "Loop Note", "loop engineering body", []float32{1, 0, 0})

	fake := &fakeRecallClient{
		resp: &brain.RecallResponse{Hits: []brain.RecallHitDTO{{SHA: "x", Title: "Should Not Appear"}}},
	}
	deps.RecallClient = fake
	deps.OnlineRecall = false
	s = NewServer(deps)

	text, isErr := callTool(t, s.handleRecall, map[string]any{"query": query})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if fake.called {
		t.Fatal("OnlineRecall=false must NOT call the daemon")
	}
	if strings.Contains(text, "Should Not Appear") {
		t.Errorf("online hit leaked into disabled path: %q", text)
	}
	if strings.Contains(text, "live results from daemon") {
		t.Errorf("disabled path must not show the live footer: %q", text)
	}
	if !strings.Contains(text, "Loop Note") {
		t.Errorf("snapshot path should surface the seeded doc: %q", text)
	}
}
