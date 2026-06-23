package wqueue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnqueueRoundTrip(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer q.Close()

	ctx := context.Background()
	item, err := q.Enqueue(ctx, EnqueueOpts{
		Kind: KindPerceive, SHA: "sha-1", PayloadJSON: []byte(`{"x":1}`),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if item.ID == 0 {
		t.Errorf("ID not populated")
	}
	d, _ := q.Depth(ctx)
	if d != 1 {
		t.Errorf("Depth = %d, want 1", d)
	}
	if err := q.Delete(ctx, item.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	d, _ = q.Depth(ctx)
	if d != 0 {
		t.Errorf("Depth after delete = %d, want 0", d)
	}
}

func TestEnqueueAttachStagingCopy(t *testing.T) {
	dir := t.TempDir()
	q, _ := Open(dir)
	defer q.Close()
	ctx := context.Background()
	body := []byte("hello world")
	item, err := q.Enqueue(ctx, EnqueueOpts{
		Kind: KindAttach, SHA: "sha-attach", PayloadJSON: []byte(`{}`),
		Bytes: body, Ext: ".bin",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	want := filepath.Join(q.StageDir(), "sha-attach.bin")
	if item.StagedPath != want {
		t.Errorf("StagedPath = %q, want %q", item.StagedPath, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("staged bytes = %q, want %q", got, body)
	}
	// Delete removes both row and staging file.
	if err := q.Delete(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Errorf("staging file still present after Delete: err=%v", err)
	}
}

func TestEnqueueValidation(t *testing.T) {
	dir := t.TempDir()
	q, _ := Open(dir)
	defer q.Close()
	ctx := context.Background()
	if _, err := q.Enqueue(ctx, EnqueueOpts{Kind: "bogus", SHA: "x"}); !errors.Is(err, ErrInvalidKind) {
		t.Errorf("expected ErrInvalidKind, got %v", err)
	}
	if _, err := q.Enqueue(ctx, EnqueueOpts{Kind: KindPerceive}); !errors.Is(err, ErrEmptySHA) {
		t.Errorf("expected ErrEmptySHA, got %v", err)
	}
}

func TestCleanupOrphanStagingFile(t *testing.T) {
	dir := t.TempDir()
	q, _ := Open(dir)
	defer q.Close()
	ctx := context.Background()
	orphanPath := filepath.Join(q.StageDir(), "orphan.bin")
	if err := os.WriteFile(orphanPath, []byte("xx"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, bytes, err := q.Cleanup(ctx)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if n != 1 || bytes != 2 {
		t.Errorf("Cleanup = (%d, %d), want (1, 2)", n, bytes)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("orphan still present")
	}
}

func TestNextEligibleRespectsBackoff(t *testing.T) {
	dir := t.TempDir()
	q, _ := Open(dir)
	defer q.Close()
	ctx := context.Background()
	item, _ := q.Enqueue(ctx, EnqueueOpts{Kind: KindLearn, SHA: "s", PayloadJSON: []byte(`{}`)})
	// Brand new: eligible immediately.
	ready, _ := q.NextEligible(ctx, time.Now(), 10)
	if len(ready) != 1 {
		t.Fatalf("expected 1 eligible, got %d", len(ready))
	}
	// Mark a recent attempt with attempts=3 — backoff window won't have expired.
	now := time.Now()
	if err := q.MarkAttempt(ctx, item.ID, now, errors.New("net")); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkAttempt(ctx, item.ID, now, errors.New("net")); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkAttempt(ctx, item.ID, now, errors.New("net")); err != nil {
		t.Fatal(err)
	}
	ready, _ = q.NextEligible(ctx, now.Add(2*time.Second), 10)
	if len(ready) != 0 {
		t.Errorf("expected 0 eligible within backoff window, got %d", len(ready))
	}
	// Way past the cap (5min) is always eligible.
	ready, _ = q.NextEligible(ctx, now.Add(10*time.Minute), 10)
	if len(ready) != 1 {
		t.Errorf("expected 1 eligible past backoff, got %d", len(ready))
	}
}

func TestClearWipesEverything(t *testing.T) {
	dir := t.TempDir()
	q, _ := Open(dir)
	defer q.Close()
	ctx := context.Background()
	_, _ = q.Enqueue(ctx, EnqueueOpts{Kind: KindAttach, SHA: "a", PayloadJSON: []byte(`{}`), Bytes: []byte("xx"), Ext: ".bin"})
	_, _ = q.Enqueue(ctx, EnqueueOpts{Kind: KindPerceive, SHA: "b", PayloadJSON: []byte(`{}`)})
	n, err := q.Clear(ctx)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if n != 2 {
		t.Errorf("Clear count = %d, want 2", n)
	}
	d, _ := q.Depth(ctx)
	if d != 0 {
		t.Errorf("Depth after clear = %d", d)
	}
	entries, _ := os.ReadDir(q.StageDir())
	if len(entries) != 0 {
		t.Errorf("staging dir not empty after Clear: %d entries", len(entries))
	}
}
