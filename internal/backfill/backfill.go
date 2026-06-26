// Package backfill is the one-shot operator loader that seeds a profile's
// existing legacy corpus (OpenSearch pb_summaries + pb_entities) into the
// new Postgres System-of-Record (records / entities / record_entities) and
// the pb_records search projection. It is Phase B2 of the daemon-cutover
// plan (docs/design/daemon-cutover-plan.md): additive parallel-run tooling
// that lets recall parity be validated BEFORE the read flip.
//
// It is standalone — it talks to OpenSearch and Postgres directly, never
// the daemon, and changes no live path. It reuses every legacy embedding
// (no re-embed) and reconstructs the entity graph by inverting the legacy
// denormalised MentionedBy[] backlinks into record_entities rows.
//
// Run is idempotent + resumable: UpsertRecord is ON CONFLICT DO NOTHING,
// MarkRecordSynthesised is an idempotent UPDATE, UpsertEntity is DO UPDATE,
// and the alias / link inserts are ON CONFLICT DO NOTHING. A re-run simply
// re-scrolls and no-ops the rows that already exist.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/osproject"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// embeddingModel labels the reused legacy vectors. Phase 6 standardised
// on nomic-embed-text (768-dim) across agent + daemon, so every legacy
// embedding we reuse carries this model tag.
const embeddingModel = "nomic-embed-text"

// VaultRef names one vault to backfill plus its resolved per-binding OS
// index prefix (binding.Storage.IndexPrefix). The same Postgres profile DB
// holds every vault of a profile, so only the OS prefix varies per vault.
type VaultRef struct {
	Vault       string
	IndexPrefix string
}

// Options configures one backfill run.
type Options struct {
	OS      *osearch.Client // legacy store, already Open'd
	PG      *pgxpool.Pool   // per-profile SoR pool (DSNForProfile)
	Profile string
	Vaults  []VaultRef

	// DryRun scrolls + counts but performs NO writes/projection. The
	// reported counts are "would-be" — every record counts as inserted
	// and every entity/alias/link as it would be applied.
	DryRun bool

	// IncludeEntities controls Pass 2 (entity-graph reconstruction). The
	// CLI's --no-entities flips it off for a records-only backfill.
	IncludeEntities bool

	// BatchSize is the OS scroll page size. <= 0 falls back to 500.
	BatchSize int
}

// VaultStats is the per-vault tally.
type VaultStats struct {
	Vault string

	RecordsInserted int // new UpsertRecord rows
	RecordsDup      int // ON CONFLICT DO NOTHING (already present)
	RecordsSynthed  int // MarkRecordSynthesised applied (legacy Synthesised=true)

	EntitiesUpserted int
	AliasesAdded     int
	LinksCreated     int
	EntityLinkMisses int // MentionedBy SHA with no record in this vault

	// Errors counts per-item failures that were LOGGED AND SKIPPED rather
	// than aborting the whole run (a single pathological doc — e.g. one
	// that still violates a constraint after sanitising — must not strand
	// the other 3969). SampleErrors holds the first few for the operator.
	Errors       int
	SampleErrors []string
}

// maxSampleErrors caps the per-vault SampleErrors slice.
const maxSampleErrors = 5

func (v *VaultStats) noteErr(err error) {
	v.Errors++
	if len(v.SampleErrors) < maxSampleErrors {
		v.SampleErrors = append(v.SampleErrors, err.Error())
	}
}

// Stats aggregates the per-vault tallies + a Total roll-up.
type Stats struct {
	PerVault []VaultStats
	Total    VaultStats
}

func (s *Stats) add(v VaultStats) {
	s.PerVault = append(s.PerVault, v)
	s.Total.RecordsInserted += v.RecordsInserted
	s.Total.RecordsDup += v.RecordsDup
	s.Total.RecordsSynthed += v.RecordsSynthed
	s.Total.EntitiesUpserted += v.EntitiesUpserted
	s.Total.AliasesAdded += v.AliasesAdded
	s.Total.LinksCreated += v.LinksCreated
	s.Total.EntityLinkMisses += v.EntityLinkMisses
	s.Total.Errors += v.Errors
}

