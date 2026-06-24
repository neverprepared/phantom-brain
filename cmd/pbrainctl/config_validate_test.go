package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBinding drops an auth.toml (and optional config.toml) under a
// config dir, creating the profiles/<p>/vaults/<v>/ tree.
func writeBinding(t *testing.T, cfgDir, profile, vault, token, configTOML string) {
	t.Helper()
	dir := filepath.Join(cfgDir, "profiles", profile, "vaults", vault)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir binding: %v", err)
	}
	auth := []byte("bearer_token = \"" + token + "\"\n")
	if err := os.WriteFile(filepath.Join(dir, "auth.toml"), auth, 0o600); err != nil {
		t.Fatalf("write auth.toml: %v", err)
	}
	if configTOML != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(configTOML), 0o644); err != nil {
			t.Fatalf("write config.toml: %v", err)
		}
	}
}

// runValidate executes `config validate` against cfgDir with the given
// positional args and returns (stdout+stderr, error).
func runValidate(t *testing.T, cfgDir string, args ...string) (string, error) {
	t.Helper()
	cmd := configValidateCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	_ = cmd.Flags().Set("config-dir", cfgDir)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// server.toml with no [opensearch] block: OpenSearch.Enabled() is
// false, so the footgun guard is skipped and no OS is needed.
const noOSServerToml = "[storage]\nbackend = \"local\"\n"

func TestConfigValidate_FullLoadClean(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, noOSServerToml)
	writeBinding(t, cfgDir, "personal", "memory", "tok-personal", "")
	writeBinding(t, cfgDir, "gsa", "memory", "tok-gsa", "")

	out, err := runValidate(t, cfgDir)
	if err != nil {
		t.Fatalf("validate failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK: 2 binding(s) validate") {
		t.Errorf("missing OK summary in:\n%s", out)
	}
	if !strings.Contains(out, "skipped storage-override footgun guard") {
		t.Errorf("expected footgun-skipped warning (no OS configured):\n%s", out)
	}
}

func TestConfigValidate_DuplicateToken(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, noOSServerToml)
	writeBinding(t, cfgDir, "a", "v", "same-token", "")
	writeBinding(t, cfgDir, "b", "v", "same-token", "")

	_, err := runValidate(t, cfgDir)
	if err == nil {
		t.Fatal("expected duplicate-token error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate bearer_token") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestConfigValidate_BadOverridePrefix(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, noOSServerToml)
	// Uppercase + dash are illegal in an index prefix.
	writeBinding(t, cfgDir, "client", "main", "tok", "[storage_overrides]\nindex_prefix = \"BAD-Prefix\"\n")

	_, err := runValidate(t, cfgDir)
	if err == nil {
		t.Fatal("expected override-prefix error, got nil")
	}
}

func TestConfigValidate_SingleBindingSkipsDedup(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, noOSServerToml)
	// Two bindings share a token — a full load would fail. Single-binding
	// mode validates one in isolation and must NOT trip the dedup check.
	writeBinding(t, cfgDir, "a", "v", "same-token", "")
	writeBinding(t, cfgDir, "b", "v", "same-token", "")

	out, err := runValidate(t, cfgDir, "a/v")
	if err != nil {
		t.Fatalf("single-binding validate failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK: binding a/v validates") {
		t.Errorf("missing single-binding OK in:\n%s", out)
	}
	if !strings.Contains(out, "token-dedup check skipped") {
		t.Errorf("expected dedup-skipped note in:\n%s", out)
	}
}

func TestConfigValidate_SingleBindingNotFound(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, noOSServerToml)
	writeBinding(t, cfgDir, "a", "v", "tok", "")

	_, err := runValidate(t, cfgDir, "nope/missing")
	if err == nil {
		t.Fatal("expected error for missing binding, got nil")
	}
}
