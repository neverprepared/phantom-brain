package brain

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// writeLegacyVault lays down a v4.x-shaped vault tree (Wiki/, Raw/) with
// a couple of files plus a node_modules scratch dir the migration must
// skip. Returns the legacy root.
func writeLegacyVault(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("Wiki/summaries/a.md", "# A")
	mk("Wiki/entities/e.md", "# E")
	mk("Raw/curated/note.md", "note")
	mk("Raw/gathered/web.md", "web")
	// Scratch dirs that must be skipped.
	mk("node_modules/pkg/index.js", "junk")
	mk(".git/config", "[core]")
	return root
}

func TestMigrateLegacyVault_HappyPath(t *testing.T) {
	legacy := writeLegacyVault(t)
	cfg := agentForTest(t)
	res, err := MigrateLegacyVault(legacy, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("MigrateLegacyVault: %v", err)
	}
	// 4 real content files; node_modules + .git skipped.
	if res.CopiedFiles != 4 {
		t.Errorf("CopiedFiles = %d, want 4", res.CopiedFiles)
	}
	// Manifest must be stamped with the legacy seed source.
	m, err := ReadManifest(res.BrainDir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.SeedSource != SeedSource("legacy-migration") {
		t.Errorf("seed_source = %q, want legacy-migration", m.SeedSource)
	}
	if m.BrainID != res.BrainID || m.Status != StatusAlive {
		t.Errorf("manifest identity wrong: %+v", m)
	}
	// Content landed under brain_dir/vault/ preserving structure.
	got, err := os.ReadFile(filepath.Join(res.BrainDir, "vault", "Wiki", "summaries", "a.md"))
	if err != nil || string(got) != "# A" {
		t.Errorf("migrated file content wrong: %q err=%v", got, err)
	}
	// Scratch dirs must NOT have been copied.
	if _, err := os.Stat(filepath.Join(res.BrainDir, "vault", "node_modules")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("node_modules should have been skipped, stat err=%v", err)
	}
}

func TestMigrateLegacyVault_AlreadyMigrated(t *testing.T) {
	legacy := writeLegacyVault(t)
	cfg := agentForTest(t)
	if _, err := MigrateLegacyVault(legacy, cfg, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	// Second run sees an existing brain dir and refuses.
	_, err := MigrateLegacyVault(legacy, cfg, slog.New(slog.DiscardHandler))
	if !errors.Is(err, ErrLegacyMigrationAlreadyDone) {
		t.Fatalf("want ErrLegacyMigrationAlreadyDone, got %v", err)
	}
}

func TestMigrateLegacyVault_InputValidation(t *testing.T) {
	cfg := agentForTest(t)
	if _, err := MigrateLegacyVault("", cfg, nil); err == nil {
		t.Error("empty path should error")
	}
	if _, err := MigrateLegacyVault("/some/path", nil, nil); err == nil {
		t.Error("nil cfg should error")
	}
	if _, err := MigrateLegacyVault(filepath.Join(t.TempDir(), "does-not-exist"), cfg, nil); err == nil {
		t.Error("inaccessible legacy path should error")
	}
}
