package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
	"github.com/neverprepared/phantom-brain/internal/projection"
)

// Phase B1 — dual-write to the Postgres System-of-Record (flag-gated,
// NON-FATAL). Legacy pb_summaries stays authoritative; reads are
// unchanged. Every new-store write here is best-effort: ANY failure (PG
// disabled, pool/River/projection error, timeout) logs a warning,
// increments d.dualWriteFailures, and returns — it must NEVER fail the
// handler request or the synth job. The per-binding DualWrite flag
// defaults OFF, so until an operator opts a binding in, these helpers are
// no-ops.
//
// Embedding handling: records.UpsertRecord (the raw insert) carries NO
// embedding column — the SoR schema only sets the embedding at synth time
// via MarkRecordSynthesised. So dualWriteRaw writes the record without a
// vector; dualWriteSynth fills body/reliability/topic/embedding once the
// distill pass has run.

// dualWriteTimeout bounds each new-store attempt so a slow or unreachable
// Postgres can't stall the live request / synth job.
const dualWriteTimeout = 5 * time.Second

// noteDualWriteFailure logs a warning with (profile, vault, sha) context
// and bumps the daemon's failure counter. This is the "meter" for
// dual-write divergence until phantom-brain grows a real metrics surface.
func (d *Daemon) noteDualWriteFailure(stage, profile, vault, sha string, err error) {
	d.dualWriteFailures.Add(1)
	d.Logger.Warn("phantom-brain: dual-write failed (non-fatal — legacy authoritative)",
		slog.String("stage", stage),
		slog.String("profile", profile),
		slog.String("vault", vault),
		slog.String("sha", sha),
		slog.String("err", err.Error()))
}

// noteDualWriteSkip is the quiet path: PG is simply not configured for the
// binding (ErrPostgresDisabled). Not an error — debug only, no counter.
func (d *Daemon) noteDualWriteSkip(stage, profile, vault, sha string) {
	d.Logger.Debug("phantom-brain: dual-write skipped (postgres disabled for binding)",
		slog.String("stage", stage),
		slog.String("profile", profile),
		slog.String("vault", vault),
		slog.String("sha", sha))
}

// DualWriteFailureCount returns the cumulative count of non-fatal
// dual-write failures since daemon start. Exposed for tests + (optionally)
// observability surfaces.
func (d *Daemon) DualWriteFailureCount() int64 {
	return d.dualWriteFailures.Load()
}

// dualWriteRaw mirrors a freshly-written legacy SummaryDoc into the
// Postgres SoR as a raw (unsynthesised) record + enqueues its projection,
// transactionally via projection.WriteRecordAndEnqueue. Called from the
// write handlers AFTER the legacy UpsertSummary + synth Enqueue succeed.
//
// NON-FATAL throughout: flag-off, PG-disabled, or any error returns
// without disturbing the caller. The handler has already returned its
// success to the agent; this is pure parallel-run mirroring.
func (d *Daemon) dualWriteRaw(ctx context.Context, b VaultBinding, doc osearch.SummaryDoc) {
	if !b.DualWrite {
		return
	}
	profile, vault, sha := doc.Profile, doc.Vault, doc.SHA

	view, err := d.resolvePG(b)
	if err != nil {
		if errors.Is(err, ErrPostgresDisabled) {
			d.noteDualWriteSkip("raw", profile, vault, sha)
			return
		}
		d.noteDualWriteFailure("raw-resolve", profile, vault, sha, err)
		return
	}

	ctx2, cancel := context.WithTimeout(ctx, dualWriteTimeout)
	defer cancel()

	params := summaryDocToUpsertParams(doc)
	if _, err := projection.WriteRecordAndEnqueue(ctx2, view.Pool(), view.River(), params); err != nil {
		d.noteDualWriteFailure("raw-write", profile, vault, sha, err)
		return
	}
	d.Logger.Debug("phantom-brain: dual-write raw record mirrored to postgres",
		slog.String("profile", profile),
		slog.String("vault", vault),
		slog.String("sha", sha))
}

