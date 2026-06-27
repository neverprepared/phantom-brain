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

// Phase D1 — the Postgres System-of-Record + its pb_records projection
// is now the SOLE authoritative store. The legacy pb_summaries/
// pb_entities/pb_attachments indices are no longer written. These
// helpers (writeRecordRaw / writeAttachRecord / writeSynthResult) are
// THE write path: every failure PROPAGATES to the caller (the write
// handlers return 502 so the agent's write-ahead queue retries; the
// synth worker logs + leaves the record raw for a later re-enqueue).
//
// PG is mandatory: buildBindingDeps fails startup when any binding has
// no Postgres view, so resolvePG returning ErrPostgresDisabled here is
// a real configuration error, not a tolerated "PG off" state.
//
// Embedding handling: records.UpsertRecord (the raw insert) carries NO
// embedding column — the SoR schema only sets the embedding at synth
// time via MarkRecordSynthesised. So writeRecordRaw writes the record
// without a vector; writeSynthResult fills body/reliability/topic/
// embedding once the distill pass has run.

// pgWriteTimeout bounds each SoR write attempt so a slow or unreachable
// Postgres can't stall the live request / synth job indefinitely.
const pgWriteTimeout = 5 * time.Second

// noteSoRWriteFailure logs a warning with (profile, vault, sha) context
// and bumps the daemon's failure counter. The error itself propagates
// to the caller; this is the structured "meter" + log for SoR write
// failures until phantom-brain grows a real metrics surface.
func (d *Daemon) noteSoRWriteFailure(stage, profile, vault, sha string, err error) {
	d.dualWriteFailures.Add(1)
	d.Logger.Warn("phantom-brain: SoR write failed",
		slog.String("stage", stage),
		slog.String("profile", profile),
		slog.String("vault", vault),
		slog.String("sha", sha),
		slog.String("err", err.Error()))
}

// DualWriteFailureCount returns the cumulative count of SoR write
// failures since daemon start. Exposed for tests + (optionally)
// observability surfaces.
func (d *Daemon) DualWriteFailureCount() int64 {
	return d.dualWriteFailures.Load()
}

// writeRecordRaw writes a freshly-perceived/learned SummaryDoc into the
// Postgres SoR as a raw (unsynthesised) record + enqueues its projection,
// transactionally via projection.WriteRecordAndEnqueue. This is THE write
// — the handler returns 502 on error so the agent's wqueue retries.
func (d *Daemon) writeRecordRaw(ctx context.Context, b VaultBinding, doc osearch.SummaryDoc) error {
	profile, vault, sha := doc.Profile, doc.Vault, doc.SHA

	view, err := d.resolvePG(b)
	if err != nil {
		d.noteSoRWriteFailure("raw-resolve", profile, vault, sha, err)
		return err
	}

	ctx2, cancel := context.WithTimeout(ctx, pgWriteTimeout)
	defer cancel()

	params := summaryDocToUpsertParams(doc)
	if _, err := projection.WriteRecordAndEnqueue(ctx2, view.Pool(), view.River(), params); err != nil {
		d.noteSoRWriteFailure("raw-write", profile, vault, sha, err)
		return err
	}
	d.Logger.Debug("phantom-brain: raw record written to postgres SoR",
		slog.String("profile", profile),
		slog.String("vault", vault),
		slog.String("sha", sha))
	return nil
}

// summaryDocToUpsertParams maps an osearch.SummaryDoc to the SoR
// UpsertRecordParams. records.source / records.tags are NOT NULL DEFAULT
// '{}' so they MUST be non-nil slices (nil → SQL NULL → constraint
// violation). The attachment fields are populated by writeAttachRecord.
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

