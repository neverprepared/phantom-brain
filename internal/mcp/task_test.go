package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/neverprepared/mcp-phantom-brain/internal/working"
)

// taskIDRE pulls the 16-hex task_id out of task_start's response.
var taskIDRE = regexp.MustCompile(`task_id: ([0-9a-f]{16})`)

func startTask(t *testing.T, s *Server, goal string) string {
	t.Helper()
	text, isErr := callTool(t, s.handleTaskStart, map[string]any{"goal": goal})
	if isErr {
		t.Fatalf("task_start: %s", text)
	}
	m := taskIDRE.FindStringSubmatch(text)
	if len(m) != 2 {
		t.Fatalf("no task_id in response: %q", text)
	}
	return m[1]
}

func TestTaskStartCreatesRow(t *testing.T) {
	s, deps := setup(t, 3, nil)
	id := startTask(t, s, "ship phase 0 day 12")
	got, err := deps.Working.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Goal != "ship phase 0 day 12" {
		t.Errorf("goal = %q", got.Goal)
	}
	if got.Status != working.StatusActive {
		t.Errorf("status = %q, want active", got.Status)
	}
}

func TestTaskStartRejectsEmptyGoal(t *testing.T) {
	s, _ := setup(t, 3, nil)
	if text, isErr := callTool(t, s.handleTaskStart, map[string]any{"goal": "   "}); !isErr {
		t.Errorf("empty goal should error; got %q", text)
	}
}

func TestTaskUpdateFindingLogged(t *testing.T) {
	s, deps := setup(t, 3, nil)
	id := startTask(t, s, "g")

	text, isErr := callTool(t, s.handleTaskUpdate, map[string]any{
		"task_id":     id,
		"type":        "finding",
		"content":     "the API uses POST",
		"importance":  "high",
		"memory_type": "semantic",
	})
	if isErr {
		t.Fatalf("update: %s", text)
	}

	findings, _ := deps.Working.ListFindings(id)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	if findings[0].Importance != "high" || findings[0].MemoryType != "semantic" {
		t.Errorf("finding = %+v", findings[0])
	}
}

func TestTaskUpdateArtifactLogged(t *testing.T) {
	s, deps := setup(t, 3, nil)
	id := startTask(t, s, "g")

	text, isErr := callTool(t, s.handleTaskUpdate, map[string]any{
		"task_id":   id,
		"type":      "artifact",
		"name":      "PR #42",
		"reference": "https://github.com/foo/bar/pull/42",
	})
	if isErr {
		t.Fatalf("update: %s", text)
	}
	arts, _ := deps.Working.ListArtifacts(id)
	if len(arts) != 1 || arts[0].Name != "PR #42" {
		t.Errorf("artifacts = %+v", arts)
	}
}

func TestTaskUpdateQuestionLogged(t *testing.T) {
	s, deps := setup(t, 3, nil)
	id := startTask(t, s, "g")

	if _, isErr := callTool(t, s.handleTaskUpdate, map[string]any{
		"task_id": id,
		"type":    "question",
		"content": "does the API rate-limit?",
	}); isErr {
		t.Fatal("update question failed")
	}
	qs, _ := deps.Working.ListQuestions(id)
	if len(qs) != 1 || qs[0].Resolved {
		t.Errorf("questions = %+v", qs)
	}
}

func TestTaskUpdateCurrentStepBumpsRow(t *testing.T) {
	s, deps := setup(t, 3, nil)
	id := startTask(t, s, "g")

	if _, isErr := callTool(t, s.handleTaskUpdate, map[string]any{
		"task_id": id,
		"type":    "current_step",
		"content": "running tests",
	}); isErr {
		t.Fatal()
	}
	got, _ := deps.Working.GetTask(id)
	if got.CurrentStep != "running tests" {
		t.Errorf("current_step = %q", got.CurrentStep)
	}
}

func TestTaskUpdateRejectsUnknownType(t *testing.T) {
	s, _ := setup(t, 3, nil)
	id := startTask(t, s, "g")
	if text, isErr := callTool(t, s.handleTaskUpdate, map[string]any{
		"task_id": id,
		"type":    "vibe",
		"content": "x",
	}); !isErr {
		t.Errorf("unknown type should error; got %q", text)
	}
}

func TestTaskUpdateRejectsMissingTask(t *testing.T) {
	s, _ := setup(t, 3, nil)
	if text, isErr := callTool(t, s.handleTaskUpdate, map[string]any{
		"task_id": "deadbeef",
		"type":    "finding",
		"content": "x",
	}); !isErr {
		t.Errorf("missing task should error; got %q", text)
	}
}

