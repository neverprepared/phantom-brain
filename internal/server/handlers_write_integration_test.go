//go:build integration

// Phase D1 integration coverage for the write-HANDLER happy paths. The
// pre-D1 unit suite asserted 202 + an in-memory fake-OS doc for
// perceive/learn/attach; post-D1 those handlers write to the live Postgres
// SoR (via writeRecordRaw), so the happy paths cannot run against a fake
// and moved here. Build-tagged OFF by default (`make test` neither
// compiles nor needs Docker). Run with:
//
//	GOFLAGS="-tags=sqlite_fts5,integration" go test ./internal/server/ -run HandlerWrite -count=1 -v
//
// Reuses the Phase A harness (startPGForServer / startOSForServer /
// newPGTestDaemon / binding / pgstore.Provision) and drives the REAL chi
// router over HTTP so auth + routing + validation + SoR write are all in
// the loop. Proves:
//   - POST /perceive → 202 + an unsynthesised SoR record (url/title/tags).
//   - POST /learn    → 202 + an unsynthesised SoR record.
//
// NOTES / follow-ups:
//   - The /attach happy path additionally needs a live MinIO backend
//     (PutAttachmentWithTags), which this harness does not stand up.
//     Attach stays covered at the validation level in the unit suite
//     (handlers_write_test.go) and at the SoR level by the dual_write
//     integration test; an end-to-end /attach HTTP happy path is a
//     follow-up once a MinIO test container is wired into this harness.
//   - handleLearn stamps Reliability=medium + GateReason="curated (...)"
//     on the in-memory doc, but summaryDocToUpsertParams does NOT carry
//     those into the raw upsert — the curated-medium signal is re-derived
//     at synth time (gateSourceType's curated short-circuit). So this test
//     asserts the raw record landed, not its reliability; the curated path
//     is a synth-time concern.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// bindingWithToken is binding() plus a bearer token so AuthMiddleware can
// resolve the request to the binding.
func bindingWithToken(profile, vault, prefix, token string) VaultBinding {
	b := binding(profile, vault, prefix)
	b.Auth = VaultAuth{BearerToken: token}
	return b
}

// newHandlerWriteDaemon builds a PG+OS-backed daemon with the real router
// mounted, returning the httptest base URL + the resolved PG view. synth
// is a no-op (the handler enqueues but we assert the raw SoR write, not
// synth output).
func newHandlerWriteDaemon(t *testing.T, baseDSN string, osc *osearch.Client, b VaultBinding) (string, *pgBindingView) {
	t.Helper()
	d := newPGTestDaemon(t, b)
	d.Logger = slog.New(slog.DiscardHandler)
	d.osBase, d.osClient, d.osExport = osc, osc, osc
	d.pgBaseDSN = baseDSN
	d.synth = noopSynthQueue{}
	if err := d.buildBindingDeps(); err != nil {
		t.Fatalf("buildBindingDeps: %v", err)
	}
	t.Cleanup(d.closePGProfiles)
	d.router = d.buildRouter()
	ts := httptest.NewServer(d.router)
	t.Cleanup(ts.Close)

	view, err := d.resolvePG(b)
	if err != nil {
		t.Fatalf("resolvePG: %v", err)
	}
	return ts.URL, view
}

func postJSON(t *testing.T, url, token, path string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, url+path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func TestHandlerWrite_Integration(t *testing.T) {
	ctx := context.Background()
	baseDSN := startPGForServer(ctx, t)
	osc := startOSForServer(ctx, t)

	if err := pgstore.Provision(ctx, baseDSN, "hwtest"); err != nil {
		t.Fatalf("provision hwtest db: %v", err)
	}

	t.Run("Perceive", func(t *testing.T) {
		token := "hwtok_perceive"
		b := bindingWithToken("hwtest", "main", "hw_perc_", token)
		url, view := newHandlerWriteDaemon(t, baseDSN, osc, b)

		sha := "1111000000000000000000000000000000000000000000000000000000000001"
		resp := postJSON(t, url, token, "/api/brain/perceive", PerceiveRequest{
			SHA: sha, Title: "Quarterly invoices", Body: "We reconciled every invoice.",
			URL: "https://example.com", Tags: []string{"a"},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}

		q := pgstore.New(view.Pool())
		rec, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{Profile: "hwtest", Vault: "main", Sha: sha})
		if err != nil {
			t.Fatalf("GetRecordBySHA: %v", err)
		}
		if rec.Synthesised {
			t.Error("perceive must leave Synthesised=false")
		}
		if rec.SourceUrl.String != "https://example.com" {
			t.Errorf("SourceUrl = %q", rec.SourceUrl.String)
		}
		if rec.Title != "Quarterly invoices" {
			t.Errorf("Title = %q", rec.Title)
		}
	})

	t.Run("Learn", func(t *testing.T) {
		token := "hwtok_learn"
		b := bindingWithToken("hwtest", "main", "hw_learn_", token)
		url, view := newHandlerWriteDaemon(t, baseDSN, osc, b)

		sha := "2222000000000000000000000000000000000000000000000000000000000002"
		resp := postJSON(t, url, token, "/api/brain/learn", LearnRequest{
			SHA: sha, Title: "Curated", Body: "A curated note.", Tags: []string{"curated"},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}

		q := pgstore.New(view.Pool())
		rec, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{Profile: "hwtest", Vault: "main", Sha: sha})
		if err != nil {
			t.Fatalf("GetRecordBySHA: %v", err)
		}
		if rec.Synthesised {
			t.Error("learn must leave Synthesised=false (distill runs at synth)")
		}
		if rec.Title != "Curated" {
			t.Errorf("Title = %q", rec.Title)
		}
	})
}