// writeAttachRecord writes an attachment into the SoR. The attach handler
// builds an AttachmentDoc (binary metadata) and a companion stub
// SummaryDoc (recall identity). We write ONE SoR record carrying the
// stub's identity + the attachment's minio_key/mime_type/size/filename so
// the projection has the attachment fields. THE write — handler returns
// 502 on error so the agent's wqueue retries.
func (d *Daemon) writeAttachRecord(ctx context.Context, b VaultBinding, stub osearch.SummaryDoc, att osearch.AttachmentDoc) error {
	profile, vault, sha := stub.Profile, stub.Vault, stub.SHA

	view, err := d.resolvePG(b)
	if err != nil {
		d.noteSoRWriteFailure("attach-raw-resolve", profile, vault, sha, err)
		return err
	}

	ctx2, cancel := context.WithTimeout(ctx, pgWriteTimeout)
	defer cancel()

	params := summaryDocToUpsertParams(stub)
	params.MinioKey = optText(att.MinIOKey)
	params.MimeType = optText(att.MIMEType)
	params.OriginalFilename = optText(att.OriginalFilename)
	if att.SizeBytes > 0 {
		params.SizeBytes = pgtype.Int8{Int64: att.SizeBytes, Valid: true}
	}

	if _, err := projection.WriteRecordAndEnqueue(ctx2, view.Pool(), view.River(), params); err != nil {
		d.noteSoRWriteFailure("attach-raw-write", profile, vault, sha, err)
		return err
	}
	d.Logger.Debug("phantom-brain: attachment record written to postgres SoR",
		slog.String("profile", profile),
		slog.String("vault", vault),
		slog.String("sha", sha))
	return nil
}

// synthResult carries the distilled state processJob computes, so the
// synth-write helper can persist it into the SoR without coupling
// synth_queue.go to Postgres types.
type synthResult struct {
	Body           string
	Reliability    string
	Topic          string
	GateReason     string
	Embedding      []float32
	EmbeddingModel string

	// CaptureMinIOKey / CaptureSizeBytes carry the raw-source capture
	// pointer the synth pass stashed in MinIO (best-effort, gated on
	// [capture]). Empty/zero when capture is off, the source had no URL,
	// or the fetch failed — writeSynthResult then persists SQL NULL.
	CaptureMinIOKey  string
	CaptureSizeBytes int64

	// EntityNames maps entity slug → display name, faithful to the synth
	// loop's parallel entities[]/entitySlugs[] (names + their slugs).
	EntityNames map[string]string
}

// writeSynthResult persists the distilled (synthesised) state into the
// SoR and re-projects, using the transactional outbox so the record
// update, entity upserts, links, and the re-projection enqueue all commit
// (or roll back) atomically. Called by the synth worker after the
// gate/distill/entity passes run.
//
// Error PROPAGATES: the worker logs it and leaves the record raw so a
// later re-enqueue (brain_reflect / brain_resynth) retries.
func (d *Daemon) writeSynthResult(ctx context.Context, b VaultBinding, profile, vault, sha string, res synthResult) error {
	view, err := d.resolvePG(b)
	if err != nil {
		d.noteSoRWriteFailure("synth-resolve", profile, vault, sha, err)
		return err
	}

	ctx2, cancel := context.WithTimeout(ctx, pgWriteTimeout)
	defer cancel()

	tx, err := view.Pool().Begin(ctx2)
	if err != nil {
		d.noteSoRWriteFailure("synth-begin", profile, vault, sha, err)
		return err
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
		// No record for this SHA — the raw write never landed (it failed,
		// so synth shouldn't have been enqueued). Meter + propagate so the
		// worker logs it; a later re-enqueue reconciles.
		d.noteSoRWriteFailure("synth-get", profile, vault, sha, err)
		return err
	}

	if err := q.MarkRecordSynthesised(ctx2, pgdb.MarkRecordSynthesisedParams{
		Body:             optText(res.Body),
		Reliability:      optText(res.Reliability),
		Topic:            optText(res.Topic),
		GateReason:       optText(res.GateReason),
		Embedding:        optVector(res.Embedding),
		EmbeddingModel:   optText(res.EmbeddingModel),
		CaptureMinioKey:  optText(res.CaptureMinIOKey),
		CaptureSizeBytes: optInt8(res.CaptureSizeBytes),
		ID:               rec.ID,
	}); err != nil {
		d.noteSoRWriteFailure("synth-mark", profile, vault, sha, err)
		return err
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
			d.noteSoRWriteFailure("synth-entity", profile, vault, sha, err)
			return err
		}
		if err := q.LinkRecordEntity(ctx2, pgdb.LinkRecordEntityParams{
			RecordID: rec.ID,
			EntityID: ent.ID,
		}); err != nil {
			d.noteSoRWriteFailure("synth-link", profile, vault, sha, err)
			return err
		}
	}

	// Re-enqueue the projection in the SAME tx (the outbox) so the
	// updated body/reliability/topic land in pb_records. River won't start
	// the job until the tx commits (snapshot visibility).
	if err := projection.EnqueueProjectTx(ctx2, view.River(), tx, rec.ID); err != nil {
		d.noteSoRWriteFailure("synth-enqueue", profile, vault, sha, err)
		return err
	}

	if err := tx.Commit(ctx2); err != nil {
		d.noteSoRWriteFailure("synth-commit", profile, vault, sha, err)
		return err
	}
	committed = true
	d.Logger.Debug("phantom-brain: synth result committed to postgres SoR",
		slog.String("profile", profile),
		slog.String("vault", vault),
		slog.String("sha", sha))
	return nil
}

