package working

import (
	"strings"
	"testing"
)

// --- Steps ---

func TestAppendStepValidation(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("t1", "goal", "", ""); err != nil {
		t.Fatal(err)
	}

	if _, err := db.AppendStep("", "desc"); err == nil {
		t.Error("empty taskID should fail")
	}
	if _, err := db.AppendStep("t1", ""); err == nil {
		t.Error("empty description should fail")
	}
	if _, err := db.AppendStep("nonexistent", "desc"); err == nil {
		t.Error("FK constraint should reject step against missing task")
	}
}

func TestAppendListAndMarkStepDone(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("t1", "goal", "", ""); err != nil {
		t.Fatal(err)
	}

	id1, err := db.AppendStep("t1", "first step")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := db.AppendStep("t1", "second step")
	if err != nil {
		t.Fatal(err)
	}
	if id1 == 0 || id2 == 0 || id1 == id2 {
		t.Fatalf("autoincrement ids look wrong: %d, %d", id1, id2)
	}

	steps, err := db.ListSteps("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("len = %d, want 2", len(steps))
	}
	// Insertion order.
	if steps[0].Description != "first step" || steps[1].Description != "second step" {
		t.Errorf("ordering wrong: %+v", steps)
	}
	// Both start pending with nil CompletedAt.
	for _, s := range steps {
		if s.Status != StepPending {
			t.Errorf("step %d status = %q, want pending", s.ID, s.Status)
		}
		if s.CompletedAt != nil {
			t.Errorf("pending step has non-nil CompletedAt: %v", s.CompletedAt)
		}
		if s.TaskID != "t1" {
			t.Errorf("step TaskID = %q, want t1", s.TaskID)
		}
	}

	// Mark the first done.
	if err := db.MarkStepDone(id1); err != nil {
		t.Fatal(err)
	}
	steps, err = db.ListSteps("t1")
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Status != StepDone {
		t.Errorf("step status = %q, want done", steps[0].Status)
	}
	if steps[0].CompletedAt == nil {
		t.Error("done step should have CompletedAt set")
	} else if steps[0].CompletedAt.IsZero() {
		t.Error("done step CompletedAt parsed to zero time")
	}
	// The other remains pending.
	if steps[1].Status != StepPending || steps[1].CompletedAt != nil {
		t.Errorf("second step should still be pending: %+v", steps[1])
	}
}

func TestMarkStepDoneMissing(t *testing.T) {
	db, _ := openTest(t)
	err := db.MarkStepDone(424242)
	if err == nil {
		t.Fatal("marking nonexistent step should fail")
	}
	if !strings.Contains(err.Error(), "no step with id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestListStepsEmpty(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("t1", "goal", "", ""); err != nil {
		t.Fatal(err)
	}
	steps, err := db.ListSteps("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Errorf("expected no steps, got %d", len(steps))
	}
	// Unknown task id also yields empty, no error.
	steps, err = db.ListSteps("never-existed")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Errorf("expected no steps for unknown task, got %d", len(steps))
	}
}

// --- Artifacts ---

func TestAppendArtifactValidation(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("t1", "goal", "", ""); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		taskID, name, ref string
	}{
		{"", "n", "r"},
		{"t1", "", "r"},
		{"t1", "n", ""},
	}
	for _, c := range cases {
		if _, err := db.AppendArtifact(c.taskID, c.name, c.ref); err == nil {
			t.Errorf("AppendArtifact(%q,%q,%q) should fail", c.taskID, c.name, c.ref)
		}
	}
	if _, err := db.AppendArtifact("nonexistent", "n", "r"); err == nil {
		t.Error("FK constraint should reject artifact against missing task")
	}
}