func TestTaskCompleteWritesPromotedNote(t *testing.T) {
	plan := map[string][]float32{
		// The promoted note's embed input is title + body. We log a few
		// findings; expected embed input includes the rendered body.
		// Use a wildcard: match any input by pre-registering a single
		// vector. Since the fakeEmbedder only sees one Embed call per
		// task_complete, we just need ANY plan entry to match.
	}
	plan = map[string][]float32{}
	s, deps := setup(t, 3, plan)
	// Re-wire: we don't know the exact embed string ahead of time
	// because timestamps and IDs vary. Easier to pre-fill with a
	// catch-all by replacing the embedder.
	s.deps.Embedder = &openEmbedder{dims: 3, fixed: []float32{1, 0, 0}}
	deps.Embedder = s.deps.Embedder

	id := startTask(t, s, "design the loader")
	if _, isErr := callTool(t, s.handleTaskUpdate, map[string]any{
		"task_id": id, "type": "finding", "content": "WAL mode is mandatory", "importance": "high", "memory_type": "semantic",
	}); isErr {
		t.Fatal()
	}
	if _, isErr := callTool(t, s.handleTaskUpdate, map[string]any{
		"task_id": id, "type": "finding", "content": "trivial detail", "importance": "low",
	}); isErr {
		t.Fatal()
	}
	if _, isErr := callTool(t, s.handleTaskUpdate, map[string]any{
		"task_id": id, "type": "artifact", "name": "spec.md", "reference": "Wiki/loader-spec.md",
	}); isErr {
		t.Fatal()
	}

	text, isErr := callTool(t, s.handleTaskComplete, map[string]any{"task_id": id})
	if isErr {
		t.Fatalf("complete: %s", text)
	}
	if !strings.Contains(text, "Promoted") || !strings.Contains(text, "finding") {
		t.Errorf("status missing summary: %q", text)
	}

	// Promoted note exists.
	want := filepath.Join(deps.VaultDir, "Raw", "curated", "task-"+id+".md")
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("expected promoted note: %v", err)
	}
	s_ := string(body)
	if !strings.Contains(s_, "WAL mode is mandatory") {
		t.Error("high-importance finding missing from promoted note")
	}
	if strings.Contains(s_, "trivial detail") {
		t.Error("low-importance finding leaked into promoted note")
	}
	if !strings.Contains(s_, "Wiki/loader-spec.md") {
		t.Error("artifact missing from promoted note")
	}

	// Task is marked complete.
	got, _ := deps.Working.GetTask(id)
	if got.Status != working.StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}

	// Promoted note is indexed.
	hits, _ := deps.Index.SearchText(context.Background(), "loader", 5)
	if len(hits) != 1 {
		t.Errorf("FTS hits for completed task = %d, want 1", len(hits))
	}
}

func TestTaskCompleteEmptyPromotion(t *testing.T) {
	// No findings/artifacts/questions; complete should still flip
	// status but skip the promotion write.
	s, deps := setup(t, 3, nil)
	id := startTask(t, s, "g")
	text, isErr := callTool(t, s.handleTaskComplete, map[string]any{"task_id": id})
	if isErr {
		t.Fatalf("complete: %s", text)
	}
	if !strings.Contains(text, "no findings to promote") {
		t.Errorf("status should explain no-promote: %q", text)
	}
	if _, err := os.Stat(filepath.Join(deps.VaultDir, "Raw", "curated", "task-"+id+".md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no note should have been written: %v", err)
	}
	got, _ := deps.Working.GetTask(id)
	if got.Status != working.StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
}

func TestTaskGetRendersEverything(t *testing.T) {
	s, _ := setup(t, 3, nil)
	id := startTask(t, s, "render everything")

	_, _ = callTool(t, s.handleTaskUpdate, map[string]any{"task_id": id, "type": "current_step", "content": "writing"})
	_, _ = callTool(t, s.handleTaskUpdate, map[string]any{"task_id": id, "type": "step", "content": "draft"})
	_, _ = callTool(t, s.handleTaskUpdate, map[string]any{"task_id": id, "type": "finding", "content": "x", "importance": "high"})
	_, _ = callTool(t, s.handleTaskUpdate, map[string]any{"task_id": id, "type": "artifact", "name": "n", "reference": "r"})
	_, _ = callTool(t, s.handleTaskUpdate, map[string]any{"task_id": id, "type": "question", "content": "why?"})

	text, isErr := callTool(t, s.handleTaskGet, map[string]any{"task_id": id})
	if isErr {
		t.Fatalf("get: %s", text)
	}
	for _, want := range []string{
		"render everything",
		"Current step:** writing",
		"## Steps",
		"## Findings",
		"## Artifacts",
		"## Questions",
		"draft",
		"why?",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in get output:\n%s", want, text)
		}
	}
}

func TestTaskGetMissingTask(t *testing.T) {
	s, _ := setup(t, 3, nil)
	if text, isErr := callTool(t, s.handleTaskGet, map[string]any{"task_id": "deadbeef"}); !isErr {
		t.Errorf("missing task should error; got %q", text)
	}
}

// openEmbedder ignores the input plan and always returns the same fixed
// vector. Useful when the embed input is hard to predict (e.g. promoted
// notes whose body contains timestamps).
type openEmbedder struct {
	dims  int
	fixed []float32
}

func (o *openEmbedder) Dims() int { return o.dims }
func (o *openEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = o.fixed
	}
	return out, nil
}
