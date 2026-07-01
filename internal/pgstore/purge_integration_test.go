//go:build integration

// Integration coverage for PurgeBinding against a real pgvector Postgres.
// Build-tagged OFF by default. Run with:
//
//	GOFLAGS="-tags=sqlite_fts5,integration" go test ./internal/pgstore/ -run Integration -count=1
package pgstore

import (
	"context"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// TestPurgeBindingIntegration proves the two contracts that make
// `binding delete --purge-data` safe post-#92:
//  1. it removes ALL of one binding's rows (records/entities/fact_history)
//     from the per-profile SoR, and
//  2. it leaves a SIBLING vault sharing the same pb_<profile> database
//     completely untouched (scoped row delete, not DROP DATABASE).
func TestPurgeBindingIntegration(t *testing.T) {
	ctx := context.Background()
	baseDSN := startPGVector(ctx, t)

	const profile = "purgetest"
	const victim, sibling = "victim", "keeper"

	if err := Provision(ctx, baseDSN, profile); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	profileDSN, err := DSNForProfile(baseDSN, profile)
	if err != nil {
		t.Fatalf("DSNForProfile: %v", err)
	}
	pool, err := Open(ctx, profileDSN)
	if err != nil {
		t.Fatalf("Open pool: %v", err)
	}
	q := New(pool)

	// Seed both vaults: 2 records each, plus one entity + one fact_history
	// row each so every purged table is exercised and every isolation
	// assertion is meaningful.
	seed := func(vault string) {
		for i, sha := range []string{vault + "-r1", vault + "-r2"} {
			if _, err := q.UpsertRecord(ctx, pgdb.UpsertRecordParams{
				Profile: profile, Vault: vault, Sha: sha, Kind: "note",
				Title: vault + "-note",
			}); err != nil {
				t.Fatalf("seed record %d for %s: %v", i, vault, err)
			}
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO entities (profile, vault, slug, name) VALUES ($1,$2,$3,$4)`,
			profile, vault, vault+"-ent", vault+" Entity"); err != nil {
			t.Fatalf("seed entity for %s: %v", vault, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO fact_history (profile, vault, attribute, value) VALUES ($1,$2,$3,$4)`,
			profile, vault, "status", "active"); err != nil {
			t.Fatalf("seed fact_history for %s: %v", vault, err)
		}
	}
	seed(victim)
	seed(sibling)
	pool.Close() // PurgeBinding opens its own connection.

	counts, err := PurgeBinding(ctx, baseDSN, profile, victim)
	if err != nil {
		t.Fatalf("PurgeBinding: %v", err)
	}
	if counts.Records != 2 || counts.Entities != 1 || counts.FactHistory != 1 {
		t.Fatalf("purge counts = %+v, want {Records:2 Entities:1 FactHistory:1}", counts)
	}

	// Re-open to verify final state.
	pool2, err := Open(ctx, profileDSN)
	if err != nil {
		t.Fatalf("re-open pool: %v", err)
	}
	defer pool2.Close()

	countRows := func(table, vault string) int {
		var n int
		if err := pool2.QueryRow(ctx,
			"SELECT count(*) FROM "+table+" WHERE profile=$1 AND vault=$2", profile, vault).Scan(&n); err != nil {
			t.Fatalf("count %s/%s: %v", table, vault, err)
		}
		return n
	}

	// Victim: everything gone.
	for _, table := range []string{"records", "entities", "fact_history"} {
		if n := countRows(table, victim); n != 0 {
			t.Errorf("victim %s still has %d rows, want 0", table, n)
		}
	}
	// Sibling: fully intact — the whole point of a scoped purge.
	if n := countRows("records", sibling); n != 2 {
		t.Errorf("sibling records = %d, want 2 (untouched)", n)
	}
	if n := countRows("entities", sibling); n != 1 {
		t.Errorf("sibling entities = %d, want 1 (untouched)", n)
	}
	if n := countRows("fact_history", sibling); n != 1 {
		t.Errorf("sibling fact_history = %d, want 1 (untouched)", n)
	}

	// Idempotent: purging again removes nothing and does not error.
	again, err := PurgeBinding(ctx, baseDSN, profile, victim)
	if err != nil {
		t.Fatalf("second PurgeBinding: %v", err)
	}
	if again.Records != 0 || again.Entities != 0 || again.FactHistory != 0 {
		t.Errorf("second purge removed rows %+v, want all zero", again)
	}
}
