package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	pbbrain "github.com/neverprepared/mcp-phantom-brain/internal/brain"
)

// helper: start daemon + httptest server, return (daemon, baseURL, token, cleanup).
func startTestRig(t *testing.T) (*Daemon, string, string, func()) {
	t.Helper()
	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	writeServerConfig(t, cfgDir, "[server]\nport = 0\n")
	tok := seedVault(t, cfgDir, "personal", "memory", "")
	d, err := Start(StartOpts{
		ConfigDir: cfgDir,
		DataDir:   DataDir(dataDir),
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ts := httptest.NewServer(d.Router())
	// Rewire the LocalBackend's baseURL to the httptest server so
	// /merge/init issues URLs that actually route back here.
	if lb, ok := d.storage.(*LocalBackend); ok {
		lb.baseURL = ts.URL
	}
	return d, ts.URL, tok, func() {
		ts.Close()
		_ = d.Shutdown(context.Background())
	}
}

// --- Birth claim -----------------------------------------------------

func TestHandler_BirthClaim_HappyPath(t *testing.T) {
	d, baseURL, tok, cleanup := startTestRig(t)
	defer cleanup()
	seedCollective(t, d.DataDir, "personal", "memory", "# hi\n")
	if _, err := BuildSnapshot(d.DataDir, "personal", "memory", 30); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(birthClaimRequest{BrainID: "brain-1", Gen: 1, TTLSecs: 3600})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/brain/birth/claim", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	// Marker file landed.
	marker := filepath.Join(d.DataDir.StagedDir("personal", "memory"), "snapshot-1", ".claims", "brain-1")
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("claim marker missing: %v", err)
	}
}

func TestHandler_BirthClaim_StaleGenReturns409(t *testing.T) {
	_, baseURL, tok, cleanup := startTestRig(t)
	defer cleanup()
	body, _ := json.Marshal(birthClaimRequest{BrainID: "brain-x", Gen: 999})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/brain/birth/claim", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status=%d, want 409", resp.StatusCode)
	}
}

// --- Merge init → upload → complete end-to-end -----------------------

func TestHandler_MergeFlow_EndToEnd(t *testing.T) {
	d, baseURL, tok, cleanup := startTestRig(t)
	defer cleanup()

	// 1. /merge/init — get upload URL.
	initBody, _ := json.Marshal(mergeInitRequest{BrainID: "brain-A", TTLSecs: 60, PayloadSize: 1024})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/brain/merge/init", bytes.NewReader(initBody))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("init status=%d body=%s", resp.StatusCode, b)
	}
	var initResp mergeInitResponse
	_ = json.NewDecoder(resp.Body).Decode(&initResp)
	resp.Body.Close()
	if initResp.URL == "" || initResp.UploadID == "" {
		t.Fatalf("init returned empty handle: %+v", initResp)
	}

	// 2. PUT the tarball to the upload URL. Use a real death payload
	// so /merge/complete + reaper produce a meaningful ledger row.
	payload := makeDeathPayload(t, pbbrain.Manifest{
		BrainID: "brain-A", Profile: "personal", Vault: "memory",
	}, map[string]string{"vault/Raw/curated/hello.md": "# hi\n"})
	put, _ := http.NewRequest(http.MethodPut, initResp.URL, bytes.NewReader(payload))
	resp, err = http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status=%d body=%s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// 3. /merge/complete — moves _uploads/<id>.tar to _pending/brain-A.tar
	completeBody, _ := json.Marshal(mergeCompleteRequest{BrainID: "brain-A"})
	req, _ = http.NewRequest(http.MethodPost, baseURL+"/api/brain/merge/complete/"+initResp.UploadID, bytes.NewReader(completeBody))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("complete status=%d body=%s", resp.StatusCode, b)
	}
	resp.Body.Close()

	pendingPath := filepath.Join(d.DataDir.BrainsDir("personal", "memory"), "_pending", "brain-A.tar")
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("pending tar missing: %v", err)
	}

	// 4. GET /merge/{brain_id} — pending state before reaper runs.
	req, _ = http.NewRequest(http.MethodGet, baseURL+"/api/brain/merge/brain-A", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ = http.DefaultClient.Do(req)
	defer resp.Body.Close()
	var status map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&status)
	if status["state"] != "pending" {
		t.Errorf("status=%v, want pending", status)
	}

	// 5. Run the reaper once and confirm /merge transitions to merged.
	binding, _ := d.registry.LookupByToken(tok)
	if _, err := ReapOnce(d.DataDir, binding, slog.New(slog.DiscardHandler), &sync.Mutex{}); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	req, _ = http.NewRequest(http.MethodGet, baseURL+"/api/brain/merge/brain-A", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ = http.DefaultClient.Do(req)
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(&status)
	if status["state"] != "merged" {
		t.Errorf("post-reap status=%v, want merged", status)
	}
}

