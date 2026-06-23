package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSnapshotRebuildCmd_RequiresOpenSearch pins issue #53: the
// subcommand must refuse to run when no [opensearch] block is present
// in server.toml, because the only correct rebuild path is the OS
// export (BuildSnapshotFromOS). The legacy local-fs BuildSnapshot is
// silently destructive against a Phase 6+ vault, so we'd rather error
// loudly than fall back.
func TestSnapshotRebuildCmd_RequiresOpenSearch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.toml"), []byte(`
[server]
port = 9998

[defaults]
retention_gens = 30
`), 0o600); err != nil {
		t.Fatalf("write server.toml: %v", err)
	}

	c := snapshotRebuildCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"--config-dir", dir, "--data-dir", t.TempDir(), "personal/memory"})
	c.SilenceUsage = true
	c.SilenceErrors = true

	err := c.Execute()
	if err == nil {
		t.Fatalf("expected error when [opensearch] missing, got nil")
	}
	if !strings.Contains(err.Error(), "opensearch") {
		t.Fatalf("expected opensearch-mentioning error, got %q", err.Error())
	}
}

// TestSnapshotRebuildCmd_LongMentionsOS guards against a regression
// to the legacy local-fs path by asserting the help text advertises
// the OS-export semantics callers should expect post-#53.
func TestSnapshotRebuildCmd_LongMentionsOS(t *testing.T) {
	c := snapshotRebuildCmd()
	if !strings.Contains(c.Long, "OpenSearch") || !strings.Contains(c.Long, "BuildSnapshotFromOS") {
		t.Fatalf("snapshot rebuild Long should mention OpenSearch + BuildSnapshotFromOS, got:\n%s", c.Long)
	}
}
