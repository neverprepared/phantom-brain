package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func runCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return buf.String(), err
}

func TestNewBrainClientFromEnv_MissingEnvErrors(t *testing.T) {
	// Clear the agent-contract env so config.LoadAgent fails.
	t.Setenv("CL_BRAIN_API", "")
	t.Setenv("CL_BRAIN_API_TOKEN", "")
	t.Setenv("CL_WORKSPACE_PROFILE", "")
	t.Setenv("CL_BRAIN_VAULT", "")
	_, err := newBrainClientFromEnv()
	if err == nil {
		t.Fatal("expected error without agent env vars")
	}
	if !strings.Contains(err.Error(), "CL_BRAIN_API") {
		t.Fatalf("error should name the required env vars, got %v", err)
	}
}

func TestClientReflectCmd_WithCandidates(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/brain/reflect" || r.Method != http.MethodGet {
			http.Error(w, "wrong route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]string{
				{"sha": "aaa111", "title": "", "reason": "stale-gate"},
				{"sha": "bbb222", "title": "A Real Title", "reason": "stale-gate"},
			},
		})
	}))
	defer ts.Close()
	setAgentEnv(t, ts.URL)

	out, err := runCmd(t, clientReflectCmd())
	if err != nil {
		t.Fatalf("reflect: %v\n%s", err, out)
	}
	if !strings.Contains(out, "2 forget-candidate(s)") {
		t.Fatalf("expected candidate count, got:\n%s", out)
	}
	if !strings.Contains(out, "aaa111") || !strings.Contains(out, "bbb222") {
		t.Fatalf("expected both SHAs, got:\n%s", out)
	}
	if !strings.Contains(out, "(untitled)") {
		t.Fatalf("blank title should render as (untitled), got:\n%s", out)
	}
	if !strings.Contains(out, "pbrainctl client forget") {
		t.Fatalf("expected apply hint, got:\n%s", out)
	}
}

func TestClientReflectCmd_NoCandidates(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"candidates": []any{}})
	}))
	defer ts.Close()
	setAgentEnv(t, ts.URL)

	out, err := runCmd(t, clientReflectCmd())
	if err != nil {
		t.Fatalf("reflect: %v\n%s", err, out)
	}
	if !strings.Contains(out, "looks clean") {
		t.Fatalf("expected clean message, got:\n%s", out)
	}
}

func TestClientForgetCmd_EmptyShaRejected(t *testing.T) {
	setAgentEnv(t, "http://127.0.0.1:0")
	// A whitespace-only sha must be rejected before any network call.
	_, err := runCmd(t, clientForgetCmd(), "   ")
	if err == nil || !strings.Contains(err.Error(), "non-empty sha") {
		t.Fatalf("expected non-empty sha error, got %v", err)
	}
}

func TestClientForgetCmd_Success(t *testing.T) {
	var gotSHA string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/brain/forget" || r.Method != http.MethodPost {
			http.Error(w, "wrong route", http.StatusNotFound)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotSHA = body["sha"]
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": gotSHA, "forgotten": true})
	}))
	defer ts.Close()
	setAgentEnv(t, ts.URL)

	out, err := runCmd(t, clientForgetCmd(), "deadbeef")
	if err != nil {
		t.Fatalf("forget: %v\n%s", err, out)
	}
	if gotSHA != "deadbeef" {
		t.Fatalf("daemon got sha %q, want deadbeef", gotSHA)
	}
	if !strings.Contains(out, "forgot deadbeef") || !strings.Contains(out, "forgotten=true") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestClientResynthCmd_DryRun(t *testing.T) {
	var gotDryRun bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/brain/resynth" {
			http.Error(w, "wrong route", http.StatusNotFound)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotDryRun, _ = body["dry_run"].(bool)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backlog_count": 2,
			"sample": []map[string]string{
				{"sha": "s1", "title": "T1"},
				{"sha": "s2", "title": ""},
			},
		})
	}))
	defer ts.Close()
	setAgentEnv(t, ts.URL)

	out, err := runCmd(t, clientResynthCmd())
	if err != nil {
		t.Fatalf("resynth: %v\n%s", err, out)
	}
	if !gotDryRun {
		t.Fatal("default invocation must send dry_run=true")
	}
	if !strings.Contains(out, "2 doc(s) stuck") {
		t.Fatalf("expected backlog count, got:\n%s", out)
	}
	if !strings.Contains(out, "(untitled)") {
		t.Fatalf("blank sample title should render as (untitled), got:\n%s", out)
	}
	if !strings.Contains(out, "re-run with --apply") {
		t.Fatalf("expected dry-run hint, got:\n%s", out)
	}
}

func TestClientResynthCmd_Apply(t *testing.T) {
	var gotDryRun = true
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotDryRun, _ = body["dry_run"].(bool)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backlog_count": 5,
			"started":       true,
			"pending":       5,
		})
	}))
	defer ts.Close()
	setAgentEnv(t, ts.URL)

	out, err := runCmd(t, clientResynthCmd(), "--apply")
	if err != nil {
		t.Fatalf("resynth --apply: %v\n%s", err, out)
	}
	if gotDryRun {
		t.Fatal("--apply must send dry_run=false")
	}
	if !strings.Contains(out, "started re-synthesis of 5") {
		t.Fatalf("expected start notice, got:\n%s", out)
	}
}

func TestClientResynthCmd_ApplyNothingToDo(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backlog_count": 0,
			"started":       false,
		})
	}))
	defer ts.Close()
	setAgentEnv(t, ts.URL)

	out, err := runCmd(t, clientResynthCmd(), "--apply")
	if err != nil {
		t.Fatalf("resynth --apply: %v\n%s", err, out)
	}
	if !strings.Contains(out, "nothing to re-synthesize") {
		t.Fatalf("expected nothing-to-do notice, got:\n%s", out)
	}
}
