package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/mart"
)

func TestHandleMartList(t *testing.T) {
	dir := t.TempDir()
	reg := mart.OpenRegistry(dir)
	if err := reg.Save(mart.Spec{Name: "taxes", Profile: "personal", Vault: "memory", Dest: "/tmp/taxes", Filters: mart.Filters{Tags: []string{"tax"}}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(mart.Spec{Name: "nocreds", Profile: "gsa", Vault: "memory", Dest: "/tmp/gsa"}); err != nil {
		t.Fatal(err)
	}
	// Creds only for taxes.
	store := mart.Credentials{}
	store.Set(mart.Credential{Profile: "personal", Vault: "memory", API: "https://x", Token: "t"})
	if err := mart.SaveCredentials(dir, store); err != nil {
		t.Fatal(err)
	}

	s := NewServer(ServerDeps{ConfigDir: dir})
	text, isErr := callTool(t, s.handleMartList, nil)
	if isErr {
		t.Fatalf("mart_list errored: %s", text)
	}
	if !strings.Contains(text, "taxes") || !strings.Contains(text, "runnable") {
		t.Errorf("taxes should be runnable in:\n%s", text)
	}
	if !strings.Contains(text, "nocreds") || !strings.Contains(text, "NO CREDS") {
		t.Errorf("nocreds should be marked NO CREDS in:\n%s", text)
	}
	if !strings.Contains(text, "tag=tax") {
		t.Errorf("filters summary missing in:\n%s", text)
	}
}

func TestMartBuild_ArgValidation(t *testing.T) {
	s := NewServer(ServerDeps{ConfigDir: t.TempDir()})
	if text, isErr := callTool(t, s.handleMartBuild, map[string]any{}); !isErr {
		t.Errorf("neither name nor all should error, got: %s", text)
	}
	if _, isErr := callTool(t, s.handleMartBuild, map[string]any{"name": "x", "all": true}); !isErr {
		t.Error("both name and all should error")
	}
}

func TestHandleMartSync_HappyPath(t *testing.T) {
	// Minimal change-feed daemon: one note record, then the caller stops (short
	// page < pageSize).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"records": []map[string]any{
				{"sha": "abc123def456", "kind": "note", "title": "Return 2025", "body": "the body", "updated_at": "2026-06-01T12:00:00Z"},
			},
			"next_after_id": 1,
			"next_since":    "2026-06-01T12:00:00Z",
		})
	}))
	defer ts.Close()

	dir := t.TempDir()
	dest := filepath.Join(t.TempDir(), "_mart")
	reg := mart.OpenRegistry(dir)
	if err := reg.Save(mart.Spec{Name: "taxes", Profile: "personal", Vault: "memory", Dest: dest, SkipAttachments: true}); err != nil {
		t.Fatal(err)
	}
	store := mart.Credentials{}
	store.Set(mart.Credential{Profile: "personal", Vault: "memory", API: ts.URL, Token: "t"})
	if err := mart.SaveCredentials(dir, store); err != nil {
		t.Fatal(err)
	}

	s := NewServer(ServerDeps{ConfigDir: dir})
	text, isErr := callTool(t, s.handleMartSync, map[string]any{"name": "taxes"})
	if isErr {
		t.Fatalf("mart_sync errored: %s", text)
	}
	if !strings.Contains(text, "synced") || !strings.Contains(text, "1 changed record") {
		t.Errorf("expected sync summary, got:\n%s", text)
	}
	if mds, _ := filepath.Glob(filepath.Join(dest, "*.md")); len(mds) < 1 {
		t.Error("sync wrote no note files")
	}
}

func TestHandleMartSync_MissingCreds(t *testing.T) {
	dir := t.TempDir()
	reg := mart.OpenRegistry(dir)
	if err := reg.Save(mart.Spec{Name: "taxes", Profile: "personal", Vault: "memory", Dest: "/tmp/x"}); err != nil {
		t.Fatal(err)
	}
	// No store creds, no matching env → the mart is skipped and reported.
	s := NewServer(ServerDeps{ConfigDir: dir})
	text, _ := callTool(t, s.handleMartSync, map[string]any{"name": "taxes"})
	if !strings.Contains(text, "SKIP") || !strings.Contains(text, "no credentials") {
		t.Errorf("expected a SKIP/no-credentials note, got:\n%s", text)
	}
}
