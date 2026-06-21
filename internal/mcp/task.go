package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/neverprepared/mcp-phantom-brain/internal/working"
)

// ---- task_start ----

func taskStartTool() mcp.Tool {
	return mcp.NewTool("task_start",
		mcp.WithDescription(
			`Open a new working-memory task. Returns a task_id you'll pass to `+
				`task_update / task_complete / task_get for the rest of the session. `+
				`Use at the top of any non-trivial multi-step work so findings and `+
				`artifacts have somewhere to land.`,
		),
		mcp.WithString("goal",
			mcp.Required(),
			mcp.Description("One-paragraph statement of what you're trying to accomplish."),
		),
		mcp.WithString("constraints",
			mcp.Description("Optional: constraints / non-goals / things that must not happen."),
		),
		mcp.WithString("plan",
			mcp.Description("Optional: initial plan or step outline. Free text; not parsed."),
		),
	)
}

func (s *Server) handleTaskStart(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	goal, err := req.RequireString("goal")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if strings.TrimSpace(goal) == "" {
		return mcp.NewToolResultError("goal must be non-empty"), nil
	}
	constraints, _ := req.RequireString("constraints")
	plan, _ := req.RequireString("plan")

	id, err := newTaskID()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("alloc task_id: %v", err)), nil
	}
	if err := s.deps.Working.CreateTask(id, goal, constraints, plan); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create task: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("task_id: %s\n\nGoal: %s", id, goal)), nil
}

// newTaskID generates a 16-hex-char task id. Random enough for one
// session's worth of tasks; not a UUID — we don't need universal
// uniqueness, just per-process uniqueness within wm-<PID>.sqlite's
// PRIMARY KEY constraint.
func newTaskID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ---- task_update ----

func taskUpdateTool() mcp.Tool {
	return mcp.NewTool("task_update",
		mcp.WithDescription(
			`Append progress to an open task. Use 'type' to discriminate: `+
				`'finding' (logs an observation), 'artifact' (records a file/PR/URL), `+
				`'question' (logs something unresolved), 'step' (appends a planned step), `+
				`or 'current_step' (sets the in-progress step description). `+
				`Medium and high importance findings are promoted to Raw/curated/ at task_complete.`,
		),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The id returned by task_start."),
		),
		mcp.WithString("type",
			mcp.Required(),
			mcp.Description("One of: finding | artifact | question | step | current_step."),
		),
		mcp.WithString("content",
			mcp.Description("For finding/question/step/current_step: the prose body. Required for those types."),
		),
		mcp.WithString("importance",
			mcp.Description("For type=finding: low | medium | high. Default medium. Only medium and high are promoted at task_complete."),
		),
		mcp.WithString("memory_type",
			mcp.Description("For type=finding: semantic | episodic | procedural. Optional but useful for downstream synthesis."),
		),
		mcp.WithString("name",
			mcp.Description("For type=artifact: human-readable name (e.g. 'API spec PDF', 'PR #42')."),
		),
		mcp.WithString("reference",
			mcp.Description("For type=artifact: the actual file path / URL / PR link."),
		),
	)
}

func (s *Server) handleTaskUpdate(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	updateType, err := req.RequireString("type")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if _, err := s.deps.Working.GetTask(taskID); err != nil {
		if errors.Is(err, working.ErrTaskNotFound) {
			return mcp.NewToolResultError(fmt.Sprintf("no task with id %q (call task_start first)", taskID)), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("get task: %v", err)), nil
	}

	switch strings.ToLower(strings.TrimSpace(updateType)) {
	case "finding":
		return s.taskUpdateFinding(req, taskID)
	case "artifact":
		return s.taskUpdateArtifact(req, taskID)
	case "question":
		return s.taskUpdateQuestion(req, taskID)
	case "step":
		return s.taskUpdateStep(req, taskID)
	case "current_step":
		return s.taskUpdateCurrentStep(req, taskID)
	default:
		return mcp.NewToolResultError(fmt.Sprintf(
			"unknown type %q (want finding|artifact|question|step|current_step)", updateType)), nil
	}
}

func (s *Server) taskUpdateFinding(req mcp.CallToolRequest, taskID string) (*mcp.CallToolResult, error) {
	content, _ := req.RequireString("content")
	if strings.TrimSpace(content) == "" {
		return mcp.NewToolResultError("finding requires content"), nil
	}
	importance := strings.TrimSpace(strings.ToLower(orDefault(req, "importance", working.ImportanceMedium)))
	memoryType := strings.TrimSpace(strings.ToLower(orDefault(req, "memory_type", "")))

	id, err := s.deps.Working.AppendFinding(taskID, content, importance, memoryType)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("append finding: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Finding #%d logged (%s).", id, importance)), nil
}

