package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestNewClient_RequiresFields(t *testing.T) {
	if _, err := NewClient(ClientOpts{}); err == nil {
		t.Error("expected error with empty opts")
	}
	if _, err := NewClient(ClientOpts{BaseURL: "https://x"}); err == nil {
		t.Error("expected error with missing token")
	}
	if _, err := NewClient(ClientOpts{Token: "t"}); err == nil {
		t.Error("expected error with missing baseURL")
	}
}

func TestClient_GetCurrentSnapshot(t *testing.T) {
	want := SnapshotManifestResponse{Profile: "p", Vault: "v", Gen: 7, SHA256: "abc", SizeBytes: 100}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/brain/snapshot/current" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer; got %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer ts.Close()
	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok"})
	got, err := c.GetCurrentSnapshot(context.Background())
	if err != nil {
		t.Fatalf("GetCurrentSnapshot: %v", err)
	}
	if got.Gen != 7 || got.SHA256 != "abc" {
		t.Errorf("got %+v", got)
	}
}

func TestClient_DownloadSnapshotTarball_SHAVerify(t *testing.T) {
	body := []byte("fake-tarball-bytes")
	// SHA of body (precomputed once for the test).
	// echo -n "fake-tarball-bytes" | sha256sum
	wantSHA := "2579179b569448f4025b6d6dd852f00ffff49c9f29d54c17cd9a4f762df403eb"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer ts.Close()
	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok"})

	// Happy path with correct expected SHA.
	var buf bytes.Buffer
	n, err := c.DownloadSnapshotTarball(context.Background(), 1, wantSHA, &buf)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("size = %d, want %d", n, len(body))
	}

	// Wrong SHA → error.
	buf.Reset()
	_, err = c.DownloadSnapshotTarball(context.Background(), 1, "deadbeef"+wantSHA[8:], &buf)
	if err == nil || !strings.Contains(err.Error(), "sha mismatch") {
		t.Errorf("expected sha mismatch, got %v", err)
	}
}