// Run loads the profile's legacy corpus into the SoR + projection, one
// vault at a time. Per vault: Pass 1 (records) MUST complete before Pass 2
// (entity links reference record ids).
func Run(ctx context.Context, opts Options) (Stats, error) {
	var stats Stats
	if opts.OS == nil {
		return stats, fmt.Errorf("backfill: nil OpenSearch client")
	}
	if !opts.DryRun && opts.PG == nil {
		return stats, fmt.Errorf("backfill: nil Postgres pool")
	}
	if opts.Profile == "" {
		return stats, fmt.Errorf("backfill: empty profile")
	}
	if len(opts.Vaults) == 0 {
		return stats, fmt.Errorf("backfill: no vaults to backfill")
	}
	batch := opts.BatchSize
	if batch <= 0 {
		batch = 500
	}

	q := pgstore.New(opts.PG)

	for _, vr := range opts.Vaults {
		vs := VaultStats{Vault: vr.Vault}

		// Pass 1 — records. Ensure the projection index exists first so
		// Project has a target (skipped on dry-run — no writes at all).
		var projector *osproject.Projector
		if !opts.DryRun {
			if err := osproject.EnsureIndex(ctx, opts.OS, vr.IndexPrefix); err != nil {
				return stats, fmt.Errorf("backfill: ensure pb_records (vault %s): %w", vr.Vault, err)
			}
			projector = osproject.New(opts.OS.WithPrefix(vr.IndexPrefix), vr.IndexPrefix)
		}

		// Pre-load attachment metadata (mime/filename/size/minio_key) keyed
		// by SHA. pb_summaries' attachment_stub docs are the recall sidecar
		// and do NOT carry the binary metadata — it lives on pb_attachments.
		// We join the two by SHA so the projected record has its attachment
		// fields. (Skipped on dry-run: no enrichment is observable anyway.)
		attMeta := map[string]osearch.AttachmentDoc{}
		if !opts.DryRun {
			if err := opts.OS.ScrollAttachmentsWithPrefix(ctx, vr.IndexPrefix, opts.Profile, vr.Vault, batch, func(a osearch.AttachmentDoc) error {
				attMeta[a.SHA] = a
				return nil
			}); err != nil {
				return stats, fmt.Errorf("backfill: scroll attachments (vault %s): %w", vr.Vault, err)
			}
		}

		if err := backfillRecords(ctx, opts, q, projector, vr, batch, attMeta, &vs); err != nil {
			return stats, err
		}

		// Pass 2 — entity graph (after records so links can resolve SHAs).
		if opts.IncludeEntities {
			if err := backfillEntities(ctx, opts, q, vr, batch, &vs); err != nil {
				return stats, err
			}
		}

		stats.add(vs)
	}
	return stats, nil
}

// backfillRecords runs Pass 1 for one vault: scroll pb_summaries, upsert
// each into records, mark already-synthed docs synthesised (reusing the
// legacy embedding), and project the fresh row into pb_records.
func backfillRecords(ctx context.Context, opts Options, q *pgdb.Queries, projector *osproject.Projector, vr VaultRef, batch int, attMeta map[string]osearch.AttachmentDoc, vs *VaultStats) error {
	return opts.OS.ScrollSummariesWithPrefix(ctx, vr.IndexPrefix, opts.Profile, vr.Vault, batch, func(d osearch.SummaryDoc) error {
		if opts.DryRun {
			// Count only — every doc is a would-be insert; a synthesised
			// legacy doc is also a would-be MarkRecordSynthesised.
			vs.RecordsInserted++
			if d.Synthesised {
				vs.RecordsSynthed++
			}
			return nil
		}

		// One pathological doc must not abort the whole run. Per-record
		// failures are counted + sampled + skipped; only a scroll-level
		// (OpenSearch transport) error propagates.
		if err := backfillOneRecord(ctx, q, projector, d, attMeta, vs); err != nil {
			vs.noteErr(fmt.Errorf("record %s: %w", d.SHA, err))
		}
		return nil
	})
}

