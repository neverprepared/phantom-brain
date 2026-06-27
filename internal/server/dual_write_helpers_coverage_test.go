package server

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
func pgTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// The opt*/nonNilStrings mappers moved to internal/pgstore (audit set D, D2);
// their unit tests now live in pgstore/mappers_test.go. The tests below cover
// the server-package mapping shims that consume them.

func TestSummaryDocToUpsertParams(t *testing.T) {
	captured := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	doc := osearch.SummaryDoc{
		Profile:    "personal",
		Vault:      "memory",
		SHA:        "abc123",
		Kind:       osearch.KindNote,
		MemoryType: osearch.MemorySemantic,
		Title:      "Title",
		RawBody:    "raw body",
		SourceURL:  "https://example.com",
		Source:     []string{"task:1"},
		Tags:       []string{"vendor:UIA"},
		CapturedAt: &captured,
	}
	p := summaryDocToUpsertParams(doc)
	if p.Profile != "personal" || p.Vault != "memory" || p.Sha != "abc123" {
		t.Errorf("identity not mapped: %+v", p)
	}
	if p.Kind != string(osearch.KindNote) {
		t.Errorf("kind = %q, want %q", p.Kind, osearch.KindNote)
	}
	if !p.MemoryType.Valid || p.MemoryType.String != "semantic" {
		t.Errorf("memory_type = %+v", p.MemoryType)
	}
	if p.Title != "Title" || !p.RawBody.Valid || p.RawBody.String != "raw body" {
		t.Errorf("title/raw_body not mapped: %+v", p)
	}
	if !p.SourceUrl.Valid || p.SourceUrl.String != "https://example.com" {
		t.Errorf("source_url = %+v", p.SourceUrl)
	}
	if p.Source == nil || p.Tags == nil {
		t.Errorf("source/tags must be non-nil for NOT NULL columns: source=%v tags=%v", p.Source, p.Tags)
	}
	if !p.CapturedAt.Valid || !p.CapturedAt.Time.Equal(captured) {
		t.Errorf("captured_at = %+v", p.CapturedAt)
	}
}

func TestSummaryDocToUpsertParams_Embedding(t *testing.T) {
	// Non-empty embedding maps to a non-nil vector param so the raw write
	// persists records.embedding (restoring kNN / semantic recall).
	withEmb := osearch.SummaryDoc{
		Profile: "p", Vault: "v", SHA: "s", Kind: osearch.KindNote, Title: "t",
		Embedding: []float32{0.1, 0.2, 0.3},
	}
	p := summaryDocToUpsertParams(withEmb)
	if p.Embedding == nil {
		t.Fatal("non-empty embedding must map to a non-nil vector param")
	}
	if got := p.Embedding.Slice(); len(got) != 3 || got[0] != 0.1 {
		t.Errorf("embedding param not mapped faithfully: %v", got)
	}

	// Nil and empty embeddings map to a nil param → SQL NULL, never an
	// all-zero vector (which pgvector rejects under cosine).
	for name, doc := range map[string]osearch.SummaryDoc{
		"nil":   {Profile: "p", Vault: "v", SHA: "s", Kind: osearch.KindNote, Title: "t", Embedding: nil},
		"empty": {Profile: "p", Vault: "v", SHA: "s", Kind: osearch.KindNote, Title: "t", Embedding: []float32{}},
	} {
		if got := summaryDocToUpsertParams(doc).Embedding; got != nil {
			t.Errorf("%s embedding must map to nil param, got %+v", name, got)
		}
	}
}

