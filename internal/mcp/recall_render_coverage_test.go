package mcp

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

// TestOnlineRecallReason covers every branch of the failure-reason
// renderer: the daemon-unreachable sentinel, the 503 "not enabled"
// envelope, a generic API error (rendered with its status code), and a
// plain non-API error (rendered verbatim).
func TestOnlineRecallReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"unreachable", brain.ErrDaemonUnreachable, "daemon unreachable"},
		{"service-unavailable", &brain.APIError{StatusCode: http.StatusServiceUnavailable, Code: "DISABLED", Message: "off"}, "not enabled for this binding"},
		{"other-api-error", &brain.APIError{StatusCode: http.StatusInternalServerError, Code: "BOOM", Message: "kaboom"}, "daemon error 500"},
		{"plain-error", errors.New("dial tcp: no route"), "dial tcp: no route"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := onlineRecallReason(c.err)
			if got != c.want {
				t.Errorf("onlineRecallReason(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// TestOnlineRecallReason_WrappedUnreachable: the sentinel is matched via
// errors.Is even when wrapped, matching how brain.Client.do wraps every
// transport failure.
func TestOnlineRecallReason_WrappedUnreachable(t *testing.T) {
	wrapped := errors.Join(errors.New("POST failed"), brain.ErrDaemonUnreachable)
	if got := onlineRecallReason(wrapped); got != "daemon unreachable" {
		t.Errorf("wrapped sentinel = %q, want %q", got, "daemon unreachable")
	}
}

// TestRecall_EmbedFailureReturnsToolError: the embedder failing (no plan
// entry for the query) short-circuits before the daemon is consulted and
// surfaces an "embed query" tool error. The RecallClient must NOT be
// called.
func TestRecall_EmbedFailureReturnsToolError(t *testing.T) {
	// Empty plan → fakeEmbedder errors for any input.
	_, deps := setup(t, 3, map[string][]float32{})
	fake := &fakeRecallClient{resp: &brain.RecallResponse{}}
	deps.RecallClient = fake
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleRecall, map[string]any{"query": "unplanned query"})
	if !isErr {
		t.Fatalf("expected a tool error when embedding fails, got success: %s", text)
	}
	if !strings.Contains(text, "embed query") {
		t.Errorf("error should name the embed step: %q", text)
	}
	if fake.called {
		t.Error("daemon Recall must not be called when embedding fails")
	}
}

// TestRenderOnlineRecallHits_TitleFallbackAndNoSnippet exercises the two
// rendering edges not hit by the happy-path test: a hit with an empty
// title falls back to its SHA as the heading, and a hit with no snippet
// omits the snippet line entirely.
func TestRenderOnlineRecallHits_TitleFallbackAndNoSnippet(t *testing.T) {
	out := renderOnlineRecallHits("q", []brain.RecallHitDTO{
		{SHA: "shaonly", Title: "", Kind: "note", Score: 0.7}, // no title, no snippet
	})
	// Heading falls back to the SHA when title is blank.
	if !strings.Contains(out, "## 1. shaonly [note]") {
		t.Errorf("expected SHA used as heading, got:\n%s", out)
	}
	// No snippet line should be rendered for a blank snippet.
	if strings.Contains(out, "- Snippet:") {
		t.Errorf("did not expect a snippet line for blank snippet:\n%s", out)
	}
	// Score still rendered.
	if !strings.Contains(out, "- Score: 0.7000") {
		t.Errorf("expected score line, got:\n%s", out)
	}
}

// TestRenderOnlineRecallHits_AttachmentWithoutFilename: an attachment hit
// with no original_filename still renders the fetch hint, just without the
// parenthetical filename.
func TestRenderOnlineRecallHits_AttachmentWithoutFilename(t *testing.T) {
	out := renderOnlineRecallHits("q", []brain.RecallHitDTO{
		{SHA: "att9", Title: "Doc", Kind: "attachment_stub", MimeType: "image/png", Score: 0.3},
	})
	if !strings.Contains(out, "GET /api/brain/attach/att9") {
		t.Errorf("expected fetch hint, got:\n%s", out)
	}
	if strings.Contains(out, "(") && strings.Contains(out, ".png)") {
		t.Errorf("did not expect a parenthetical filename, got:\n%s", out)
	}
	if !strings.Contains(out, "[attachment png]") {
		t.Errorf("expected mime-derived label, got:\n%s", out)
	}
}
