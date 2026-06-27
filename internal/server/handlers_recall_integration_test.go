//go:build integration

// Phase C integration coverage for POST /api/brain/recall — the
// always-online recall endpoint backed by the per-binding Postgres
// projection (pb_records) Recaller. Build-tagged OFF by default so
// `make test` neither compiles this file nor needs Docker. Run with:
//
//	GOFLAGS="-tags=sqlite_fts5,integration" go test ./internal/server/ -run Recall -count=1 -v
//
// Reuses the harness from pg_binding_views_integration_test.go
// (startPGForServer, startOSForServer, newPGTestDaemon, binding) plus
// pgvector/pgvector:pg17 + opensearchproject/opensearch:2.18.0.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/osproject"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// authedBinding mirrors the harness `binding` constructor but also sets
// a bearer token so AuthMiddleware admits requests for this binding.
func authedBinding(profile, vault, prefix, token string) VaultBinding {
	b := binding(profile, vault, prefix)
	b.Auth.BearerToken = token
	return b
}

// seedRecord projects one pgdb.Record into the binding's pb_records
// index via the per-binding Projector (wait-for-refresh so it's
// immediately searchable). Bypasses River — direct projection is the
// shortest path to a queryable doc.
func seedRecord(t *testing.T, ctx context.Context, osc *osearch.Client, prefix, sha, title, body string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	rec := pgdb.Record{
		ID:        1,
		Profile:   "tctest",
		Vault:     "main",
		Sha:       sha,
		Kind:      "note",
		Title:     title,
		Body:      pgtype.Text{String: body, Valid: true},
		RawBody:   pgtype.Text{String: "RAW: not projected", Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
	// Refresh-on-write projector so the doc is searchable immediately.
	proj := osproject.NewWithRefresh(osc, prefix)
	if err := proj.Project(ctx, rec); err != nil {
		t.Fatalf("project record %s: %v", sha, err)
	}
}

// doRecall drives POST /api/brain/recall through the daemon's chi router
// with the supplied bearer token and JSON body, returning status + body.
func doRecall(t *testing.T, d *Daemon, token string, body RecallRequest) (int, RecallResponse) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal recall body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/brain/recall", bytes.NewReader(raw))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	d.buildRouter().ServeHTTP(rec, req)

	var out RecallResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode recall response: %v (body=%s)", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

func TestRecallEndpoint_Integration(t *testing.T) {
	ctx := context.Background()
	baseDSN := startPGForServer(ctx, t)
	osc := startOSForServer(ctx, t)

	if err := pgstore.Provision(ctx, baseDSN, "tctest"); err != nil {
		t.Fatalf("provision tctest db: %v", err)
	}

	const token = "recall-test-token-aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b := authedBinding("tctest", "main", "pbc_recall_", token)
	d := newPGTestDaemon(t, b)
	d.osBase = osc
	d.osClient = osc
	d.pgBaseDSN = baseDSN

	d.buildBindingDeps()
	t.Cleanup(d.closePGProfiles)

	// Confirm the binding's PG view (and thus its pb_records index) is
	// live before seeding directly into the index.
	if _, err := d.resolvePG(b); err != nil {
		t.Fatalf("resolvePG: %v", err)
	}

	// Seed two distinct records into the binding's prefixed index.
	seedRecord(t, ctx, osc, b.Storage.IndexPrefix,
		"sha-loop", "Loop Engineering", "the new meta for AI coding agents")
	seedRecord(t, ctx, osc, b.Storage.IndexPrefix,
		"sha-cooking", "Sourdough Bread", "fermentation and baking technique")

	t.Run("ReturnsSeededDocs", func(t *testing.T) {
		code, resp := doRecall(t, d, token, RecallRequest{Query: "engineering", Limit: 10})
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		if len(resp.Hits) == 0 {
			t.Fatal("expected at least one hit")
		}
		var found bool
		for _, h := range resp.Hits {
			if h.SHA == "sha-loop" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected sha-loop in hits, got %+v", resp.Hits)
		}
	})

	t.Run("TextRanksMatchingDoc", func(t *testing.T) {
		code, resp := doRecall(t, d, token, RecallRequest{Query: "sourdough fermentation", Limit: 10})
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		if len(resp.Hits) == 0 {
			t.Fatal("expected hits for sourdough query")
		}
		if resp.Hits[0].SHA != "sha-cooking" {
			t.Errorf("expected sha-cooking ranked first, got %s (%+v)", resp.Hits[0].SHA, resp.Hits)
		}
	})

	t.Run("EmptyQueryIs400", func(t *testing.T) {
		code, _ := doRecall(t, d, token, RecallRequest{Query: "   "})
		if code != http.StatusBadRequest {
			t.Errorf("expected 400 for empty query, got %d", code)
		}
	})

	t.Run("NoPGBindingIs503", func(t *testing.T) {
		// A binding with a token but NO Postgres view ⇒ resolvePG
		// returns ErrPostgresDisabled ⇒ 503.
		const tok2 = "recall-nopg-token-bbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		nopg := authedBinding("tctest", "nopg", "pbc_nopg_", tok2)
		d2 := newPGTestDaemon(t, nopg)
		d2.osBase = osc
		d2.osClient = osc
		// pgBaseDSN intentionally empty ⇒ PG disabled.
		d2.buildBindingDeps()

		code, _ := doRecall(t, d2, tok2, RecallRequest{Query: "anything"})
		if code != http.StatusServiceUnavailable {
			t.Errorf("expected 503 when PG disabled, got %d", code)
		}
	})
}
