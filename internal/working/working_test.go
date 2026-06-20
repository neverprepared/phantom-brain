package working

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dir
}

// --- Open / Close / Delete ---

func TestOpenCreatesShard(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if db.PID() != os.Getpid() {
		t.Errorf("PID = %d, want %d", db.PID(), os.Getpid())
	}
	if !strings.HasSuffix(db.Path(), ".sqlite") {
		t.Errorf("Path = %q, want *.sqlite", db.Path())
	}
	if _, err := os.Stat(db.Path()); err != nil {
		t.Errorf("expected file at %q: %v", db.Path(), err)
	}
}

func TestOpenRejectsEmptyDir(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("Open with empty dir should fail")
	}
}

func TestDeleteRemovesShardAndSidecars(t *testing.T) {
	db, _ := openTest(t)
	path := db.Path()

	// Write something so WAL sidecar exists.
	if err := db.CreateTask("t1", "do thing", "", ""); err != nil {
		t.Fatal(err)
	}

	if err := db.Delete(); err != nil {
		t.Fatal(err)
	}

	for _, suf := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(path + suf); err == nil {
			t.Errorf("file%s still exists after Delete: %s", suf, path+suf)
		}
	}
}

// --- Task CRUD ---

func TestCreateAndGetTask(t *testing.T) {
	db, _ := openTest(t)

	if err := db.CreateTask("t1", "ship phase 0", "no breaking schema", "step1, step2"); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "t1" || got.Goal != "ship phase 0" {
		t.Errorf("task = %+v", got)
	}
	if got.Constraints != "no breaking schema" || got.Plan != "step1, step2" {
		t.Errorf("constraints/plan not stored: %+v", got)
	}
	if got.Status != StatusActive {
		t.Errorf("status = %q, want %q", got.Status, StatusActive)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps zero: %+v", got)
	}
	if time.Since(got.CreatedAt) > time.Minute {
		t.Errorf("created_at too old: %v", got.CreatedAt)
	}
}

func TestCreateTaskRequiresIDAndGoal(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("", "goal", "", ""); err == nil {
		t.Error("empty id should fail")
	}
	if err := db.CreateTask("id", "", "", ""); err == nil {
		t.Error("empty goal should fail")
	}
}

func TestGetTaskNotFound(t *testing.T) {
	db, _ := openTest(t)
	_, err := db.GetTask("missing")
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("got %v, want ErrTaskNotFound", err)
	}
}

func TestSetCurrentStepBumpsUpdatedAt(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("t1", "goal", "", ""); err != nil {
		t.Fatal(err)
	}
	before, _ := db.GetTask("t1")

	time.Sleep(10 * time.Millisecond)
	if err := db.SetCurrentStep("t1", "now working on X"); err != nil {
		t.Fatal(err)
	}
	after, _ := db.GetTask("t1")

	if after.CurrentStep != "now working on X" {
		t.Errorf("current_step = %q", after.CurrentStep)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("updated_at not bumped: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}
}

func TestSetCurrentStepNotFound(t *testing.T) {
	db, _ := openTest(t)
	err := db.SetCurrentStep("missing", "x")
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("got %v, want ErrTaskNotFound", err)
	}
}

func TestCompleteTask(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("t1", "goal", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteTask("t1"); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetTask("t1")
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", got.Status, StatusCompleted)
	}
}

func TestListActiveTasksOrdersByMostRecentUpdate(t *testing.T) {
	db, _ := openTest(t)

	for _, id := range []string{"a", "b", "c"} {
		if err := db.CreateTask(id, "goal-"+id, "", ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond) // ensure timestamps differ
	}
	if err := db.CompleteTask("b"); err != nil {
		t.Fatal(err)
	}
	// Touch 'a' to make it most-recently-updated.
	time.Sleep(5 * time.Millisecond)
	if err := db.SetCurrentStep("a", "fresh"); err != nil {
		t.Fatal(err)
	}

	tasks, err := db.ListActiveTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("active count = %d, want 2", len(tasks))
	}
	if tasks[0].ID != "a" || tasks[1].ID != "c" {
		t.Errorf("ordering = [%s,%s], want [a,c]", tasks[0].ID, tasks[1].ID)
	}
}

// --- Findings ---

func TestAppendFindingsAndList(t *testing.T) {
	db, _ := openTest(t)
	if err := db.CreateTask("t1", "goal", "", ""); err != nil {
		t.Fatal(err)
	}

	id1, err := db.AppendFinding("t1", "the API takes JSON", ImportanceHigh, MemoryTypeSemantic)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := db.AppendFinding("t1", "we tried curl first and failed", ImportanceMedium, MemoryTypeEpisodic)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == 0 || id2 == 0 || id1 == id2 {
		t.Errorf("autoincrement ids look wrong: %d, %d", id1, id2)
	}

	findings, err := db.ListFindings("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("len = %d, want 2", len(findings))
	}
	if findings[0].Content != "the API takes JSON" || findings[0].MemoryType != MemoryTypeSemantic {
		t.Errorf("first finding = %+v", findings[0])
	}
}

