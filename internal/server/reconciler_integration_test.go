//go:build integration

// Integration coverage for the pb_records projection reconciler (design-review
// item #5). Build-tagged OFF by default so `make test` neither compiles this
// file nor needs Docker. Run with:
//
//	GOFLAGS="-tags=sqlite_fts5,integration" go test ./internal/server/ -run ReconcileProjection -count=1 -v
//
// Reuses the Phase A harness (startPGForServer / startOSForServer /
// newPGTestDaemon / binding / pgstore.Provision). Proves the reconciler:
//  1. Missing (SoR→OS): a SoR record whose projection doc was deleted directly
//     is re-projected.
//  2. Orphan (OS→SoR): a pb_records doc with no SoR record is deleted.
//  3. Healthy: an in-sync record is left untouched (not re-projected, not
//     deleted).
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/osproject"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// osSearchTotal runs a search body against a physical index and returns the
// total hit count.
func osSearchTotal(ctx context.Context, c *osearch.Client, index string, body map[string]any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	resp, err := c.API().Search(ctx, &osapi.SearchReq{
		Indices: []string{index},
		Body:    bytes.NewReader(b),
	})
	if err != nil {
		return 0, err
	}
	return resp.Hits.Total.Value, nil
}

// osDocExists polls (with a short deadline to absorb the 1s refresh) for the
// presence/absence of a pb_records doc by (profile, vault, sha), returning the
// final observed state at the deadline.
func osDocExists(ctx context.Context, t *testing.T, c *osearch.Client, prefix, profile, vault, sha string, want bool) bool {
	t.Helper()
	id := osearch.DocID(profile, vault, sha)
	deadline := time.Now().Add(10 * time.Second)
	var last bool
	for {
		last = indexHasDoc(ctx, t, c, prefix, id)
		if last == want || time.Now().After(deadline) {
			return last
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// indexHasDoc reports whether the pb_records index holds a doc at _id.
func indexHasDoc(ctx context.Context, t *testing.T, c *osearch.Client, prefix, id string) bool {
	t.Helper()
	name := osearch.IndexNameWithPrefix(prefix, osproject.LogicalRecords)
	// A term query on _id, refreshed, is the simplest existence probe that
	// doesn't depend on Document.Get error shapes.
	total, err := osSearchTotal(ctx, c, name, map[string]any{
		"query": map[string]any{"ids": map[string]any{"values": []string{id}}},
	})
	if err != nil {
		t.Fatalf("existence search %s: %v", id, err)
	}
	return total > 0
}

func TestReconcileProjection_Integration(t *testing.T) {
	ctx := context.Background()
	baseDSN := startPGForServer(ctx, t)
	osc := startOSForServer(ctx, t)

	const profile, vault, prefix = "tctest", "main", "pbd_recon_"

	if err := pgstore.Provision(ctx, baseDSN, profile); err != nil {
		t.Fatalf("provision %s db: %v", profile, err)
	}

	b := binding(profile, vault, prefix)
	d := newPGTestDaemon(t, b)
	d.osBase, d.osClient = osc, osc
	d.pgBaseDSN = baseDSN
	if err := d.buildBindingDeps(); err != nil {
		t.Fatalf("buildBindingDeps: %v", err)
	}
	t.Cleanup(d.closePGProfiles)

	view, err := d.resolvePG(b)
	if err != nil {
		t.Fatalf("resolvePG: %v", err)
	}
	q := pgstore.New(view.Pool())

	// A refresh-forced projector so seeding + repairs are immediately visible.
	proj := osproject.NewWithRefresh(osc, prefix)
	if err := osproject.EnsureIndex(ctx, osc, prefix); err != nil {
		t.Fatalf("ensure index: %v", err)
	}

	// Seed three SoR records. healthy + missing are projected up front; orphan
	// is projected WITHOUT a SoR record (a leftover from an exhausted forget).
	seed := func(sha, title string) pgdb.Record {
		rec, err := q.UpsertRecord(ctx, pgdb.UpsertRecordParams{
			Profile: profile, Vault: vault, Sha: sha, Kind: "note", Title: title,
			RawBody: pgtype.Text{String: "body of " + title, Valid: true},
			Source:  []string{"task:recon"}, Tags: []string{"recon"},
		})
		if err != nil {
			t.Fatalf("UpsertRecord %s: %v", sha, err)
		}
		return rec
	}
	healthy := seed("recon-healthy", "Healthy")
	missing := seed("recon-missing", "Missing")

	// Project healthy + missing.
	if err := proj.Project(ctx, healthy); err != nil {
		t.Fatalf("project healthy: %v", err)
	}
	if err := proj.Project(ctx, missing); err != nil {
		t.Fatalf("project missing: %v", err)
	}

	// Simulate a lost projection for "missing" by deleting its doc directly.
	if err := proj.DeleteProjection(ctx, profile, vault, "recon-missing"); err != nil {
		t.Fatalf("delete missing doc: %v", err)
	}

	// Insert an orphan doc: a pb_records entry whose SHA has no SoR record.
	orphanRec := pgdb.Record{
		ID: 9999, Profile: profile, Vault: vault, Sha: "recon-orphan",
		Kind: "note", Title: "Orphan",
	}
	if err := proj.Project(ctx, orphanRec); err != nil {
		t.Fatalf("project orphan: %v", err)
	}

	// Pre-conditions.
	if osDocExists(ctx, t, osc, prefix, profile, vault, "recon-healthy", true) != true {
		t.Fatal("precondition: healthy doc should exist")
	}
	if osDocExists(ctx, t, osc, prefix, profile, vault, "recon-missing", false) != false {
		t.Fatal("precondition: missing doc should be absent")
	}
	if osDocExists(ctx, t, osc, prefix, profile, vault, "recon-orphan", true) != true {
		t.Fatal("precondition: orphan doc should exist")
	}

	// Run one reconcile cycle against the real SoR + a refresh-forced
	// projection surface (so repairs are immediately searchable).
	r := NewReconciler(nil, time.Hour) // interval unused — we call reconcileBinding directly
	r.Resolve = func(p, v string) (reconcileSoR, reconcileProjection, bool) {
		return &pgReconcileSoR{view: view},
			&osReconcileProjection{client: osc, prefix: prefix, projector: proj},
			true
	}
	if err := r.reconcileBinding(ctx, VaultKey{Profile: profile, Vault: vault}); err != nil {
		t.Fatalf("reconcileBinding: %v", err)
	}

	// Post-conditions: missing re-projected, orphan deleted, healthy untouched.
	if osDocExists(ctx, t, osc, prefix, profile, vault, "recon-missing", true) != true {
		t.Error("missing doc should be re-projected after reconcile")
	}
	if osDocExists(ctx, t, osc, prefix, profile, vault, "recon-orphan", false) != false {
		t.Error("orphan doc should be deleted after reconcile")
	}
	if osDocExists(ctx, t, osc, prefix, profile, vault, "recon-healthy", true) != true {
		t.Error("healthy doc should remain after reconcile")
	}
}