func TestSummaryDocToUpsertParams_AttachmentKindCollapses(t *testing.T) {
	// SoR collapses the dual-index attachment shape to a single
	// "attachment" record — the check constraint rejects the stub kind.
	doc := osearch.SummaryDoc{
		Profile: "p", Vault: "v", SHA: "s",
		Kind:  osearch.KindAttachmentStub,
		Title: "file.pdf",
	}
	p := summaryDocToUpsertParams(doc)
	if p.Kind != "attachment" {
		t.Errorf("attachment_stub should collapse to %q, got %q", "attachment", p.Kind)
	}
	// nil source/tags still become non-nil.
	if p.Source == nil || p.Tags == nil {
		t.Errorf("nil source/tags must become non-nil, got source=%v tags=%v", p.Source, p.Tags)
	}
}

func TestPGRecordToSummaryDoc(t *testing.T) {
	captured := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	rec := pgdb.Record{
		Profile:     "personal",
		Vault:       "memory",
		Sha:         "deadbeef",
		Kind:        "attachment", // SoR singular → KindAttachmentStub
		MemoryType:  pgText("episodic"),
		Title:       "A title",
		RawBody:     pgText("raw"),
		Body:        pgText("distilled"),
		SourceUrl:   pgText("https://x"),
		Source:      []string{"agent:1"},
		Tags:        []string{"t"},
		Reliability: pgText("high"),
		Topic:       pgText("memory"),
		GateReason:  pgText("good"),
		Synthesised: true,
		CapturedAt:  pgTimestamptz(captured),
	}
	doc := pgRecordToSummaryDoc(rec)
	if doc.Kind != osearch.KindAttachmentStub {
		t.Errorf("kind %q should translate back to attachment_stub, got %q", rec.Kind, doc.Kind)
	}
	if doc.Profile != "personal" || doc.SHA != "deadbeef" {
		t.Errorf("identity not mapped: %+v", doc)
	}
	if doc.MemoryType != osearch.MemoryEpisodic {
		t.Errorf("memory_type = %q", doc.MemoryType)
	}
	if doc.Body != "distilled" || doc.RawBody != "raw" {
		t.Errorf("body/raw not mapped: body=%q raw=%q", doc.Body, doc.RawBody)
	}
	if doc.Reliability != osearch.Reliability("high") || doc.Topic != "memory" {
		t.Errorf("gate verdict not mapped: %+v", doc)
	}
	if !doc.Synthesised {
		t.Error("synthesised should be true")
	}
	if doc.CapturedAt == nil || !doc.CapturedAt.Equal(captured) {
		t.Errorf("captured_at = %+v", doc.CapturedAt)
	}
	if doc.Embedding != nil {
		t.Errorf("nil embedding should stay nil, got %v", doc.Embedding)
	}
}

func TestPGRecordToSummaryDoc_NonAttachmentKindAndNoCapture(t *testing.T) {
	rec := pgdb.Record{
		Profile: "p", Vault: "v", Sha: "x",
		Kind:  "note",
		Title: "t",
		// CapturedAt invalid (zero value) → doc.CapturedAt stays nil.
	}
	doc := pgRecordToSummaryDoc(rec)
	if doc.Kind != osearch.Kind("note") {
		t.Errorf("non-attachment kind should pass through, got %q", doc.Kind)
	}
	if doc.CapturedAt != nil {
		t.Errorf("invalid captured_at should map to nil pointer, got %+v", doc.CapturedAt)
	}
}

func TestErrIsNoRows(t *testing.T) {
	if !errIsNoRows(pgx.ErrNoRows) {
		t.Error("pgx.ErrNoRows should be recognised")
	}
	if !errIsNoRows(errWrap(pgx.ErrNoRows)) {
		t.Error("wrapped pgx.ErrNoRows should be recognised via errors.Is")
	}
	if errIsNoRows(errors.New("other")) {
		t.Error("unrelated error should not be reported as no-rows")
	}
	if errIsNoRows(nil) {
		t.Error("nil should not be reported as no-rows")
	}
}

func errWrap(err error) error { return &wrappedErr{err} }

type wrappedErr struct{ err error }

func (w *wrappedErr) Error() string { return "wrapped: " + w.err.Error() }
func (w *wrappedErr) Unwrap() error { return w.err }
