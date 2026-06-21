// Gated MinIO smoke test. Lives at test/integration/ so it can
// import both internal/brain and internal/server. Skipped by
// default — set MINIO_INTEGRATION=1 with the env block below to
// run it against a real bucket.
//
// Required env:
//   MINIO_INTEGRATION=1
//   MINIO_ENDPOINT=minio.example.com[:port]
//   MINIO_BUCKET=phantom-brain
//   MINIO_ACCESS_KEY=...
//   MINIO_SECRET_KEY=...
//   MINIO_USE_SSL=true|false   (default true)
//
// What it exercises:
//
//  1. pbserver.Start with backend = "minio" against the real endpoint
//  2. agent (Lifecycle.Start) births greenfield (daemon has no
//     snapshot yet) — seeds a Raw file in its brain dir
//  3. agent.Shutdown packs death payload locally
//  4. UploadShipQueue ships via:
//       a. POST /merge/init  → daemon presigns S3 PUT URL
//       b. PUT <presigned>   → bytes go directly to MinIO
//       c. POST /merge/complete → daemon GETs from MinIO, writes
//          to brains/_pending/<brain_id>.tar, deletes from MinIO
//  5. server.ReapOnce merges + ledger row
//  6. server.SynthesizeOne writes Wiki
//  7. assertions: ledger row exists, Wiki populated, MinIO bucket
//     no longer has the _uploads/<id>.tar object (best-effort
//     cleanup ran)
//
// Failure modes the test surfaces clearly (operator gets a real
// error message, not a stack trace):
//   - bucket doesn't exist                       → daemon Start log + 404
//   - access key lacks PutObject permission      → presign succeeds, PUT fails
//   - endpoint scheme mismatch (ssl vs no-ssl)   → connection refused
//   - presigned URL host blocked from this host  → upload step fails
package integration_test

import (
	"archive/tar"
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/neverprepared/mcp-phantom-brain/internal/brain"
	"github.com/neverprepared/mcp-phantom-brain/internal/config"
	"github.com/neverprepared/mcp-phantom-brain/internal/server"
)

