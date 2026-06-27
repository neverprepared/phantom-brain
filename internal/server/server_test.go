package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- paths ------------------------------------------------------------

func TestPaths_ComposeCorrectly(t *testing.T) {
	d := DataDir("/var/lib/phantom-brain")
	if d.CollectiveDir("personal", "memory") != "/var/lib/phantom-brain/personal/memory/collective" {
		t.Errorf("CollectiveDir = %q", d.CollectiveDir("personal", "memory"))
	}
	if d.GlobalFlockPath() != "/var/lib/phantom-brain/_daemon/locks/brain-server.pid" {
		t.Errorf("GlobalFlockPath = %q", d.GlobalFlockPath())
	}
}

func TestEnsureCollectiveSkeleton_Idempotent(t *testing.T) {
	d := DataDir(t.TempDir())
	if err := EnsureCollectiveSkeleton(d, "personal", "memory"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := EnsureCollectiveSkeleton(d, "personal", "memory"); err != nil {
		t.Fatalf("second: %v", err)
	}
	for _, sub := range []string{
		"personal/memory/collective/vault/Wiki/summaries",
		"personal/memory/collective/vault/Raw/curated",
		"personal/memory/collective/_index",
		"personal/memory/collective/brains/_pending",
		"personal/memory/collective/ledger",
	} {
		st, err := os.Stat(filepath.Join(string(d), sub))
		if err != nil || !st.IsDir() {
			t.Errorf("expected dir %s, err=%v", sub, err)
		}
	}
}

func TestEnsureCollectiveSkeleton_RejectsEmpty(t *testing.T) {
	if err := EnsureCollectiveSkeleton(DataDir(t.TempDir()), "", "memory"); err == nil {
		t.Error("expected error on empty profile")
	}
}

// --- config -----------------------------------------------------------

// writeServerConfig drops a server.toml into dir with the given body.
func writeServerConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "server.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadServerConfig_AppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	writeServerConfig(t, dir, `[server]
port = 0
`)
	cfg, err := LoadServerConfig(dir)
	if err != nil {
		t.Fatalf("LoadServerConfig: %v", err)
	}
	if cfg.Server.Port != 9998 {
		t.Errorf("port default not applied: %d", cfg.Server.Port)
	}
	if cfg.Storage.Backend != "local" {
		t.Errorf("storage backend default = %q", cfg.Storage.Backend)
	}
	if cfg.Defaults.ReaperPollIntervalSecs != 5 {
		t.Errorf("reaper poll default = %d", cfg.Defaults.ReaperPollIntervalSecs)
	}
}

func TestLoadServerConfig_HonorsExplicitValues(t *testing.T) {
	dir := t.TempDir()
	writeServerConfig(t, dir, `[server]
port = 12345
host = "127.0.0.1"

[defaults]
reaper_poll_interval_secs = 60
max_tarball_bytes = 123456
`)
	cfg, err := LoadServerConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 12345 || cfg.Server.Host != "127.0.0.1" {
		t.Errorf("explicit server values dropped: %+v", cfg.Server)
	}
	if cfg.Defaults.ReaperPollIntervalSecs != 60 || cfg.Defaults.MaxTarballBytes != 123456 {
		t.Errorf("explicit defaults dropped: %+v", cfg.Defaults)
	}
}

func TestMergedDefaults_OverrideZeroLeavesGlobal(t *testing.T) {
	// Phase D2b: retention_gens was removed; exercise the merge with two
	// surviving knobs — one overridden, one left zero (keeps the global).
	g := VaultDefaults{ReaperPollIntervalSecs: 5, MaxTarballBytes: 99}
	o := VaultOverrides{ReaperPollIntervalSecs: 50} // MaxTarballBytes stays zero
	out := MergedDefaults(g, o)
	if out.ReaperPollIntervalSecs != 50 {
		t.Errorf("override not applied: %d", out.ReaperPollIntervalSecs)
	}
	if out.MaxTarballBytes != 99 {
		t.Errorf("zero override clobbered global: %d", out.MaxTarballBytes)
	}
}

// --- registry ---------------------------------------------------------

// seedVault writes minimal config.toml + auth.toml for a vault.
// Returns the bearer token written.
func seedVault(t *testing.T, configDir, profile, vault, overrides string) string {
	t.Helper()
	base := filepath.Join(configDir, "profiles", profile, "vaults", vault)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if overrides != "" {
		if err := os.WriteFile(filepath.Join(base, "config.toml"), []byte(overrides), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	token := "pb_" + profile + "_" + vault + "_tok"
	if err := os.WriteFile(filepath.Join(base, "auth.toml"),
		[]byte("bearer_token = \""+token+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestRegistry_LoadAndLookup(t *testing.T) {
	dir := t.TempDir()
	tokA := seedVault(t, dir, "personal", "memory", "")
	tokB := seedVault(t, dir, "work", "core", "reaper_poll_interval_secs = 100\n")

	r := NewRegistry()
	n, err := r.Load(LoadOpts{ConfigDir: dir, Defaults: VaultDefaults{ReaperPollIntervalSecs: 5}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n != 2 {
		t.Fatalf("loaded %d, want 2", n)
	}
	a, ok := r.LookupByToken(tokA)
	if !ok || a.Key.Profile != "personal" || a.Key.Vault != "memory" {
		t.Errorf("token A lookup failed: %+v", a)
	}
	b, ok := r.LookupByToken(tokB)
	if !ok || b.Defaults.ReaperPollIntervalSecs != 100 {
		t.Errorf("token B overrides not applied: %+v", b.Defaults)
	}
}

func TestRegistry_StorageOverrides_FallbackToDefaults(t *testing.T) {
	dir := t.TempDir()
	_ = seedVault(t, dir, "personal", "memory", "") // no [storage_overrides]
	r := NewRegistry()
	if _, err := r.Load(LoadOpts{
		ConfigDir:          dir,
		DefaultIndexPrefix: "pb_",
		DefaultBucket:      "default-bucket",
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, ok := r.LookupByVault(VaultKey{Profile: "personal", Vault: "memory"})
	if !ok {
		t.Fatal("binding missing")
	}
	if b.Storage.IndexPrefix != "pb_" {
		t.Errorf("IndexPrefix = %q, want %q", b.Storage.IndexPrefix, "pb_")
	}
	if b.Storage.Bucket != "default-bucket" {
		t.Errorf("Bucket = %q, want %q", b.Storage.Bucket, "default-bucket")
	}
}

func TestRegistry_StorageOverrides_Applied(t *testing.T) {
	dir := t.TempDir()
	_ = seedVault(t, dir, "client", "x", `
[storage_overrides]
index_prefix = "client_x_"
bucket = "client-x-bucket"
`)
	r := NewRegistry()
	if _, err := r.Load(LoadOpts{
		ConfigDir:          dir,
		DefaultIndexPrefix: "pb_",
		DefaultBucket:      "default-bucket",
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, ok := r.LookupByVault(VaultKey{Profile: "client", Vault: "x"})
	if !ok {
		t.Fatal("binding missing")
	}
	// Global prefix stays first, override appended.
	if b.Storage.IndexPrefix != "pb_client_x_" {
		t.Errorf("IndexPrefix = %q, want %q", b.Storage.IndexPrefix, "pb_client_x_")
	}
	if b.Storage.Bucket != "client-x-bucket" {
		t.Errorf("Bucket = %q, want %q", b.Storage.Bucket, "client-x-bucket")
	}
}

func TestRegistry_StorageOverrides_PrefixOnly(t *testing.T) {
	dir := t.TempDir()
	_ = seedVault(t, dir, "c", "v", `
[storage_overrides]
index_prefix = "scoped_"
`)
	r := NewRegistry()
	if _, err := r.Load(LoadOpts{
		ConfigDir:          dir,
		DefaultIndexPrefix: "",
		DefaultBucket:      "shared-bucket",
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, _ := r.LookupByVault(VaultKey{Profile: "c", Vault: "v"})
	if b.Storage.IndexPrefix != "scoped_" {
		t.Errorf("IndexPrefix = %q", b.Storage.IndexPrefix)
	}
	if b.Storage.Bucket != "shared-bucket" {
		t.Errorf("Bucket = %q, want fallback", b.Storage.Bucket)
	}
}

func TestRegistry_StorageOverrides_InvalidPrefixRejected(t *testing.T) {
	cases := []string{
		"Bad_Caps",
		"has space",
		"dash-not-ok",
		"slash/here",
		"dot.here",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			dir := t.TempDir()
			_ = seedVault(t, dir, "c", "v", "[storage_overrides]\nindex_prefix = \""+p+"\"\n")
			r := NewRegistry()
			_, err := r.Load(LoadOpts{ConfigDir: dir})
			if err == nil || !strings.Contains(err.Error(), "index_prefix") {
				t.Fatalf("expected index_prefix validation error, got %v", err)
			}
		})
	}
}

func TestRegistry_DuplicateTokenRejected(t *testing.T) {
	dir := t.TempDir()
	// Two vaults with the same bearer_token — hand-craft so we
	// guarantee the collision (seedVault always picks unique tokens).
	for _, p := range []string{"a", "b"} {
		base := filepath.Join(dir, "profiles", p, "vaults", "v")
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "auth.toml"),
			[]byte(`bearer_token = "dup-token"`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := NewRegistry()
	if _, err := r.Load(LoadOpts{ConfigDir: dir, Defaults: VaultDefaults{}}); err == nil ||
		!strings.Contains(err.Error(), "duplicate bearer_token") {
		t.Fatalf("expected duplicate-token error, got %v", err)
	}
}

func TestRegistry_EmptyConfigDirIsValid(t *testing.T) {
	r := NewRegistry()
	n, err := r.Load(LoadOpts{ConfigDir: t.TempDir(), Defaults: VaultDefaults{}})
	if err != nil {
		t.Fatalf("expected ok on empty config, got %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 vaults, got %d", n)
	}
}

func TestRegistry_Diff(t *testing.T) {
	dir := t.TempDir()
	_ = seedVault(t, dir, "personal", "memory", "")
	r := NewRegistry()
	if _, err := r.Load(LoadOpts{ConfigDir: dir}); err != nil {
		t.Fatal(err)
	}
	added, removed := r.Diff([]VaultKey{{Profile: "old", Vault: "gone"}})
	if len(added) != 1 || added[0].Vault != "memory" {
		t.Errorf("added = %v", added)
	}
	if len(removed) != 1 || removed[0].Profile != "old" {
		t.Errorf("removed = %v", removed)
	}
}

// --- auth -------------------------------------------------------------

func TestBearerFromHeader(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want string
	}{
		{"Bearer abc", true, "abc"},
		{"bearer xyz", true, "xyz"},
		{"  Bearer   tok ", true, "tok"},
		{"Bearer", false, ""},
		{"", false, ""},
		{"Basic abc", false, ""},
	}
	for _, c := range cases {
		got, ok := bearerFromHeader(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("bearerFromHeader(%q) = (%q,%v); want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestAuthMiddleware_RejectsMissing(t *testing.T) {
	r := NewRegistry()
	mw := AuthMiddleware(r)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("inner handler should not have run")
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("not JSON: %v / %q", err, rec.Body.String())
	}
	if env.Error.Code != ErrCodeInvalidToken {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestAuthMiddleware_AcceptsValidAndPlumbsBinding(t *testing.T) {
	dir := t.TempDir()
	tok := seedVault(t, dir, "personal", "memory", "")
	r := NewRegistry()
	if _, err := r.Load(LoadOpts{ConfigDir: dir}); err != nil {
		t.Fatal(err)
	}
	mw := AuthMiddleware(r)
	var gotBinding VaultBinding
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotBinding, _ = BindingFromContext(req.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotBinding.Key.Profile != "personal" || gotBinding.Key.Vault != "memory" {
		t.Errorf("binding not plumbed: %+v", gotBinding)
	}
}

// --- Daemon end-to-end ------------------------------------------------

// Phase D1: Start() now mandates a live Postgres SoR. The /health endpoint
// only reads d.registry.Vaults() (no PG), so it's restored here against
// the no-Start router rig (newRouterRig in handlers_test.go) instead of
// being dropped to integration.
//
// TestDaemon_StartRefusesSecondInstance genuinely needs a first Start() to
// succeed (to take the lockfile) before the second is rejected — and a
// successful Start() now requires PG — so it stays out of the unit suite
// and needs PG-backed integration coverage (follow-up).
//
// TestDaemon_MissingConfigErrorsHelpfully stays: it asserts Start fails on
// a MISSING server.toml, which errors during config load — before the
// Postgres gate — so it still passes without PG.

func TestDaemon_HealthEndpointListsVaults(t *testing.T) {
	r := newRouterRig(t)
	resp := r.do(t, http.MethodGet, "/api/brain/health", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"profile": "personal"`) ||
		!strings.Contains(string(body), `"vault": "memory"`) {
		t.Errorf("health body missing vault: %s", body)
	}
}

func TestDaemon_MissingConfigErrorsHelpfully(t *testing.T) {
	_, err := Start(StartOpts{ConfigDir: t.TempDir(), DataDir: DataDir(t.TempDir()), Logger: slog.New(slog.DiscardHandler)})
	if err == nil || !strings.Contains(err.Error(), "server.toml") {
		t.Fatalf("expected server.toml error, got %v", err)
	}
}
