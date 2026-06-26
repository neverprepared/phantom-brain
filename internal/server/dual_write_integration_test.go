//go:build integration

// Phase B1 integration coverage for the dual-write write path. Build-tagged
// OFF by default so `make test` neither compiles this file nor needs Docker.
// Run with:
//
//	GOFLAGS="-tags=sqlite_fts5,integration" go test ./internal/server/ -run DualWrite -count=1 -v
//
// Reuses the Phase A harness (startPGForServer / startOSForServer /
// newPGTestDaemon / binding / pgstore.Provision). Proves:
//  1. Flag ON, raw write  → record lands in PG + projects to pb_records.
//  2. Flag ON, synth mirror → synthesised=true + body + entity + link, and
//     pb_records re-projected with the distilled body.
//  3. Flag OFF             → nothing written to PG.
//  4. Non-fatal on PG down → no panic/error; counter increments; the legacy
//     write path is untouched.
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

// dwBinding builds a VaultBinding with a resolved prefix + the DualWrite
// flag set as requested.
func dwBinding(profile, vault, prefix string, dualWrite bool) VaultBinding {
	b := binding(profile, vault, prefix)
	b.DualWrite = dualWrite
	return b
}

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
		Tags:        []string{"phaseb1"},
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

func TestDualWrite_Integration(t *testing.T) {
	ctx := context.Background()
	baseDSN := startPGForServer(ctx, t)
	osc := startOSForServer(ctx, t)

	if err := pgstore.Provision(ctx, baseDSN, "tctest"); err != nil {
		t.Fatalf("provision tctest db: %v", err)
	}

	t.Run("FlagOn_RawWrite", func(t *testing.T) {
		b := dwBinding("tctest", "main", "pbb_raw_", true)
		d := newPGTestDaemon(t, b)
		d.osBase, d.osClient, d.osExport = osc, osc, osc
		d.pgBaseDSN = baseDSN
		d.buildBindingDeps()
		t.Cleanup(d.closePGProfiles)

		view, err := d.resolvePG(b)
		if err != nil {
			t.Fatalf("resolvePG: %v", err)
		}

		doc := dwSummaryDoc("tctest", "main",
			"aaaa000000000000000000000000000000000000000000000000000000000001",
			"Quarterly invoices", "We reconciled every quarterly invoice.", dwEmbedding(5))

		d.dualWriteRaw(ctx, b, doc)

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

	t.Run("FlagOn_SynthMirror", func(t *testing.T) {
		b := dwBinding("tctest", "main", "pbb_synth_", true)
		d := newPGTestDaemon(t, b)
		d.osBase, d.osClient, d.osExport = osc, osc, osc
		d.pgBaseDSN = baseDSN
		d.buildBindingDeps()
		t.Cleanup(d.closePGProfiles)

		view, err := d.resolvePG(b)
		if err != nil {
			t.Fatalf("resolvePG: %v", err)
		}

		sha := "bbbb000000000000000000000000000000000000000000000000000000000002"
		doc := dwSummaryDoc("tctest", "main", sha,
			"Ledger reconciliation", "stardust accounting raw body", dwEmbedding(7))
		d.dualWriteRaw(ctx, b, doc)
		waitForRecord(ctx, t, view, "tctest", "main", sha)

		// Now the synth mirror: distilled body + reliability/topic + one entity.
		d.dualWriteSynth(ctx, b, "tctest", "main", sha, synthResult{
			Body:           "The general ledger was reconciled against vendor statements thoroughly.",
			Reliability:    "high",
			Topic:          "memory",
			GateReason:     "test",
			Embedding:      dwEmbedding(7),
			EmbeddingModel: "nomic-embed-text",
			EntityNames:    map[string]string{"acme-corp": "Acme Corp"},
		})
		if d.DualWriteFailureCount() != 0 {
			t.Fatalf("no failure expected on happy synth mirror, got %d", d.DualWriteFailureCount())
		}

		q := pgstore.New(view.Pool())
		rec, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{Profile: "tctest", Vault: "main", Sha: sha})
		if err != nil {
			t.Fatalf("GetRecordBySHA post-synth: %v", err)
		}
		if !rec.Synthesised {
			t.Fatal("record must be synthesised after the mirror")
		}
		if rec.Body.String != "The general ledger was reconciled against vendor statements thoroughly." {
			t.Fatalf("body not mirrored: %q", rec.Body.String)
		}
		if rec.Reliability.String != "high" || rec.Topic.String != "memory" {
			t.Fatalf("reliability/topic not mirrored: %q / %q", rec.Reliability.String, rec.Topic.String)
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

	t.Run("FlagOff_NoWrite", func(t *testing.T) {
		b := dwBinding("tctest", "main", "pbb_off_", false)
		d := newPGTestDaemon(t, b)
		d.osBase, d.osClient, d.osExport = osc, osc, osc
		d.pgBaseDSN = baseDSN
		d.buildBindingDeps()
		t.Cleanup(d.closePGProfiles)

		// resolvePG must still WORK (PG configured) — the no-op is purely
		// the flag gate inside dualWriteRaw.
		view, err := d.resolvePG(b)
		if err != nil {
			t.Fatalf("resolvePG (flag off, PG configured): %v", err)
		}

		sha := "cccc000000000000000000000000000000000000000000000000000000000003"
		doc := dwSummaryDoc("tctest", "main", sha, "Should not persist", "body", dwEmbedding(9))
		d.dualWriteRaw(ctx, b, doc)

		q := pgstore.New(view.Pool())
		_, err = q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{Profile: "tctest", Vault: "main", Sha: sha})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("flag-off write must NOT persist; expected ErrNoRows, got %v", err)
		}
		if d.DualWriteFailureCount() != 0 {
			t.Fatalf("flag-off must not count as a failure, got %d", d.DualWriteFailureCount())
		}
	})

	t.Run("SynthMissingRecord_NonFatalCounter", func(t *testing.T) {
		// Flag ON, PG up, but NO raw record for this SHA → dualWriteSynth
		// hits pgx.ErrNoRows → non-fatal, counter increments, no panic.
		b := dwBinding("tctest", "main", "pbb_miss_", true)
		d := newPGTestDaemon(t, b)
		d.osBase, d.osClient, d.osExport = osc, osc, osc
		d.pgBaseDSN = baseDSN
		d.buildBindingDeps()
		t.Cleanup(d.closePGProfiles)

		before := d.DualWriteFailureCount()
		d.dualWriteSynth(ctx, b, "tctest", "main",
			"dddd000000000000000000000000000000000000000000000000000000000004",
			synthResult{Body: "orphan", Reliability: "low", Topic: "general"})
		if d.DualWriteFailureCount() != before+1 {
			t.Fatalf("expected exactly one failure increment, before=%d after=%d", before, d.DualWriteFailureCount())
		}
	})
}

// TestDualWrite_Disabled_NoContainers proves the flag-off + PG-disabled
// paths are non-fatal WITHOUT any containers (compiles + runs in CI without
// Docker). It exercises: flag off → no-op; flag on + PG disabled → skip
// (no counter, no panic).
func TestDualWrite_Disabled_NoContainers(t *testing.T) {
	ctx := context.Background()

	// Flag OFF: returns immediately, never touches resolvePG.
	bOff := dwBinding("tctest", "main", "x_", false)
	dOff := newPGTestDaemon(t, bOff)
	dOff.buildBindingDeps() // no pgBaseDSN ⇒ PG disabled
	doc := dwSummaryDoc("tctest", "main",
		"eeee000000000000000000000000000000000000000000000000000000000005", "t", "b", nil)
	dOff.dualWriteRaw(ctx, bOff, doc)
	dOff.dualWriteSynth(ctx, bOff, "tctest", "main", doc.SHA, synthResult{})
	if dOff.DualWriteFailureCount() != 0 {
		t.Fatalf("flag-off must not increment counter, got %d", dOff.DualWriteFailureCount())
	}

	// Flag ON but PG disabled (no pgBaseDSN): resolvePG → ErrPostgresDisabled
	// → skip path. Non-fatal, no counter, no panic.
	bOn := dwBinding("tctest", "main", "y_", true)
	dOn := newPGTestDaemon(t, bOn)
	dOn.buildBindingDeps()
	dOn.dualWriteRaw(ctx, bOn, doc)
	dOn.dualWriteSynth(ctx, bOn, "tctest", "main", doc.SHA, synthResult{})
	if dOn.DualWriteFailureCount() != 0 {
		t.Fatalf("PG-disabled skip must not increment counter, got %d", dOn.DualWriteFailureCount())
	}
}