func (s *Server) taskUpdateArtifact(req mcp.CallToolRequest, taskID string) (*mcp.CallToolResult, error) {
	name, _ := req.RequireString("name")
	reference, _ := req.RequireString("reference")
	if strings.TrimSpace(name) == "" || strings.TrimSpace(reference) == "" {
		return mcp.NewToolResultError("artifact requires name + reference"), nil
	}
	id, err := s.deps.Working.AppendArtifact(taskID, name, reference)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("append artifact: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Artifact #%d logged: %s -> %s", id, name, reference)), nil
}

func (s *Server) taskUpdateQuestion(req mcp.CallToolRequest, taskID string) (*mcp.CallToolResult, error) {
	content, _ := req.RequireString("content")
	if strings.TrimSpace(content) == "" {
		return mcp.NewToolResultError("question requires content"), nil
	}
	id, err := s.deps.Working.AppendQuestion(taskID, content)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("append question: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Question #%d logged.", id)), nil
}

func (s *Server) taskUpdateStep(req mcp.CallToolRequest, taskID string) (*mcp.CallToolResult, error) {
	content, _ := req.RequireString("content")
	if strings.TrimSpace(content) == "" {
		return mcp.NewToolResultError("step requires content"), nil
	}
	id, err := s.deps.Working.AppendStep(taskID, content)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("append step: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Step #%d appended.", id)), nil
}

func (s *Server) taskUpdateCurrentStep(req mcp.CallToolRequest, taskID string) (*mcp.CallToolResult, error) {
	content, _ := req.RequireString("content")
	if strings.TrimSpace(content) == "" {
		return mcp.NewToolResultError("current_step requires content"), nil
	}
	if err := s.deps.Working.SetCurrentStep(taskID, content); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("set current step: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Current step set: %s", content)), nil
}

// orDefault returns the trimmed string at key, or def if missing/empty.
func orDefault(req mcp.CallToolRequest, key, def string) string {
	got, _ := req.RequireString(key)
	if strings.TrimSpace(got) == "" {
		return def
	}
	return got
}

// ---- task_complete ----

func taskCompleteTool() mcp.Tool {
	return mcp.NewTool("task_complete",
		mcp.WithDescription(
			`Mark a task complete and promote its medium/high importance findings to `+
				`Raw/curated/ as a single aggregated note. After this, the task is `+
				`searchable via brain_recall like any other curated knowledge. The task `+
				`row stays in working memory (queryable via task_get) until the orphan `+
				`reaper sweeps the process's shard at next startup.`,
		),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The task to complete."),
		),
	)
}

func (s *Server) handleTaskComplete(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	task, err := s.deps.Working.GetTask(taskID)
	if err != nil {
		if errors.Is(err, working.ErrTaskNotFound) {
			return mcp.NewToolResultError(fmt.Sprintf("no task with id %q", taskID)), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("get task: %v", err)), nil
	}

	important, err := s.deps.Working.ListImportantFindings(taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list findings: %v", err)), nil
	}
	artifacts, err := s.deps.Working.ListArtifacts(taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list artifacts: %v", err)), nil
	}
	questions, err := s.deps.Working.ListQuestions(taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list questions: %v", err)), nil
	}

	// If nothing was logged, complete the task without writing a
	// promoted note — there'd be nothing to index.
	if len(important) == 0 && len(artifacts) == 0 && len(questions) == 0 {
		if err := s.deps.Working.CompleteTask(taskID); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mark complete: %v", err)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Task %s marked complete (no findings to promote).", taskID)), nil
	}

	body := renderPromotedTaskBody(task, important, artifacts, questions)
	title := fmt.Sprintf("Task: %s", strings.TrimSpace(task.Goal))

	res, errMsg, ok := s.ingestMarkdown(ctx, ingestParams{
		Subdir:    "curated",
		StampKey:  "completed_at",
		Content:   body,
		Title:     title,
		Filename:  fmt.Sprintf("task-%s.md", taskID),
		SourceURL: "",
	})
	if !ok {
		return mcp.NewToolResultError(fmt.Sprintf("promote: %s", errMsg)), nil
	}

	if err := s.deps.Working.CompleteTask(taskID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("mark complete: %v", err)), nil
	}

	status := res.Status
	if status == "stored" {
		return mcp.NewToolResultText(fmt.Sprintf(
			"Task %s complete. Promoted %d finding(s), %d artifact(s), %d question(s) to %s. SHA: %s.",
			taskID, len(important), len(artifacts), len(questions), res.RelativePath, res.SHA,
		)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf(
		"Task %s complete. Promotion was a duplicate of an existing note (SHA: %s).", taskID, res.SHA,
	)), nil
}