// backfillOneRecord upserts + (when synthesised) marks + projects a single
// legacy doc. Returns the error for the caller to count/skip.
func backfillOneRecord(ctx context.Context, q *pgdb.Queries, projector *osproject.Projector, d osearch.SummaryDoc, attMeta map[string]osearch.AttachmentDoc, vs *VaultStats) error {
	att, hasAtt := attMeta[d.SHA]
	rec, inserted, err := upsertRecord(ctx, q, d, att, hasAtt)
	if err != nil {
		return err
	}
	if inserted {
		vs.RecordsInserted++
	} else {
		vs.RecordsDup++
	}

	// Reuse the legacy embedding — NO re-embed. A legacy doc already
	// past the gate/distill pass (Synthesised=true) carries its
	// distilled body + verdict + (usually) a 768-dim vector.
	if d.Synthesised {
		if err := q.MarkRecordSynthesised(ctx, pgdb.MarkRecordSynthesisedParams{
			Body:             optText(d.Body),
			Reliability:      optText(string(d.Reliability)),
			Topic:            optText(d.Topic),
			GateReason:       optText(d.GateReason),
			Embedding:        optVector(d.Embedding),
			EmbeddingModel:   optText(embeddingModel),
			EmbeddingVersion: pgtype.Text{},
			ID:               rec.ID,
		}); err != nil {
			return fmt.Errorf("mark synthesised: %w", err)
		}
		vs.RecordsSynthed++
	}

	// Project the FRESH row (re-fetch so body/embedding from the
	// MarkRecordSynthesised UPDATE are included — the row returned by
	// UpsertRecord predates the mark, and on a dup it's the old row).
	fresh, err := q.GetRecordByID(ctx, rec.ID)
	if err != nil {
		return fmt.Errorf("refetch record %d: %w", rec.ID, err)
	}
	if err := projector.Project(ctx, fresh); err != nil {
		return fmt.Errorf("project: %w", err)
	}
	return nil
}

// upsertRecord maps a SummaryDoc to UpsertRecordParams and inserts it. On
// ON CONFLICT DO NOTHING (pgx.ErrNoRows), the record already exists — fetch
// it via GetRecordBySHA and report inserted=false.
func upsertRecord(ctx context.Context, q *pgdb.Queries, d osearch.SummaryDoc, att osearch.AttachmentDoc, hasAtt bool) (rec pgdb.Record, inserted bool, err error) {
	params := summaryDocToUpsertParams(d, att, hasAtt)
	rec, err = q.UpsertRecord(ctx, params)
	if err == nil {
		return rec, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return pgdb.Record{}, false, fmt.Errorf("backfill: upsert record %s: %w", d.SHA, err)
	}
	// Duplicate — fetch the existing row so the caller can still
	// mark-synthesised + project it (idempotent convergence on re-run).
	rec, err = q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{
		Profile: d.Profile,
		Vault:   d.Vault,
		Sha:     d.SHA,
	})
	if err != nil {
		return pgdb.Record{}, false, fmt.Errorf("backfill: get existing record %s: %w", d.SHA, err)
	}
	return rec, false, nil
}

// summaryDocToUpsertParams mirrors internal/server/dual_write.go's mapping
// (kept in sync for consistency): source / tags MUST be non-nil (the
// columns are NOT NULL DEFAULT '{}'), the embedding is set at synth time
// via MarkRecordSynthesised (not here), and attachment fields populate when
// the doc is an attachment_stub carrying them.
func summaryDocToUpsertParams(d osearch.SummaryDoc, att osearch.AttachmentDoc, hasAtt bool) pgdb.UpsertRecordParams {
	p := pgdb.UpsertRecordParams{
		Profile:    d.Profile,
		Vault:      d.Vault,
		Sha:        d.SHA,
		Kind:       osearch.SoRKind(d.Kind),
		MemoryType: optText(string(d.MemoryType)),
		Title:      pgstore.SanitizeText(d.Title),
		RawBody:    optText(d.RawBody),
		SourceUrl:  optText(d.SourceURL),
		Source:     nonNilStrings(d.Source),
		Tags:       nonNilStrings(d.Tags),
		CapturedAt: optTimestamptz(d.CapturedAt),
	}
	// Attachment metadata lives on the companion pb_attachments doc, joined
	// here by SHA (the pb_summaries attachment_stub is only the recall
	// sidecar). Populate the fetch-time fields the projection renders.
	if hasAtt {
		p.MinioKey = optText(att.MinIOKey)
		p.MimeType = optText(att.MIMEType)
		p.OriginalFilename = optText(att.OriginalFilename)
		if att.SizeBytes > 0 {
			p.SizeBytes = pgtype.Int8{Int64: att.SizeBytes, Valid: true}
		}
	}
	return p
}