// summaryDocToUpsertParams maps a legacy osearch.SummaryDoc to the SoR
// UpsertRecordParams. records.source / records.tags are NOT NULL DEFAULT
// '{}' so they MUST be non-nil slices (nil → SQL NULL → constraint
// violation). The attachment fields are populated when present (the attach
// handler routes its companion SummaryDoc here too, but the binary
// metadata lives on the AttachmentDoc — dualWriteAttachRaw fills it).
func summaryDocToUpsertParams(doc osearch.SummaryDoc) pgdb.UpsertRecordParams {
	return pgdb.UpsertRecordParams{
		Profile:    doc.Profile,
		Vault:      doc.Vault,
		Sha:        doc.SHA,
		Kind:       osearch.SoRKind(doc.Kind),
		MemoryType: optText(string(doc.MemoryType)),
		Title:      pgstore.SanitizeText(doc.Title),
		RawBody:    optText(doc.RawBody),
		SourceUrl:  optText(doc.SourceURL),
		Source:     nonNilStrings(doc.Source),
		Tags:       nonNilStrings(doc.Tags),
		CapturedAt: optTimestamptz(doc.CapturedAt),
	}
}

// dualWriteAttachRaw mirrors an attachment write into the SoR. The attach
// handler builds both an AttachmentDoc (binary metadata) and a companion
// SummaryDoc (recall stub). We write ONE SoR record carrying the stub's
// identity + the attachment's minio_key/mime_type/size/filename so the
// projection has the attachment fields. NON-FATAL like dualWriteRaw.
func (d *Daemon) dualWriteAttachRaw(ctx context.Context, b VaultBinding, stub osearch.SummaryDoc, att osearch.AttachmentDoc) {
	if !b.DualWrite {
		return
	}
	profile, vault, sha := stub.Profile, stub.Vault, stub.SHA

	view, err := d.resolvePG(b)
	if err != nil {
		if errors.Is(err, ErrPostgresDisabled) {
			d.noteDualWriteSkip("attach-raw", profile, vault, sha)
			return
		}
		d.noteDualWriteFailure("attach-raw-resolve", profile, vault, sha, err)
		return
	}

	ctx2, cancel := context.WithTimeout(ctx, dualWriteTimeout)
	defer cancel()

	params := summaryDocToUpsertParams(stub)
	params.MinioKey = optText(att.MinIOKey)
	params.MimeType = optText(att.MIMEType)
	params.OriginalFilename = optText(att.OriginalFilename)
	if att.SizeBytes > 0 {
		params.SizeBytes = pgtype.Int8{Int64: att.SizeBytes, Valid: true}
	}

	if _, err := projection.WriteRecordAndEnqueue(ctx2, view.Pool(), view.River(), params); err != nil {
		d.noteDualWriteFailure("attach-raw-write", profile, vault, sha, err)
		return
	}
	d.Logger.Debug("phantom-brain: dual-write attachment record mirrored to postgres",
		slog.String("profile", profile),
		slog.String("vault", vault),
		slog.String("sha", sha))
}

// synthResult carries the distilled state processJob computes, so the
// OnSynthesised callback can mirror it into the SoR without coupling
// synth_queue.go to Postgres types.
type synthResult struct {
	Body           string
	Reliability    string
	Topic          string
	GateReason     string
	Embedding      []float32
	EmbeddingModel string

	// EntityNames maps entity slug → display name, faithful to the synth
	// loop's parallel entities[]/entitySlugs[] (names + their slugs).
	EntityNames map[string]string
}

