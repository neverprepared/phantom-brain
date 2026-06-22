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
	"testing"
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil { t.Fatal(err) }
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil { t.Fatal(err) }
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status=%d, want 409", resp.StatusCode)
	}
}

// --- Merge init → upload → complete end-to-end -----------------------

// Phase 6: /merge handlers + LocalBackend HMAC upload tests removed.
// Writes go agent → daemon POST → OS directly; there's no upload-
// token handshake or death-payload reaper to exercise.

// --- Maintenance -----------------------------------------------------

func TestHandler_Maintenance_EnterExitGet(t *testing.T) {
	_, baseURL, tok, cleanup := startTestRig(t)
	defer cleanup()

	enter := func() int {
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/brain/maintenance/enter", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil { t.Fatal(err) }
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := enter(); got != http.StatusOK {
		t.Fatalf("enter status=%d", got)
	}

	getState := func() bool {
		req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/brain/maintenance", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil { t.Fatal(err) }
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil { t.Fatal(err) }
	resp.Body.Close()
	if getState() {
		t.Fatal("expected maintenance false after exit")
	}
}

// Local backend HMAC tests removed in Phase 6 with the death-payload
// upload-token handshake.