func TestMinIO_AgentDaemonSmoke(t *testing.T) {
	if os.Getenv("MINIO_INTEGRATION") != "1" {
		t.Skip("set MINIO_INTEGRATION=1 with MINIO_{ENDPOINT,BUCKET,ACCESS_KEY,SECRET_KEY,USE_SSL} to run")
	}
	endpoint := mustEnv(t, "MINIO_ENDPOINT")
	bucket := mustEnv(t, "MINIO_BUCKET")
	accessKey := mustEnv(t, "MINIO_ACCESS_KEY")
	secretKey := mustEnv(t, "MINIO_SECRET_KEY")
	useSSL := os.Getenv("MINIO_USE_SSL") != "false"

	// Sanity-check the bucket exists + creds work before we spin
	// up the daemon. Fails loud with a clear pointer if not.
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		t.Fatalf("minio client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if exists, err := mc.BucketExists(ctx, bucket); err != nil {
		t.Fatalf("BucketExists %q: %v (check endpoint scheme + creds)", bucket, err)
	} else if !exists {
		t.Fatalf("bucket %q does not exist — create it (mc mb)", bucket)
	}
	t.Logf("MinIO endpoint=%s bucket=%s ssl=%v — preflight OK", endpoint, bucket, useSSL)

	// --- Daemon configured with backend=minio --------------------
	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	writeMinIOServerToml(t, cfgDir, endpoint, bucket, accessKey, secretKey, useSSL)
	tok := seedVaultAuth(t, cfgDir, "smoketest", "memory")

	d, err := server.Start(server.StartOpts{
		ConfigDir: cfgDir,
		DataDir:   server.DataDir(dataDir),
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		t.Fatalf("daemon Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	ts := httptest.NewServer(d.Router())
	t.Cleanup(ts.Close)
	// The presigned URL minio-go produces points at the real MinIO
	// endpoint — we don't need to rewire it. But the daemon's own
	// /merge/init returns that URL via this httptest server, so
	// the agent's HTTP calls (init / complete) target ts.URL.

	// --- Agent births greenfield + seeds a Raw file --------------
	agent := buildSmokeAgent(t, ts.URL, tok, t.TempDir())
	lc, err := brain.Start(brain.StartOpts{
		Agent:    agent,
		Platform: brain.NewPlatform(),
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		t.Fatalf("agent Start: %v", err)
	}

	rawDir := filepath.Join(lc.VaultDir(), "Raw", "curated")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rawDir, "minio-smoke.md"),
		[]byte("# minio smoke\n\nThe **MinIO Smoke Test** verifies end-to-end ship via S3."),
		0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := lc.Shutdown(context.Background()); err != nil {
		t.Fatalf("agent Shutdown: %v", err)
	}

	// --- Ship: init → presigned PUT to MinIO → complete -----------
	res, err := brain.UploadShipQueue(context.Background(), agent,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err != nil {
		t.Fatalf("UploadShipQueue: %v", err)
	}
	if len(res.Shipped) != 1 {
		// Helpful diagnostic — the partition tells you which step failed.
		t.Fatalf("expected 1 shipped, got: shipped=%v skipped=%v failed=%v",
			res.Shipped, res.Skipped, res.Failed)
	}
	t.Logf("ship complete: %s", res.Shipped[0])

	// --- Reap + synthesize ----------------------------------------
	binding, _ := d.LookupBindingForTest(server.VaultKey{Profile: "smoketest", Vault: "memory"})
	if _, err := server.ReapOnce(server.DataDir(dataDir), binding,
		slog.New(slog.DiscardHandler), &fakeMutex{}); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	synthCtx, synthCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer synthCancel()
	if _, err := server.SynthesizeOne(synthCtx, server.DataDir(dataDir), binding,
		slog.New(slog.DiscardHandler), &fakeMutex{}); err != nil {
		t.Fatalf("SynthesizeOne: %v", err)
	}

	// --- Assertions -----------------------------------------------
	ledger, err := server.OpenLedger(server.DataDir(dataDir), "smoketest", "memory")
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	defer ledger.Close()
	rows, _ := ledger.List(10)
	if len(rows) != 1 {
		t.Fatalf("expected 1 ledger row, got %d", len(rows))
	}
	t.Logf("ledger: brain_id=%s raw_count=%d payload_bytes=%d",
		rows[0].BrainID, rows[0].RawCount, rows[0].PayloadBytes)

	wikiDir := filepath.Join(server.DataDir(dataDir).VaultDir("smoketest", "memory"), "Wiki", "summaries")
	entries, _ := os.ReadDir(wikiDir)
	if len(entries) == 0 {
		t.Fatal("Wiki/summaries is empty after synth")
	}

	// Bucket cleanliness: best-effort RemoveObject in CompleteUpload
	// should have left no _uploads/* objects for this run. We list
	// to confirm; an object lingering here would mean the cleanup
	// silently failed (worth knowing but not a hard fail).
	leftovers := 0
	for obj := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{
		Prefix: "smoketest/memory/_uploads/",
	}) {
		if obj.Err != nil {
			t.Logf("list error (ok if RBAC scopes ListObjects out): %v", obj.Err)
			break
		}
		leftovers++
		t.Logf("leftover upload: %s", obj.Key)
	}
	if leftovers > 0 {
		t.Errorf("bucket has %d leftover _uploads/ objects after merge; expected 0", leftovers)
	}

	t.Log("MinIO agent↔daemon roundtrip succeeded end-to-end")
}

// TestMinIO_LeaveUploadInBucket is the "show me the bytes" companion
// to TestMinIO_AgentDaemonSmoke. Drives /merge/init + the presigned
// PUT, then STOPS before /merge/complete so the upload object stays
// in the bucket for operator inspection.
//
// After it runs:
//   mc ls --recursive np/$MINIO_BUCKET/smoketest/memory/_uploads/
//   mc cat np/$MINIO_BUCKET/<full key from the test output> | tar tf -
//
// Cleanup (run yourself when done — the test deliberately doesn't):
//   mc rm --recursive --force np/$MINIO_BUCKET/smoketest/memory/_uploads/
func TestMinIO_LeaveUploadInBucket(t *testing.T) {
	if os.Getenv("MINIO_INTEGRATION") != "1" {
		t.Skip("set MINIO_INTEGRATION=1 to run")
	}
	endpoint := mustEnv(t, "MINIO_ENDPOINT")
	bucket := mustEnv(t, "MINIO_BUCKET")
	accessKey := mustEnv(t, "MINIO_ACCESS_KEY")
	secretKey := mustEnv(t, "MINIO_SECRET_KEY")
	useSSL := os.Getenv("MINIO_USE_SSL") != "false"

	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	writeMinIOServerToml(t, cfgDir, endpoint, bucket, accessKey, secretKey, useSSL)
	tok := seedVaultAuth(t, cfgDir, "smoketest", "memory")

	d, err := server.Start(server.StartOpts{
		ConfigDir: cfgDir,
		DataDir:   server.DataDir(dataDir),
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		t.Fatalf("daemon Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	ts := httptest.NewServer(d.Router())
	t.Cleanup(ts.Close)

	// Build a recognisable tarball so `mc cat | tar tf -` shows
	// something obvious during inspection.
	payload := buildInspectablePayload(t)

	// Use the agent client (same code path the real shipqueue uses)
	// to drive init + PUT. Stop before complete.
	agent := buildSmokeAgent(t, ts.URL, tok, t.TempDir())
	client, err := brain.NewClient(brain.ClientOpts{BaseURL: agent.API, Token: agent.Token})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	init, err := client.InitMerge(ctx, "brain-inspectable", int64(len(payload)), 3600)
	if err != nil {
		t.Fatalf("InitMerge: %v", err)
	}
	t.Logf("upload_id=%s", init.UploadID)
	t.Logf("presigned URL=%s", init.URL)

	if _, err := client.UploadTarball(ctx, init.URL, bytesReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("UploadTarball: %v", err)
	}

	objKey := "smoketest/memory/_uploads/" + init.UploadID + ".tar"
	t.Logf("UPLOAD LEFT IN BUCKET — inspect with:")
	t.Logf("  mc ls np/%s/%s", bucket, objKey)
	t.Logf("  mc cat np/%s/%s | tar tvf -", bucket, objKey)
	t.Logf("Cleanup when done:")
	t.Logf("  mc rm np/%s/%s", bucket, objKey)
}

// buildInspectablePayload returns a tiny tarball with two files at
// known names so `tar tvf -` output looks like something a human
// would recognise.
func buildInspectablePayload(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	for name, body := range map[string]string{
		"manifest.json":              `{"brain_id":"brain-inspectable","note":"left for ops inspection"}`,
		"vault/Raw/curated/hello.md": "# inspectable\n\nIf you can read this with `mc cat | tar -xO`, MinIO held the bytes.\n",
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: now,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// bytesReader keeps the test's import block tight — could use
// bytes.NewReader directly but defining one helper keeps the smoke
// test free of unrelated stdlib pull-ins.
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// --- helpers ---------------------------------------------------------

func mustEnv(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("env %s required", k)
	}
	return v
}

// writeMinIOServerToml drops a server.toml with the [storage] block.
// Pretty-printed because the operator will likely cat it when
// debugging.
func writeMinIOServerToml(t *testing.T, dir, endpoint, bucket, ak, sk string, ssl bool) {
	t.Helper()
	body := strings.TrimSpace(`
[server]
port = 0
host = "127.0.0.1"

[storage]
backend = "minio"
minio_endpoint   = "` + endpoint + `"
minio_bucket     = "` + bucket + `"
minio_access_key = "` + ak + `"
minio_secret_key = "` + sk + `"
minio_use_ssl    = ` + boolToToml(ssl) + `
`) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "server.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func boolToToml(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// buildSmokeAgent mirrors buildAgentConfig in agent_daemon_test.go
// but uses "smoketest/memory" as the vault to avoid colliding with
// any real personal/memory state on the operator's machine.
func buildSmokeAgent(t *testing.T, apiURL, tok, dataDir string) *config.Agent {
	t.Helper()
	t.Setenv("CL_BRAIN_API", apiURL)
	t.Setenv("CL_BRAIN_API_TOKEN", tok)
	t.Setenv("CL_WORKSPACE_PROFILE", "smoketest")
	t.Setenv("CL_BRAIN_VAULT", "memory")
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("CL_BRAIN_ID", "")
	a, err := config.LoadAgent()
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	return a
}

// keep httptest import referenced — used inside the smoke test body
// but not anywhere else, and go vet would flag the import otherwise
// when the test is skipped via build flag in some toolchains.
var _ = httptest.NewServer
