package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	pbserver "github.com/neverprepared/phantom-brain/internal/server"
)

// profileTestCmd builds a command carrying the config-dir/data-dir flags
// the step functions read via resolveConfigDir/resolveDataDir.
func profileTestCmd(t *testing.T, configDir string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{}
	c.Flags().String("config-dir", configDir, "")
	c.Flags().String("data-dir", t.TempDir(), "")
	return c
}

// TestWriteBindingConfigStep_FreshWritesDerivedNames covers the happy
// path: a fresh binding gets auth.toml (a token) + config.toml carrying
// the caller-derived index_prefix + bucket, and reports "created".
func TestWriteBindingConfigStep_FreshWritesDerivedNames(t *testing.T) {
	cfgDir := t.TempDir()
	cmd := profileTestCmd(t, cfgDir)
	key := pbserver.VaultKey{Profile: "gsa", Vault: "memory"}

	s := writeBindingConfigStep(cmd, key, "gsa_", "gsa-archives", "")
	if s.err != nil || s.result != "created" {
		t.Fatalf("fresh write: result=%q err=%v", s.result, s.err)
	}

	dir := filepath.Join(cfgDir, "profiles", "gsa", "vaults", "memory")
	auth, err := os.ReadFile(filepath.Join(dir, "auth.toml"))
	if err != nil {
		t.Fatalf("auth.toml missing: %v", err)
	}
	if !strings.Contains(string(auth), "bearer_token") {
		t.Errorf("auth.toml has no bearer_token: %s", auth)
	}
	cfg, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("config.toml missing: %v", err)
	}
	for _, want := range []string{"[storage_overrides]", `index_prefix = "gsa_"`, `bucket = "gsa-archives"`} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("config.toml missing %q in:\n%s", want, cfg)
		}
	}
}

// TestWriteBindingConfigStep_IdempotentKeepsToken is the safety contract
// that separates `profile create` from `binding create`: re-running on an
// existing binding must NEVER clobber the live bearer token.
func TestWriteBindingConfigStep_IdempotentKeepsToken(t *testing.T) {
	cfgDir := t.TempDir()
	cmd := profileTestCmd(t, cfgDir)
	key := pbserver.VaultKey{Profile: "gsa", Vault: "memory"}
	authPath := filepath.Join(cfgDir, "profiles", "gsa", "vaults", "memory", "auth.toml")

	first := writeBindingConfigStep(cmd, key, "gsa_", "gsa-archives", "")
	if first.result != "created" {
		t.Fatalf("first write result=%q err=%v", first.result, first.err)
	}
	tok1, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}

	// Re-run with a DIFFERENT explicit token — it must be ignored.
	second := writeBindingConfigStep(cmd, key, "gsa_", "gsa-archives", "an-intruding-token")
	if second.err != nil || second.result != "exists" {
		t.Fatalf("second write: result=%q err=%v", second.result, second.err)
	}
	tok2, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(tok1) != string(tok2) {
		t.Fatalf("token clobbered on re-run:\n  before=%s\n  after =%s", tok1, tok2)
	}
	if strings.Contains(string(tok2), "an-intruding-token") {
		t.Fatal("explicit --token overwrote the existing secret")
	}
}

// TestValidateConfigStep_SeesFreshBinding confirms the validation step
// loads the registry and finds a binding written moments earlier.
func TestValidateConfigStep_SeesFreshBinding(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, `
[server]
port = 9998

[storage]
backend = "local"
`)
	cmd := profileTestCmd(t, cfgDir)
	key := pbserver.VaultKey{Profile: "gsa", Vault: "memory"}

	if s := writeBindingConfigStep(cmd, key, "gsa_", "gsa-archives", ""); s.err != nil {
		t.Fatalf("write step failed: %v", s.err)
	}
	if s := validateConfigStep(cmd, key); s.err != nil || s.result != "ok" {
		t.Fatalf("validate: result=%q err=%v", s.result, s.err)
	}
}

// TestReportSteps_ExitsNonZeroOnFailure locks the aggregate-and-report
// contract: any failed step yields a non-nil error (non-zero exit),
// while an all-clean run returns nil.
func TestReportSteps_ExitsNonZeroOnFailure(t *testing.T) {
	buf := &bytes.Buffer{}
	err := reportSteps(buf, []stepStatus{
		{name: "a", result: "ok"},
		{name: "b", result: "failed", err: os.ErrPermission},
	})
	if err == nil {
		t.Fatal("expected non-nil error when a step failed")
	}

	buf.Reset()
	if err := reportSteps(buf, []stepStatus{{name: "a", result: "ok"}, {name: "b", result: "exists"}}); err != nil {
		t.Fatalf("all-clean run should return nil, got %v", err)
	}
	if !strings.Contains(buf.String(), "binding live.") {
		t.Errorf("clean run should print success banner, got:\n%s", buf.String())
	}
}

// TestAnnotatePGConnectErr adds the host/container hint only to
// connection-shaped failures, and leaves other errors untouched.
func TestAnnotatePGConnectErr(t *testing.T) {
	connErr := annotatePGConnectErr(errFromString("failed to connect: dial tcp 127.0.0.1:1: connection refused"))
	if !strings.Contains(connErr.Error(), "localhost:5433") {
		t.Errorf("connect error missing host hint: %v", connErr)
	}
	plain := annotatePGConnectErr(errFromString("migration checksum mismatch"))
	if strings.Contains(plain.Error(), "localhost:5433") {
		t.Errorf("non-connect error should not get the hint: %v", plain)
	}
}

func errFromString(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
