package vault

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureSkeletonCreatesAllPaths(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "vault")

	if err := EnsureSkeleton(root); err != nil {
		t.Fatal(err)
	}
	for _, p := range SkeletonPaths(root) {
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("missing %s: %v", p, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", p)
		}
	}
}

func TestEnsureSkeletonIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "vault")

	for i := 0; i < 3; i++ {
		if err := EnsureSkeleton(root); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	// Path count unchanged after multiple runs.
	for _, p := range SkeletonPaths(root) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing after repeated EnsureSkeleton: %s", p)
		}
	}
}

func TestEnsureSkeletonRejectsEmpty(t *testing.T) {
	if err := EnsureSkeleton(""); err == nil {
		t.Error("empty dir should error")
	}
}

func TestEnsureSkeletonDoesNotTouchExistingFiles(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "vault")
	if err := EnsureSkeleton(root); err != nil {
		t.Fatal(err)
	}
	// Drop a sentinel inside one of the directories.
	sentinel := filepath.Join(root, WikiDir, "preserved.md")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSkeleton(root); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("sentinel disappeared: %v", err)
	}
	if string(got) != "keep me" {
		t.Errorf("sentinel content = %q, want %q", got, "keep me")
	}
}
