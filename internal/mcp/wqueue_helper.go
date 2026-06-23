package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/brain/wqueue"
)

// daemonOp is the small surface enqueueAndAttempt needs to invoke
// the right daemon endpoint for an enqueued item. Each MCP write
// handler supplies one (closure that calls client.Perceive / Learn /
// Attach with the request it already has in hand).
type daemonOp func(ctx context.Context) error

// queueWriteResult mirrors the daemon's WriteResponse but adds a
// human-readable Notice that the MCP handler appends to its text
// result. Notice is empty on the happy path (posted to daemon
// successfully). When the daemon was unreachable, Notice carries the
// "Queued (daemon unreachable since 2m). 3 writes pending sync." line.
type queueWriteResult struct {
	Posted bool
	Notice string
}

// enqueueAndAttempt persists the write to the per-Lifecycle wqueue,
// immediately tries op(ctx), and returns a notice string ("" on
// success). On daemon failure the row stays in the queue, the
// connectivity state flips to degraded (or stays offline), and the
// caller surfaces success-with-notice to the user — never an error.
//
// Returns (Posted=true, Notice="") when the daemon accepted the
// write. Returns (Posted=false, Notice="Queued ...") when it didn't.
// Returns an error ONLY for queue I/O failures (sqlite write error,
// staging disk full, etc.) — these are real and the caller should
// surface them.
//
// Legacy mode (no Lifecycle, or Lifecycle without a queue) falls
// straight through to op(ctx); errors propagate as before so the
// BRAIN_VAULT_PATH-only tests still see the old behavior.
func (s *Server) enqueueAndAttempt(
	ctx context.Context,
	kind wqueue.Kind,
	sha string,
	wireReq any,
	attachBytes []byte,
	attachExt string,
	op daemonOp,
) (queueWriteResult, error) {
	lc := s.deps.Lifecycle
	if lc == nil || lc.Queue() == nil {
		// Legacy path — no queue, attempt directly. Pre-#61 behavior:
		// on failure the caller's wrapper returns an error to the user.
		if err := op(ctx); err != nil {
			return queueWriteResult{}, err
		}
		return queueWriteResult{Posted: true}, nil
	}
	payload, err := json.Marshal(wireReq)
	if err != nil {
		return queueWriteResult{}, fmt.Errorf("marshal wire request: %w", err)
	}
	item, err := lc.Queue().Enqueue(ctx, wqueue.EnqueueOpts{
		Kind:        kind,
		SHA:         sha,
		PayloadJSON: payload,
		Bytes:       attachBytes,
		Ext:         attachExt,
	})
	if err != nil {
		return queueWriteResult{}, fmt.Errorf("wqueue enqueue: %w", err)
	}
	now := time.Now()
	if opErr := op(ctx); opErr != nil {
		// Failure: leave the row, flip connectivity, return notice.
		_ = lc.Queue().MarkAttempt(ctx, item.ID, now, opErr)
		lc.Connectivity().NoteFailure(now, opErr)
		depth, _ := lc.Queue().Depth(ctx)
		return queueWriteResult{
			Posted: false,
			Notice: formatQueueNotice(lc.Connectivity().Snapshot(), depth),
		}, nil
	}
	// Success: drop the row, flip connectivity online.
	_ = lc.Queue().Delete(ctx, item.ID)
	lc.Connectivity().NoteSuccess(now)
	return queueWriteResult{Posted: true}, nil
}

// formatQueueNotice renders the user-facing "your write is queued"
// line. Designed to be cheap to read inside a tool result: one
// sentence, no jargon, conveys both the outage age and the pending
// depth so the operator can decide whether to investigate.
func formatQueueNotice(snap brain.ConnectivitySnapshot, depth int) string {
	var since string
	if snap.LastSuccessAt.IsZero() {
		since = "since process start"
	} else {
		since = "since " + humanizeAge(time.Since(snap.LastSuccessAt)) + " ago"
	}
	plural := "writes"
	if depth == 1 {
		plural = "write"
	}
	return fmt.Sprintf("\n\nQueued (daemon unreachable %s). %d %s pending sync.", since, depth, plural)
}

// humanizeAge renders a Duration as a short human string suitable for
// terse status lines: "10s", "2m", "1h", "3d". Always rounds toward
// the nearest stable unit so the value is comprehensible at a glance.
func humanizeAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
