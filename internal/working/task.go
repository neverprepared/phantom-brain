package working

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TaskStatus values. Stored as TEXT for human readability; constants
// keep the wire form consistent.
const (
	StatusActive    = "active"
	StatusCompleted = "completed"
)

// Task is the central row in working memory. Created via
// CreateTask and mutated through the small set of explicit setters
// below. Avoid arbitrary Update; we want every state transition to
// surface in code review as a named method.
type Task struct {
	ID          string
	Goal        string
	Constraints string
	Plan        string
	CurrentStep string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ErrTaskNotFound is returned by GetTask when no row matches.
var ErrTaskNotFound = errors.New("working: task not found")

// CreateTask inserts a new task with status='active' and now() for
// both timestamps. Returns ErrTaskExists if the id collides.
func (d *DB) CreateTask(id, goal, constraints, plan string) error {
	if id == "" {
		return fmt.Errorf("working: CreateTask: id is required")
	}
	if goal == "" {
		return fmt.Errorf("working: CreateTask: goal is required")
	}
	now := nowRFC3339()
	_, err := d.sql.Exec(
		`INSERT INTO tasks (task_id, goal, constraints, plan, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, goal, nullIfEmpty(constraints), nullIfEmpty(plan), StatusActive, now, now,
	)
	if err != nil {
		return fmt.Errorf("working: CreateTask: %w", err)
	}
	return nil
}

// GetTask returns the row for id, or ErrTaskNotFound.
func (d *DB) GetTask(id string) (*Task, error) {
	row := d.sql.QueryRow(
		`SELECT task_id, goal, COALESCE(constraints,''), COALESCE(plan,''),
		        COALESCE(current_step,''), status, created_at, updated_at
		 FROM tasks WHERE task_id = ?`,
		id,
	)
	var (
		t                                                                 Task
		createdRaw, updatedRaw                                            string
	)
	err := row.Scan(&t.ID, &t.Goal, &t.Constraints, &t.Plan, &t.CurrentStep, &t.Status, &createdRaw, &updatedRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("working: GetTask: %w", err)
	}
	t.CreatedAt = mustParseTime(createdRaw)
	t.UpdatedAt = mustParseTime(updatedRaw)
	return &t, nil
}

// SetCurrentStep updates current_step and bumps updated_at.
func (d *DB) SetCurrentStep(taskID, step string) error {
	res, err := d.sql.Exec(
		`UPDATE tasks SET current_step = ?, updated_at = ? WHERE task_id = ?`,
		step, nowRFC3339(), taskID,
	)
	if err != nil {
		return fmt.Errorf("working: SetCurrentStep: %w", err)
	}
	return mustAffectOne(res, taskID)
}

// CompleteTask flips status to 'completed' and bumps updated_at.
// Idempotent: completing an already-completed task is a no-op (no
// error). Use Delete on the DB to also drop the on-disk shard.
func (d *DB) CompleteTask(taskID string) error {
	res, err := d.sql.Exec(
		`UPDATE tasks SET status = ?, updated_at = ? WHERE task_id = ?`,
		StatusCompleted, nowRFC3339(), taskID,
	)
	if err != nil {
		return fmt.Errorf("working: CompleteTask: %w", err)
	}
	return mustAffectOne(res, taskID)
}

// ListActiveTasks returns every task with status='active', most-recently-
// updated first. Useful for "what's in flight" prompts when a new
// Claude Code session attaches to an existing brain.
func (d *DB) ListActiveTasks() ([]Task, error) {
	rows, err := d.sql.Query(
		`SELECT task_id, goal, COALESCE(constraints,''), COALESCE(plan,''),
		        COALESCE(current_step,''), status, created_at, updated_at
		 FROM tasks WHERE status = ?
		 ORDER BY updated_at DESC`,
		StatusActive,
	)
	if err != nil {
		return nil, fmt.Errorf("working: ListActiveTasks: %w", err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		var (
			t                      Task
			createdRaw, updatedRaw string
		)
		if err := rows.Scan(&t.ID, &t.Goal, &t.Constraints, &t.Plan, &t.CurrentStep, &t.Status, &createdRaw, &updatedRaw); err != nil {
			return nil, fmt.Errorf("working: ListActiveTasks: scan: %w", err)
		}
		t.CreatedAt = mustParseTime(createdRaw)
		t.UpdatedAt = mustParseTime(updatedRaw)
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- shared helpers ---

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mustParseTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		// Fall back to RFC3339 (no nano) for backward compatibility.
		t, _ = time.Parse(time.RFC3339, raw)
	}
	return t
}

func mustAffectOne(res sql.Result, key string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("working: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w (id=%q)", ErrTaskNotFound, key)
	}
	return nil
}
