// Package integration_test holds end-to-end tests that import both
// internal/brain and internal/server. Lives at the top level so the
// dependency direction stays clean (internal/server imports
// internal/brain; this package imports both and is imported by no
// production code).
package integration_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neverprepared/mcp-phantom-brain/internal/brain"
	"github.com/neverprepared/mcp-phantom-brain/internal/config"
	"github.com/neverprepared/mcp-phantom-brain/internal/server"
)

// writeServerToml drops a minimal server.toml into dir.
func writeServerToml(t *testing.T, dir string) {
	t.Helper()
	body := "[server]\nport = 0\n"
	if err := os.WriteFile(filepath.Join(dir, "server.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedVaultAuth drops a profiles/<p>/vaults/<v>/auth.toml so the
// daemon's registry picks up the vault. Returns the bearer token.
func seedVaultAuth(t *testing.T, configDir, profile, vault string) string {
	t.Helper()
	base := filepath.Join(configDir, "profiles", profile, "vaults", vault)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	tok := "pb_" + profile + "_" + vault + "_tok_e2e"
	if err := os.WriteFile(filepath.Join(base, "auth.toml"),
		[]byte("bearer_token = \""+tok+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return tok
}

// writeFakeClaude injects a stub `claude` binary on PATH so the
// daemon's gate + summarize calls return a deterministic JSON
// verdict without needing the real Claude Code CLI on the test host.
func writeFakeClaude(t *testing.T, gateJSON string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\nprintf %s '" + strings.ReplaceAll(gateJSON, "'", "'\\''") + "'\n"
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// TestAgentDaemonRoundTrip is the Phase 2.5 capstone: a real daemon
// boots, an agent's Lifecycle.Start births from it (downloads the
// snapshot when one exists, greenfield otherwise), the agent's
// shipqueue uploads a death payload, the daemon's reaper + synthesizer
// process it, and a subsequent snapshot fetch surfaces the new
// content.
func TestAgentDaemonRoundTrip(t *testing.T) {
	writeFakeClaude(t, `{"reliability":"high","topic":"tools","reason":"primary docs"}`)

	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	writeServerToml(t, cfgDir)
	tok := seedVaultAuth(t, cfgDir, "personal", "memory")

	// --- Daemon ---
	d, err := server.Start(server.StartOpts{
		ConfigDir: cfgDir,
		DataDir:   server.DataDir(dataDir),
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("daemon Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	ts := httptest.NewServer(d.Router())
	t.Cleanup(ts.Close)
	// Rewire local backend baseURL so /merge/init issues URLs that
	// route back to this httptest server (rather than the daemon's
	// nominal :0 listener which isn't actually listening).
	if lb, ok := internalStorageOf(d); ok {
		lb.OverrideBaseURL(ts.URL)
	}

	// --- Agent #1 — births greenfield (daemon has no snapshot yet),
	// ingests a raw file, dies.
	agentDataDir := t.TempDir()
	agent := buildAgentConfig(t, ts.URL, tok, agentDataDir)

	lc1, err := brain.Start(brain.StartOpts{
		Agent:    agent,
		Platform: brain.NewPlatform(),
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("agent1 Start: %v", err)
	}
	// Seed a Raw file into the brain's vault as if brain_perceive had
	// landed it.
	rawDir := filepath.Join(lc1.VaultDir(), "Raw", "curated")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rawBody := "# end-to-end\n\nThe **Roundtrip Project** verifies the agent-daemon loop closes."
	if err := os.WriteFile(filepath.Join(rawDir, "roundtrip.md"), []byte(rawBody), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := lc1.Shutdown(context.Background()); err != nil {
		t.Fatalf("agent1 Shutdown: %v", err)
	}

	// --- Ship + reap + synth ---
	res, err := brain.UploadShipQueue(context.Background(), agent, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("UploadShipQueue: %v", err)
	}
	if len(res.Shipped) != 1 {
		t.Fatalf("expected 1 shipped, got %+v", res)
	}

	binding, _ := registryLookup(d, "personal", "memory")
	if _, err := server.ReapOnce(server.DataDir(dataDir), binding, slog.New(slog.DiscardHandler), &fakeMutex{}); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := server.SynthesizeOne(ctx, server.DataDir(dataDir), binding, slog.New(slog.DiscardHandler), &fakeMutex{}); err != nil {
		t.Fatalf("SynthesizeOne: %v", err)
	}

	// Build a snapshot containing the synthesizer's output.
	if _, err := server.BuildSnapshot(server.DataDir(dataDir), "personal", "memory", 30); err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}

	// --- Agent #2 — should birth from a real snapshot now and see
	// the Wiki page Agent #1's death produced.
	agent2DataDir := t.TempDir()
	agent2 := buildAgentConfig(t, ts.URL, tok, agent2DataDir)

	lc2, err := brain.Start(brain.StartOpts{
		Agent:    agent2,
		Platform: brain.NewPlatform(),
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("agent2 Start: %v", err)
	}
	t.Cleanup(func() {
		_, _ = lc2.Shutdown(context.Background())
	})

	manifest := lc2.Snapshot()
	if manifest.SeedSource != brain.SeedTarball {
		t.Errorf("agent2 seed_source = %q, want tarball", manifest.SeedSource)
	}
	if manifest.ParentGen == nil || *manifest.ParentGen == 0 {
		t.Errorf("agent2 parent_gen = %v, want >0", manifest.ParentGen)
	}

	// The Wiki/summaries dir in agent2's brain should contain the
	// summary the daemon synthesizer wrote.
	wikiDir := filepath.Join(lc2.VaultDir(), "Wiki", "summaries")
	entries, err := os.ReadDir(wikiDir)
	if err != nil {
		t.Fatalf("read agent2 wiki: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("agent2 birthed from snapshot but Wiki is empty — synth output didn't reach the next brain")
	}
}

// buildAgentConfig produces a config.Agent for use against the test
// daemon. Uses t.Setenv so XDG_DATA_HOME and friends apply to this
// process for the duration of the test.
func buildAgentConfig(t *testing.T, apiURL, tok, dataDir string) *config.Agent {
	t.Helper()
	t.Setenv("CL_BRAIN_API", apiURL)
	t.Setenv("CL_BRAIN_API_TOKEN", tok)
	t.Setenv("CL_WORKSPACE_PROFILE", "personal")
	t.Setenv("CL_BRAIN_VAULT", "memory")
	t.Setenv("XDG_DATA_HOME", dataDir)
	// Clear CL_BRAIN_ID so each agent allocates a fresh uuid.
	t.Setenv("CL_BRAIN_ID", "")
	agent, err := config.LoadAgent()
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	return agent
}

// --- test helpers — keep glue out of the main test body --------------

// fakeMutex satisfies the {Lock(); Unlock()} interface that
// server.ReapOnce + server.SynthesizeOne accept for cross-runner
// ordering. The real daemon shares the runner's mutex; tests outside
// the runner loop pass this stand-in.
type fakeMutex struct{}

func (fakeMutex) Lock()   {}
func (fakeMutex) Unlock() {}

// internalStorageOf reaches into the daemon for its LocalBackend so
// tests can override the baseURL after httptest.NewServer assigns
// the real port. The daemon's listener is on :0 (not actually
// serving in this test), so without the override /merge/init would
// hand out unreachable URLs.
func internalStorageOf(d *server.Daemon) (*server.LocalBackend, bool) {
	return d.LocalBackendForTest()
}

// registryLookup pulls a vault binding out of the daemon. Test-only
// helper since the registry isn't exported.
func registryLookup(d *server.Daemon, profile, vault string) (server.VaultBinding, bool) {
	return d.LookupBindingForTest(server.VaultKey{Profile: profile, Vault: vault})
}
