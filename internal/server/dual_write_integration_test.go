//go:build integration

// Phase D1 integration coverage for the Postgres-SoR write path (the sole
// authoritative store). Build-tagged OFF by default so `make test` neither
// compiles this file nor needs Docker. Run with:
//
//	GOFLAGS="-tags=sqlite_fts5,integration" go test ./internal/server/ -run SoRWrite -count=1 -v
//
// Reuses the Phase A harness (startPGForServer / startOSForServer /
// newPGTestDaemon / binding / pgstore.Provision). Proves:
//  1. Raw write   → record lands in PG + projects to pb_records.
//  2. Synth write → synthesised=true + body + entity + link, and pb_records
//     re-projected with the distilled body.
//  3. Missing raw record on synth → write returns an error + the counter
//     increments (the raw write never landed; a re-enqueue reconciles).
//  4. PG disabled → the SoR write returns an error + counter increments
//     (PG is mandatory in D1, so this is a real configuration error).
package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/osproject"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// dwEmbedding builds a 768-dim vector with a single hot dimension so the
// kNN reach is unambiguous under cosine.
func dwEmbedding(hotDim int) []float32 {
	v := make([]float32, osearch.EmbeddingDim)
	for i := range v {
		v[i] = 0.001
	}
	v[hotDim] = 1.0
	return v
}

// dwSummaryDoc builds a raw (pre-synth) SummaryDoc the way the perceive
// handler does.
func dwSummaryDoc(profile, vault, sha, title, body string, emb []float32) osearch.SummaryDoc {
	now := time.Now().UTC()
	return osearch.SummaryDoc{
		Profile:     profile,
		Vault:       vault,
		SHA:         sha,
		Kind:        osearch.KindNote,
		Title:       title,
		RawBody:     body,
		Tags:        []string{"phased1"},
		CreatedAt:   now,
		UpdatedAt:   now,
		Synthesised: false,
		Embedding:   emb,
	}
}