func TestAppendFindingValidatesImportance(t *testing.T) {
	db, _ := openTest(t)
	_ = db.CreateTask("t1", "goal", "", "")
	if _, err := db.AppendFinding("t1", "c", "critical", ""); err == nil {
		t.Error("bogus importance should fail")
	}
	if _, err := db.AppendFinding("t1", "", ImportanceHigh, ""); err == nil {
		t.Error("empty content should fail")
	}
}

func TestListImportantFindingsDropsLow(t *testing.T) {
	db, _ := openTest(t)
	_ = db.CreateTask("t1", "goal", "", "")

	_, _ = db.AppendFinding("t1", "high",   ImportanceHigh,   MemoryTypeSemantic)
	_, _ = db.AppendFinding("t1", "low",    ImportanceLow,    MemoryTypeEpisodic)
	_, _ = db.AppendFinding("t1", "medium", ImportanceMedium, MemoryTypeProcedural)

	all, _ := db.ListFindings("t1")
	if len(all) != 3 {
		t.Fatalf("all count = %d", len(all))
	}

	important, err := db.ListImportantFindings("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(important) != 2 {
		t.Errorf("important count = %d, want 2", len(important))
	}
	for _, f := range important {
		if f.Importance == ImportanceLow {
			t.Errorf("low-importance leaked into important set: %+v", f)
		}
	}
}

func TestAppendFindingRejectsMissingTask(t *testing.T) {
	db, _ := openTest(t)
	// Foreign key constraint should reject; we set foreign_keys=ON in
	// internal/sqlite.Open.
	if _, err := db.AppendFinding("nonexistent", "c", ImportanceHigh, ""); err == nil {
		t.Error("FK constraint should reject finding against missing task")
	}
}

// --- Orphan reaper ---

func TestReapOrphanedShardsLeavesLiveProcesses(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	res, err := ReapOrphanedShards(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1", res.Scanned)
	}
	if res.SelfSkipped != os.Getpid() {
		t.Errorf("SelfSkipped = %d, want %d", res.SelfSkipped, os.Getpid())
	}
	if len(res.Reaped) != 0 {
		t.Errorf("Reaped = %v, want empty", res.Reaped)
	}
}

func TestReapOrphanedShardsRemovesDeadPIDs(t *testing.T) {
	dir := t.TempDir()

	// Find a definitely-dead PID. PID 1 is init and is always alive,
	// so go past the system max + a margin.
	deadPID := 999999

	// Make sure the dead PID is actually dead on this system.
	if pidAlive(deadPID) {
		t.Skipf("PID %d is alive on this system; skipping", deadPID)
	}

	// Plant a fake shard for deadPID.
	deadPath := filepath.Join(dir, fmt.Sprintf("wm-%d.sqlite", deadPID))
	if err := os.WriteFile(deadPath, []byte("not a real db"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Plant fake sidecars too.
	_ = os.WriteFile(deadPath+"-wal", []byte("x"), 0o644)
	_ = os.WriteFile(deadPath+"-shm", []byte("x"), 0o644)

	res, err := ReapOrphanedShards(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1", res.Scanned)
	}
	if len(res.Reaped) != 1 || res.Reaped[0] != deadPID {
		t.Errorf("Reaped = %v, want [%d]", res.Reaped, deadPID)
	}
	for _, suf := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(deadPath + suf); err == nil {
			t.Errorf("orphan file%s not removed", suf)
		}
	}
}

func TestReapOrphanedShardsIgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()

	// These should be ignored by shardRE.
	for _, name := range []string{
		"vectors.db",
		"wm-foo.sqlite",
		"wm-.sqlite",
		"wm-12.sqlite.bak",
		"notes.md",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := ReapOrphanedShards(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 0 {
		t.Errorf("Scanned = %d, want 0", res.Scanned)
	}
	for _, name := range []string{"vectors.db", "wm-foo.sqlite", "notes.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("unrelated file %q was deleted: %v", name, err)
		}
	}
}

func TestReapOrphanedShardsHandlesMissingDir(t *testing.T) {
	res, err := ReapOrphanedShards(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Errorf("missing dir should not error: %v", err)
	}
	if res.Scanned != 0 {
		t.Errorf("Scanned = %d, want 0", res.Scanned)
	}
}
