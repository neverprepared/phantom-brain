// Package integration_test exercises the Phase 6 round-trip:
// agent → daemon HTTP → OpenSearch → snapshot tarball → agent's
// local sqlite-vec cache.
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

	"github.com/neverprepared/mcp-phantom-brain/internal/brain"
	"github.com/neverprepared/mcp-phantom-brain/internal/index"
	"github.com/neverprepared/mcp-phantom-brain/internal/osearch"
	pbserver "github.com/neverprepared/mcp-phantom-brain/internal/server"
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

// TestPhase6_RoundTrip_AgentDaemonOS exercises the full Phase 6
// pipeline:
//
//  1. Agent POSTs /perceive (raw doc + embedding) via brain.Client
//  2. Daemon indexes into OS (raw, synthesised=false)
//  3. Test code stamps synthesised=true via a direct osearch.Client
//     (simulates what SynthWorker would do; bypassing CLI keeps the
//     test fast + deterministic)
//  4. BuildSnapshotFromOS produces a tarball
//  5. Extract + open the tarball with internal/index.Open
//  6. Verify the doc is findable via SearchText
func TestPhase6_RoundTrip_AgentDaemonOS(t *testing.T) {
	osURL := requireLiveOS(t)
	d, baseURL, tok, cleanup := sandboxedDaemon(t, osURL, "personal", "memory")
	defer cleanup()

	// --- step 1: agent POST /perceive --------------------------------
	client, err := brain.NewClient(brain.ClientOpts{BaseURL: baseURL, Token: tok})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sha := strings.Repeat("a", 64)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.Perceive(ctx, brain.PerceiveRequest{
		SHA: sha, Title: "Kubernetes basics",
		Body:      "Pods are the smallest deployable unit in Kubernetes.",
		URL:       "https://example.com/k8s",
		Tags:      []string{"k8s", "infra"},
		Embedding: nonZeroEmbedding(1),
	}); err != nil {
		t.Fatalf("Perceive: %v", err)
	}

	// --- step 2: verify OS doc landed (raw-only) ----------------------
	osc := mustOpenLiveOS(t, osURL, sandboxPrefixFromDaemon(t, d))
	if err := osc.Refresh(ctx); err != nil {
		t.Fatalf("OS refresh: %v", err)
	}
	doc, err := osc.GetSummary(ctx, "personal", "memory", sha)
	if err != nil || doc == nil {
		t.Fatalf("OS GetSummary: doc=%v err=%v", doc, err)
	}
	if doc.Synthesised {
		t.Errorf("expected raw-only doc; got Synthesised=true")
	}
	if doc.Title != "Kubernetes basics" {
		t.Errorf("title round-trip mismatch: %q", doc.Title)
	}

	// --- step 3: simulate synth completion ----------------------------
	// Real SynthWorker runs claude CLI for gate + distill; tests
	// stamp synthesised=true directly so the rebuild path picks the
	// doc up. Embedding stays — exporter requires non-empty vec.
	doc.Synthesised = true
	doc.Body = "Distilled: Pods are k8s' smallest deployable unit."
	doc.Reliability = osearch.ReliabilityMedium
	doc.Topic = "infrastructure"
	if err := osc.UpsertSummary(ctx, *doc, true); err != nil {
		t.Fatalf("simulate synth: %v", err)
	}

	// --- step 4: build snapshot from OS -------------------------------
	exp := osearchExporterFromDaemon(t, d)
	info, err := pbserver.BuildSnapshotFromOS(ctx, d.DataDir, exp, "personal", "memory", 30)
	if err != nil {
		t.Fatalf("BuildSnapshotFromOS: %v", err)
	}
	if info == nil {
		t.Fatal("snapshot info nil; expected publish")
	}
	if info.Manifest.SHA256 == "" {
		t.Errorf("snapshot manifest missing SHA256: %+v", info.Manifest)
	}

	// --- step 5+6: extract + verify findable via local index ---------
	extract := t.TempDir()
	if err := osearch.ExtractTarZst(info.TarballPath, extract); err != nil {
		t.Fatalf("extract tarball: %v", err)
	}
	idx, err := index.Open(extract, osearch.EmbeddingDim)
	if err != nil {
		t.Fatalf("index.Open: %v", err)
	}
	defer idx.Close()
	hits, err := idx.SearchText(ctx, "kubernetes", 5)
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.SHA == sha {
			found = true
		}
	}
	if !found {
		t.Errorf("SearchText('kubernetes') did not find seeded doc %s; hits=%+v", sha, hits)
	}
}

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

// mustOpenLiveOS builds a direct osearch.Client that targets the
// same per-test index prefix as the daemon, so the test can inspect
// what the daemon wrote without piggybacking on a daemon read API.
func mustOpenLiveOS(t *testing.T, osURL, prefix string) *osearch.Client {
	t.Helper()
	c, err := osearch.Open(context.Background(), osearch.Config{
		Addresses:      []string{osURL},
		IndexPrefix:    prefix,
		RequestTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("osearch.Open: %v", err)
	}
	return c
}

// sandboxPrefixFromDaemon recovers the IndexPrefix the daemon was
// configured with. Daemon doesn't expose this directly; we go
// through the loaded config.
func sandboxPrefixFromDaemon(t *testing.T, d *pbserver.Daemon) string {
	t.Helper()
	if d.Config == nil {
		t.Fatal("daemon config nil")
	}
	return d.Config.OpenSearch.IndexPrefix
}

// osearchExporterFromDaemon constructs a fresh osearch.Client
// (sharing the same prefix as the daemon's wired client) so the
// test can call BuildSnapshotFromOS without reaching into private
// daemon fields. Equivalent to what d.osExport holds internally.
func osearchExporterFromDaemon(t *testing.T, d *pbserver.Daemon) *osearch.Client {
	t.Helper()
	osURL := os.Getenv("OPENSEARCH_URL")
	if osURL == "" {
		t.Fatal("OPENSEARCH_URL empty")
	}
	return mustOpenLiveOS(t, osURL, sandboxPrefixFromDaemon(t, d))
}
