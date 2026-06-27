//go:build integration

// Phase B2 integration coverage for the backfill-to-pg loader. Build-tagged
// OFF by default so `make test` neither compiles this file nor needs Docker.
// Run with:
//
//	GOFLAGS="-tags=sqlite_fts5,integration" go test ./internal/backfill/ -run Integration -count=1 -v
//
// Brings up pgvector/pgvector:pg17 (per-profile SoR) and
// opensearchproject/opensearch:2.18.0 (legacy pb_summaries/pb_entities +
// the pb_records projection). Seeds a legacy corpus directly via the OS
// client, runs backfill.Run, and asserts records + entity-graph + projection
// land correctly, that a re-run is a pure no-op (idempotent), and that
// dry-run writes nothing.
package backfill

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcopensearch "github.com/testcontainers/testcontainers-go/modules/opensearch"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/osproject"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

const (
	pgImage = "pgvector/pgvector:pg17"
	osImage = "opensearchproject/opensearch:2.18.0"
)

// startPG boots a pgvector container and returns the base maintenance DSN.
func startPG(ctx context.Context, t *testing.T) string {
	t.Helper()
	const (
		dbUser = "pbrain"
		dbPass = "pbrain"
		dbName = "phantom_brain"
	)
	req := testcontainers.ContainerRequest{
		Image:        pgImage,
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
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start pgvector container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("pg host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("pg mapped port: %v", err)
	}
	return "postgres://" + dbUser + ":" + dbPass + "@" + host + ":" + port.Port() + "/" + dbName + "?sslmode=disable"
}

// startOS boots a single-node OpenSearch and returns a Client.
func startOS(ctx context.Context, t *testing.T) *osearch.Client {
	t.Helper()
	ctr, err := tcopensearch.Run(ctx, osImage)
	if err != nil {
		t.Fatalf("start opensearch container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	addr, err := ctr.Address(ctx)
	if err != nil {
		t.Fatalf("os address: %v", err)
	}
	cfg := osearch.DefaultConfig()
	cfg.Addresses = []string{addr}
	cfg.RequestTimeout = 15 * time.Second
	cfg.Username = ctr.User
	cfg.Password = ctr.Password
	openCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c, err := osearch.Open(openCtx, cfg)
	if err != nil {
		t.Fatalf("osearch.Open: %v", err)
	}
	return c
}

// sha pads a short id to a 64-hex content-address shape.
func sha(suffix string) string {
	base := "0000000000000000000000000000000000000000000000000000000000000000"
	return base[:64-len(suffix)] + suffix
}

// embed builds a 768-dim vector with one hot dimension.
func embed(hot int) []float32 {
	v := make([]float32, osearch.EmbeddingDim)
	for i := range v {
		v[i] = 0.001
	}
	v[hot] = 1.0
	return v
}

// seedLegacy writes the legacy pb_summaries / pb_entities / pb_attachments
// corpus the backfill will read. Returns the SHAs used so assertions can
// reference them. prefix is the per-binding index prefix.
type seeded struct {
	synthSHA, rawSHA, attSHA, danglingSHA, nulSHA string
}

func seedLegacy(ctx context.Context, t *testing.T, osc *osearch.Client, prefix, profile, vault string) seeded {
	t.Helper()
	// Create the legacy indices with their real mappings (knn_vector
	// embedding etc.) BEFORE writing docs — otherwise OS auto-maps
	// `embedding` as long from the first numeric and rejects the vectors.
	// Phase D2b: the EnsureIndices* wrappers + legacy mappings were removed
	// from production; create the indices for this migration test via the
	// retained EnsureIndexWithMapping primitive with inline mappings.
	ensureLegacyIndicesForTest(ctx, t, osc, prefix)
	now := time.Now().UTC()
	s := seeded{
		synthSHA:    sha("a1"),
		rawSHA:      sha("b2"),
		attSHA:      sha("c3"),
		danglingSHA: sha("d4"), // referenced by an entity but never a record
		nulSHA:      sha("e5"), // body/title carry a NUL byte (the prod failure)
	}

	// 1. A synthesised note (distilled body + verdict + embedding to reuse).
	synth := osearch.SummaryDoc{
		Profile: profile, Vault: vault, SHA: s.synthSHA,
		Kind: osearch.KindNote, Title: "Quarterly invoices",
		RawBody:     "raw invoice text",
		Body:        "The quarterly invoices were reconciled against vendor statements.",
		Tags:        []string{"finance"},
		Topic:       "memory",
		Reliability: osearch.ReliabilityHigh,
		GateReason:  "curated",
		CreatedAt:   now, UpdatedAt: now,
		Synthesised: true,
		Embedding:   embed(5),
	}
	if err := osc.UpsertSummaryWithPrefix(ctx, prefix, synth, true); err != nil {
		t.Fatalf("seed synth summary: %v", err)
	}

	// 2. A raw (not-yet-synthed) note — no body, no embedding.
	raw := osearch.SummaryDoc{
		Profile: profile, Vault: vault, SHA: s.rawSHA,
		Kind: osearch.KindWebScrape, Title: "Scraped page about ledgers",
		RawBody:   "ledger ledger ledger raw scrape body",
		CreatedAt: now, UpdatedAt: now,
		Synthesised: false,
	}
	if err := osc.UpsertSummaryWithPrefix(ctx, prefix, raw, true); err != nil {
		t.Fatalf("seed raw summary: %v", err)
	}

	// 3. An attachment: a synthesised stub summary + the companion
	//    pb_attachments doc carrying mime/filename/size.
	stub := osearch.SummaryDoc{
		Profile: profile, Vault: vault, SHA: s.attSHA,
		Kind: osearch.KindAttachmentStub, Title: "contract.pdf",
		RawBody:     "extracted contract text",
		Body:        "extracted contract text",
		CreatedAt:   now, UpdatedAt: now,
		Synthesised: true,
		Embedding:   embed(11),
		Attachments: []string{s.attSHA},
	}
	if err := osc.UpsertSummaryWithPrefix(ctx, prefix, stub, true); err != nil {
		t.Fatalf("seed attachment stub: %v", err)
	}
	att := osearch.AttachmentDoc{
		Profile: profile, Vault: vault, SHA: s.attSHA,
		OriginalFilename: "contract.pdf",
		MIMEType:         "application/pdf",
		SizeBytes:        4096,
		MinIOKey:         profile + "/" + vault + "/attachments/" + s.attSHA + ".pdf",
		CreatedAt:        now,
	}
	if err := osc.UpsertAttachmentWithPrefix(ctx, prefix, att, true); err != nil {
		t.Fatalf("seed attachment doc: %v", err)
	}

	// 3b. A synthesised doc whose title + body carry NUL bytes (0x00) —
	//     OpenSearch/JSON tolerate them, Postgres TEXT rejects them
	//     (SQLSTATE 22021). This is the exact production failure; the
	//     backfill must SANITISE (strip NUL) and write it, not abort.
	nul := osearch.SummaryDoc{
		Profile: profile, Vault: vault, SHA: s.nulSHA,
		Kind: osearch.KindNote, Title: "Bad\x00title",
		RawBody:     "raw\x00body",
		Body:        "distilled\x00body about widgets",
		CreatedAt:   now, UpdatedAt: now,
		Synthesised: true,
		Embedding:   embed(7),
	}
	if err := osc.UpsertSummaryWithPrefix(ctx, prefix, nul, true); err != nil {
		t.Fatalf("seed nul summary: %v", err)
	}

	// 4. Two entities. One ("acme-corp") has an alias + mentions the synth
	//    record AND a dangling SHA (the miss). The other ("ledger-inc")
	//    mentions the raw record.
	e1 := osearch.EntityDoc{
		Profile: profile, Vault: vault, Slug: "acme-corp",
		Name:        "Acme Corp",
		Aliases:     []string{"ACME", "Acme Corporation"},
		Body:        "A vendor.",
		MentionedBy: []string{s.synthSHA, s.danglingSHA},
		CreatedAt:   now, UpdatedAt: now,
	}
	if err := osc.UpsertEntityWithPrefix(ctx, prefix, e1, true); err != nil {
		t.Fatalf("seed entity acme: %v", err)
	}
	e2 := osearch.EntityDoc{
		Profile: profile, Vault: vault, Slug: "ledger-inc",
		Name:        "Ledger Inc",
		MentionedBy: []string{s.rawSHA},
		CreatedAt:   now, UpdatedAt: now,
	}
	if err := osc.UpsertEntityWithPrefix(ctx, prefix, e2, true); err != nil {
		t.Fatalf("seed entity ledger: %v", err)
	}
	return s
}

func TestBackfill_Integration(t *testing.T) {
	ctx := context.Background()
	baseDSN := startPG(ctx, t)
	osc := startOS(ctx, t)

	const (
		profile = "tctest"
		vault   = "main"
		prefix  = "bf_"
	)
	if err := pgstore.Provision(ctx, baseDSN, profile); err != nil {
		t.Fatalf("provision %s: %v", profile, err)
	}
	profileDSN, err := pgstore.DSNForProfile(baseDSN, profile)
	if err != nil {
		t.Fatalf("DSNForProfile: %v", err)
	}
	pool, err := pgstore.Open(ctx, profileDSN)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	s := seedLegacy(ctx, t, osc, prefix, profile, vault)
	q := pgstore.New(pool)
	opts := Options{
		OS: osc, PG: pool, Profile: profile,
		Vaults:          []VaultRef{{Vault: vault, IndexPrefix: prefix}},
		IncludeEntities: true,
		BatchSize:       100,
	}

	// --- Pass 1+2: full backfill ---
	stats, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	total := stats.Total
	if total.RecordsInserted != 4 { // synth note + raw + attachment stub + nul doc
		t.Fatalf("expected 4 records inserted, got %d", total.RecordsInserted)
	}
	if total.RecordsSynthed != 3 { // synth note + attachment stub + nul doc
		t.Fatalf("expected 3 synthesised, got %d", total.RecordsSynthed)
	}
	if total.Errors != 0 { // the NUL doc must be sanitised + written, NOT skipped
		t.Fatalf("expected 0 errors (NUL sanitised, not aborted), got %d: %v",
			total.Errors, stats.PerVault)
	}
	if total.EntitiesUpserted != 2 {
		t.Fatalf("expected 2 entities, got %d", total.EntitiesUpserted)
	}
	if total.AliasesAdded != 2 {
		t.Fatalf("expected 2 aliases, got %d", total.AliasesAdded)
	}
	if total.LinksCreated != 2 { // synth←acme, raw←ledger
		t.Fatalf("expected 2 links, got %d", total.LinksCreated)
	}
	if total.EntityLinkMisses != 1 { // dangling SHA
		t.Fatalf("expected 1 link miss, got %d", total.EntityLinkMisses)
	}

	// Records in PG: synth has body + embedding + synthesised; raw has no
	// body + not synthesised; attachment has mime/filename.
	synthRec := getRec(ctx, t, q, profile, vault, s.synthSHA)
	if !synthRec.Synthesised || synthRec.Body.String == "" || synthRec.Embedding == nil {
		t.Fatalf("synth record wrong: synthesised=%v body=%q embNil=%v",
			synthRec.Synthesised, synthRec.Body.String, synthRec.Embedding == nil)
	}
	if synthRec.EmbeddingModel.String != embeddingModel {
		t.Fatalf("embedding_model not set: %q", synthRec.EmbeddingModel.String)
	}
	rawRec := getRec(ctx, t, q, profile, vault, s.rawSHA)
	if rawRec.Synthesised || rawRec.Body.Valid {
		t.Fatalf("raw record should be unsynthesised with no body: synthesised=%v bodyValid=%v",
			rawRec.Synthesised, rawRec.Body.Valid)
	}
	attRec := getRec(ctx, t, q, profile, vault, s.attSHA)
	if attRec.MimeType.String != "application/pdf" || attRec.OriginalFilename.String != "contract.pdf" {
		t.Fatalf("attachment metadata not joined: mime=%q filename=%q",
			attRec.MimeType.String, attRec.OriginalFilename.String)
	}
	// The NUL doc landed, with every NUL stripped from its text fields.
	nulRec := getRec(ctx, t, q, profile, vault, s.nulSHA)
	for _, f := range []string{nulRec.Title, nulRec.Body.String, nulRec.RawBody.String} {
		if strings.ContainsRune(f, 0) {
			t.Fatalf("NUL byte survived into the SoR: %q", f)
		}
	}
	if nulRec.Title != "Badtitle" || nulRec.Body.String != "distilledbody about widgets" {
		t.Fatalf("NUL sanitisation wrong: title=%q body=%q", nulRec.Title, nulRec.Body.String)
	}

	// Entity graph: acme has the alias + a link to the synth record.
	ent, err := q.GetEntityBySlug(ctx, pgdb.GetEntityBySlugParams{Profile: profile, Vault: vault, Slug: "acme-corp"})
	if err != nil {
		t.Fatalf("GetEntityBySlug acme: %v", err)
	}
	if ent.Name != "Acme Corp" {
		t.Fatalf("entity name: %q", ent.Name)
	}
	resolved, err := q.ResolveEntityByAlias(ctx, pgdb.ResolveEntityByAliasParams{Profile: profile, Vault: vault, Alias: "ACME"})
	if err != nil || resolved.Slug != "acme-corp" {
		t.Fatalf("alias ACME did not resolve to acme-corp: %v (%+v)", err, resolved)
	}
	linked, err := q.RecordsMentioningEntity(ctx, pgdb.RecordsMentioningEntityParams{Profile: profile, Vault: vault, Slug: "acme-corp"})
	if err != nil {
		t.Fatalf("RecordsMentioningEntity acme: %v", err)
	}
	if len(linked) != 1 || linked[0].Sha != s.synthSHA {
		t.Fatalf("acme link wrong (dangling SHA must NOT link): %+v", linked)
	}

	// Projection: the synth doc must be findable in pb_records. Project()
	// uses waitForRefresh=false, so poll a refresh + recall until the doc
	// surfaces (1s refresh_interval + index propagation).
	rec := osproject.NewRecaller(osc, prefix)
	deadline := time.Now().Add(15 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		if err := osc.RefreshWithPrefix(ctx, prefix); err != nil {
			t.Fatalf("refresh pb_records: %v", err)
		}
		hits, err := rec.Recall(ctx, osproject.RecallQuery{Profile: profile, Vault: vault, Text: "vendor statements", Size: 10})
		if err != nil {
			t.Fatalf("recall: %v", err)
		}
		if hasSHA(hits, s.synthSHA) {
			found = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !found {
		t.Fatalf("projected synth doc %s never recallable in pb_records", s.synthSHA)
	}

	// --- Idempotent re-run: all-dup, no PK violation, links unchanged ---
	stats2, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("idempotent Run: %v", err)
	}
	if stats2.Total.RecordsInserted != 0 || stats2.Total.RecordsDup != 4 {
		t.Fatalf("re-run should be all-dup: inserted=%d dup=%d",
			stats2.Total.RecordsInserted, stats2.Total.RecordsDup)
	}
	if stats2.Total.LinksCreated != 2 || stats2.Total.EntityLinkMisses != 1 {
		t.Fatalf("re-run links/misses changed: links=%d misses=%d",
			stats2.Total.LinksCreated, stats2.Total.EntityLinkMisses)
	}
	linkedAgain, _ := q.RecordsMentioningEntity(ctx, pgdb.RecordsMentioningEntityParams{Profile: profile, Vault: vault, Slug: "acme-corp"})
	if len(linkedAgain) != 1 {
		t.Fatalf("re-run must not duplicate links: %d", len(linkedAgain))
	}
}

func TestBackfill_DryRun_Integration(t *testing.T) {
	ctx := context.Background()
	baseDSN := startPG(ctx, t)
	osc := startOS(ctx, t)

	const (
		profile = "drytest"
		vault   = "main"
		prefix  = "dry_"
	)
	if err := pgstore.Provision(ctx, baseDSN, profile); err != nil {
		t.Fatalf("provision: %v", err)
	}
	profileDSN, _ := pgstore.DSNForProfile(baseDSN, profile)
	pool, err := pgstore.Open(ctx, profileDSN)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	s := seedLegacy(ctx, t, osc, prefix, profile, vault)
	q := pgstore.New(pool)

	stats, err := Run(ctx, Options{
		OS: osc, PG: pool, Profile: profile,
		Vaults:          []VaultRef{{Vault: vault, IndexPrefix: prefix}},
		DryRun:          true,
		IncludeEntities: true,
		BatchSize:       100,
	})
	if err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}
	// Would-be counts reflect the corpus...
	if stats.Total.RecordsInserted != 4 || stats.Total.EntitiesUpserted != 2 {
		t.Fatalf("dry-run counts wrong: inserted=%d entities=%d",
			stats.Total.RecordsInserted, stats.Total.EntitiesUpserted)
	}
	// ...but PG has ZERO rows.
	if _, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{Profile: profile, Vault: vault, Sha: s.synthSHA}); err != pgx.ErrNoRows {
		t.Fatalf("dry-run must not write records; expected ErrNoRows, got %v", err)
	}
	if _, err := q.GetEntityBySlug(ctx, pgdb.GetEntityBySlugParams{Profile: profile, Vault: vault, Slug: "acme-corp"}); err != pgx.ErrNoRows {
		t.Fatalf("dry-run must not write entities; expected ErrNoRows, got %v", err)
	}
}

func getRec(ctx context.Context, t *testing.T, q *pgdb.Queries, profile, vault, sha string) pgdb.Record {
	t.Helper()
	rec, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{Profile: profile, Vault: vault, Sha: sha})
	if err != nil {
		t.Fatalf("GetRecordBySHA %s: %v", sha, err)
	}
	return rec
}

func hasSHA(hits []osproject.RecallHit, sha string) bool {
	for _, h := range hits {
		if h.SHA == sha {
			return true
		}
	}
	return false
}

// ensureLegacyIndicesForTest creates the legacy pb_summaries / pb_entities /
// pb_attachments indices under prefix with knn_vector embedding mappings,
// using the retained osearch.Client.EnsureIndexWithMapping primitive. The
// legacy mappings live here (test-only) because Phase D2b removed them from
// production — they are only needed to stage a corpus for the migration test.
func ensureLegacyIndicesForTest(ctx context.Context, t *testing.T, osc *osearch.Client, prefix string) {
	t.Helper()
	knn := map[string]any{
		"type":      "knn_vector",
		"dimension": osearch.EmbeddingDim,
		"method": map[string]any{
			"name":       "hnsw",
			"space_type": "cosinesimil",
			"engine":     "lucene",
			"parameters": map[string]any{"ef_construction": 128, "m": 16},
		},
	}
	settings := map[string]any{
		"index": map[string]any{
			"knn":                true,
			"number_of_shards":   1,
			"number_of_replicas": 0,
			"refresh_interval":   "1s",
		},
	}
	mk := func(props map[string]any) map[string]any {
		props["embedding"] = knn
		return map[string]any{"settings": settings, "mappings": map[string]any{"properties": props}}
	}
	indices := []struct {
		logical string
		mapping map[string]any
	}{
		{osearch.IndexSummaries, mk(map[string]any{
			"profile": map[string]any{"type": "keyword"}, "vault": map[string]any{"type": "keyword"},
			"sha": map[string]any{"type": "keyword"}, "kind": map[string]any{"type": "keyword"},
			"memory_type": map[string]any{"type": "keyword"}, "source_path": map[string]any{"type": "keyword"},
			"source_url": map[string]any{"type": "keyword"}, "source": map[string]any{"type": "keyword"},
			"created_at": map[string]any{"type": "date"}, "updated_at": map[string]any{"type": "date"},
			"captured_at": map[string]any{"type": "date"}, "title": map[string]any{"type": "text"},
			"body": map[string]any{"type": "text"}, "raw_body": map[string]any{"type": "text"},
			"tags": map[string]any{"type": "keyword"}, "topic": map[string]any{"type": "keyword"},
			"reliability": map[string]any{"type": "keyword"}, "entities": map[string]any{"type": "keyword"},
			"attachments": map[string]any{"type": "keyword"}, "references": map[string]any{"type": "keyword"},
			"capture_minio_key": map[string]any{"type": "keyword"}, "capture_size_bytes": map[string]any{"type": "long"},
			"synthesised": map[string]any{"type": "boolean"},
		})},
		{osearch.IndexEntities, mk(map[string]any{
			"profile": map[string]any{"type": "keyword"}, "vault": map[string]any{"type": "keyword"},
			"slug": map[string]any{"type": "keyword"}, "name": map[string]any{"type": "text"},
			"aliases": map[string]any{"type": "keyword"}, "body": map[string]any{"type": "text"},
			"tags": map[string]any{"type": "keyword"}, "topic": map[string]any{"type": "keyword"},
			"mentioned_by": map[string]any{"type": "keyword"}, "created_at": map[string]any{"type": "date"},
			"updated_at": map[string]any{"type": "date"},
		})},
		{osearch.IndexAttachments, mk(map[string]any{
			"profile": map[string]any{"type": "keyword"}, "vault": map[string]any{"type": "keyword"},
			"sha": map[string]any{"type": "keyword"}, "kind": map[string]any{"type": "keyword"},
			"memory_type": map[string]any{"type": "keyword"}, "original_filename": map[string]any{"type": "text"},
			"title": map[string]any{"type": "text"}, "mime_type": map[string]any{"type": "keyword"},
			"size_bytes": map[string]any{"type": "long"}, "created_at": map[string]any{"type": "date"},
			"captured_at": map[string]any{"type": "date"}, "minio_key": map[string]any{"type": "keyword"},
			"extracted_text": map[string]any{"type": "text"}, "summary_sha": map[string]any{"type": "keyword"},
			"source": map[string]any{"type": "keyword"}, "references": map[string]any{"type": "keyword"},
			"tags": map[string]any{"type": "keyword"},
		})},
	}
	for _, idx := range indices {
		if err := osc.EnsureIndexWithMapping(ctx, prefix, idx.logical, idx.mapping); err != nil {
			t.Fatalf("ensure legacy %s: %v", idx.logical, err)
		}
	}
}