func TestHandler_MergeInit_RejectsOversizedPayload(t *testing.T) {
	_, baseURL, tok, cleanup := startTestRig(t)
	defer cleanup()
	body, _ := json.Marshal(mergeInitRequest{BrainID: "x", PayloadSize: 10 * 1024 * 1024 * 1024 * 1024}) // 10 TB
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/brain/merge/init", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d, want 413", resp.StatusCode)
	}
}

func TestHandler_MergeInit_RejectsWhenMaintenance(t *testing.T) {
	d, baseURL, tok, cleanup := startTestRig(t)
	defer cleanup()
	_ = SetMaintenance(d.DataDir, VaultKey{Profile: "personal", Vault: "memory"})
	body, _ := json.Marshal(mergeInitRequest{BrainID: "x", PayloadSize: 100})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/brain/merge/init", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", resp.StatusCode)
	}
}

// --- Maintenance -----------------------------------------------------

func TestHandler_Maintenance_EnterExitGet(t *testing.T) {
	_, baseURL, tok, cleanup := startTestRig(t)
	defer cleanup()

	enter := func() int {
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/brain/maintenance/enter", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := enter(); got != http.StatusOK {
		t.Fatalf("enter status=%d", got)
	}

	getState := func() bool {
		req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/brain/maintenance", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, _ := http.DefaultClient.Do(req)
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		v, _ := m["maintenance"].(bool)
		return v
	}
	if !getState() {
		t.Fatal("expected maintenance true after enter")
	}

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/brain/maintenance/exit", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if getState() {
		t.Fatal("expected maintenance false after exit")
	}
}

// --- Local backend HMAC ---------------------------------------------

func TestLocalBackend_TokenRejectsTamper(t *testing.T) {
	lb, err := NewLocalBackend(DataDir(t.TempDir()), "http://example")
	if err != nil {
		t.Fatal(err)
	}
	h, err := lb.NewUpload("brain-X", 60*1e9)
	if err != nil {
		t.Fatal(err)
	}
	lb.RegisterUpload(h.UploadID, "brain-X", "personal", "memory", h.Expires)
	// Wrong token rejected.
	if _, err := lb.VerifyToken(h.UploadID, "deadbeef"); err == nil {
		t.Fatal("wrong token should fail")
	}
	// Right token accepted.
	if _, err := lb.VerifyToken(h.UploadID, h.Token); err != nil {
		t.Fatalf("right token rejected: %v", err)
	}
}

func TestLocalBackend_AcceptAndComplete(t *testing.T) {
	d := DataDir(t.TempDir())
	if err := EnsureCollectiveSkeleton(d, "personal", "memory"); err != nil {
		t.Fatal(err)
	}
	lb, _ := NewLocalBackend(d, "http://localhost")
	h, _ := lb.NewUpload("brain-Y", 60*1e9)
	lb.RegisterUpload(h.UploadID, "brain-Y", "personal", "memory", h.Expires)

	if _, err := lb.AcceptUpload(h.UploadID, strings.NewReader("hello tar"), 1024); err != nil {
		t.Fatalf("AcceptUpload: %v", err)
	}
	pending, err := lb.CompleteUpload("personal", "memory", "brain-Y", h.UploadID)
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Errorf("pending tar missing: %v", err)
	}
}