// errIsNoRows reports whether err is pgx.ErrNoRows, kept as a small
// helper for the synth read path.
func errIsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// pgRecordToSummaryDoc maps a SoR pgdb.Record back into an in-memory
// osearch.SummaryDoc so the synth pipeline (CheckCoherence / RunGate /
// SummarizeContent / extractEntitiesBest) keeps working against its
// existing SummaryDoc shape with minimal churn. Only the fields the
// pipeline reads are populated; the result is NEVER written to OpenSearch
// — it is a read-side adapter only.
//
// Kind: the SoR stores "attachment" (singular) for attachments; the
// pipeline keys off KindAttachmentStub, so we translate back. Everything
// else passes through unchanged.
func pgRecordToSummaryDoc(rec pgdb.Record) osearch.SummaryDoc {
	kind := osearch.Kind(rec.Kind)
	if rec.Kind == "attachment" {
		kind = osearch.KindAttachmentStub
	}
	doc := osearch.SummaryDoc{
		Profile:     rec.Profile,
		Vault:       rec.Vault,
		SHA:         rec.Sha,
		Kind:        kind,
		MemoryType:  osearch.MemoryType(rec.MemoryType.String),
		Title:       rec.Title,
		RawBody:     rec.RawBody.String,
		Body:        rec.Body.String,
		SourceURL:   rec.SourceUrl.String,
		Source:      rec.Source,
		Tags:        rec.Tags,
		Reliability: osearch.Reliability(rec.Reliability.String),
		Topic:       rec.Topic.String,
		GateReason:  rec.GateReason.String,
		Synthesised: rec.Synthesised,
	}
	if rec.CapturedAt.Valid {
		t := rec.CapturedAt.Time
		doc.CapturedAt = &t
	}
	if rec.Embedding != nil {
		doc.Embedding = rec.Embedding.Slice()
	}
	return doc
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

// optInt8 returns a NULL pgtype.Int8 for a non-positive value, else a
// valid one. Mirrors the >0 guard used for attachment size_bytes so an
// absent/zero capture size lands as SQL NULL rather than 0.
func optInt8(n int64) pgtype.Int8 {
	if n <= 0 {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: n, Valid: true}
}

// optVector returns nil for an empty embedding (pgvector column stays
// NULL), else a *pgvector.Vector. An empty slice maps to NULL, never a
// zero vector.
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