// waitForRecord polls GetRecordBySHA until the row exists or the deadline
// passes.
func waitForRecord(ctx context.Context, t *testing.T, view *pgBindingView, profile, vault, sha string) pgdb.Record {
	t.Helper()
	q := pgstore.New(view.Pool())
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{Profile: profile, Vault: vault, Sha: sha})
		if err == nil {
			return rec
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("GetRecordBySHA: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("record %s never appeared in PG", sha)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForRecall polls the binding's Recaller until a hit for sha appears
// (River drains the projection job, then the index refreshes ~1s).
func waitForRecall(ctx context.Context, t *testing.T, view *pgBindingView, profile, vault, text, sha string) osproject.RecallHit {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		hits, err := view.Recaller().Recall(ctx, osproject.RecallQuery{
			Profile: profile, Vault: vault, Text: text, Size: 10,
		})
		if err == nil {
			for _, h := range hits {
				if h.SHA == sha {
					return h
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("sha %s never appeared in pb_records recall for text %q", sha, text)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func TestSoRWrite_Integration(t *testing.T) {
	ctx := context.Background()
	baseDSN := startPGForServer(ctx, t)
	osc := startOSForServer(ctx, t)

	if err := pgstore.Provision(ctx, baseDSN, "tctest"); err != nil {
		t.Fatalf("provision tctest db: %v", err)
	}

	t.Run("RawWrite", func(t *testing.T) {
		b := binding("tctest", "main", "pbd_raw_")
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

		doc := dwSummaryDoc("tctest", "main",
			"aaaa000000000000000000000000000000000000000000000000000000000001",
			"Quarterly invoices", "We reconciled every quarterly invoice.", dwEmbedding(5))

		if err := d.writeRecordRaw(ctx, b, doc); err != nil {
			t.Fatalf("writeRecordRaw: %v", err)
		}
		if d.DualWriteFailureCount() != 0 {
			t.Fatalf("no failure expected on happy raw write, got %d", d.DualWriteFailureCount())
		}

		rec := waitForRecord(ctx, t, view, "tctest", "main", doc.SHA)
		if rec.Synthesised {
			t.Fatal("raw write must NOT be synthesised yet")
		}
		if rec.Title != "Quarterly invoices" {
			t.Fatalf("title mismatch: %q", rec.Title)
		}
		// Projection lands async via River → assert it shows in pb_records.
		waitForRecall(ctx, t, view, "tctest", "main", "invoice", doc.SHA)
	})

	t.Run("SynthWrite", func(t *testing.T) {
		b := binding("tctest", "main", "pbd_synth_")
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

		sha := "bbbb000000000000000000000000000000000000000000000000000000000002"
		doc := dwSummaryDoc("tctest", "main", sha,
			"Ledger reconciliation", "stardust accounting raw body", dwEmbedding(7))
		if err := d.writeRecordRaw(ctx, b, doc); err != nil {
			t.Fatalf("writeRecordRaw: %v", err)
		}
		waitForRecord(ctx, t, view, "tctest", "main", sha)

		// Now the synth write: distilled body + reliability/topic + one entity.
		if err := d.writeSynthResult(ctx, b, "tctest", "main", sha, synthResult{
			Body:           "The general ledger was reconciled against vendor statements thoroughly.",
			Reliability:    "high",
			Topic:          "memory",
			GateReason:     "test",
			Embedding:      dwEmbedding(7),
			EmbeddingModel: "nomic-embed-text",
			EntityNames:    map[string]string{"acme-corp": "Acme Corp"},
		}); err != nil {
			t.Fatalf("writeSynthResult: %v", err)
		}
		if d.DualWriteFailureCount() != 0 {
			t.Fatalf("no failure expected on happy synth write, got %d", d.DualWriteFailureCount())
		}

		q := pgstore.New(view.Pool())
		rec, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{Profile: "tctest", Vault: "main", Sha: sha})
		if err != nil {
			t.Fatalf("GetRecordBySHA post-synth: %v", err)
		}
		if !rec.Synthesised {
			t.Fatal("record must be synthesised after the write")
		}
		if rec.Body.String != "The general ledger was reconciled against vendor statements thoroughly." {
			t.Fatalf("body not written: %q", rec.Body.String)
		}
		if rec.Reliability.String != "high" || rec.Topic.String != "memory" {
			t.Fatalf("reliability/topic not written: %q / %q", rec.Reliability.String, rec.Topic.String)
		}

		// Entity + link exist.
		ent, err := q.GetEntityBySlug(ctx, pgdb.GetEntityBySlugParams{Profile: "tctest", Vault: "main", Slug: "acme-corp"})
		if err != nil {
			t.Fatalf("GetEntityBySlug: %v", err)
		}
		if ent.Name != "Acme Corp" {
			t.Fatalf("entity name mismatch: %q", ent.Name)
		}
		linked, err := q.RecordsMentioningEntity(ctx, pgdb.RecordsMentioningEntityParams{Profile: "tctest", Vault: "main", Slug: "acme-corp"})
		if err != nil {
			t.Fatalf("RecordsMentioningEntity: %v", err)
		}
		if len(linked) != 1 || linked[0].Sha != sha {
			t.Fatalf("record_entities link missing: %+v", linked)
		}

		// pb_records re-projected with the distilled body — recall on a body term.
		waitForRecall(ctx, t, view, "tctest", "main", "vendor statements", sha)
	})

	t.Run("SynthMissingRecord_ErrorsAndCounts", func(t *testing.T) {
		// PG up, but NO raw record for this SHA → writeSynthResult hits
		// pgx.ErrNoRows → returns an error + the counter increments (the
		// raw write never landed; a re-enqueue reconciles).
		b := binding("tctest", "main", "pbd_miss_")
		d := newPGTestDaemon(t, b)
		d.osBase, d.osClient = osc, osc
		d.pgBaseDSN = baseDSN
		if err := d.buildBindingDeps(); err != nil {
			t.Fatalf("buildBindingDeps: %v", err)
		}
		t.Cleanup(d.closePGProfiles)

		before := d.DualWriteFailureCount()
		err := d.writeSynthResult(ctx, b, "tctest", "main",
			"dddd000000000000000000000000000000000000000000000000000000000004",
			synthResult{Body: "orphan", Reliability: "low", Topic: "general"})
		if err == nil {
			t.Fatal("expected an error when synth-writing a SHA with no raw record")
		}
		if d.DualWriteFailureCount() != before+1 {
			t.Fatalf("expected exactly one failure increment, before=%d after=%d", before, d.DualWriteFailureCount())
		}
	})
}

// TestSoRWrite_PGDisabled_NoContainers proves the PG-disabled path returns
// an error + increments the counter WITHOUT any containers (compiles + runs
// in CI without Docker). Phase D1: Postgres is mandatory, so a write with PG
// disabled is a real configuration error (resolvePG → ErrPostgresDisabled),
// not a tolerated no-op.
func TestSoRWrite_PGDisabled_NoContainers(t *testing.T) {
	ctx := context.Background()

	b := binding("tctest", "main", "x_")
	d := newPGTestDaemon(t, b)
	// No pgBaseDSN ⇒ PG disabled. buildBindingDeps returns an error in D1;
	// we ignore it here and exercise the per-write error path directly.
	_ = d.buildBindingDeps()
	doc := dwSummaryDoc("tctest", "main",
		"eeee000000000000000000000000000000000000000000000000000000000005", "t", "b", nil)

	if err := d.writeRecordRaw(ctx, b, doc); !errors.Is(err, ErrPostgresDisabled) {
		t.Fatalf("PG-disabled raw write: want ErrPostgresDisabled, got %v", err)
	}
	if err := d.writeSynthResult(ctx, b, "tctest", "main", doc.SHA, synthResult{}); !errors.Is(err, ErrPostgresDisabled) {
		t.Fatalf("PG-disabled synth write: want ErrPostgresDisabled, got %v", err)
	}
	if d.DualWriteFailureCount() != 2 {
		t.Fatalf("PG-disabled writes must each increment the counter, got %d", d.DualWriteFailureCount())
	}
}
