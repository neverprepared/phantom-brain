package brain

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestReflinkOrCopyFile_FallsBackOnUnsupportedFS(t *testing.T) {
	// macOS tempdir is APFS (reflink works); Linux's tmpfs is not.
	// Either way we should get bytes at dst — ReflinkOrCopyFile
	// transparently degrades.
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("hello reflink"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ReflinkOrCopyFile(src, dst); err != nil {
		t.Fatalf("ReflinkOrCopyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello reflink" {
		t.Errorf("got %q", got)
	}
}

func TestReflinkOrCopyTree_MirrorsLayout(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a", "top.txt"), []byte("top"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a", "b", "nested.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := ReflinkOrCopyTree(src, dst); err != nil {
		t.Fatalf("ReflinkOrCopyTree: %v", err)
	}
	for _, rel := range []string{"a/top.txt", "a/b/nested.txt"} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("expected %s in dst tree: %v", rel, err)
		}
	}
}

func TestReflinkOrCopyTree_RejectsExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	err := ReflinkOrCopyTree(src, dst)
	if err == nil {
		t.Fatal("expected refusal on existing destination")
	}
}

func TestReflinkFile_ErrIsSentinelSentinel(t *testing.T) {
	// Sanity: ErrReflinkUnsupported is unwrappable to itself. Catches
	// regressions where the sentinel gets shadowed by a fmt.Errorf
	// without %w.
	if !errors.Is(ErrReflinkUnsupported, ErrReflinkUnsupported) {
		t.Fatal("sentinel sentinel: errors.Is on self should be true")
	}
}

// --- shipqueue --------------------------------------------------------

func TestShipQueue_EmptyWhenNothingDied(t *testing.T) {
	agent := agentForTest(t)
	items, err := ListShipQueue(agent)
	if err != nil {
		t.Fatalf("ListShipQueue: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected empty queue, got %d items", len(items))
	}
}

func TestShipQueue_DepthMatchesPayloads(t *testing.T) {
	agent := agentForTest(t)
	// Synthesize two payloads in the ship-pending tree.
	for i, bid := range []string{"b1", "b2"} {
		dir := filepath.Join(agent.ShipPendingDir(), bid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := bytes.Repeat([]byte{byte(i + 1)}, 256)
		if err := os.WriteFile(filepath.Join(dir, "death-100.tar"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	items, err := ListShipQueue(agent)
	if err != nil {
		t.Fatalf("ListShipQueue: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	depth, err := ShipQueueDepthBytes(agent)
	if err != nil {
		t.Fatalf("ShipQueueDepthBytes: %v", err)
	}
	if depth != 512 {
		t.Errorf("depth = %d, want 512", depth)
	}
}

func TestShipQueue_UploadEmptyQueueIsNoOp(t *testing.T) {
	agent := agentForTest(t)
	res, err := UploadShipQueue(context.Background(), agent, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("UploadShipQueue: %v", err)
	}
	if res == nil || len(res.Shipped)+len(res.Skipped)+len(res.Failed) != 0 {
		t.Errorf("expected empty result, got %+v", res)
	}
}

// --- snapcache --------------------------------------------------------

func TestSnapcache_EmptyByDefault(t *testing.T) {
	agent := agentForTest(t)
	got, err := ListCachedSnapshots(agent)
	if err != nil {
		t.Fatalf("ListCachedSnapshots: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no cached snapshots, got %v", got)
	}
}

func TestSnapcache_FetchReturnsErrorWhenDaemonUnreachable(t *testing.T) {
	// agentForTest sets CL_BRAIN_API to https://example.invalid which
	// reliably fails to resolve / connect. Phase 2.5: this surface
	// returns a wrapped HTTP error rather than the Phase 1
	// ErrDaemonUnavailable sentinel.
	agent := agentForTest(t)
	_, err := FetchSnapshotFromDaemon(context.Background(), agent, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected error when daemon unreachable")
	}
}
