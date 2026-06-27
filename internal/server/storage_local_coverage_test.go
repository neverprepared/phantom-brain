package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newLocalBackendForTest(t *testing.T) (*LocalBackend, DataDir) {
	t.Helper()
	dir := DataDir(t.TempDir())
	b, err := NewLocalBackend(dir, "http://localhost:9998/")
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	return b, dir
}

func TestNewLocalBackend_RequiresBaseURL(t *testing.T) {
	if _, err := NewLocalBackend(DataDir(t.TempDir()), ""); err == nil {
		t.Fatal("empty baseURL should error")
	}
	b, err := NewLocalBackend(DataDir(t.TempDir()), "http://h/")
	if err != nil {
		t.Fatalf("valid baseURL: %v", err)
	}
	// Trailing slash is trimmed so URLs don't get doubled.
	if strings.HasSuffix(b.baseURL, "/") {
		t.Errorf("baseURL should be right-trimmed, got %q", b.baseURL)
	}
}

func TestLocalBackend_NewUpload_URLAndTokenRoundTrip(t *testing.T) {
	b, _ := newLocalBackendForTest(t)
	h, err := b.NewUpload("brain-1", time.Hour)
	if err != nil {
		t.Fatalf("NewUpload: %v", err)
	}
	if h.UploadID == "" || h.Token == "" {
		t.Fatalf("empty upload handle: %+v", h)
	}
	if !strings.Contains(h.URL, h.UploadID) {
		t.Errorf("URL %q should embed upload_id %q", h.URL, h.UploadID)
	}
	if !strings.HasPrefix(h.URL, "http://localhost:9998/api/brain/merge/upload/") {
		t.Errorf("unexpected URL shape: %q", h.URL)
	}
	// VerifyToken needs the binding to have been registered.
	b.RegisterUpload(h.UploadID, "brain-1", "personal", "memory", h.Expires)
	gotBrain, err := b.VerifyToken(h.UploadID, h.Token)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if gotBrain != "brain-1" {
		t.Errorf("VerifyToken brain = %q, want brain-1", gotBrain)
	}
}

func TestLocalBackend_VerifyToken_RejectsTamperAndUnknown(t *testing.T) {
	b, _ := newLocalBackendForTest(t)
	if _, err := b.VerifyToken("does-not-exist", "tok"); err == nil {
		t.Error("unknown upload_id should error")
	}
	h, _ := b.NewUpload("brain-x", time.Hour)
	b.RegisterUpload(h.UploadID, "brain-x", "p", "v", h.Expires)
	if _, err := b.VerifyToken(h.UploadID, h.Token+"deadbeef"); err == nil {
		t.Error("tampered token should be rejected")
	}
}

func TestLocalBackend_VerifyToken_RejectsExpired(t *testing.T) {
	b, _ := newLocalBackendForTest(t)
	past := time.Now().Add(-time.Minute)
	h, _ := b.NewUpload("brain-e", time.Hour)
	// Register with an already-expired window; re-sign over that window
	// so the HMAC matches but the expiry check fails.
	b.RegisterUpload(h.UploadID, "brain-e", "p", "v", past)
	tok := b.sign(h.UploadID, "brain-e", past)
	if _, err := b.VerifyToken(h.UploadID, tok); err == nil {
		t.Error("expired token should be rejected")
	}
}

func TestLocalBackend_LookupUpload(t *testing.T) {
	b, _ := newLocalBackendForTest(t)
	if _, ok := b.LookupUpload("nope"); ok {
		t.Error("unknown upload_id should not be found")
	}
	exp := time.Now().Add(time.Hour)
	b.RegisterUpload("u1", "brain", "prof", "vlt", exp)
	st, ok := b.LookupUpload("u1")
	if !ok {
		t.Fatal("registered upload should be found")
	}
	if st.BrainID != "brain" || st.Profile != "prof" || st.Vault != "vlt" {
		t.Errorf("lookup state = %+v", st)
	}
}

