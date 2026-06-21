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
	"time"

	pbbrain "github.com/neverprepared/mcp-phantom-brain/internal/brain"
)

// TestIntegration_TwoVaultsIsolatedEndToEnd is the Phase 2 day 6
// smoke test: configure two vaults (personal/memory + work/core),
// issue distinct bearer tokens, exercise the full
// init→upload→complete→reap→synth→snapshot pipeline against each,
// and verify the vaults' artefacts do not bleed across.
//
// Uses a fake `claude` CLI (PATH-injected) so the synthesizer's
// gate calls return a fixed JSON verdict without needing the real
// Claude binary on the test host.
func TestIntegration_TwoVaultsIsolatedEndToEnd(t *testing.T) {
	_ = writeFakeClaude(t, `{"reliability":"high","topic":"tools","reason":"primary source"}`)

	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	writeServerConfig(t, cfgDir, "[server]\nport = 0\n")
	tokA := seedVault(t, cfgDir, "personal", "memory", "")
	tokB := seedVault(t, cfgDir, "work", "core", "")

	d, err := Start(StartOpts{
		ConfigDir: cfgDir,
		DataDir:   DataDir(dataDir),
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer d.Shutdown(context.Background())

	ts := httptest.NewServer(d.Router())
	defer ts.Close()
	// Rewire baseURL so /merge/init URLs route to httptest.
	if lb, ok := d.storage.(*LocalBackend); ok {
		lb.baseURL = ts.URL
	}

	// Helper: run the full ship + reap + synth for one (token, vault, brain_id, raw_body).
	ship := func(t *testing.T, tok string, key VaultKey, brainID, rawBody string) {
		t.Helper()
		payload := makeDeathPayload(t, pbbrain.Manifest{
			BrainID: brainID, Profile: key.Profile, Vault: key.Vault,
		}, map[string]string{
			"vault/Raw/curated/" + brainID + "-notes.md": rawBody,
		})
		// init
		initBody, _ := json.Marshal(mergeInitRequest{BrainID: brainID, TTLSecs: 60, PayloadSize: int64(len(payload))})
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/brain/merge/init", bytes.NewReader(initBody))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("[%s] init: %v", brainID, err)
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("[%s] init status=%d body=%s", brainID, resp.StatusCode, b)
		}
		var initResp mergeInitResponse
		_ = json.NewDecoder(resp.Body).Decode(&initResp)
		resp.Body.Close()

		// upload
		put, _ := http.NewRequest(http.MethodPut, initResp.URL, bytes.NewReader(payload))
		resp, err = http.DefaultClient.Do(put)
		if err != nil {
			t.Fatalf("[%s] upload: %v", brainID, err)
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("[%s] upload status=%d body=%s", brainID, resp.StatusCode, b)
		}
		resp.Body.Close()

		// complete
		completeBody, _ := json.Marshal(mergeCompleteRequest{BrainID: brainID})
		req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/brain/merge/complete/"+initResp.UploadID, bytes.NewReader(completeBody))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("[%s] complete: %v", brainID, err)
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("[%s] complete status=%d body=%s", brainID, resp.StatusCode, b)
		}
		resp.Body.Close()

		// reap + synth, manually (the runner loops are running but
		// we don't want to wait on their tickers for a deterministic
		// smoke test).
		binding, _ := d.registry.LookupByVault(key)
		if _, err := ReapOnce(d.DataDir, binding, slog.New(slog.DiscardHandler), &sync.Mutex{}); err != nil {
			t.Fatalf("[%s] ReapOnce: %v", brainID, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := SynthesizeOne(ctx, d.DataDir, binding, slog.New(slog.DiscardHandler), &sync.Mutex{}); err != nil {
			t.Fatalf("[%s] SynthesizeOne: %v", brainID, err)
		}
	}

	keyA := VaultKey{Profile: "personal", Vault: "memory"}
	keyB := VaultKey{Profile: "work", Vault: "core"}
	bodyA := "# personal\n\nThe **Personal Notes** project covers... " + strings.Repeat("more letters and content. ", 20)
	bodyB := "# work\n\nThe **Work Core** project covers... " + strings.Repeat("more text content here. ", 20)

	ship(t, tokA, keyA, "brain-personal-1", bodyA)
	ship(t, tokB, keyB, "brain-work-1", bodyB)

	// --- Isolation assertions ---

	// Each vault has exactly one summary page.
	for _, key := range []VaultKey{keyA, keyB} {
		summaryDir := filepath.Join(d.DataDir.VaultDir(key.Profile, key.Vault), "Wiki", "summaries")
		entries, _ := os.ReadDir(summaryDir)
		if len(entries) != 1 {
			t.Errorf("[%s] expected 1 summary, got %d", key, len(entries))
		}
	}

	// Personal's summary must NOT mention "Work Core" and vice versa.
	a := readAllSummaries(t, d.DataDir, keyA)
	b := readAllSummaries(t, d.DataDir, keyB)
	if strings.Contains(a, "Work Core") {
		t.Errorf("personal vault leaked work content: %s", a)
	}
	if strings.Contains(b, "Personal Notes") {
		t.Errorf("work vault leaked personal content: %s", b)
	}

	// Cross-token isolation: tokA cannot read keyB's snapshot.
	if _, err := BuildSnapshot(d.DataDir, keyB.Profile, keyB.Vault, 30); err != nil {
		t.Fatalf("build B snapshot: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/brain/snapshot/current", nil)
	req.Header.Set("Authorization", "Bearer "+tokA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { t.Fatal(err) }
	defer resp.Body.Close()
	var aSnap SnapshotManifest
	_ = json.NewDecoder(resp.Body).Decode(&aSnap)
	if aSnap.Vault == "core" || aSnap.Profile == "work" {
		t.Errorf("tokA snapshot leaked vault B: %+v", aSnap)
	}

	// Each vault's ledger has exactly one row.
	for _, key := range []VaultKey{keyA, keyB} {
		l, _ := OpenLedger(d.DataDir, key.Profile, key.Vault)
		list, _ := l.List(10)
		_ = l.Close()
		if len(list) != 1 {
			t.Errorf("[%s] ledger has %d rows, want 1", key, len(list))
		}
	}

	// Each vault's _log.md exists and mentions its own title.
	logA, _ := os.ReadFile(filepath.Join(d.DataDir.VaultDir(keyA.Profile, keyA.Vault), "Wiki", "_log.md"))
	logB, _ := os.ReadFile(filepath.Join(d.DataDir.VaultDir(keyB.Profile, keyB.Vault), "Wiki", "_log.md"))
	if !strings.Contains(string(logA), "brain-personal-1-notes") {
		t.Errorf("log A missing entry: %s", logA)
	}
	if !strings.Contains(string(logB), "brain-work-1-notes") {
		t.Errorf("log B missing entry: %s", logB)
	}
}

// readAllSummaries concatenates every Wiki/summaries/*.md so the
// isolation assertion can string-search across the vault.
func readAllSummaries(t *testing.T, d DataDir, key VaultKey) string {
	t.Helper()
	dir := filepath.Join(d.VaultDir(key.Profile, key.Vault), "Wiki", "summaries")
	entries, _ := os.ReadDir(dir)
	var b strings.Builder
	for _, e := range entries {
		raw, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		b.Write(raw)
	}
	return b.String()
}
