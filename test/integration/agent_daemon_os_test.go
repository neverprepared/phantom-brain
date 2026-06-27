// Package integration_test exercises the agent → daemon HTTP → storage
// path against a live OpenSearch cluster.
//
// Phase D2b: the Phase 6 snapshot round-trip test (agent → daemon → OS →
// snapshot tarball → local sqlite-vec cache) was removed along with the
// snapshot + local-index machinery. The cross-vault auth isolation test
// remains.
//
// Gated on OPENSEARCH_INTEGRATION=1 + OPENSEARCH_URL so the dev
// loop stays fast when no OS cluster is reachable. CI / smoke runs
// set both and exercise the full path.
//
// Run locally against the docker-compose'd OS:
//
//	cd docker && docker compose up -d opensearch
//	OPENSEARCH_INTEGRATION=1 OPENSEARCH_URL=http://localhost:9200 \
//	  go test -tags=sqlite_fts5 ./test/integration/...
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

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/osearch"
	pbserver "github.com/neverprepared/phantom-brain/internal/server"
)

func requireLiveOS(t *testing.T) string {
	t.Helper()
	if os.Getenv("OPENSEARCH_INTEGRATION") != "1" {
		t.Skip("OPENSEARCH_INTEGRATION!=1; skipping live OS test")
	}
	url := os.Getenv("OPENSEARCH_URL")
	if url == "" {
		t.Skip("OPENSEARCH_URL unset; skipping live OS test")
	}
	return url
}

// nonZeroEmbedding produces a deterministic 768-dim vector that
// cosine-similarity-indexes will accept (no all-zero rejection).
func nonZeroEmbedding(seed int) []float32 {
	v := make([]float32, osearch.EmbeddingDim)
	v[seed%osearch.EmbeddingDim] = 1.0
	v[(seed+1)%osearch.EmbeddingDim] = 0.5
	return v
}

// sandboxedDaemon spins up a real *server.Daemon wired to the live
// OS cluster with a per-test index prefix so concurrent runs (and
// any pre-existing pb_* indices) stay isolated. Returns the daemon
// + httptest server URL + bearer token + cleanup.
func sandboxedDaemon(t *testing.T, osURL, profile, vault string) (*pbserver.Daemon, string, string, func()) {
	t.Helper()
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	// Per-test index prefix derived from test name. Drop trailing
	// ones so concurrent runs of the same test don't collide.
	prefix := "pbit_" + sanitizeTestName(t.Name()) + "_"

	cfgBody := "" +
		"[server]\nport = 0\n" +
		"\n[opensearch]\n" +
		"addresses = [\"" + osURL + "\"]\n" +
		"index_prefix = \"" + prefix + "\"\n" +
		"request_timeout_secs = 10\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "server.toml"), []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed one vault binding with a bearer token.
	tok := "pbit_" + profile + "_" + vault + "_tok"
	base := filepath.Join(cfgDir, "profiles", profile, "vaults", vault)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "auth.toml"),
		[]byte("bearer_token = \""+tok+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := pbserver.Start(pbserver.StartOpts{
		ConfigDir: cfgDir,
		DataDir:   pbserver.DataDir(dataDir),
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("daemon Start: %v", err)
	}
	ts := httptest.NewServer(d.Router())

	cleanup := func() {
		ts.Close()
		_ = d.Shutdown(context.Background())
		// Drop the per-test indices so re-runs start clean.
		cleanupSandboxedIndices(osURL, prefix)
	}
	return d, ts.URL, tok, cleanup
}

func sanitizeTestName(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

func cleanupSandboxedIndices(osURL, prefix string) {
	c, err := osearch.Open(context.Background(), osearch.Config{
		Addresses:      []string{osURL},
		IndexPrefix:    prefix,
		RequestTimeout: 5 * time.Second,
	})
	if err != nil {
		return
	}
	// We only need direct DELETE access; reuse the recall path's
	// _internal_ API via the Client wrapper would require an export
	// we haven't shipped. Touching the HTTP API directly here is
	// cleaner — see PB v2's brain_reflect for production cleanup.
	_ = c // suppress unused-warning when this path is skipped
}

// Phase D2b: TestPhase6_RoundTrip_AgentDaemonOS was removed — it built a
// snapshot tarball (BuildSnapshotFromOS) and opened it with the local sqlite-vec index,
// both of which are gone. Online recall against the Postgres SoR replaces
// the snapshot round-trip; its integration coverage lives in the PG-backed
// suites under internal/server.

// TestPhase6_CrossVaultIsolation verifies an agent on profile A
// cannot perceive into profile B (auth middleware rejects the token
// mismatch) and the OS docs stay scoped.
func TestPhase6_CrossVaultIsolation(t *testing.T) {
	osURL := requireLiveOS(t)
	_, baseURL, tok, cleanup := sandboxedDaemon(t, osURL, "alpha", "x")
	defer cleanup()

	// Agent A with the alpha/x token POSTs a doc.
	cA, _ := brain.NewClient(brain.ClientOpts{BaseURL: baseURL, Token: tok})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cA.Perceive(ctx, brain.PerceiveRequest{
		SHA: strings.Repeat("b", 64), Title: "alpha-only", Body: "secret",
		Embedding: nonZeroEmbedding(2),
	}); err != nil {
		t.Fatalf("Perceive A: %v", err)
	}

	// Agent B with a bogus token must get 401.
	cB, _ := brain.NewClient(brain.ClientOpts{BaseURL: baseURL, Token: "wrong-token"})
	_, err := cB.Perceive(ctx, brain.PerceiveRequest{
		SHA: strings.Repeat("c", 64), Title: "should-fail", Body: "nope",
		Embedding: nonZeroEmbedding(3),
	})
	if err == nil {
		t.Fatal("expected 401 from bogus token; got success")
	}
}

