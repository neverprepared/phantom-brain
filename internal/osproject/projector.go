package osproject

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
	"github.com/neverprepared/phantom-brain/internal/projection"
)

// Projector is the SoR→OS write projection: it upserts one pgdb.Record
// into the pb_records search index, keyed on _id = profile:vault:sha
// (osearch.DocID). The upsert is idempotent — re-projecting the same SHA
// replaces the doc in place, so River's at-least-once delivery (a job
// may be worked twice after a crash) converges to the same state.
//
// Embedding is OPTIONAL: a record projects on write, often before synth
// has computed its vector, so the `embedding` field is simply absent
// until a later re-project fills it. A nil/empty vector is never sent —
// OpenSearch rejects zero/empty knn vectors.
type Projector struct {
	client         *osearch.Client
	prefix         string
	waitForRefresh bool
}

// New builds a Projector that writes into the pb_records index resolved
// from the supplied per-binding prefix. waitForRefresh defaults false
// (production relies on the 1s refresh_interval); use NewWithRefresh in
// tests that need writes searchable on return.
func New(client *osearch.Client, prefix string) *Projector {
	return &Projector{client: client, prefix: prefix}
}

// NewWithRefresh is New with waitForRefresh forced — every Project call
// makes the doc immediately searchable. Test-only.
func NewWithRefresh(client *osearch.Client, prefix string) *Projector {
	return &Projector{client: client, prefix: prefix, waitForRefresh: true}
}

// recordDoc is the projected shape. json tags match the pb_records
// mapping field names exactly. Fields are omitempty so absent values
// (null pgtype, nil vector) do not serialise — in particular a nil
// embedding must be dropped entirely, and absent dates must not become
// the zero epoch.
type recordDoc struct {
	ID      int64  `json:"id"`
	Profile string `json:"profile"`
	Vault   string `json:"vault"`
	Sha     string `json:"sha"`

	Kind        string `json:"kind"`
	MemoryType  string `json:"memory_type,omitempty"`
	Topic       string `json:"topic,omitempty"`
	Reliability string `json:"reliability,omitempty"`

	Source           []string `json:"source,omitempty"`
	SourceURL        string   `json:"source_url,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	MimeType         string   `json:"mime_type,omitempty"`
	OriginalFilename string   `json:"original_filename,omitempty"`
	EmbeddingModel   string   `json:"embedding_model,omitempty"`

	Title         string `json:"title,omitempty"`
	Body          string `json:"body,omitempty"`
	ExtractedText string `json:"extracted_text,omitempty"`

	CapturedAt *time.Time `json:"captured_at,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`

	Embedding []float32 `json:"embedding,omitempty"`
}

// textOrEmpty returns the string when the pgtype.Text is non-NULL,
// empty otherwise.
func textOrEmpty(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

// timeOrNil returns a *time.Time when the timestamp is non-NULL, nil
// otherwise — so omitempty drops absent dates rather than serialising
// the zero epoch.
func timeOrNil(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	tt := t.Time
	return &tt
}

// Project upserts rec into pb_records. raw_body is intentionally skipped
// (Postgres-only pre-synth noise); body and extracted_text are the
// analyzed text the renderer + recall use.
func (p *Projector) Project(ctx context.Context, rec pgdb.Record) error {
	doc := recordDoc{
		ID:               rec.ID,
		Profile:          rec.Profile,
		Vault:            rec.Vault,
		Sha:              rec.Sha,
		Kind:             rec.Kind,
		MemoryType:       textOrEmpty(rec.MemoryType),
		Topic:            textOrEmpty(rec.Topic),
		Reliability:      textOrEmpty(rec.Reliability),
		Source:           rec.Source,
		SourceURL:        textOrEmpty(rec.SourceUrl),
		Tags:             rec.Tags,
		MimeType:         textOrEmpty(rec.MimeType),
		OriginalFilename: textOrEmpty(rec.OriginalFilename),
		EmbeddingModel:   textOrEmpty(rec.EmbeddingModel),
		Title:            rec.Title,
		Body:             textOrEmpty(rec.Body),
		ExtractedText:    textOrEmpty(rec.ExtractedText),
		CapturedAt:       timeOrNil(rec.CapturedAt),
		CreatedAt:        timeOrNil(rec.CreatedAt),
		UpdatedAt:        timeOrNil(rec.UpdatedAt),
	}
	// Optional embedding — omit the field entirely when absent; never
	// send a nil/empty vector (OpenSearch rejects zero/empty knn vectors).
	if rec.Embedding != nil {
		if s := rec.Embedding.Slice(); len(s) > 0 {
			doc.Embedding = s
		}
	}

	id := osearch.DocID(rec.Profile, rec.Vault, rec.Sha)
	version := projectionVersion(rec)
	if err := p.client.PutDocVersioned(ctx, p.prefix, LogicalRecords, id, doc, version, p.waitForRefresh); err != nil {
		if errors.Is(err, osearch.ErrVersionConflict) {
			// A newer version of this record already won the race in pb_records
			// (e.g. the post-synth distilled re-projection landed before this
			// older raw upsert). Dropping the stale write IS the desired end
			// state, so this is success — not an error to retry.
			slog.Debug("osproject: dropped stale projection (version conflict)",
				"record_id", rec.ID, "id", id, "version", version)
			return nil
		}
		return fmt.Errorf("osproject: project record %d (%s): %w", rec.ID, id, err)
	}
	return nil
}

// projectionVersion derives the OpenSearch external_gte version for a record
// from updated_at — the timestamp synth bumps when it rewrites the row — so a
// post-synth re-projection always outranks the initial raw projection
// regardless of which HTTP write lands last. Falls back to created_at, then to
// 0 (unversioned last-write-wins) when neither timestamp is set. Postgres
// stores microsecond resolution, so distinct writes differ by >=1000ns; the
// initial and post-synth writes are seconds apart, well clear of collision.
func projectionVersion(rec pgdb.Record) int64 {
	if rec.UpdatedAt.Valid {
		return rec.UpdatedAt.Time.UnixNano()
	}
	if rec.CreatedAt.Valid {
		return rec.CreatedAt.Time.UnixNano()
	}
	return 0
}

// DeleteProjection removes the pb_records doc for (profile, vault, sha).
// Idempotent — deleting an absent doc is not an error.
func (p *Projector) DeleteProjection(ctx context.Context, profile, vault, sha string) error {
	id := osearch.DocID(profile, vault, sha)
	if err := p.client.DeleteDoc(ctx, p.prefix, LogicalRecords, id); err != nil {
		return fmt.Errorf("osproject: delete projection %s: %w", id, err)
	}
	return nil
}

// Compile-time assertion that *Projector satisfies the projection
// worker's pluggable target.
var _ projection.Projector = (*Projector)(nil)