// backfillEntities runs Pass 2 for one vault: scroll pb_entities, upsert
// each entity (+ aliases), then invert the legacy MentionedBy[] backlinks
// into record_entities links. A MentionedBy SHA with no record in this
// vault is a non-fatal miss (the record may live in another vault, or be
// absent) — counted, not errored.
func backfillEntities(ctx context.Context, opts Options, q *pgdb.Queries, vr VaultRef, batch int, vs *VaultStats) error {
	return opts.OS.ScrollEntitiesWithPrefix(ctx, vr.IndexPrefix, opts.Profile, vr.Vault, batch, func(e osearch.EntityDoc) error {
		slug := e.Slug
		if slug == "" {
			slug = osearch.EntitySlug(e.Name)
		}
		if slug == "" {
			// Unidentifiable entity (no slug, no name) — skip silently.
			return nil
		}
		name := e.Name
		if name == "" {
			name = slug
		}

		if opts.DryRun {
			vs.EntitiesUpserted++
			vs.AliasesAdded += len(e.Aliases)
			for _, sha := range e.MentionedBy {
				if _, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{Profile: opts.Profile, Vault: vr.Vault, Sha: sha}); err != nil {
					// On dry-run the records weren't written, so EVERY
					// MentionedBy resolves to ErrNoRows. Counting these as
					// misses would be misleading; instead count them all as
					// would-be links (the records would exist post-apply).
					if errors.Is(err, pgx.ErrNoRows) {
						vs.LinksCreated++
						continue
					}
					return fmt.Errorf("backfill: dry-run lookup %s: %w", sha, err)
				}
				vs.LinksCreated++
			}
			return nil
		}

		// Per-entity failures count + skip rather than aborting the run.
		if err := backfillOneEntity(ctx, q, opts, vr, slug, name, e, vs); err != nil {
			vs.noteErr(fmt.Errorf("entity %s: %w", slug, err))
		}
		return nil
	})
}

// backfillOneEntity upserts one entity, its aliases, and the inverted
// MentionedBy[] links. Returns the error for the caller to count/skip.
func backfillOneEntity(ctx context.Context, q *pgdb.Queries, opts Options, vr VaultRef, slug, name string, e osearch.EntityDoc, vs *VaultStats) error {
	ent, err := q.UpsertEntity(ctx, pgdb.UpsertEntityParams{
		Profile:     opts.Profile,
		Vault:       vr.Vault,
		Slug:        pgstore.SanitizeText(slug),
		Name:        pgstore.SanitizeText(name),
		Description: optText(e.Body),
		Embedding:   optVector(e.Embedding),
	})
	if err != nil {
		return fmt.Errorf("upsert entity: %w", err)
	}
	vs.EntitiesUpserted++

	for _, alias := range e.Aliases {
		if alias == "" {
			continue
		}
		if err := q.AddEntityAlias(ctx, pgdb.AddEntityAliasParams{EntityID: ent.ID, Alias: pgstore.SanitizeText(alias)}); err != nil {
			return fmt.Errorf("add alias %q: %w", alias, err)
		}
		vs.AliasesAdded++
	}

	// Invert the denormalised backlink: each MentionedBy SHA → a
	// record_entities link, when the record exists in this vault.
	for _, sha := range e.MentionedBy {
		if sha == "" {
			continue
		}
		rec, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{Profile: opts.Profile, Vault: vr.Vault, Sha: sha})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				vs.EntityLinkMisses++
				continue
			}
			return fmt.Errorf("lookup mentioned-by %s: %w", sha, err)
		}
		if err := q.LinkRecordEntity(ctx, pgdb.LinkRecordEntityParams{RecordID: rec.ID, EntityID: ent.ID}); err != nil {
			return fmt.Errorf("link record %d: %w", rec.ID, err)
		}
		vs.LinksCreated++
	}
	return nil
}

// --- small mapping helpers (kept in step with internal/server/dual_write.go) ---

func optText(s string) pgtype.Text {
	s = pgstore.SanitizeText(s)
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func optTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func optVector(emb []float32) *pgvector.Vector {
	if len(emb) == 0 {
		return nil
	}
	v := pgvector.NewVector(emb)
	return &v
}

func nonNilStrings(in []string) []string {
	return pgstore.SanitizeTexts(in)
}