func TestAppendAndListArtifacts(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("t1", "goal", "", ""); err != nil {
		t.Fatal(err)
	}

	id1, err := db.AppendArtifact("t1", "PR", "https://github.com/x/y/pull/1")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := db.AppendArtifact("t1", "commit", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if id1 == 0 || id2 == 0 || id1 == id2 {
		t.Fatalf("autoincrement ids look wrong: %d, %d", id1, id2)
	}

	arts, err := db.ListArtifacts("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 2 {
		t.Fatalf("len = %d, want 2", len(arts))
	}
	if arts[0].Name != "PR" || arts[0].Reference != "https://github.com/x/y/pull/1" {
		t.Errorf("first artifact = %+v", arts[0])
	}
	if arts[1].Name != "commit" || arts[1].Reference != "abc123" {
		t.Errorf("second artifact = %+v", arts[1])
	}
	if arts[0].TaskID != "t1" {
		t.Errorf("artifact TaskID = %q, want t1", arts[0].TaskID)
	}
	if arts[0].CreatedAt.IsZero() {
		t.Error("artifact CreatedAt should be set")
	}
}

func TestListArtifactsEmpty(t *testing.T) {
	db, _ := openTest(t)
	arts, err := db.ListArtifacts("no-task")
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 0 {
		t.Errorf("expected no artifacts, got %d", len(arts))
	}
}

// --- Questions ---

func TestAppendQuestionValidation(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("t1", "goal", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendQuestion("", "q"); err == nil {
		t.Error("empty taskID should fail")
	}
	if _, err := db.AppendQuestion("t1", ""); err == nil {
		t.Error("empty question should fail")
	}
	if _, err := db.AppendQuestion("nonexistent", "q"); err == nil {
		t.Error("FK constraint should reject question against missing task")
	}
}

func TestAppendResolveAndListQuestions(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("t1", "goal", "", ""); err != nil {
		t.Fatal(err)
	}

	id1, err := db.AppendQuestion("t1", "which auth token?")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := db.AppendQuestion("t1", "is the bucket created?")
	if err != nil {
		t.Fatal(err)
	}
	if id1 == 0 || id2 == 0 || id1 == id2 {
		t.Fatalf("autoincrement ids look wrong: %d, %d", id1, id2)
	}

	// Both open initially.
	qs, err := db.ListQuestions("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 2 {
		t.Fatalf("len = %d, want 2", len(qs))
	}
	for _, q := range qs {
		if q.Resolved {
			t.Errorf("question %d should be open: %+v", q.ID, q)
		}
		if q.Resolution != "" {
			t.Errorf("open question has resolution: %+v", q)
		}
	}
	// Insertion order.
	if qs[0].Question != "which auth token?" || qs[1].Question != "is the bucket created?" {
		t.Errorf("ordering wrong: %+v", qs)
	}

	// Resolve the first.
	if err := db.ResolveQuestion(id1, "CL_BRAIN_API_TOKEN from auth.toml"); err != nil {
		t.Fatal(err)
	}
	qs, err = db.ListQuestions("t1")
	if err != nil {
		t.Fatal(err)
	}
	if !qs[0].Resolved {
		t.Errorf("question 1 should be resolved: %+v", qs[0])
	}
	if qs[0].Resolution != "CL_BRAIN_API_TOKEN from auth.toml" {
		t.Errorf("resolution = %q", qs[0].Resolution)
	}
	if qs[1].Resolved {
		t.Errorf("question 2 should remain open: %+v", qs[1])
	}
}

func TestResolveQuestionMissing(t *testing.T) {
	db, _ := openTest(t)
	err := db.ResolveQuestion(999, "answer")
	if err == nil {
		t.Fatal("resolving nonexistent question should fail")
	}
	if !strings.Contains(err.Error(), "no question with id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestListQuestionsEmpty(t *testing.T) {
	db, _ := openTest(t)
	qs, err := db.ListQuestions("no-task")
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 0 {
		t.Errorf("expected no questions, got %d", len(qs))
	}
}

// --- Task lifecycle edge cases ---

// CompleteTask is documented as idempotent: completing an
// already-completed task matches the row and does not error.
func TestCompleteTaskIdempotent(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("t1", "goal", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteTask("t1"); err != nil {
		t.Fatal(err)
	}
	// Second completion still finds the row -> no ErrTaskNotFound.
	if err := db.CompleteTask("t1"); err != nil {
		t.Errorf("second CompleteTask should be a no-op, got: %v", err)
	}
	got, _ := db.GetTask("t1")
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
}

func TestCompleteTaskMissing(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CompleteTask("never"); err == nil {
		t.Error("completing a missing task should fail")
	}
}

// ListActiveTasks on a fresh DB returns an empty slice, no error.
func TestListActiveTasksEmpty(t *testing.T) {
	db, _ := openTest(t)
	tasks, err := db.ListActiveTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected no active tasks, got %d", len(tasks))
	}
}

// CreateTask with a duplicate id collides on the PRIMARY KEY.
func TestCreateTaskDuplicateID(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("dup", "goal", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask("dup", "other goal", "", ""); err == nil {
		t.Error("duplicate task id should fail on primary key")
	}
}

// Optional fields (constraints, plan) round-trip as empty strings when
// not supplied — nullIfEmpty stores NULL, GetTask COALESCEs back to "".
func TestCreateTaskOptionalFieldsRoundTrip(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("t1", "goal", "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != "" || got.Plan != "" || got.CurrentStep != "" {
		t.Errorf("expected empty optional fields, got %+v", got)
	}
}

func TestCtxBackground(t *testing.T) {
	if ctxBackground() == nil {
		t.Error("ctxBackground returned nil")
	}
}
