package working

import (
	"fmt"
	"time"
)

// Importance values. The promotion gate (task_complete -> Raw/curated/)
// keeps everything >= medium and discards low.
const (
	ImportanceLow    = "low"
	ImportanceMedium = "medium"
	ImportanceHigh   = "high"
)

// MemoryType classifies a finding for downstream synthesis. Loose
// strings rather than an enum so the prompt-engineering layer can
// introduce new types without a schema migration.
const (
	MemoryTypeSemantic   = "semantic"
	MemoryTypeEpisodic   = "episodic"
	MemoryTypeProcedural = "procedural"
)

// Finding is a single observation logged during a task. They are the
// raw material the synthesizer promotes to Raw/curated/ when the task
// completes — see v5.0 §2 invariant #5.
type Finding struct {
	ID         int64
	TaskID     string
	Content    string
	Importance string
	MemoryType string // optional; "" if not set
	CreatedAt  time.Time
}

// AppendFinding records one observation against an existing task.
// Returns the row's autoincrement id.
//
// Importance must be one of low/medium/high. MemoryType may be empty
// (callers that don't know yet can leave it). Empty content is
// rejected — a finding with no content is a bug.
func (d *DB) AppendFinding(taskID, content, importance, memoryType string) (int64, error) {
	if taskID == "" {
		return 0, fmt.Errorf("working: AppendFinding: taskID is required")
	}
	if content == "" {
		return 0, fmt.Errorf("working: AppendFinding: content is required")
	}
	if !validImportance(importance) {
		return 0, fmt.Errorf("working: AppendFinding: importance must be low/medium/high; got %q", importance)
	}

	res, err := d.sql.Exec(
		`INSERT INTO findings (task_id, content, importance, memory_type, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		taskID, content, importance, nullIfEmpty(memoryType), nowRFC3339(),
	)
	if err != nil {
		return 0, fmt.Errorf("working: AppendFinding: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("working: AppendFinding: last id: %w", err)
	}
	return id, nil
}

// ListFindings returns every finding for a task in insertion order
// (autoincrement id ascending).
func (d *DB) ListFindings(taskID string) ([]Finding, error) {
	return d.queryFindings(`SELECT id, task_id, content, importance,
	                              COALESCE(memory_type,''), created_at
	                       FROM findings WHERE task_id = ?
	                       ORDER BY id ASC`, taskID)
}

// ListImportantFindings returns only findings with importance in
// (medium, high), insertion order. This is the set the promotion
// gate keeps; ListFindings is mostly for debugging.
func (d *DB) ListImportantFindings(taskID string) ([]Finding, error) {
	return d.queryFindings(`SELECT id, task_id, content, importance,
	                              COALESCE(memory_type,''), created_at
	                       FROM findings
	                       WHERE task_id = ? AND importance IN ('medium','high')
	                       ORDER BY id ASC`, taskID)
}

func (d *DB) queryFindings(query, taskID string) ([]Finding, error) {
	rows, err := d.sql.Query(query, taskID)
	if err != nil {
		return nil, fmt.Errorf("working: queryFindings: %w", err)
	}
	defer rows.Close()

	var out []Finding
	for rows.Next() {
		var (
			f          Finding
			createdRaw string
		)
		if err := rows.Scan(&f.ID, &f.TaskID, &f.Content, &f.Importance, &f.MemoryType, &createdRaw); err != nil {
			return nil, fmt.Errorf("working: queryFindings: scan: %w", err)
		}
		f.CreatedAt = mustParseTime(createdRaw)
		out = append(out, f)
	}
	return out, rows.Err()
}

func validImportance(s string) bool {
	switch s {
	case ImportanceLow, ImportanceMedium, ImportanceHigh:
		return true
	}
	return false
}
