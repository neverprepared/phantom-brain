package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/brain/wqueue"
)

func newTestQueue(t *testing.T) (*wqueue.Queue, string) {
	t.Helper()
	dir := t.TempDir()
	q, err := wqueue.Open(dir)
	if err != nil {
		t.Fatalf("wqueue.Open: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q, dir
}

func runQueueCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := clientQueueCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return buf.String(), err
}

func TestQueueListEmpty(t *testing.T) {
	_, dir := newTestQueue(t)
	out, err := runQueueCmd(t, "list", "--queue-dir", dir)
	if err != nil {
		t.Fatalf("list empty: %v\n%s", err, out)
	}
	if !strings.Contains(out, "(empty)") {
		t.Fatalf("expected (empty) marker, got:\n%s", out)
	}
}

func TestQueueListSeeded(t *testing.T) {
	q, dir := newTestQueue(t)
	ctx := context.Background()
	if _, err := q.Enqueue(ctx, wqueue.EnqueueOpts{Kind: wqueue.KindPerceive, SHA: "abcdef0123456789", PayloadJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, wqueue.EnqueueOpts{Kind: wqueue.KindLearn, SHA: "1111222233334444", PayloadJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	out, err := runQueueCmd(t, "list", "--queue-dir", dir)
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "abcdef012345") || !strings.Contains(out, "111122223333") {
		t.Fatalf("expected both short SHAs in output, got:\n%s", out)
	}
	if !strings.Contains(out, "perceive") || !strings.Contains(out, "learn") {
		t.Fatalf("expected both kinds, got:\n%s", out)
	}
}

func TestQueueListJSON(t *testing.T) {
	q, dir := newTestQueue(t)
	_, _ = q.Enqueue(context.Background(), wqueue.EnqueueOpts{Kind: wqueue.KindLearn, SHA: "deadbeef", PayloadJSON: []byte(`{}`)})
	out, err := runQueueCmd(t, "list", "--queue-dir", dir, "--json")
	if err != nil {
		t.Fatalf("list --json: %v\n%s", err, out)
	}
	var got queueListJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out)
	}
	if len(got.Items) != 1 || got.Items[0].SHA != "deadbeef" {
		t.Fatalf("unexpected json: %+v", got)
	}
}

func TestQueueClearRequiresConfirm(t *testing.T) {
	q, dir := newTestQueue(t)
	_, _ = q.Enqueue(context.Background(), wqueue.EnqueueOpts{Kind: wqueue.KindLearn, SHA: "x", PayloadJSON: []byte(`{}`)})
	out, err := runQueueCmd(t, "clear", "--queue-dir", dir)
	if err == nil {
		t.Fatalf("expected error without --confirm, got nil; out=%s", out)
	}
	if !strings.Contains(out, "would delete 1") {
		t.Fatalf("expected dry-run preview, got:\n%s", out)
	}
	n, _ := q.Depth(context.Background())
	if n != 1 {
		t.Fatalf("queue should be untouched, depth=%d", n)
	}
}

func TestQueueClearConfirm(t *testing.T) {
	q, dir := newTestQueue(t)
	_, _ = q.Enqueue(context.Background(), wqueue.EnqueueOpts{Kind: wqueue.KindLearn, SHA: "x", PayloadJSON: []byte(`{}`)})
	out, err := runQueueCmd(t, "clear", "--queue-dir", dir, "--confirm")
	if err != nil {
		t.Fatalf("clear --confirm: %v\n%s", err, out)
	}
	if !strings.Contains(out, "cleared 1") {
		t.Fatalf("expected cleared count, got:\n%s", out)
	}
	n, _ := q.Depth(context.Background())
	if n != 0 {
		t.Fatalf("depth after clear=%d", n)
	}
}

func TestQueueDrainNowEmpty(t *testing.T) {
	_, dir := newTestQueue(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"sha":"x","indexed_at":1,"synth_enqueued":true}`))
	}))
	defer ts.Close()
	setAgentEnv(t, ts.URL)
	out, err := runQueueCmd(t, "drain-now", "--queue-dir", dir)
	if err != nil {
		t.Fatalf("drain-now empty: %v\n%s", err, out)
	}
	if !strings.Contains(out, "sent=0 failed=0") {
		t.Fatalf("expected zero counts, got:\n%s", out)
	}
}

func TestQueueDrainNowPostsAll(t *testing.T) {
	q, dir := newTestQueue(t)
	for _, sha := range []string{"a", "b", "c"} {
		payload, _ := json.Marshal(brain.PerceiveRequest{SHA: sha, Title: "t", Body: "b"})
		if _, err := q.Enqueue(context.Background(), wqueue.EnqueueOpts{
			Kind: wqueue.KindPerceive, SHA: sha, PayloadJSON: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"sha":"x","indexed_at":1,"synth_enqueued":true}`))
	}))
	defer ts.Close()
	setAgentEnv(t, ts.URL)
	out, err := runQueueCmd(t, "drain-now", "--queue-dir", dir)
	if err != nil {
		t.Fatalf("drain-now: %v\n%s", err, out)
	}
	if hits != 3 {
		t.Fatalf("expected 3 POSTs, got %d", hits)
	}
	if !strings.Contains(out, "sent=3 failed=0") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	n, _ := q.Depth(context.Background())
	if n != 0 {
		t.Fatalf("queue not drained, depth=%d", n)
	}
}

