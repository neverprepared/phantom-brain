package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/neverprepared/phantom-brain/internal/index"
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

// setup builds a Server backed by a temp vault + open index + a fake
// embedder seeded with deterministic vectors for the texts the test
// is going to use.
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
	idx, err := index.Open(indexDir, dims)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	wm, err := working.Open(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wm.Close() })

	deps := ServerDeps{
		Index:    idx,
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

func TestPerceiveHappyPath(t *testing.T) {
	plan := map[string][]float32{
		"My Page\n\nbody contents here": {1, 0, 0},
	}
	s, deps := setup(t, 3, plan)

	text, isErr := callTool(t, s.handlePerceive, map[string]any{
		"content":    "body contents here",
		"title":      "My Page",
		"source_url": "https://example.com/page",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if !strings.Contains(text, "Stored to Raw/gathered/my-page.md") {
		t.Errorf("status missing expected path: %q", text)
	}
	if !strings.Contains(text, "SHA:") {
		t.Errorf("status missing SHA: %q", text)
	}

	// File on disk has frontmatter + body.
	wrote, err := os.ReadFile(filepath.Join(deps.VaultDir, "Raw", "gathered", "my-page.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(wrote)
	if !strings.HasPrefix(got, "---\n") {
		t.Errorf("written file should start with frontmatter:\n%s", got)
	}
	if !strings.Contains(got, "title: My Page") {
		t.Errorf("written file missing title: %s", got)
	}
	if !strings.Contains(got, "source_url: https://example.com/page") {
		t.Errorf("written file missing source_url: %s", got)
	}
	if !strings.Contains(got, "body contents here") {
		t.Errorf("written file missing body: %s", got)
	}

	// Index contains it.
	hits, err := deps.Index.SearchText(context.Background(), "page", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Errorf("FTS hits after perceive = %d, want 1", len(hits))
	}
}

func TestPerceiveFilenameHintOverridesSlug(t *testing.T) {
	plan := map[string][]float32{
		"Another Page\n\nbody": {0, 1, 0},
	}
	s, deps := setup(t, 3, plan)

	text, isErr := callTool(t, s.handlePerceive, map[string]any{
		"content":  "body",
		"title":    "Another Page",
		"filename": "custom-name.md",
	})
	if isErr {
		t.Fatalf("err: %s", text)
	}
	if _, err := os.Stat(filepath.Join(deps.VaultDir, "Raw", "gathered", "custom-name.md")); err != nil {
		t.Errorf("expected custom-name.md to exist: %v", err)
	}
}

func TestPerceiveFilenameHintAddsMdSuffix(t *testing.T) {
	plan := map[string][]float32{
		"X\n\nb": {0, 0, 1},
	}
	s, deps := setup(t, 3, plan)

	if _, isErr := callTool(t, s.handlePerceive, map[string]any{
		"content":  "b",
		"title":    "X",
		"filename": "no-ext",
	}); isErr {
		t.Fatal("expected success")
	}
	if _, err := os.Stat(filepath.Join(deps.VaultDir, "Raw", "gathered", "no-ext.md")); err != nil {
		t.Errorf("expected no-ext.md: %v", err)
	}
}

func TestPerceiveDuplicateReturnsDuplicateStatus(t *testing.T) {
	plan := map[string][]float32{
		"Same Page\n\nidentical body": {1, 0, 0},
	}
	s, _ := setup(t, 3, plan)

	args := map[string]any{
		"content": "identical body",
		"title":   "Same Page",
	}
	if _, isErr := callTool(t, s.handlePerceive, args); isErr {
		t.Fatal("first ingest should succeed")
	}
	text, isErr := callTool(t, s.handlePerceive, args)
	if isErr {
		t.Fatalf("duplicate should NOT be an MCP error: %s", text)
	}
	if !strings.Contains(text, "Duplicate") {
		t.Errorf("expected Duplicate in status: %q", text)
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
	// All-non-ascii title slugs to "" — perceive must surface that
	// rather than write to an empty filename.
	plan := map[string][]float32{
		"日本語\n\nbody": {1, 0, 0},
	}
	s, _ := setup(t, 3, plan)
	text, isErr := callTool(t, s.handlePerceive, map[string]any{
		"content": "body",
		"title":   "日本語",
	})
	if !isErr {
		t.Errorf("expected error from empty slug; got %q", text)
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
