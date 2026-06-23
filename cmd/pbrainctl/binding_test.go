package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestBindingCreate_WritesFilesAndPerms covers the v3.3 happy path:
// new binding under a fresh config dir gets auth.toml (mode 0o600),
// config.toml (mode 0o644) with overrides, and a 64-char hex token
// printed exactly once.
func TestBindingCreate_WritesFilesAndPerms(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, `
[server]
port = 9998

[storage]
backend = "local"
`)
	cmd := bindingCreateCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	_ = cmd.Flags().Set("config-dir", cfgDir)
	_ = cmd.Flags().Set("data-dir", t.TempDir())
	_ = cmd.Flags().Set("index-prefix", "client_x_")
	_ = cmd.Flags().Set("bucket", "pb-client-x")
	cmd.SetArgs([]string{"client_x/main"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\noutput: %s", err, buf.String())
	}

	bindingDir := filepath.Join(cfgDir, "profiles", "client_x", "vaults", "main")
	if info, err := os.Stat(bindingDir); err != nil {
		t.Fatalf("binding dir missing: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("binding dir perms = %o, want 0700", info.Mode().Perm())
	}

	authPath := filepath.Join(bindingDir, "auth.toml")
	authStat, err := os.Stat(authPath)
	if err != nil {
		t.Fatalf("auth.toml missing: %v", err)
	}
	if runtime.GOOS != "windows" && authStat.Mode().Perm() != 0o600 {
		t.Fatalf("auth.toml perms = %o, want 0600", authStat.Mode().Perm())
	}
	authBody, _ := os.ReadFile(authPath)
	if !strings.Contains(string(authBody), "bearer_token = \"") {
		t.Fatalf("auth.toml missing bearer_token line: %s", authBody)
	}

	cfgPath := filepath.Join(bindingDir, "config.toml")
	cfgStat, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("config.toml missing: %v", err)
	}
	if runtime.GOOS != "windows" && cfgStat.Mode().Perm() != 0o644 {
		t.Fatalf("config.toml perms = %o, want 0644", cfgStat.Mode().Perm())
	}
	cfgBody, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(cfgBody), `index_prefix = "client_x_"`) ||
		!strings.Contains(string(cfgBody), `bucket = "pb-client-x"`) {
		t.Fatalf("config.toml missing overrides: %s", cfgBody)
	}

	// Token must appear once + be 64 chars hex.
	tokenLineCount := strings.Count(buf.String(), "token  :")
	if tokenLineCount != 1 {
		t.Fatalf("expected token printed once, got %d times\noutput: %s", tokenLineCount, buf.String())
	}
	// Extract token from output to assert shape.
	out := buf.String()
	idx := strings.Index(out, "token  : ")
	if idx < 0 {
		t.Fatalf("no token line in output: %s", out)
	}
	rest := out[idx+len("token  : "):]
	tok := strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0])
	if len(tok) != 64 {
		t.Fatalf("token len = %d, want 64; tok=%q", len(tok), tok)
	}
	for _, r := range tok {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("token not lowercase hex: %q", tok)
		}
	}
}

