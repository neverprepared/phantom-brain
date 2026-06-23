package server

import (
	"context"
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
		"personal/memory/collective/_published/staged",
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
	if cfg.Defaults.RetentionGens != 30 {
		t.Errorf("retention default = %d", cfg.Defaults.RetentionGens)
	}
}

func TestLoadServerConfig_HonorsExplicitValues(t *testing.T) {
	dir := t.TempDir()
	writeServerConfig(t, dir, `[server]
port = 12345
host = "127.0.0.1"

[defaults]
retention_gens = 50
reaper_poll_interval_secs = 60
`)
	cfg, err := LoadServerConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 12345 || cfg.Server.Host != "127.0.0.1" {
		t.Errorf("explicit server values dropped: %+v", cfg.Server)
	}
	if cfg.Defaults.RetentionGens != 50 || cfg.Defaults.ReaperPollIntervalSecs != 60 {
		t.Errorf("explicit defaults dropped: %+v", cfg.Defaults)
	}
}

func TestMergedDefaults_OverrideZeroLeavesGlobal(t *testing.T) {
	g := VaultDefaults{RetentionGens: 30, ReaperPollIntervalSecs: 5}
	o := VaultOverrides{RetentionGens: 50} // ReaperPollIntervalSecs stays zero
	out := MergedDefaults(g, o)
	if out.RetentionGens != 50 {
		t.Errorf("override not applied: %d", out.RetentionGens)
	}
	if out.ReaperPollIntervalSecs != 5 {
		t.Errorf("zero override clobbered global: %d", out.ReaperPollIntervalSecs)
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
	tokB := seedVault(t, dir, "work", "core", "retention_gens = 100\n")

	r := NewRegistry()
	n, err := r.Load(LoadOpts{ConfigDir: dir, Defaults: VaultDefaults{RetentionGens: 30}})
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
	if !ok || b.Defaults.RetentionGens != 100 {
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

func startTestDaemon(t *testing.T) (*Daemon, func()) {
	t.Helper()
	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	writeServerConfig(t, cfgDir, `[server]
port = 0
host = "127.0.0.1"
`)
	_ = seedVault(t, cfgDir, "personal", "memory", "")
	d, err := Start(StartOpts{
		ConfigDir: cfgDir,
		DataDir:   DataDir(dataDir),
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cleanup := func() {
		_ = d.Shutdown(context.Background())
	}
	return d, cleanup
}

func TestDaemon_HealthEndpointListsVaults(t *testing.T) {
	d, cleanup := startTestDaemon(t)
	defer cleanup()

	ts := httptest.NewServer(d.Router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/brain/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
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

func TestDaemon_StartRefusesSecondInstance(t *testing.T) {
	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	writeServerConfig(t, cfgDir, "[server]\nport = 0\n")

	first, err := Start(StartOpts{ConfigDir: cfgDir, DataDir: DataDir(dataDir), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	t.Cleanup(func() { _ = first.Shutdown(context.Background()) })

	_, err = Start(StartOpts{ConfigDir: cfgDir, DataDir: DataDir(dataDir), Logger: slog.New(slog.DiscardHandler)})
	if err == nil || !strings.Contains(err.Error(), "another pbrainctl serve") {
		t.Fatalf("expected second-instance rejection, got %v", err)
	}
}

func TestDaemon_MissingConfigErrorsHelpfully(t *testing.T) {
	_, err := Start(StartOpts{ConfigDir: t.TempDir(), DataDir: DataDir(t.TempDir()), Logger: slog.New(slog.DiscardHandler)})
	if err == nil || !strings.Contains(err.Error(), "server.toml") {
		t.Fatalf("expected server.toml error, got %v", err)
	}
}
