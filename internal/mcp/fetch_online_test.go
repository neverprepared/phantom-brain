package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

// fakeFetchClient is the test seam for the daemon online-fetch call,
// mirroring fakeRecallClient. resp/err drive the handler down the
// success / daemon-error branches; called records invocation; gotSHA
// captures the forwarded SHA.
type fakeFetchClient struct {
	resp   *brain.FetchResponse
	err    error
	called bool
	gotSHA string
}

func (f *fakeFetchClient) Fetch(_ context.Context, sha string) (*brain.FetchResponse, error) {
	f.called = true
	f.gotSHA = sha
	return f.resp, f.err
}

// TestFetchOnline_Success: a daemon that returns a record → output renders
// the title, kind, source, tags, and full body. Phase D2a: brain_fetch is
// online-only, so the daemon is ALWAYS consulted.
func TestFetchOnline_Success(t *testing.T) {
	_, deps := setup(t, 3, nil)

	fake := &fakeFetchClient{
		resp: &brain.FetchResponse{
			SHA:       "abc123",
			Title:     "Live Doc",
			Kind:      "note",
			SourceURL: "https://example.com/x",
			Tags:      []string{"alpha", "beta"},
			Body:      "the full untruncated body from the daemon",
		},
	}
	deps.FetchClient = fake
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleFetch, map[string]any{"sha": "abc123"})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if !fake.called {
		t.Fatal("expected online Fetch to be called")
	}
	if fake.gotSHA != "abc123" {
		t.Errorf("expected sha forwarded, got %q", fake.gotSHA)
	}
	for _, want := range []string{"Live Doc", "note", "https://example.com/x",
		"alpha, beta", "abc123", "the full untruncated body from the daemon"} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
}

// TestFetchOnline_NotFound: the daemon returns a 404 APIError → brain_fetch
// renders a friendly "no document" text result (not a tool error).
func TestFetchOnline_NotFound(t *testing.T) {
	_, deps := setup(t, 3, nil)

	fake := &fakeFetchClient{err: &brain.APIError{StatusCode: 404, Code: "NOT_FOUND", Message: "no document with that SHA"}}
	deps.FetchClient = fake
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleFetch, map[string]any{"sha": "abc123"})
	if isErr {
		t.Fatalf("404 should render a text result, not a tool error: %s", text)
	}
	if !fake.called {
		t.Fatal("expected online Fetch to be attempted")
	}
	if !strings.Contains(text, "no document with SHA") {
		t.Errorf("output should explain the SHA is unknown: %q", text)
	}
}

// TestFetchOnline_DaemonError: the daemon is unreachable → Phase D2a has no
// local fallback, so brain_fetch returns a clear tool error.
func TestFetchOnline_DaemonError(t *testing.T) {
	_, deps := setup(t, 3, nil)

	fake := &fakeFetchClient{err: brain.ErrDaemonUnreachable}
	deps.FetchClient = fake
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleFetch, map[string]any{"sha": "abc123"})
	if !isErr {
		t.Fatalf("expected a tool error on daemon failure, got success: %s", text)
	}
	if !fake.called {
		t.Fatal("expected online Fetch to be attempted")
	}
	if !strings.Contains(text, "brain_fetch") {
		t.Errorf("error should name the tool: %q", text)
	}
}

// TestFetchOnline_NoClient: no FetchClient wired → brain_fetch is
// online-only and returns a clear "not configured" tool error rather than
// serving stale local data.
func TestFetchOnline_NoClient(t *testing.T) {
	_, deps := setup(t, 3, nil)
	deps.FetchClient = nil
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleFetch, map[string]any{"sha": "abc123"})
	if !isErr {
		t.Fatalf("expected a tool error with no fetch client, got success: %s", text)
	}
	if !strings.Contains(text, "online-only") {
		t.Errorf("error should explain online-only requirement: %q", text)
	}
}
