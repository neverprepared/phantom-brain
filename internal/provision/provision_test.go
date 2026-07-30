package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pbserver "github.com/neverprepared/phantom-brain/internal/server"
)

// localServerToml is a minimal server.toml with the local blob backend —
// enough for LoadServerConfig + registry load without MinIO/Postgres.
const localServerToml = `
[server]
port = 9998

[storage]
backend = "local"
`

func seedConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.toml"), []byte(localServerToml), 0o644); err != nil {
		t.Fatalf("write server.toml: %v", err)
	}
	return dir
}

func step(res Result, name string) (StepResult, bool) {
	for _, s := range res.Steps {
		if strings.HasPrefix(s.Name, name) {
			return s, true
		}
	}
	return StepResult{}, false
}

// TestProfileCreate_FreshDerivesNamesWritesTokenValidates covers the happy
// path with no Postgres and the local backend: names derive from the
// profile, a token is generated, config validates, and the effective
// token is returned to the caller.
func TestProfileCreate_FreshDerivesNamesWritesTokenValidates(t *testing.T) {
	cfgDir := seedConfigDir(t)
	res, err := ProfileCreate(context.Background(),
		Deps{ConfigDir: cfgDir}, // BaseDSN empty, MinIO nil → pg + bucket skip
		Spec{Profile: "gsa", Vault: "memory"})
	if err != nil {
		t.Fatalf("precondition error: %v", err)
	}
	if res.Failed() {
		t.Fatalf("unexpected step failure: %+v", res.Steps)
	}
	if res.Bucket != "gsa-archives" || res.IndexPrefix != "gsa_" {
		t.Fatalf("derived names wrong: bucket=%q prefix=%q", res.Bucket, res.IndexPrefix)
	}
	if res.Token == "" || !res.TokenCreated {
		t.Fatalf("expected a freshly created token, got token=%q created=%v", res.Token, res.TokenCreated)
	}
	if pg, _ := step(res, "postgres"); pg.Result != "skipped" {
		t.Errorf("pg step should skip with empty DSN, got %q", pg.Result)
	}
	if b, _ := step(res, "minio"); b.Result != "skipped" {
		t.Errorf("bucket step should skip with nil MinIO, got %q", b.Result)
	}
	if bc, _ := step(res, "binding"); bc.Result != "created" {
		t.Errorf("binding step should be created, got %q", bc.Result)
	}
	if v, _ := step(res, "config validate"); v.Result != "ok" {
		t.Errorf("validate should be ok, got %q (%v)", v.Result, v.Err)
	}

	// Files on disk carry the derived overrides + a token.
	dir := filepath.Join(cfgDir, "profiles", "gsa", "vaults", "memory")
	auth, err := os.ReadFile(filepath.Join(dir, "auth.toml"))
	if err != nil || !strings.Contains(string(auth), res.Token) {
		t.Fatalf("auth.toml missing token %q: %v\n%s", res.Token, err, auth)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	for _, want := range []string{"[storage_overrides]", `index_prefix = "gsa_"`, `bucket = "gsa-archives"`} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("config.toml missing %q in:\n%s", want, cfg)
		}
	}
}

// TestProfileCreate_CallerSuppliedTokenIsUsed is the mechanism behind the
// token-orphan fix: the caller (orchestrator) can supply a token it has
// already persisted, so the durable side effect carries a known value.
func TestProfileCreate_CallerSuppliedTokenIsUsed(t *testing.T) {
	cfgDir := seedConfigDir(t)
	const supplied = "pb_caller_supplied_deadbeef"
	res, err := ProfileCreate(context.Background(),
		Deps{ConfigDir: cfgDir},
		Spec{Profile: "gsa", Vault: "memory", Token: supplied})
	if err != nil || res.Failed() {
		t.Fatalf("create failed: err=%v steps=%+v", err, res.Steps)
	}
	if res.Token != supplied || !res.TokenCreated {
		t.Fatalf("supplied token not used: token=%q created=%v", res.Token, res.TokenCreated)
	}
}

// TestProfileCreate_IdempotentKeepsAndReturnsToken is the twin safety
// contract: a re-run (a) NEVER clobbers the live token even when a
// different one is supplied, and (b) still RETURNS the live token so the
// caller can recover it — the fix for the "mint-once-and-hide" orphan
// hazard.
func TestProfileCreate_IdempotentKeepsAndReturnsToken(t *testing.T) {
	cfgDir := seedConfigDir(t)
	ctx := context.Background()
	spec := Spec{Profile: "gsa", Vault: "memory"}

	first, err := ProfileCreate(ctx, Deps{ConfigDir: cfgDir}, spec)
	if err != nil || first.Failed() {
		t.Fatalf("first create failed: err=%v steps=%+v", err, first.Steps)
	}
	orig := first.Token

	// Re-run with a DIFFERENT explicit token — it must be ignored, and the
	// original token must come back in the Result.
	spec.Token = "an-intruding-token"
	second, err := ProfileCreate(ctx, Deps{ConfigDir: cfgDir}, spec)
	if err != nil || second.Failed() {
		t.Fatalf("second create failed: err=%v steps=%+v", err, second.Steps)
	}
	if bc, _ := step(second, "binding"); bc.Result != "exists" {
		t.Fatalf("re-run binding step should be exists, got %q", bc.Result)
	}
	if second.TokenCreated {
		t.Error("re-run must not report TokenCreated")
	}
	if second.Token != orig {
		t.Fatalf("re-run must return the live token: orig=%q got=%q", orig, second.Token)
	}
	auth, _ := os.ReadFile(filepath.Join(cfgDir, "profiles", "gsa", "vaults", "memory", "auth.toml"))
	if strings.Contains(string(auth), "an-intruding-token") {
		t.Fatal("supplied token overwrote the live secret on re-run")
	}
}

func TestProfileCreate_RejectsBadSegments(t *testing.T) {
	cfgDir := seedConfigDir(t)
	for _, bad := range []Spec{
		{Profile: "a/b", Vault: "memory"},
		{Profile: "gsa", Vault: ".."},
		{Profile: "", Vault: "memory"},
		{Profile: "gsa", Vault: "memory", Token: "has space"},
	} {
		if _, err := ProfileCreate(context.Background(), Deps{ConfigDir: cfgDir}, bad); err == nil {
			t.Errorf("expected precondition error for %+v", bad)
		}
	}
}

// TestProfileCreate_VaultKeyPreserved is a small guard that Result echoes
// the key the caller asked for (the orchestrator records it).
func TestProfileCreate_VaultKeyPreserved(t *testing.T) {
	cfgDir := seedConfigDir(t)
	res, err := ProfileCreate(context.Background(), Deps{ConfigDir: cfgDir}, Spec{Profile: "work", Vault: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Key != (pbserver.VaultKey{Profile: "work", Vault: "memory"}) {
		t.Fatalf("key mismatch: %+v", res.Key)
	}
}
