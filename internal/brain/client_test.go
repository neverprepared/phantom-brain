package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// Phase D2b: the snapshot client methods (GetCurrentSnapshot,
// DownloadSnapshotTarball, ClaimBirth) and SnapshotManifestResponse were
// removed with the snapshot machinery. Their tests
// (TestClient_GetCurrentSnapshot, TestClient_DownloadSnapshotTarball_SHAVerify,
// TestSnapcache_FetchFromDaemonHappyPath) are gone. The error-envelope
// decoding they exercised is retained below, now driven through a live
// POST method (Forget).

func TestClient_APIErrorEnvelopeDecoded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"CONFLICT","message":"gone","details":{"sha":"x"}}}`)
	}))
	defer ts.Close()
	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok"})
	_, err := c.Forget(context.Background(), "sha-1")
	if err == nil {
		t.Fatal("expected err")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err is not *APIError: %T %v", err, err)
	}
	if apiErr.Code != "CONFLICT" || apiErr.StatusCode != http.StatusConflict {
		t.Errorf("got %+v", apiErr)
	}
	if !IsAPIErrorCode(err, "CONFLICT") {
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

// TestClient_Fetch covers the Phase D2a online-fetch path: GET
// /api/brain/fetch/{sha} with the bearer token, decoding the FetchResponse.
func TestClient_Fetch(t *testing.T) {
	want := FetchResponse{
		SHA:       "deadbeef",
		Title:     "A Doc",
		Kind:      "note",
		SourceURL: "https://example.com/x",
		Tags:      []string{"a", "b"},
		Body:      "the full body",
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/api/brain/fetch/deadbeef" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer; got %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer ts.Close()
	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok"})
	got, err := c.Fetch(context.Background(), "deadbeef")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.SHA != want.SHA || got.Title != want.Title || got.Body != want.Body {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if len(got.Tags) != 2 {
		t.Errorf("tags = %v", got.Tags)
	}
}

// TestClient_Fetch_NotFound: a 404 envelope decodes to *APIError so
// brain_fetch can render the friendly "no such doc" message.
func TestClient_Fetch_NotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"NOT_FOUND","message":"no document with that SHA"}}`)
	}))
	defer ts.Close()
	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok"})
	_, err := c.Fetch(context.Background(), "deadbeef")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err is not *APIError: %T %v", err, err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", apiErr.StatusCode)
	}
}

// ShipQueue + /merge tests removed in Phase 6 — agents POST writes
// to the daemon during life, so death produces no payload and the
// /merge handshake is retired.
