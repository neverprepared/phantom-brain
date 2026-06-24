package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pbbrain "github.com/neverprepared/phantom-brain/internal/brain"
)

// reflectDaemon is a tiny stub daemon serving the reflect/forget
// endpoints so the MCP tool handlers can be exercised end-to-end
// without a live OpenSearch.
func reflectDaemon(t *testing.T) (*pbbrain.Client, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/brain/reflect", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"sha":"aaa","title":"stale doc","reason":"stale-gate: never synthesised"}]}`))
	})
	mux.HandleFunc("/api/brain/forget", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sha":"aaa","forgotten":true}`))
	})
	ts := httptest.NewServer(mux)
	client, err := pbbrain.NewClient(pbbrain.ClientOpts{BaseURL: ts.URL, Token: "tok"})
	if err != nil {
		ts.Close()
		t.Fatalf("NewClient: %v", err)
	}
	return client, ts.Close
}

func TestReflectTool_ListsCandidates(t *testing.T) {
	client, cleanup := reflectDaemon(t)
	defer cleanup()

	_, deps := setup(t, 3, map[string][]float32{})
	deps.Client = client
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleReflect, map[string]any{})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if !strings.Contains(text, "aaa") || !strings.Contains(text, "stale-gate") {
		t.Errorf("reflect output missing candidate: %s", text)
	}
}

func TestReflectTool_LegacyModeErrors(t *testing.T) {
	_, deps := setup(t, 3, map[string][]float32{})
	deps.Client = nil // legacy BRAIN_VAULT_PATH mode
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleReflect, map[string]any{})
	if !isErr {
		t.Fatalf("expected error result in legacy mode, got: %s", text)
	}
}

func TestForgetTool_HappyPath(t *testing.T) {
	client, cleanup := reflectDaemon(t)
	defer cleanup()

	_, deps := setup(t, 3, map[string][]float32{})
	deps.Client = client
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleForget, map[string]any{"sha": "aaa"})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if !strings.Contains(text, "aaa") {
		t.Errorf("forget output missing sha: %s", text)
	}
}

func TestForgetTool_EmptySHA(t *testing.T) {
	client, cleanup := reflectDaemon(t)
	defer cleanup()

	_, deps := setup(t, 3, map[string][]float32{})
	deps.Client = client
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleForget, map[string]any{"sha": ""})
	if !isErr {
		t.Fatalf("expected error for empty sha, got: %s", text)
	}
}

func TestForgetTool_LegacyModeErrors(t *testing.T) {
	_, deps := setup(t, 3, map[string][]float32{})
	deps.Client = nil
	s := NewServer(deps)

	text, isErr := callTool(t, s.handleForget, map[string]any{"sha": "aaa"})
	if !isErr {
		t.Fatalf("expected error result in legacy mode, got: %s", text)
	}
}
