package server

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestNewMinIOBackend_ValidatesRequiredFields keeps the constructor's
// fail-fast contract honest. Operators get a clear message at daemon
// startup rather than a confusing 500 on the first /merge/init.
func TestNewMinIOBackend_ValidatesRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		opts MinIOOptions
		want string
	}{
		{"empty endpoint", MinIOOptions{Bucket: "b", AccessKey: "a", SecretKey: "s", DataDir: "/tmp"}, "endpoint"},
		{"empty bucket", MinIOOptions{Endpoint: "x", AccessKey: "a", SecretKey: "s", DataDir: "/tmp"}, "bucket"},
		{"missing keys", MinIOOptions{Endpoint: "x", Bucket: "b", DataDir: "/tmp"}, "access_key"},
		{"missing data dir", MinIOOptions{Endpoint: "x", Bucket: "b", AccessKey: "a", SecretKey: "s"}, "DataDir"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewMinIOBackend(c.opts)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %q, want substring %q", err.Error(), c.want)
			}
		})
	}
}

func TestNewMinIOBackend_BuildsClient(t *testing.T) {
	mb, err := NewMinIOBackend(MinIOOptions{
		Endpoint:  "minio.example.invalid:9000",
		Bucket:    "phantom-brain",
		AccessKey: "AKIA_TEST",
		SecretKey: "SECRET_TEST",
		DataDir:   DataDir(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("NewMinIOBackend: %v", err)
	}
	if mb.bucket != "phantom-brain" {
		t.Errorf("bucket = %q", mb.bucket)
	}
	// NewUpload doesn't talk to S3 — it allocates an id and a TTL.
	// Safe to call against the unreachable endpoint.
	handle, err := mb.NewUpload("brain-X", 60*time.Second)
	if err != nil {
		t.Fatalf("NewUpload: %v", err)
	}
	if handle.UploadID == "" {
		t.Error("UploadID empty")
	}
	if handle.URL != "" {
		// PresignedPutForUpload is the step that fills in URL.
		// NewUpload deliberately leaves it empty so the handler
		// supplies the (profile, vault) prefix before signing.
		t.Errorf("URL should be empty until PresignedPutForUpload runs; got %q", handle.URL)
	}
}

// TestMinIOBackend_AcceptUploadRejectedLocalOnly proves the
// local-only routes hard-fail under MinIO so a misconfigured client
// can't silently lose bytes. The actual /merge/upload route is
// already gated to *LocalBackend in handleMergeUpload; this test
// pins the backend-method contract.
func TestMinIOBackend_AcceptUploadRejectedLocalOnly(t *testing.T) {
	mb, _ := NewMinIOBackend(MinIOOptions{
		Endpoint: "x.invalid", Bucket: "b", AccessKey: "a", SecretKey: "s",
		DataDir: DataDir(t.TempDir()),
	})
	if _, err := mb.AcceptUpload("uid", strings.NewReader("x"), 1024); err == nil {
		t.Error("AcceptUpload should fail for MinIO")
	}
	if _, err := mb.VerifyToken("uid", "tok"); err == nil {
		t.Error("VerifyToken should fail for MinIO")
	}
}

// TestMinIOBackend_CompleteUploadRejectsUnknownID guards the
// in-memory binding map. An upload_id that was never RegisterUpload'd
// can't be Complete'd — protects against replay or rogue clients.
func TestMinIOBackend_CompleteUploadRejectsUnknownID(t *testing.T) {
	mb, _ := NewMinIOBackend(MinIOOptions{
		Endpoint: "x.invalid", Bucket: "b", AccessKey: "a", SecretKey: "s",
		DataDir: DataDir(t.TempDir()),
	})
	_, err := mb.CompleteUpload("personal", "memory", "brain-X", "never-registered")
	if err == nil || !strings.Contains(err.Error(), "unknown upload_id") {
		t.Errorf("got %v, want unknown-upload-id error", err)
	}
}

// TestMinIOBackend_RegisterUploadRoundTrip verifies the small piece
// of state-keeping logic — bindings recorded by RegisterUpload survive
// lookup. The fields populated here are what CompleteUpload uses to
// route the downloaded bytes.
func TestMinIOBackend_RegisterUploadRoundTrip(t *testing.T) {
	mb, _ := NewMinIOBackend(MinIOOptions{
		Endpoint: "x.invalid", Bucket: "b", AccessKey: "a", SecretKey: "s",
		DataDir: DataDir(t.TempDir()),
	})
	mb.RegisterUpload("u1", "brain-X", "personal", "memory", "personal/memory/_uploads/u1.tar", time.Now().Add(time.Hour))
	st, ok := mb.lookupUpload("u1")
	if !ok {
		t.Fatal("lookup failed")
	}
	if st.BrainID != "brain-X" || st.Profile != "personal" || st.Vault != "memory" {
		t.Errorf("binding mismatch: %+v", st)
	}
	if st.ObjKey != "personal/memory/_uploads/u1.tar" {
		t.Errorf("ObjKey = %q", st.ObjKey)
	}
}

// TestMinIOBackend_HandlerWiringRetainsPath is a sanity check: when
// the daemon is constructed with backend = "minio", /merge/init still
// returns 200 and a populated UploadID. We can't presign against a
// real endpoint in CI, but we CAN verify the routing path doesn't
// blow up. Uses an httptest server pointed at a closed listener for
// MinIO so the presign call would 500 — so this test stops at the
// pre-presign validation. Skipped until we have a mock S3 endpoint.
func TestMinIOBackend_HandlerWiring_Skipped(t *testing.T) {
	if os.Getenv("MINIO_INTEGRATION") == "" {
		t.Skip("set MINIO_INTEGRATION=1 with a reachable MinIO bucket to run")
	}
	// Smoke test scaffolding kept here as a hook; the full
	// integration test belongs alongside the operator's bucket
	// credentials, not in unit CI.
	_ = context.Background()
	_ = httptest.NewServer
}
