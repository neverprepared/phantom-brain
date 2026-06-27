package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/neverprepared/phantom-brain/internal/vault"
	"github.com/neverprepared/phantom-brain/internal/working"
)

// fakeEmbedder produces a deterministic vector keyed on the input
// string so test queries match recorded ingest content without
// needing an Ollama running.
type fakeEmbedder struct {
	dims int
	plan map[string][]float32
}

func (f *fakeEmbedder) Dims() int { return f.dims }
func (f *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, s := range inputs {
		v, ok := f.plan[s]
		if !ok {
			return nil, errors.New("fakeEmbedder: no plan for " + s)
		}
		out[i] = v
	}
	return out, nil
}

// setup builds a Server backed by a temp vault + working memory + a fake
// embedder seeded with deterministic vectors for the texts the test is
// going to use. Phase D2b: there is no local read cache (the sqlite-vec index package
// is gone), so setup wires no Index and no daemon Client — write-path
// tests add a recording daemon via setupWithDaemon.
func setup(t *testing.T, dims int, plan map[string][]float32) (*Server, ServerDeps) {
	t.Helper()
	dir := t.TempDir()
	vaultDir := filepath.Join(dir, "vault")
	indexDir := filepath.Join(dir, "_index")
	if err := vault.EnsureSkeleton(vaultDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	wm, err := working.Open(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wm.Close() })

	deps := ServerDeps{
		Working:  wm,
		Embedder: &fakeEmbedder{dims: dims, plan: plan},
		VaultDir: vaultDir,
	}
	return NewServer(deps), deps
}

// callTool builds an mcp.CallToolRequest, invokes a handler, and
// returns the rendered text plus the IsError flag.
func callTool(
	t *testing.T,
	handler func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error),
	args map[string]any,
) (text string, isError bool) {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned err: %v", err)
	}
	if res == nil {
		t.Fatal("handler returned nil result")
	}
	if len(res.Content) == 0 {
		t.Fatal("result has no content")
	}
	tc, ok := res.Content[0].(mcpgo.TextContent)
	if !ok {
		t.Fatalf("result content[0] = %T, want TextContent", res.Content[0])
	}
	return tc.Text, res.IsError
}

// --- brain_perceive ---
//
// Phase D2b: writes are daemon-only (no local file/index path), so the
// write-path assertions run against the recording daemon (setupWithDaemon)
// rather than the local vault. The default-slug happy path is covered by
// daemon_post_test.go's TestPerceive_PostsToDaemon. The following local-path
// unit tests were removed as testing-removed-behavior:
//
//   - TestPerceiveHappyPath          (superseded by TestPerceive_PostsToDaemon)
//   - TestPerceiveDuplicateReturnsDuplicateStatus (local dedup removed; the
//     daemon SHA-dedups now, so the agent no longer short-circuits)
//
// The filename-derivation coverage that those exercised is retained below,
// asserting the source_path the agent sends to the daemon.

func TestPerceiveFilenameHintOverridesSlug(t *testing.T) {
	plan := map[string][]float32{
		"Another Page\n\nbody": {0, 1, 0},
	}
	s, _, daemon, cleanup := setupWithDaemon(t, 3, plan)
	defer cleanup()

	text, isErr := callTool(t, s.handlePerceive, map[string]any{
		"content":  "body",
		"title":    "Another Page",
		"filename": "custom-name.md",
	})
	if isErr {
		t.Fatalf("err: %s", text)
	}
	calls := daemon.calls("/api/brain/perceive")
	if len(calls) != 1 {
		t.Fatalf("perceive calls = %d, want 1", len(calls))
	}
	if got := calls[0]["source_path"]; got != "Raw/gathered/custom-name.md" {
		t.Errorf("source_path = %v, want Raw/gathered/custom-name.md", got)
	}
}

func TestPerceiveFilenameHintAddsMdSuffix(t *testing.T) {
	plan := map[string][]float32{
		"X\n\nb": {0, 0, 1},
	}
	s, _, daemon, cleanup := setupWithDaemon(t, 3, plan)
	defer cleanup()

	if _, isErr := callTool(t, s.handlePerceive, map[string]any{
		"content":  "b",
		"title":    "X",
		"filename": "no-ext",
	}); isErr {
		t.Fatal("expected success")
	}
	calls := daemon.calls("/api/brain/perceive")
	if len(calls) != 1 {
		t.Fatalf("perceive calls = %d, want 1", len(calls))
	}
	if got := calls[0]["source_path"]; got != "Raw/gathered/no-ext.md" {
		t.Errorf("source_path = %v, want Raw/gathered/no-ext.md", got)
	}
}

func TestPerceiveRejectsEmptyContentOrTitle(t *testing.T) {
	s, _ := setup(t, 3, nil)
	if text, isErr := callTool(t, s.handlePerceive, map[string]any{"content": "", "title": "x"}); !isErr {
		t.Errorf("empty content should error; got %q", text)
	}
	if text, isErr := callTool(t, s.handlePerceive, map[string]any{"content": "x", "title": "   "}); !isErr {
		t.Errorf("whitespace title should error; got %q", text)
	}
}

func TestPerceiveSlugFallbackOnNoFilenameButOddTitle(t *testing.T) {
	// All-non-ascii title slugs to "" — Phase D2b's daemon-only path
	// falls back to source_path "Raw/gathered/untitled.md" rather than
	// erroring (the old local path returned an empty-filename error).
	plan := map[string][]float32{
		"日本語\n\nbody": {1, 0, 0},
	}
	s, _, daemon, cleanup := setupWithDaemon(t, 3, plan)
	defer cleanup()
	text, isErr := callTool(t, s.handlePerceive, map[string]any{
		"content": "body",
		"title":   "日本語",
	})
	if isErr {
		t.Fatalf("err: %s", text)
	}
	calls := daemon.calls("/api/brain/perceive")
	if len(calls) != 1 {
		t.Fatalf("perceive calls = %d, want 1", len(calls))
	}
	if got := calls[0]["source_path"]; got != "Raw/gathered/untitled.md" {
		t.Errorf("source_path = %v, want Raw/gathered/untitled.md", got)
	}
}

// --- brain_recall ---

// Phase D1: brain_recall is now online-only — the legacy local-snapshot
// recall path was removed in the Postgres cutover (see recall.go). The
// hit-rendering / no-results / default-limit / limit-cap behaviours that
// used to be exercised against the local index now live behind the daemon
// online-recall call and are covered by recall_online_test.go's
// fakeRecallClient seam. The following snapshot-recall unit tests were
// removed (superseded by the online tests; deeper coverage needs the
// PG-backed integration suite):
//
//   - TestRecallReturnsHitsForKnownContent
//   - TestRecallNoResultsRendersHelpfully
//   - TestRecallDefaultLimit
//   - TestRecallLimitCappedAt50

func TestRecallRejectsEmptyQuery(t *testing.T) {
	s, _ := setup(t, 3, nil)
	text, isErr := callTool(t, s.handleRecall, map[string]any{"query": ""})
	if !isErr {
		t.Errorf("empty query should error; got %q", text)
	}
}
