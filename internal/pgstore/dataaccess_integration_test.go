//go:build integration

// Integration coverage for the sqlc data-access layer against a real
// pgvector Postgres. Build-tagged OFF by default so `make test` neither
// compiles this file nor needs a Docker daemon. Run with:
//
//	GOFLAGS="-tags=sqlite_fts5,integration" go test ./internal/pgstore/ -run Integration -count=1
package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// embed768 builds a deterministic non-zero 768-dim vector (OS/pgvector reject
// all-zero vectors under cosine; a non-zero one also proves real values
// round-trip through the binary codec).
func embed768(seed float32) pgvector.Vector {
	v := make([]float32, 768)
	for i := range v {
		v[i] = seed + float32(i%7)*0.001
	}
	return pgvector.NewVector(v)
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// startPGVector boots a pgvector container and returns a baseDSN against the
// maintenance db. Mirrors the Provision integration test's setup.
func startPGVector(ctx context.Context, t *testing.T) string {
	t.Helper()
	const (
		dbUser = "pbrain"
		dbPass = "pbrain"
		dbName = "phantom_brain"
	)
	req := testcontainers.ContainerRequest{
		Image:        "pgvector/pgvector:pg17",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     dbUser,
			"POSTGRES_PASSWORD": dbPass,
			"POSTGRES_DB":       dbName,
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(2 * time.Minute),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start pgvector container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}
	return "postgres://" + dbUser + ":" + dbPass + "@" + host + ":" + port.Port() + "/" + dbName + "?sslmode=disable"
}

func TestDataAccessIntegration(t *testing.T) {
	ctx := context.Background()
	baseDSN := startPGVector(ctx, t)

	const profile, vault = "tctest", "main"

	// Provision the per-profile db (creates pb_tctest, extensions, migrations).
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
	defer pool.Close()
	q := New(pool)

	// ── records ──────────────────────────────────────────────────────────
	emb := embed768(0.1)
	rec, err := q.UpsertRecord(ctx, pgdb.UpsertRecordParams{
		Profile:    profile,
		Vault:      vault,
		Sha:        "sha-record-1",
		Kind:       "note",
		MemoryType: text("semantic"),
		Title:      "the alpha note",
		RawBody:    text("alpha raw body about Project Phoenix"),
		Source:     []string{"task:t1"},
		Tags:       []string{"topic:memory"},
	})
	if err != nil {
		t.Fatalf("UpsertRecord: %v", err)
	}
	if rec.ID == 0 {
		t.Fatal("UpsertRecord returned zero ID (expected RETURNING row on fresh insert)")
	}
	if rec.Embedding != nil {
		t.Errorf("expected NULL embedding on fresh record, got %v", rec.Embedding)
	}

	// Dedup: a conflicting sha returns no row (DO NOTHING).
	_, err = q.UpsertRecord(ctx, pgdb.UpsertRecordParams{
		Profile: profile, Vault: vault, Sha: "sha-record-1", Kind: "note", Title: "dup",
	})
	if err == nil {
		t.Fatal("expected pgx.ErrNoRows on conflicting UpsertRecord (DO NOTHING returns no row)")
	}

	// GetRecordBySHA returns the existing row.
	got, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{Profile: profile, Vault: vault, Sha: "sha-record-1"})
	if err != nil {
		t.Fatalf("GetRecordBySHA: %v", err)
	}
	if got.ID != rec.ID || got.Title != "the alpha note" {
		t.Errorf("GetRecordBySHA mismatch: %+v", got)
	}

	// ListUnsynthesised includes it (and exercises the NULL-embedding scan).
	unsynth, err := q.ListUnsynthesised(ctx, pgdb.ListUnsynthesisedParams{Profile: profile, Vault: vault, Lim: 10})
	if err != nil {
		t.Fatalf("ListUnsynthesised: %v", err)
	}
	if len(unsynth) != 1 || unsynth[0].ID != rec.ID {
		t.Fatalf("ListUnsynthesised = %d rows, want 1 with id %d", len(unsynth), rec.ID)
	}
	if unsynth[0].Embedding != nil {
		t.Errorf("expected nil embedding on unsynthesised row, got %v", unsynth[0].Embedding)
	}

	cnt, err := q.CountUnsynthesised(ctx, pgdb.CountUnsynthesisedParams{Profile: profile, Vault: vault})
	if err != nil {
		t.Fatalf("CountUnsynthesised: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("CountUnsynthesised = %d, want 1", cnt)
	}

	// MarkRecordSynthesised flips it and stores a real embedding.
	if err := q.MarkRecordSynthesised(ctx, pgdb.MarkRecordSynthesisedParams{
		ID:               rec.ID,
		Body:             text("distilled body"),
		Reliability:      text("medium"),
		Topic:            text("memory"),
		GateReason:       text("curated"),
		Embedding:        &emb,
		EmbeddingModel:   text("nomic-embed-text"),
		EmbeddingVersion: text("v1.5"),
	}); err != nil {
		t.Fatalf("MarkRecordSynthesised: %v", err)
	}

	after, err := q.GetRecordByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("GetRecordByID: %v", err)
	}
	if !after.Synthesised {
		t.Error("expected synthesised=true after MarkRecordSynthesised")
	}
	if after.Embedding == nil {
		t.Fatal("expected non-nil embedding after MarkRecordSynthesised")
	}
	if len(after.Embedding.Slice()) != 768 {
		t.Errorf("embedding round-trip dim = %d, want 768", len(after.Embedding.Slice()))
	}
	if got := after.Embedding.Slice()[0]; got != emb.Slice()[0] {
		t.Errorf("embedding round-trip value[0] = %v, want %v", got, emb.Slice()[0])
	}

	cnt, err = q.CountUnsynthesised(ctx, pgdb.CountUnsynthesisedParams{Profile: profile, Vault: vault})
	if err != nil {
		t.Fatalf("CountUnsynthesised (post): %v", err)
	}
	if cnt != 0 {
		t.Fatalf("CountUnsynthesised post-synth = %d, want 0", cnt)
	}

	// SetRecordExtractedText (attachment enrichment path).
	if err := q.SetRecordExtractedText(ctx, pgdb.SetRecordExtractedTextParams{
		ID: rec.ID, ExtractedText: text("ocr output"),
	}); err != nil {
		t.Fatalf("SetRecordExtractedText: %v", err)
	}

	// ── entities ─────────────────────────────────────────────────────────
	eEmb := embed768(0.2)
	ent, err := q.UpsertEntity(ctx, pgdb.UpsertEntityParams{
		Profile:        profile,
		Vault:          vault,
		Slug:           "project-phoenix",
		Name:           "Project Phoenix",
		Description:    text("the rewrite"),
		Embedding:      &eEmb,
		EmbeddingModel: text("nomic-embed-text"),
	})
	if err != nil {
		t.Fatalf("UpsertEntity: %v", err)
	}

	// Re-upsert with NULL description must keep the existing blurb (COALESCE).
	ent2, err := q.UpsertEntity(ctx, pgdb.UpsertEntityParams{
		Profile: profile, Vault: vault, Slug: "project-phoenix", Name: "Project Phoenix (renamed)",
	})
	if err != nil {
		t.Fatalf("UpsertEntity (re-upsert): %v", err)
	}
	if ent2.ID != ent.ID {
		t.Errorf("re-upsert created a new entity: %d != %d", ent2.ID, ent.ID)
	}
	if !ent2.Description.Valid || ent2.Description.String != "the rewrite" {
		t.Errorf("COALESCE failed: description = %+v, want preserved 'the rewrite'", ent2.Description)
	}
	if ent2.Name != "Project Phoenix (renamed)" {
		t.Errorf("name not updated on re-upsert: %q", ent2.Name)
	}

	if err := q.AddEntityAlias(ctx, pgdb.AddEntityAliasParams{EntityID: ent.ID, Alias: "Phoenix"}); err != nil {
		t.Fatalf("AddEntityAlias: %v", err)
	}
	// Idempotent: duplicate alias is a no-op.
	if err := q.AddEntityAlias(ctx, pgdb.AddEntityAliasParams{EntityID: ent.ID, Alias: "Phoenix"}); err != nil {
		t.Fatalf("AddEntityAlias (dup): %v", err)
	}

	resolved, err := q.ResolveEntityByAlias(ctx, pgdb.ResolveEntityByAliasParams{Profile: profile, Vault: vault, Alias: "Phoenix"})
	if err != nil {
		t.Fatalf("ResolveEntityByAlias: %v", err)
	}
	if resolved.ID != ent.ID {
		t.Errorf("ResolveEntityByAlias = %d, want %d", resolved.ID, ent.ID)
	}

	if err := q.LinkRecordEntity(ctx, pgdb.LinkRecordEntityParams{RecordID: rec.ID, EntityID: ent.ID}); err != nil {
		t.Fatalf("LinkRecordEntity: %v", err)
	}

	mentions, err := q.RecordsMentioningEntity(ctx, pgdb.RecordsMentioningEntityParams{Profile: profile, Vault: vault, Slug: "project-phoenix"})
	if err != nil {
		t.Fatalf("RecordsMentioningEntity: %v", err)
	}
	if len(mentions) != 1 || mentions[0].ID != rec.ID {
		t.Fatalf("RecordsMentioningEntity = %d rows, want 1 with id %d", len(mentions), rec.ID)
	}

	// ── facts ────────────────────────────────────────────────────────────
	fact, err := q.UpsertFact(ctx, pgdb.UpsertFactParams{
		Profile:        profile,
		Vault:          vault,
		EntityID:       ent.ID,
		Attribute:      "status",
		Value:          "in-progress",
		SourceRecordID: pgtype.Int8{Int64: rec.ID, Valid: true},
		Confidence:     pgtype.Float4{Float32: 0.9, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertFact (initial): %v", err)
	}
	if fact.Value != "in-progress" {
		t.Errorf("fact value = %q, want in-progress", fact.Value)
	}

	gotFact, err := q.GetFact(ctx, pgdb.GetFactParams{EntityID: ent.ID, Attribute: "status"})
	if err != nil {
		t.Fatalf("GetFact: %v", err)
	}
	if gotFact.ID != fact.ID {
		t.Errorf("GetFact id mismatch: %d != %d", gotFact.ID, fact.ID)
	}

	// Archive the superseded value, then upsert the new one.
	if err := q.InsertFactHistory(ctx, pgdb.InsertFactHistoryParams{
		Profile:              profile,
		Vault:                vault,
		EntityID:             ent.ID,
		Attribute:            "status",
		Value:                fact.Value,
		SourceRecordID:       fact.SourceRecordID,
		ValidFrom:            fact.ValidFrom,
		SupersededByRecordID: pgtype.Int8{Int64: rec.ID, Valid: true},
	}); err != nil {
		t.Fatalf("InsertFactHistory: %v", err)
	}

	fact2, err := q.UpsertFact(ctx, pgdb.UpsertFactParams{
		Profile:        profile,
		Vault:          vault,
		EntityID:       ent.ID,
		Attribute:      "status",
		Value:          "shipped",
		SourceRecordID: pgtype.Int8{Int64: rec.ID, Valid: true},
		Confidence:     pgtype.Float4{Float32: 1.0, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertFact (update): %v", err)
	}
	if fact2.ID != fact.ID {
		t.Errorf("UpsertFact update created a new row: %d != %d", fact2.ID, fact.ID)
	}
	if fact2.Value != "shipped" {
		t.Errorf("updated fact value = %q, want shipped", fact2.Value)
	}

	facts, err := q.ListFactsForEntity(ctx, ent.ID)
	if err != nil {
		t.Fatalf("ListFactsForEntity: %v", err)
	}
	if len(facts) != 1 || facts[0].Value != "shipped" {
		t.Fatalf("ListFactsForEntity = %d rows (want 1, value shipped): %+v", len(facts), facts)
	}
}
