package wqueue

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) *Queue {
	t.Helper()
	q, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { q.Close() })
	return q
}

func TestOpenRejectsRelative(t *testing.T) {
	if _, err := Open("relative/path"); err == nil {
		t.Fatal("expected error for relative path")
	}
}

func TestOpenIdempotent(t *testing.T) {
	dir := t.TempDir()
	q1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	q1.Close()
	q2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(2): %v", err)
	}
	defer q2.Close()
	if _, err := q2.Depth(context.Background()); err != nil {
		t.Fatalf("Depth: %v", err)
	}
}

func TestEnqueueRoundTrip(t *testing.T) {
	q := openTest(t)
	ctx := context.Background()
	it, err := q.Enqueue(ctx, EnqueueOpts{
		Kind:        KindPerceive,
		SHA:         "abc123",
		PayloadJSON: []byte(`{"title":"t"}`),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if it.ID == 0 {
		t.Fatal("expected non-zero ID")
	}
	n, err := q.Depth(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Depth = %d, %v; want 1, nil", n, err)
	}
	items, err := q.NextEligible(ctx, time.Now(), 10)
	if err != nil || len(items) != 1 || items[0].SHA != "abc123" {
		t.Fatalf("NextEligible = %v, %v", items, err)
	}
	if err := q.Delete(ctx, it.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	n, _ = q.Depth(ctx)
	if n != 0 {
		t.Fatalf("Depth after Delete = %d, want 0", n)
	}
}

func TestEnqueueValidations(t *testing.T) {
	q := openTest(t)
	if _, err := q.Enqueue(context.Background(), EnqueueOpts{Kind: "bogus", SHA: "x"}); !errors.Is(err, ErrInvalidKind) {
		t.Fatalf("want ErrInvalidKind, got %v", err)
	}
	if _, err := q.Enqueue(context.Background(), EnqueueOpts{Kind: KindLearn, SHA: ""}); !errors.Is(err, ErrEmptySHA) {
		t.Fatalf("want ErrEmptySHA, got %v", err)
	}
}

func TestEnqueueAttachStagingCopy(t *testing.T) {
	q := openTest(t)
	ctx := context.Background()
	payload := []byte("payload-without-bytes")
	blob := []byte("the actual file bytes")
	it, err := q.Enqueue(ctx, EnqueueOpts{
		Kind:        KindAttach,
		SHA:         "deadbeef",
		PayloadJSON: payload,
		Bytes:       blob,
		Ext:         ".pdf",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	want := filepath.Join(q.attachDir, "deadbeef.pdf")
	if it.StagedPath != want {
		t.Fatalf("StagedPath = %q, want %q", it.StagedPath, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(blob) {
		t.Fatalf("staged bytes mismatch")
	}
	if err := q.Delete(ctx, it.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("staged file should be gone, stat err = %v", err)
	}
}

func TestEnqueueOversizeAttach(t *testing.T) {
	q := openTest(t)
	big := make([]byte, MaxStagedBytes+1)
	_, err := q.Enqueue(context.Background(), EnqueueOpts{
		Kind: KindAttach, SHA: "x", Bytes: big,
	})
	if !errors.Is(err, ErrOversize) {
		t.Fatalf("want ErrOversize, got %v", err)
	}
	n, _ := q.Depth(context.Background())
	if n != 0 {
		t.Fatalf("Depth = %d, want 0 (no row should have been inserted)", n)
	}
	// Also: no staging file left behind.
	entries, _ := os.ReadDir(q.attachDir)
	if len(entries) != 0 {
		t.Fatalf("attach dir not empty: %v", entries)
	}
}

func TestBackoffMath(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 30 * time.Second},
		{1, 60 * time.Second},
		{2, 120 * time.Second},
		{3, 240 * time.Second},
		{4, 5 * time.Minute},
		{10, 5 * time.Minute},
	}
	for _, c := range cases {
		got := backoffBase(c.attempts)
		if got != c.want {
			t.Errorf("backoffBase(%d) = %s, want %s", c.attempts, got, c.want)
		}
	}
	// Jitter stays within ±20%.
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		d := BackoffFor(2, rng)
		base := 120 * time.Second
		lo := time.Duration(float64(base) * 0.8)
		hi := time.Duration(float64(base) * 1.2)
		if d < lo || d > hi {
			t.Errorf("BackoffFor jitter out of band: %s", d)
		}
	}
}

func TestNextEligibleRespectsBackoff(t *testing.T) {
	q := openTest(t)
	ctx := context.Background()
	it, err := q.Enqueue(ctx, EnqueueOpts{Kind: KindLearn, SHA: "s1"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	t0 := time.Now()
	if err := q.MarkAttempt(ctx, it.ID, t0, errors.New("nope")); err != nil {
		t.Fatalf("MarkAttempt: %v", err)
	}
	// attempts now 1 → backoff base 60s.
	items, _ := q.NextEligible(ctx, t0.Add(10*time.Second), 10)
	if len(items) != 0 {
		t.Fatalf("within backoff window, got %d items", len(items))
	}
	items, _ = q.NextEligible(ctx, t0.Add(2*time.Minute), 10)
	if len(items) != 1 {
		t.Fatalf("past backoff window, got %d items", len(items))
	}
	if items[0].Attempts != 1 || items[0].LastError != "nope" {
		t.Fatalf("attempt fields not persisted: %+v", items[0])
	}
}

func TestCleanupOrphanStagingFile(t *testing.T) {
	q := openTest(t)
	ctx := context.Background()
	orphan := filepath.Join(q.attachDir, "orphan.bin")
	if err := os.WriteFile(orphan, []byte("xxxx"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, freed, err := q.Cleanup(ctx)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if n != 1 || freed != 4 {
		t.Fatalf("Cleanup = (%d, %d); want (1, 4)", n, freed)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan still present: %v", err)
	}
}

func TestCleanupPreservesReferenced(t *testing.T) {
	q := openTest(t)
	ctx := context.Background()
	it, _ := q.Enqueue(ctx, EnqueueOpts{
		Kind: KindAttach, SHA: "keep", Bytes: []byte("k"), Ext: ".bin",
	})
	if _, _, err := q.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(it.StagedPath); err != nil {
		t.Fatalf("referenced staged file removed: %v", err)
	}
}

func TestListNewestFirst(t *testing.T) {
	q := openTest(t)
	ctx := context.Background()
	for _, s := range []string{"a", "b", "c"} {
		if _, err := q.Enqueue(ctx, EnqueueOpts{Kind: KindLearn, SHA: s}); err != nil {
			t.Fatal(err)
		}
	}
	items, _ := q.List(ctx, 0)
	if len(items) != 3 || items[0].SHA != "c" || items[2].SHA != "a" {
		t.Fatalf("unexpected order: %+v", items)
	}
}

func TestClear(t *testing.T) {
	q := openTest(t)
	ctx := context.Background()
	q.Enqueue(ctx, EnqueueOpts{Kind: KindLearn, SHA: "x"})
	q.Enqueue(ctx, EnqueueOpts{Kind: KindAttach, SHA: "y", Bytes: []byte("z"), Ext: ".bin"})
	n, err := q.Clear(ctx)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if n != 2 {
		t.Fatalf("Clear returned %d, want 2", n)
	}
	d, _ := q.Depth(ctx)
	if d != 0 {
		t.Fatalf("Depth after Clear = %d", d)
	}
	entries, _ := os.ReadDir(q.attachDir)
	if len(entries) != 0 {
		t.Fatalf("attach dir not empty after Clear: %v", entries)
	}
}

func TestConcurrentEnqueueDelete(t *testing.T) {
	q := openTest(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	const N = 50
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			it, err := q.Enqueue(ctx, EnqueueOpts{
				Kind: KindPerceive, SHA: "sha" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			})
			if err != nil {
				t.Errorf("Enqueue: %v", err)
				return
			}
			if err := q.Delete(ctx, it.ID); err != nil {
				t.Errorf("Delete: %v", err)
			}
		}(i)
	}
	wg.Wait()
	n, _ := q.Depth(ctx)
	if n != 0 {
		t.Fatalf("Depth = %d, want 0", n)
	}
}