func renderPromotedTaskBody(task *working.Task, findings []working.Finding, artifacts []working.Artifact, questions []working.Question) string {
	var b strings.Builder

	if task.Goal != "" {
		fmt.Fprintf(&b, "**Goal:** %s\n\n", strings.TrimSpace(task.Goal))
	}
	if task.Constraints != "" {
		fmt.Fprintf(&b, "**Constraints:** %s\n\n", strings.TrimSpace(task.Constraints))
	}
	if task.Plan != "" {
		fmt.Fprintf(&b, "**Plan:** %s\n\n", strings.TrimSpace(task.Plan))
	}

	if len(findings) > 0 {
		b.WriteString("## Findings\n\n")
		for _, f := range findings {
			fmt.Fprintf(&b, "### [%s] ", strings.ToUpper(f.Importance))
			if f.MemoryType != "" {
				fmt.Fprintf(&b, "(%s) ", f.MemoryType)
			}
			fmt.Fprintf(&b, "#%d — %s\n\n", f.ID, f.CreatedAt.Format(time.RFC3339))
			b.WriteString(strings.TrimSpace(f.Content))
			b.WriteString("\n\n")
		}
	}
	if len(artifacts) > 0 {
		b.WriteString("## Artifacts\n\n")
		for _, a := range artifacts {
			fmt.Fprintf(&b, "- **%s** — %s\n", a.Name, a.Reference)
		}
		b.WriteString("\n")
	}
	if len(questions) > 0 {
		b.WriteString("## Open Questions\n\n")
		for _, q := range questions {
			marker := "?"
			if q.Resolved {
				marker = "+"
			}
			fmt.Fprintf(&b, "- (%s) %s\n", marker, q.Question)
			if q.Resolved && q.Resolution != "" {
				fmt.Fprintf(&b, "   - **Resolution:** %s\n", q.Resolution)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ---- task_get ----

func taskGetTool() mcp.Tool {
	return mcp.NewTool("task_get",
		mcp.WithDescription(
			`Read full state for a task: goal, status, current step, all findings, `+
				`artifacts, and questions. Returns whether the task is still active.`,
		),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The task to inspect."),
		),
	)
}

func (s *Server) handleTaskGet(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	task, err := s.deps.Working.GetTask(taskID)
	if err != nil {
		if errors.Is(err, working.ErrTaskNotFound) {
			return mcp.NewToolResultError(fmt.Sprintf("no task with id %q", taskID)), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("get task: %v", err)), nil
	}
	findings, _ := s.deps.Working.ListFindings(taskID)
	artifacts, _ := s.deps.Working.ListArtifacts(taskID)
	questions, _ := s.deps.Working.ListQuestions(taskID)
	steps, _ := s.deps.Working.ListSteps(taskID)

	var b strings.Builder
	fmt.Fprintf(&b, "# Task %s — %s\n\n", task.ID, task.Status)
	fmt.Fprintf(&b, "**Goal:** %s\n\n", task.Goal)
	if task.Constraints != "" {
		fmt.Fprintf(&b, "**Constraints:** %s\n\n", task.Constraints)
	}
	if task.Plan != "" {
		fmt.Fprintf(&b, "**Plan:** %s\n\n", task.Plan)
	}
	if task.CurrentStep != "" {
		fmt.Fprintf(&b, "**Current step:** %s\n\n", task.CurrentStep)
	}
	fmt.Fprintf(&b, "Created: %s · Updated: %s\n\n", task.CreatedAt.Format(time.RFC3339), task.UpdatedAt.Format(time.RFC3339))

	if len(steps) > 0 {
		b.WriteString("## Steps\n\n")
		for _, st := range steps {
			marker := "[ ]"
			if st.Status == working.StepDone {
				marker = "[x]"
			}
			fmt.Fprintf(&b, "- %s #%d %s\n", marker, st.ID, st.Description)
		}
		b.WriteString("\n")
	}
	if len(findings) > 0 {
		b.WriteString("## Findings\n\n")
		for _, f := range findings {
			fmt.Fprintf(&b, "- [%s", f.Importance)
			if f.MemoryType != "" {
				fmt.Fprintf(&b, "/%s", f.MemoryType)
			}
			fmt.Fprintf(&b, "] #%d — %s\n", f.ID, strings.TrimSpace(f.Content))
		}
		b.WriteString("\n")
	}
	if len(artifacts) > 0 {
		b.WriteString("## Artifacts\n\n")
		for _, a := range artifacts {
			fmt.Fprintf(&b, "- #%d **%s** — %s\n", a.ID, a.Name, a.Reference)
		}
		b.WriteString("\n")
	}
	if len(questions) > 0 {
		b.WriteString("## Questions\n\n")
		for _, q := range questions {
			marker := "open"
			if q.Resolved {
				marker = "resolved"
			}
			fmt.Fprintf(&b, "- (%s) #%d %s\n", marker, q.ID, q.Question)
		}
		b.WriteString("\n")
	}
	return mcp.NewToolResultText(b.String()), nil
}