func TestQueueDrainNowFailures(t *testing.T) {
	q, dir := newTestQueue(t)
	payload, _ := json.Marshal(brain.PerceiveRequest{SHA: "z", Title: "t", Body: "b"})
	_, _ = q.Enqueue(context.Background(), wqueue.EnqueueOpts{
		Kind: wqueue.KindPerceive, SHA: "z", PayloadJSON: payload,
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"BOOM","message":"nope"}}`, http.StatusInternalServerError)
	}))
	defer ts.Close()
	setAgentEnv(t, ts.URL)
	out, err := runQueueCmd(t, "drain-now", "--queue-dir", dir)
	if err == nil {
		t.Fatalf("expected non-nil error on failure, got nil; out=%s", out)
	}
	if !strings.Contains(out, "sent=0 failed=1") {
		t.Fatalf("expected failed=1 in output, got:\n%s", out)
	}
	n, _ := q.Depth(context.Background())
	if n != 1 {
		t.Fatalf("failed item should remain queued, depth=%d", n)
	}
}

func TestQueueListMissingPrintsFriendlyMessage(t *testing.T) {
	// Bare temp dir with no wqueue.sqlite — list must succeed (exit 0)
	// and tell the operator there's nothing yet without creating files.
	dir := t.TempDir()
	out, err := runQueueCmd(t, "list", "--queue-dir", dir)
	if err != nil {
		t.Fatalf("list missing: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no queue") {
		t.Fatalf("expected 'no queue' message, got:\n%s", out)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("OpenReadOnly path created files: %+v", entries)
	}
}

func TestQueueDrainNowDaemonUnreachableExitsZero(t *testing.T) {
	q, dir := newTestQueue(t)
	payload, _ := json.Marshal(brain.PerceiveRequest{SHA: "u", Title: "t", Body: "b"})
	_, _ = q.Enqueue(context.Background(), wqueue.EnqueueOpts{
		Kind: wqueue.KindPerceive, SHA: "u", PayloadJSON: payload,
	})
	// Point at a closed httptest server so client.Do fails with a
	// network error (connection refused) — wrapped as ErrDaemonUnreachable.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts.Close()
	setAgentEnv(t, ts.URL)
	out, err := runQueueCmd(t, "drain-now", "--queue-dir", dir)
	if err != nil {
		t.Fatalf("drain-now should exit 0 when daemon is unreachable; err=%v\nout=%s", err, out)
	}
	if !strings.Contains(out, "daemon unreachable") {
		t.Fatalf("expected 'daemon unreachable' notice, got:\n%s", out)
	}
	n, _ := q.Depth(context.Background())
	if n != 1 {
		t.Fatalf("item should still be queued, depth=%d", n)
	}
}

func TestQueueRejectsPathTraversalProfile(t *testing.T) {
	_, err := runQueueCmd(t, "list", "--profile", "../etc", "--vault", "v")
	if err == nil {
		t.Fatalf("expected error rejecting path traversal")
	}
	if !strings.Contains(err.Error(), "--profile") {
		t.Fatalf("unexpected err: %v", err)
	}
}

func setAgentEnv(t *testing.T, baseURL string) {
	t.Helper()
	t.Setenv("CL_BRAIN_API", baseURL)
	t.Setenv("CL_BRAIN_API_TOKEN", "test-token")
	t.Setenv("CL_WORKSPACE_PROFILE", "p")
	t.Setenv("CL_BRAIN_VAULT", "v")
	// Make sure data home resolution doesn't break.
	if os.Getenv("XDG_DATA_HOME") == "" && os.Getenv("HOME") == "" {
		t.Setenv("HOME", t.TempDir())
	}
}