func TestClient_APIErrorEnvelopeDecoded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"STALE_SNAPSHOT","message":"gone","details":{"gen":42}}}`)
	}))
	defer ts.Close()
	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok"})
	err := c.ClaimBirth(context.Background(), "brain-1", 42, 60)
	if err == nil {
		t.Fatal("expected err")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err is not *APIError: %T %v", err, err)
	}
	if apiErr.Code != "STALE_SNAPSHOT" || apiErr.StatusCode != http.StatusConflict {
		t.Errorf("got %+v", apiErr)
	}
	if !IsAPIErrorCode(err, "STALE_SNAPSHOT") {
		t.Error("IsAPIErrorCode should match")
	}
}

func TestClient_InitMergeAndComplete(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/brain/merge/init", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(MergeInitResponse{
			UploadID: "u1", URL: "http://upload.invalid/u1", Token: "tok", Expires: 9999999999,
		})
	})
	gotComplete := false
	mux.HandleFunc("/api/brain/merge/complete/u1", func(w http.ResponseWriter, r *http.Request) {
		gotComplete = true
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["brain_id"] != "brain-X" {
			t.Errorf("brain_id = %q", body["brain_id"])
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok"})

	init, err := c.InitMerge(context.Background(), "brain-X", 1024, 60)
	if err != nil {
		t.Fatalf("InitMerge: %v", err)
	}
	if init.UploadID != "u1" {
		t.Errorf("upload_id = %q", init.UploadID)
	}
	if err := c.CompleteMerge(context.Background(), "u1", "brain-X"); err != nil {
		t.Fatalf("CompleteMerge: %v", err)
	}
	if !gotComplete {
		t.Error("complete not hit")
	}
}

func TestClient_UploadTarball(t *testing.T) {
	gotBody := bytes.Buffer{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %q", r.Method)
		}
		_, _ = io.Copy(&gotBody, r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"received_bytes": gotBody.Len()})
	}))
	defer ts.Close()
	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok"})
	body := []byte("payload")
	n, err := c.UploadTarball(context.Background(), ts.URL+"/upload", bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("UploadTarball: %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("received = %d, want %d", n, len(body))
	}
	if !bytes.Equal(gotBody.Bytes(), body) {
		t.Errorf("body mismatch: got %q", gotBody.String())
	}
}

func TestShipQueue_UploadEndToEndAgainstFakeDaemon(t *testing.T) {
	// httptest server that implements /merge/init + upload + /merge/complete
	// with just enough fidelity for UploadShipQueue to drive end-to-end.
	var got struct {
		init     bool
		uploaded []byte
		complete bool
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/brain/merge/init", func(w http.ResponseWriter, _ *http.Request) {
		got.init = true
		_ = json.NewEncoder(w).Encode(MergeInitResponse{
			UploadID: "u1",
			URL:      "PLACEHOLDER",
		})
	})
	mux.HandleFunc("/upload/u1", func(w http.ResponseWriter, r *http.Request) {
		got.uploaded, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"received_bytes": len(got.uploaded)})
	})
	mux.HandleFunc("/api/brain/merge/complete/u1", func(w http.ResponseWriter, _ *http.Request) {
		got.complete = true
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// init handler returns a placeholder URL — substitute the real
	// httptest base. We do this by wrapping the handler.
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/api/brain/merge/init", func(w http.ResponseWriter, _ *http.Request) {
		got.init = true
		_ = json.NewEncoder(w).Encode(MergeInitResponse{
			UploadID: "u1", URL: ts.URL + "/upload/u1",
		})
	})
	mux2.Handle("/upload/u1", mux)
	mux2.Handle("/api/brain/merge/complete/u1", mux)
	ts2 := httptest.NewServer(mux2)
	defer ts2.Close()

	agent := agentForTest(t)
	agent.API = ts2.URL

	// Seed a pending payload as if Death() had run.
	pendingDir := agent.ShipPendingDir() + "/brain-Z"
	if err := os.MkdirAll(pendingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("death-tarball-bytes")
	if err := os.WriteFile(pendingDir+"/death-100.tar", payload, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := UploadShipQueue(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("UploadShipQueue: %v", err)
	}
	if len(res.Shipped) != 1 || len(res.Failed) != 0 {
		t.Errorf("unexpected result: %+v", res)
	}
	if !got.init || !got.complete {
		t.Errorf("missing daemon calls: init=%v complete=%v", got.init, got.complete)
	}
	if !bytes.Equal(got.uploaded, payload) {
		t.Errorf("upload body mismatch: got %d bytes, want %d", len(got.uploaded), len(payload))
	}
	// Local payload should be gone after shipping.
	if _, err := os.Stat(pendingDir + "/death-100.tar"); !os.IsNotExist(err) {
		t.Errorf("local payload should be gone after ship: %v", err)
	}
}

func TestShipQueue_UploadSkipsOnMergeInProgress(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/brain/merge/init", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"MERGE_IN_PROGRESS","message":"dup"}}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agent := agentForTest(t)
	agent.API = ts.URL
	pendingDir := agent.ShipPendingDir() + "/brain-Q"
	_ = os.MkdirAll(pendingDir, 0o755)
	_ = os.WriteFile(pendingDir+"/death-1.tar", []byte("x"), 0o644)

	res, _ := UploadShipQueue(context.Background(), agent, nil)
	if len(res.Skipped) != 1 || len(res.Shipped) != 0 {
		t.Errorf("expected 1 skipped, got %+v", res)
	}
	// Skipped payload stays on disk.
	if _, err := os.Stat(pendingDir + "/death-1.tar"); err != nil {
		t.Errorf("skipped payload should remain on disk: %v", err)
	}
}

func TestSnapcache_FetchFromDaemonHappyPath(t *testing.T) {
	// Build a tiny tar.zst that extractSnapshotTarball can chew on.
	// We don't actually extract here — we only verify FetchSnapshotFromDaemon
	// downloads + caches the tarball + writes the metadata sidecar.
	body := []byte("fake-tarball-bytes")
	wantSHA := "2579179b569448f4025b6d6dd852f00ffff49c9f29d54c17cd9a4f762df403eb"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/brain/snapshot/current", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(SnapshotManifestResponse{
			Profile: "personal", Vault: "memory", Gen: 5, SHA256: wantSHA, SizeBytes: int64(len(body)),
		})
	})
	mux.HandleFunc("/api/brain/snapshot/5/tarball", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agent := agentForTest(t)
	agent.API = ts.URL

	cs, err := FetchSnapshotFromDaemon(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("FetchSnapshotFromDaemon: %v", err)
	}
	if cs == nil || cs.Gen != 5 {
		t.Fatalf("got %+v", cs)
	}
	// Tarball should exist on disk under SnapshotCacheDir.
	if data, err := os.ReadFile(cs.TarballPath); err != nil || !bytes.Equal(data, body) {
		t.Errorf("cached tarball mismatch: err=%v len=%d", err, len(data))
	}
}