// dualWriteSynth mirrors the distilled (synthesised) state into the SoR
// and re-projects, using the transactional outbox so the record update,
// entity upserts, links, and the re-projection enqueue all commit (or roll
// back) atomically. Called from the synth worker's OnSynthesised callback
// AFTER the legacy UpsertSummary(Synthesised=true) succeeds.
//
// If the raw dual-write never landed (no record for this SHA), we warn +
// meter + return — the backfill / next write reconciles. NON-FATAL
// throughout: any error rolls the tx back, bumps the counter, returns nil.
func (d *Daemon) dualWriteSynth(ctx context.Context, b VaultBinding, profile, vault, sha string, res synthResult) {
	if !b.DualWrite {
		return
	}

	view, err := d.resolvePG(b)
	if err != nil {
		if errors.Is(err, ErrPostgresDisabled) {
			d.noteDualWriteSkip("synth", profile, vault, sha)
			return
		}
		d.noteDualWriteFailure("synth-resolve", profile, vault, sha, err)
		return
	}

	ctx2, cancel := context.WithTimeout(ctx, dualWriteTimeout)
	defer cancel()

	tx, err := view.Pool().Begin(ctx2)
	if err != nil {
		d.noteDualWriteFailure("synth-begin", profile, vault, sha, err)
		return
	}
	// Rollback is a no-op after a successful Commit; this guarantees
	// rollback on every early-return path below.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx2)
		}
	}()

	q := pgstore.New(tx)

	rec, err := q.GetRecordBySHA(ctx2, pgdb.GetRecordBySHAParams{
		Profile: profile,
		Vault:   vault,
		Sha:     sha,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The raw dual-write didn't land (flag flipped on between
			// raw + synth, or the raw write failed). Backfill / next
			// write reconciles. Meter it so divergence is visible.
			d.noteDualWriteFailure("synth-missing-record", profile, vault, sha, err)
			return
		}
		d.noteDualWriteFailure("synth-get", profile, vault, sha, err)
		return
	}

	if err := q.MarkRecordSynthesised(ctx2, pgdb.MarkRecordSynthesisedParams{
		Body:           optText(res.Body),
		Reliability:    optText(res.Reliability),
		Topic:          optText(res.Topic),
		GateReason:     optText(res.GateReason),
		Embedding:      optVector(res.Embedding),
		EmbeddingModel: optText(res.EmbeddingModel),
		ID:             rec.ID,
	}); err != nil {
		d.noteDualWriteFailure("synth-mark", profile, vault, sha, err)
		return
	}

	for slug, name := range res.EntityNames {
		if slug == "" {
			continue
		}
		if name == "" {
			name = slug
		}
		ent, err := q.UpsertEntity(ctx2, pgdb.UpsertEntityParams{
			Profile: profile,
			Vault:   vault,
			Slug:    pgstore.SanitizeText(slug),
			Name:    pgstore.SanitizeText(name),
		})
		if err != nil {
			d.noteDualWriteFailure("synth-entity", profile, vault, sha, err)
			return
		}
		if err := q.LinkRecordEntity(ctx2, pgdb.LinkRecordEntityParams{
			RecordID: rec.ID,
			EntityID: ent.ID,
		}); err != nil {
			d.noteDualWriteFailure("synth-link", profile, vault, sha, err)
			return
		}
	}

	// Re-enqueue the projection in the SAME tx (the outbox) so the
	// updated body/reliability/topic land in pb_records. River won't start
	// the job until the tx commits (snapshot visibility).
	if err := projection.EnqueueProjectTx(ctx2, view.River(), tx, rec.ID); err != nil {
		d.noteDualWriteFailure("synth-enqueue", profile, vault, sha, err)
		return
	}

	if err := tx.Commit(ctx2); err != nil {
		d.noteDualWriteFailure("synth-commit", profile, vault, sha, err)
		return
	}
	committed = true
	d.Logger.Debug("phantom-brain: dual-write synth mirror committed to postgres",
		slog.String("profile", profile),
		slog.String("vault", vault),
		slog.String("sha", sha))
}

// --- small mapping helpers ----------------------------------------

// optText returns a NULL pgtype.Text for an empty string, else a valid
// one. Keeps empty optional fields out of the SoR as SQL NULL.
func optText(s string) pgtype.Text {
	s = pgstore.SanitizeText(s)
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// optTimestamptz returns a NULL pgtype.Timestamptz for a nil time, else a
// valid one.
func optTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// optVector returns nil for an empty embedding (pgvector column stays
// NULL), else a *pgvector.Vector. OS rejects all-zero vectors but the SoR
// pgvector column does not — an empty slice maps to NULL, never a zero
// vector.
func optVector(emb []float32) *pgvector.Vector {
	if len(emb) == 0 {
		return nil
	}
	v := pgvector.NewVector(emb)
	return &v
}

// nonNilStrings guarantees a non-nil slice for NOT NULL DEFAULT '{}'
// columns (records.source / records.tags). A nil input becomes an empty
// (non-nil) slice so pgx sends '{}' rather than NULL.
func nonNilStrings(in []string) []string {
	return pgstore.SanitizeTexts(in)
}