func TestLocalBackend_AcceptUpload_HappyAndGuards(t *testing.T) {
	b, dataDir := newLocalBackendForTest(t)
	exp := time.Now().Add(time.Hour)
	b.RegisterUpload("u-accept", "brain", "personal", "memory", exp)

	payload := "hello world payload"
	n, err := b.AcceptUpload("u-accept", strings.NewReader(payload), 1<<20)
	if err != nil {
		t.Fatalf("AcceptUpload: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("bytes received = %d, want %d", n, len(payload))
	}
	written := filepath.Join(dataDir.BrainsDir("personal", "memory"), "_uploads", "u-accept.tar")
	got, err := os.ReadFile(written)
	if err != nil {
		t.Fatalf("read written upload: %v", err)
	}
	if string(got) != payload {
		t.Errorf("written bytes = %q, want %q", got, payload)
	}

	// Unknown upload_id.
	if _, err := b.AcceptUpload("ghost", strings.NewReader("x"), 1<<20); err == nil {
		t.Error("unknown upload_id should error")
	}
}

func TestLocalBackend_AcceptUpload_Expired(t *testing.T) {
	b, _ := newLocalBackendForTest(t)
	b.RegisterUpload("u-old", "brain", "p", "v", time.Now().Add(-time.Second))
	if _, err := b.AcceptUpload("u-old", strings.NewReader("x"), 1<<20); err == nil {
		t.Error("expired upload should be rejected")
	}
}

func TestLocalBackend_AcceptUpload_ExceedsMax(t *testing.T) {
	b, dataDir := newLocalBackendForTest(t)
	b.RegisterUpload("u-big", "brain", "p", "v", time.Now().Add(time.Hour))
	_, err := b.AcceptUpload("u-big", strings.NewReader("0123456789"), 4)
	if err == nil {
		t.Fatal("upload larger than maxBytes should error")
	}
	// The partial file must be cleaned up.
	leftover := filepath.Join(dataDir.BrainsDir("p", "v"), "_uploads", "u-big.tar")
	if _, statErr := os.Stat(leftover); !os.IsNotExist(statErr) {
		t.Errorf("oversized upload file should be removed, stat err = %v", statErr)
	}
}

func TestLocalBackend_CompleteUpload_HappyAndMismatch(t *testing.T) {
	b, dataDir := newLocalBackendForTest(t)
	b.RegisterUpload("u-done", "brain-c", "personal", "memory", time.Now().Add(time.Hour))
	if _, err := b.AcceptUpload("u-done", strings.NewReader("payload"), 1<<20); err != nil {
		t.Fatalf("AcceptUpload: %v", err)
	}

	// Binding mismatch is rejected before any rename.
	if _, err := b.CompleteUpload("wrong", "memory", "brain-c", "u-done"); err == nil {
		t.Error("profile mismatch should be rejected")
	}

	dst, err := b.CompleteUpload("personal", "memory", "brain-c", "u-done")
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	wantDir := filepath.Join(dataDir.BrainsDir("personal", "memory"), "_pending")
	if filepath.Dir(dst) != wantDir {
		t.Errorf("completed path %q not under _pending %q", dst, wantDir)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("completed payload should exist: %v", err)
	}
	// Upload state is dropped after completion.
	if _, ok := b.LookupUpload("u-done"); ok {
		t.Error("completed upload state should be deleted")
	}
}

func TestLocalBackend_CompleteUpload_UnknownAndMissingBytes(t *testing.T) {
	b, _ := newLocalBackendForTest(t)
	if _, err := b.CompleteUpload("p", "v", "b", "ghost"); err == nil {
		t.Error("unknown upload_id should error")
	}
	// Registered but never accepted → bytes not on disk.
	b.RegisterUpload("u-nobytes", "b", "p", "v", time.Now().Add(time.Hour))
	if _, err := b.CompleteUpload("p", "v", "b", "u-nobytes"); err == nil {
		t.Error("missing bytes on disk should error")
	}
}

func TestLocalBackend_PruneExpired(t *testing.T) {
	b, _ := newLocalBackendForTest(t)
	b.RegisterUpload("live", "b", "p", "v", time.Now().Add(time.Hour))
	b.RegisterUpload("dead", "b", "p", "v", time.Now().Add(-time.Hour))
	b.PruneExpired()
	if _, ok := b.LookupUpload("dead"); ok {
		t.Error("expired upload should be pruned")
	}
	if _, ok := b.LookupUpload("live"); !ok {
		t.Error("live upload should survive prune")
	}
}

func TestLocalBackend_OverrideBaseURL(t *testing.T) {
	b, _ := newLocalBackendForTest(t)
	b.OverrideBaseURL("http://proxy.example:8080/")
	h, err := b.NewUpload("brain", time.Hour)
	if err != nil {
		t.Fatalf("NewUpload: %v", err)
	}
	if !strings.HasPrefix(h.URL, "http://proxy.example:8080/api/brain/merge/upload/") {
		t.Errorf("override not applied, URL = %q", h.URL)
	}
}

func TestParseDurationSecs(t *testing.T) {
	def := 30 * time.Second
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"60", 60 * time.Second},
		{"1", time.Second},
		{"0", def},   // non-positive → default
		{"-5", def},  // negative → default
		{"", def},    // unparseable → default
		{"abc", def}, // unparseable → default
		{"12x", def}, // unparseable → default
	}
	for _, c := range cases {
		if got := parseDurationSecs(c.in, def); got != c.want {
			t.Errorf("parseDurationSecs(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
