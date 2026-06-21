package working

import (
	"fmt"
	"time"
)

// Step is one decomposed step of a task plan.
type Step struct {
	ID          int64
	TaskID      string
	Description string
	Status      string     // "pending" | "done"
	CompletedAt *time.Time // nil while pending
}

// Step status constants.
const (
	StepPending = "pending"
	StepDone    = "done"
)

// AppendStep adds a step in 'pending' state.
func (d *DB) AppendStep(taskID, description string) (int64, error) {
	if taskID == "" {
		return 0, fmt.Errorf("working: AppendStep: taskID is required")
	}
	if description == "" {
		return 0, fmt.Errorf("working: AppendStep: description is required")
	}
	res, err := d.sql.Exec(
		`INSERT INTO steps (task_id, description, status) VALUES (?, ?, ?)`,
		taskID, description, StepPending,
	)
	if err != nil {
		return 0, fmt.Errorf("working: AppendStep: %w", err)
	}
	return res.LastInsertId()
}

// MarkStepDone flips a step to 'done' and records completed_at.
func (d *DB) MarkStepDone(stepID int64) error {
	res, err := d.sql.Exec(
		`UPDATE steps SET status = ?, completed_at = ? WHERE id = ?`,
		StepDone, nowRFC3339(), stepID,
	)
	if err != nil {
		return fmt.Errorf("working: MarkStepDone: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("working: MarkStepDone: no step with id=%d", stepID)
	}
	return nil
}

// ListSteps returns all steps for a task in insertion order.
func (d *DB) ListSteps(taskID string) ([]Step, error) {
	rows, err := d.sql.Query(
		`SELECT id, task_id, description, status, completed_at
		 FROM steps WHERE task_id = ? ORDER BY id ASC`,
		taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("working: ListSteps: %w", err)
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var (
			s            Step
			completedRaw *string
		)
		if err := rows.Scan(&s.ID, &s.TaskID, &s.Description, &s.Status, &completedRaw); err != nil {
			return nil, fmt.Errorf("working: ListSteps: scan: %w", err)
		}
		if completedRaw != nil {
			t := mustParseTime(*completedRaw)
			s.CompletedAt = &t
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Artifact is a file/PR/commit/URL the task produced.
type Artifact struct {
	ID        int64
	TaskID    string
	Name      string
	Reference string
	CreatedAt time.Time
}

// AppendArtifact records a produced artifact.
func (d *DB) AppendArtifact(taskID, name, reference string) (int64, error) {
	if taskID == "" || name == "" || reference == "" {
		return 0, fmt.Errorf("working: AppendArtifact: taskID, name, reference all required")
	}
	res, err := d.sql.Exec(
		`INSERT INTO artifacts (task_id, name, reference, created_at) VALUES (?, ?, ?, ?)`,
		taskID, name, reference, nowRFC3339(),
	)
	if err != nil {
		return 0, fmt.Errorf("working: AppendArtifact: %w", err)
	}
	return res.LastInsertId()
}

// ListArtifacts returns every artifact for a task.
func (d *DB) ListArtifacts(taskID string) ([]Artifact, error) {
	rows, err := d.sql.Query(
		`SELECT id, task_id, name, reference, created_at
		 FROM artifacts WHERE task_id = ? ORDER BY id ASC`,
		taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("working: ListArtifacts: %w", err)
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var (
			a          Artifact
			createdRaw string
		)
		if err := rows.Scan(&a.ID, &a.TaskID, &a.Name, &a.Reference, &createdRaw); err != nil {
			return nil, fmt.Errorf("working: ListArtifacts: scan: %w", err)
		}
		a.CreatedAt = mustParseTime(createdRaw)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Question is an open question raised during task execution.
type Question struct {
	ID         int64
	TaskID     string
	Question   string
	Resolved   bool
	Resolution string // empty while open
}

// AppendQuestion records an open question.
func (d *DB) AppendQuestion(taskID, question string) (int64, error) {
	if taskID == "" || question == "" {
		return 0, fmt.Errorf("working: AppendQuestion: taskID + question required")
	}
	res, err := d.sql.Exec(
		`INSERT INTO questions (task_id, question, resolved) VALUES (?, ?, 0)`,
		taskID, question,
	)
	if err != nil {
		return 0, fmt.Errorf("working: AppendQuestion: %w", err)
	}
	return res.LastInsertId()
}

// ResolveQuestion marks a question resolved and stores the answer.
func (d *DB) ResolveQuestion(questionID int64, resolution string) error {
	res, err := d.sql.Exec(
		`UPDATE questions SET resolved = 1, resolution = ? WHERE id = ?`,
		resolution, questionID,
	)
	if err != nil {
		return fmt.Errorf("working: ResolveQuestion: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("working: ResolveQuestion: no question with id=%d", questionID)
	}
	return nil
}

// ListQuestions returns every question for a task.
func (d *DB) ListQuestions(taskID string) ([]Question, error) {
	rows, err := d.sql.Query(
		`SELECT id, task_id, question, resolved, COALESCE(resolution,'')
		 FROM questions WHERE task_id = ? ORDER BY id ASC`,
		taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("working: ListQuestions: %w", err)
	}
	defer rows.Close()
	var out []Question
	for rows.Next() {
		var (
			q          Question
			resolvedI  int
		)
		if err := rows.Scan(&q.ID, &q.TaskID, &q.Question, &resolvedI, &q.Resolution); err != nil {
			return nil, fmt.Errorf("working: ListQuestions: scan: %w", err)
		}
		q.Resolved = resolvedI != 0
		out = append(out, q)
	}
	return out, rows.Err()
}
