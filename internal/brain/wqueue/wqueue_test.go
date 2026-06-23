package wqueue

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestQueue(t *testing.T) *Queue {
	t.Helper()
	dir := t.TempDir()
	q, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func TestEnqueueRoundTrip(t *testing.T) {
	q := openTestQueue(t)
	ctx := context.Background()
	it, err := q.Enqueue(ctx, EnqueueOpts{Kind: KindPerceive, SHA: "abc", PayloadJSON: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if it.ID == 0 {
		t.Fatalf("expected non-zero id")
	}
	n, err := q.Depth(ctx)
	if err != nil || n != 1 {
		t.Fatalf("depth=%d err=%v", n, err)
	}
	if err := q.Delete(ctx, it.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	n, _ = q.Depth(ctx)
	if n != 0 {
		t.Fatalf("depth after delete = %d", n)
	}
}

func TestEnqueueAttachStaging(t *testing.T) {
	q := openTestQueue(t)
	ctx := context.Background()
	it, err := q.Enqueue(ctx, EnqueueOpts{
		Kind:        KindAttach,
		SHA:         "deadbeef",
		PayloadJSON: []byte(`{}`),
		Bytes:       []byte("hello world"),
		Ext:         ".bin",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := os.Stat(it.StagedPath); err != nil {
		t.Fatalf("staging file missing: %v", err)
	}
	if err := q.Delete(ctx, it.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(it.StagedPath); !os.IsNotExist(err) {
		t.Fatalf("staging file not removed: %v", err)
	}
}

func TestEnqueueInvalidKind(t *testing.T) {
	q := openTestQueue(t)
	if _, err := q.Enqueue(context.Background(), EnqueueOpts{Kind: "bogus", SHA: "x"}); err != ErrInvalidKind {
		t.Fatalf("want ErrInvalidKind, got %v", err)
	}
}

func TestListNewestFirst(t *testing.T) {
	q := openTestQueue(t)
	ctx := context.Background()
	for _, sha := range []string{"a", "b", "c"} {
		if _, err := q.Enqueue(ctx, EnqueueOpts{Kind: KindLearn, SHA: sha, PayloadJSON: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := q.List(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].SHA != "c" || items[2].SHA != "a" {
		t.Fatalf("unexpected list order: %+v", items)
	}
}

func TestClear(t *testing.T) {
	q := openTestQueue(t)
	ctx := context.Background()
	_, _ = q.Enqueue(ctx, EnqueueOpts{Kind: KindAttach, SHA: "a", PayloadJSON: []byte(`{}`), Bytes: []byte("x"), Ext: ".bin"})
	_, _ = q.Enqueue(ctx, EnqueueOpts{Kind: KindLearn, SHA: "b", PayloadJSON: []byte(`{}`)})
	n, err := q.Clear(ctx)
	if err != nil || n != 2 {
		t.Fatalf("clear n=%d err=%v", n, err)
	}
	entries, _ := os.ReadDir(q.StagingDir())
	if len(entries) != 0 {
		t.Fatalf("staging not cleared: %d entries", len(entries))
	}
}

func TestCleanupOrphan(t *testing.T) {
	q := openTestQueue(t)
	orphan := filepath.Join(q.StagingDir(), "stray.bin")
	if err := os.WriteFile(orphan, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, freed, err := q.Cleanup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || freed != 4 {
		t.Fatalf("cleanup n=%d freed=%d", n, freed)
	}
}

func TestNextEligibleRespectsBackoff(t *testing.T) {
	q := openTestQueue(t)
	ctx := context.Background()
	it, _ := q.Enqueue(ctx, EnqueueOpts{Kind: KindLearn, SHA: "x", PayloadJSON: []byte(`{}`)})
	now := time.Now()
	if err := q.MarkAttempt(ctx, it.ID, now, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.NextEligible(ctx, now.Add(5*time.Second), 16); len(got) != 0 {
		t.Fatalf("expected backoff to hide row; got %d", len(got))
	}
	if got, _ := q.NextEligible(ctx, now.Add(10*time.Minute), 16); len(got) != 1 {
		t.Fatalf("expected row visible after backoff; got %d", len(got))
	}
}

func TestBackoffCap(t *testing.T) {
	if d := BackoffFor(20, nil); d != 5*time.Minute {
		t.Fatalf("expected capped at 5m, got %v", d)
	}
	if d := BackoffFor(0, nil); d != 30*time.Second {
		t.Fatalf("expected 30s, got %v", d)
	}
}
