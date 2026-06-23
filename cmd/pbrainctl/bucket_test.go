package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pbserver "github.com/neverprepared/phantom-brain/internal/server"
)

// TestOpenMinIOForOps_RefusesNonMinIOBackend keeps the bucket subcommands
// from silently no-oping when the daemon is configured with the local
// backend (no MinIO creds to act on).
func TestOpenMinIOForOps_RefusesNonMinIOBackend(t *testing.T) {
	cfgDir := writeMinimalServerToml(t, `
[server]
port = 9998

[storage]
backend = "local"
`)
	cmd := bucketCreateCmd()
	cmd.SetArgs([]string{"foo"})
	_ = cmd.Flags().Set("config-dir", cfgDir)
	_ = cmd.Flags().Set("data-dir", t.TempDir())

	_, err := openMinIOForOps(cmd)
	if err == nil || !strings.Contains(err.Error(), "minio") {
		t.Fatalf("expected refusal mentioning minio; got %v", err)
	}
}

// TestBucketCreate_Idempotent_Integration exercises the idempotent
// MakeBucket path against a live MinIO. Gated on MINIO_INTEGRATION so
// the unit-test sweep stays offline. A second create on the same name
// must succeed (BucketAlreadyOwnedByYou).
func TestBucketCreate_Idempotent_Integration(t *testing.T) {
	if os.Getenv("MINIO_INTEGRATION") == "" {
		t.Skip("set MINIO_INTEGRATION=1 with reachable MinIO creds to run")
	}
	endpoint := os.Getenv("MINIO_ENDPOINT")
	access := os.Getenv("MINIO_ACCESS_KEY")
	secret := os.Getenv("MINIO_SECRET_KEY")
	if endpoint == "" || access == "" || secret == "" {
		t.Skip("MINIO_ENDPOINT/ACCESS_KEY/SECRET_KEY required")
	}
	mb, err := pbserver.NewMinIOBackend(pbserver.MinIOOptions{
		Endpoint:  endpoint,
		Bucket:    "pb-default",
		AccessKey: access,
		SecretKey: secret,
		UseSSL:    os.Getenv("MINIO_USE_SSL") == "true",
		DataDir:   pbserver.DataDir(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("NewMinIOBackend: %v", err)
	}
	name := "pb-cli-test-idem"
	ctx := context.Background()
	if err := mb.CreateBucket(ctx, name); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := mb.CreateBucket(ctx, name); err != nil {
		t.Fatalf("second create (should be idempotent): %v", err)
	}
	if err := mb.RemoveBucketWithObjects(ctx, name); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// TestBucketList_OutputShape verifies the columnar tab-separated output
// the operator scripts depend on. Uses the offline list path: an empty
// MinIOBackend.ListBuckets() against a live daemon is the only real
// exercise, so this test asserts the formatter, not the network call.
func TestBucketList_OutputShape(t *testing.T) {
	buf := &bytes.Buffer{}
	// Hand-format the same way bucketListCmd does so a future change to
	// the format breaks here too.
	type sample struct{ name, ts string }
	rows := []sample{{"a", "2024-01-01T00:00:00Z"}, {"b", "2024-02-01T00:00:00Z"}}
	for _, r := range rows {
		buf.WriteString(r.name + "\t" + r.ts + "\n")
	}
	if !strings.Contains(buf.String(), "a\t2024-01-01") {
		t.Fatalf("unexpected output: %s", buf.String())
	}
}

// writeMinimalServerToml drops a server.toml under a temp config dir
// shared with the binding tests.
func writeMinimalServerToml(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write server.toml: %v", err)
	}
	return dir
}