// TestBindingCreate_RefusesExisting catches re-creation against an
// already-populated config dir. Operator must explicitly delete first.
func TestBindingCreate_RefusesExisting(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, "[storage]\nbackend = \"local\"\n")
	bindingDir := filepath.Join(cfgDir, "profiles", "p", "vaults", "v")
	if err := os.MkdirAll(bindingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := bindingCreateCmd()
	_ = cmd.Flags().Set("config-dir", cfgDir)
	cmd.SetArgs([]string{"p/v"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected refusal; got %v", err)
	}
}

// TestBindingCreate_CreateBucketRequiresBucket guards the flag pair.
func TestBindingCreate_CreateBucketRequiresBucket(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, "[storage]\nbackend = \"minio\"\n")
	cmd := bindingCreateCmd()
	_ = cmd.Flags().Set("config-dir", cfgDir)
	_ = cmd.Flags().Set("create-bucket", "true")
	cmd.SetArgs([]string{"p/v"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--bucket") {
		t.Fatalf("expected --bucket required; got %v", err)
	}
}

// TestBindingCreate_BadIndexPrefix bounces invalid characters through
// the shared registry validator.
func TestBindingCreate_BadIndexPrefix(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, "[storage]\nbackend = \"local\"\n")
	cmd := bindingCreateCmd()
	_ = cmd.Flags().Set("config-dir", cfgDir)
	_ = cmd.Flags().Set("index-prefix", "Client-X!") // uppercase + dash + bang
	cmd.SetArgs([]string{"p/v"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "index_prefix") {
		t.Fatalf("expected prefix validation error; got %v", err)
	}
}

// TestBindingList_OutputShape covers both <shared>/<default> placeholders
// and the overridden case using two bindings in the same config tree.
func TestBindingList_OutputShape(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, `
[server]
port = 9998

[storage]
backend = "local"
minio_bucket = "pb-default"

[opensearch]
addresses = ["http://os:9200"]
index_prefix = "pb_global_"
`)
	// Shared binding (no overrides).
	must(t, os.MkdirAll(filepath.Join(cfgDir, "profiles", "p1", "vaults", "v1"), 0o700))
	must(t, os.WriteFile(filepath.Join(cfgDir, "profiles", "p1", "vaults", "v1", "auth.toml"),
		[]byte(`bearer_token = "tok1"`+"\n"), 0o600))

	// Overridden binding.
	must(t, os.MkdirAll(filepath.Join(cfgDir, "profiles", "p2", "vaults", "v2"), 0o700))
	must(t, os.WriteFile(filepath.Join(cfgDir, "profiles", "p2", "vaults", "v2", "auth.toml"),
		[]byte(`bearer_token = "tok2"`+"\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(cfgDir, "profiles", "p2", "vaults", "v2", "config.toml"),
		[]byte("[storage_overrides]\nindex_prefix = \"v2_\"\nbucket = \"pb-v2\"\n"), 0o644))

	cmd := bindingListCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	_ = cmd.Flags().Set("config-dir", cfgDir)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nout: %s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "PROFILE/VAULT") {
		t.Fatalf("header missing: %s", out)
	}
	if !strings.Contains(out, "p1/v1\t<shared>\t<default>") {
		t.Fatalf("shared row missing: %s", out)
	}
	if !strings.Contains(out, "p2/v2\tpb_global_v2_\tpb-v2") {
		t.Fatalf("override row missing or wrong: %s", out)
	}
}

// TestBindingDelete_DryRunVsConfirm verifies the default is dry-run and
// --confirm actually removes the config dir.
func TestBindingDelete_DryRunVsConfirm(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, `
[storage]
backend = "local"
`)
	bindingDir := filepath.Join(cfgDir, "profiles", "p", "vaults", "v")
	must(t, os.MkdirAll(bindingDir, 0o700))
	must(t, os.WriteFile(filepath.Join(bindingDir, "auth.toml"),
		[]byte(`bearer_token = "tok"`+"\n"), 0o600))

	// Dry-run: dir must survive.
	cmd := bindingDeleteCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	_ = cmd.Flags().Set("config-dir", cfgDir)
	cmd.SetArgs([]string{"p/v"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("dry-run: %v\n%s", err, buf.String())
	}
	if _, err := os.Stat(bindingDir); err != nil {
		t.Fatalf("dry-run removed binding dir: %v", err)
	}
	if !strings.Contains(buf.String(), "DRY-RUN") {
		t.Fatalf("dry-run output missing marker: %s", buf.String())
	}

	// --confirm: dir removed.
	cmd = bindingDeleteCmd()
	buf.Reset()
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	_ = cmd.Flags().Set("config-dir", cfgDir)
	_ = cmd.Flags().Set("confirm", "true")
	cmd.SetArgs([]string{"p/v"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("confirm: %v\n%s", err, buf.String())
	}
	if _, err := os.Stat(bindingDir); !os.IsNotExist(err) {
		t.Fatalf("binding dir survived --confirm: stat err=%v", err)
	}
}

// TestBindingDelete_PurgeRefusesSharedWithoutAllow guards the
// foot-gun: --purge-data on a binding with no [storage_overrides] is
// always refused, no matter the other flags.
func TestBindingDelete_PurgeRefusesSharedWithoutAllow(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, `
[storage]
backend = "local"
minio_bucket = "pb-default"
`)
	bindingDir := filepath.Join(cfgDir, "profiles", "p", "vaults", "v")
	must(t, os.MkdirAll(bindingDir, 0o700))
	must(t, os.WriteFile(filepath.Join(bindingDir, "auth.toml"),
		[]byte(`bearer_token = "tok"`+"\n"), 0o600))

	cmd := bindingDeleteCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	_ = cmd.Flags().Set("config-dir", cfgDir)
	_ = cmd.Flags().Set("confirm", "true")
	_ = cmd.Flags().Set("purge-data", "true")
	cmd.SetArgs([]string{"p/v"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no [storage_overrides]") {
		t.Fatalf("expected refusal on shared binding; got %v\n%s", err, buf.String())
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%v", err)
	}
}
