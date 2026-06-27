package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Phase D1 testability: the daemon's startup now makes the Postgres SoR
// mandatory (buildBindingDeps → buildPGBindingDeps errors without a DSN),
// so Start() cannot stand up in the unit suite. The birth-claim /
// maintenance / snapshot HTTP handlers, however, are purely DataDir
// (filesystem) backed — they never touch Postgres. So rather than drop
// them to integration, we mount the REAL chi router against a Daemon built
// WITHOUT Start() (newRouterRig): a loaded registry for AuthMiddleware, a
// DataDir, a no-op synth queue, and a fake AttachmentStore. That keeps the
// full auth + routing + handler path under unit coverage, no Docker.

// routerRig is a daemon with the real router mounted over httptest, built
// without Start() (and therefore without Postgres).
type routerRig struct {
	d      *Daemon
	server *httptest.Server
	token  string
}

func newRouterRig(t *testing.T) *routerRig {
	t.Helper()
	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	tok := seedVault(t, cfgDir, "personal", "memory", "")

	reg := NewRegistry()
	if _, err := reg.Load(LoadOpts{ConfigDir: cfgDir, Defaults: VaultDefaults{RetentionGens: 30}}); err != nil {
		t.Fatalf("registry load: %v", err)
	}

	d := &Daemon{
		DataDir:             DataDir(dataDir),
		Logger:              slog.New(slog.DiscardHandler),
		registry:            reg,
		synth:               noopSynthQueue{},
		attach:              newFakeAttach(),
		allowSharedFallback: true, // resolveAttach returns d.attach without per-binding views
	}
	d.router = d.buildRouter()

	ts := httptest.NewServer(d.router)
	t.Cleanup(ts.Close)
	return &routerRig{d: d, server: ts, token: tok}
}

func (r *routerRig) do(t *testing.T, method, path string, body []byte) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, r.server.URL+path, rdr)
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// --- birth/claim --------------------------------------------------

func TestHandler_BirthClaim_HappyPath(t *testing.T) {
	r := newRouterRig(t)
	seedCollective(t, r.d.DataDir, "personal", "memory", "# hi\n")
	if _, err := BuildSnapshot(r.d.DataDir, "personal", "memory", 30); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(birthClaimRequest{BrainID: "brain-1", Gen: 1, TTLSecs: 3600})
	resp := r.do(t, http.MethodPost, "/api/brain/birth/claim", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	// Marker file landed.
	marker := filepath.Join(r.d.DataDir.StagedDir("personal", "memory"), "snapshot-1", ".claims", "brain-1")
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("claim marker missing: %v", err)
	}
}

func TestHandler_BirthClaim_StaleGenReturns409(t *testing.T) {
	r := newRouterRig(t)
	body, _ := json.Marshal(birthClaimRequest{BrainID: "brain-x", Gen: 999})
	resp := r.do(t, http.MethodPost, "/api/brain/birth/claim", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status=%d, want 409", resp.StatusCode)
	}
}

// --- maintenance --------------------------------------------------

func TestHandler_Maintenance_EnterExitGet(t *testing.T) {
	r := newRouterRig(t)

	enter := func() int {
		resp := r.do(t, http.MethodPost, "/api/brain/maintenance/enter", nil)
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := enter(); got != http.StatusOK {
		t.Fatalf("enter status=%d", got)
	}

	getState := func() bool {
		resp := r.do(t, http.MethodGet, "/api/brain/maintenance", nil)
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		v, _ := m["maintenance"].(bool)
		return v
	}
	if !getState() {
		t.Fatal("expected maintenance true after enter")
	}

	resp := r.do(t, http.MethodPost, "/api/brain/maintenance/exit", nil)
	resp.Body.Close()
	if getState() {
		t.Fatal("expected maintenance false after exit")
	}
}

// --- trace --------------------------------------------------------

// handleTrace is unchanged by Phase D1: it appends a structured line to
// the per-vault Wiki/_log.md on the daemon's DataDir and never touches
// OpenSearch or Postgres. So it stays under unit coverage via newRouterRig.
func TestHandlerTrace_AppendsLog(t *testing.T) {
	r := newRouterRig(t)

	body, _ := json.Marshal(TraceRequest{Kind: "decision", Message: "chose option B"})
	resp := r.do(t, http.MethodPost, "/api/brain/trace", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("trace status=%d, want 202", resp.StatusCode)
	}

	logPath := filepath.Join(r.d.DataDir.VaultDir("personal", "memory"), "Wiki", "_log.md")
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read _log.md: %v", err)
	}
	if !bytes.Contains(got, []byte("chose option B")) || !bytes.Contains(got, []byte("decision")) {
		t.Errorf("log missing trace entry; got %q", got)
	}
}
