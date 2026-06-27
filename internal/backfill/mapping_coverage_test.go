// Unit coverage for the PURE mapping / transform / stat-accumulation logic
// in the backfill loader. Everything here is fakeable without live infra:
// SummaryDoc→UpsertRecordParams field mapping, the pgtype/pgvector option
// helpers, slice sanitisation, the per-vault error sampler, and the Stats
// roll-up. The end-to-end Run() flow needs live Postgres + OpenSearch and
// is covered separately under //go:build integration.
package backfill

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

func TestSummaryDocToUpsertParams_FullMapping(t *testing.T) {
	captured := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	d := osearch.SummaryDoc{
		Profile:    "p1",
		Vault:      "v1",
		SHA:        "deadbeef",
		Kind:       osearch.KindWebScrape,
		MemoryType: osearch.MemoryType("semantic"),
		Title:      "Title here",
		RawBody:    "raw body text",
		SourceURL:  "https://example.com/page",
		Source:     []string{"https://example.com/page", "agent:abc"},
		Tags:       []string{"vendor:UIA", "type:invoice"},
		CapturedAt: &captured,
	}

	p := summaryDocToUpsertParams(d, osearch.AttachmentDoc{}, false)

	if p.Profile != "p1" || p.Vault != "v1" || p.Sha != "deadbeef" {
		t.Fatalf("identity not mapped: %+v", p)
	}
	if p.Kind != "web_scrape" {
		t.Errorf("Kind = %q, want web_scrape", p.Kind)
	}
	if !p.MemoryType.Valid || p.MemoryType.String != "semantic" {
		t.Errorf("MemoryType = %+v, want valid semantic", p.MemoryType)
	}
	if p.Title != "Title here" {
		t.Errorf("Title = %q", p.Title)
	}
	if !p.RawBody.Valid || p.RawBody.String != "raw body text" {
		t.Errorf("RawBody = %+v", p.RawBody)
	}
	if !p.SourceUrl.Valid || p.SourceUrl.String != "https://example.com/page" {
		t.Errorf("SourceUrl = %+v", p.SourceUrl)
	}
	if len(p.Source) != 2 || p.Source[0] != "https://example.com/page" {
		t.Errorf("Source = %+v", p.Source)
	}
	if len(p.Tags) != 2 || p.Tags[1] != "type:invoice" {
		t.Errorf("Tags = %+v", p.Tags)
	}
	if !p.CapturedAt.Valid || !p.CapturedAt.Time.Equal(captured) {
		t.Errorf("CapturedAt = %+v, want %v", p.CapturedAt, captured)
	}
	// No attachment metadata when hasAtt=false — these must stay zero/invalid.
	if p.MinioKey.Valid || p.MimeType.Valid || p.OriginalFilename.Valid || p.SizeBytes.Valid {
		t.Errorf("attachment fields populated despite hasAtt=false: %+v", p)
	}
}

func TestSummaryDocToUpsertParams_AttachmentKindCollapsesAndEnriches(t *testing.T) {
	d := osearch.SummaryDoc{
		Profile: "p", Vault: "v", SHA: "sha1",
		Kind:  osearch.KindAttachmentStub,
		Title: "a.pdf",
	}
	att := osearch.AttachmentDoc{
		SHA:              "sha1",
		MinIOKey:         "p/v/attachments/sha1.pdf",
		MIMEType:         "application/pdf",
		OriginalFilename: "a.pdf",
		SizeBytes:        4096,
	}

	p := summaryDocToUpsertParams(d, att, true)

	// SoRKind collapses attachment_stub -> attachment.
	if p.Kind != "attachment" {
		t.Errorf("Kind = %q, want attachment (collapsed)", p.Kind)
	}
	if !p.MinioKey.Valid || p.MinioKey.String != "p/v/attachments/sha1.pdf" {
		t.Errorf("MinioKey = %+v", p.MinioKey)
	}
	if !p.MimeType.Valid || p.MimeType.String != "application/pdf" {
		t.Errorf("MimeType = %+v", p.MimeType)
	}
	if !p.OriginalFilename.Valid || p.OriginalFilename.String != "a.pdf" {
		t.Errorf("OriginalFilename = %+v", p.OriginalFilename)
	}
	if !p.SizeBytes.Valid || p.SizeBytes.Int64 != 4096 {
		t.Errorf("SizeBytes = %+v, want 4096", p.SizeBytes)
	}
}

func TestSummaryDocToUpsertParams_ZeroSizeAttachmentLeavesSizeInvalid(t *testing.T) {
	// SizeBytes <= 0 must NOT set the Int8 (the column stays NULL rather
	// than recording a bogus 0-byte size).
	att := osearch.AttachmentDoc{SHA: "s", MinIOKey: "k", MIMEType: "text/plain", SizeBytes: 0}
	p := summaryDocToUpsertParams(osearch.SummaryDoc{SHA: "s", Kind: osearch.KindAttachmentStub}, att, true)
	if p.SizeBytes.Valid {
		t.Errorf("SizeBytes Valid for zero-size attachment: %+v", p.SizeBytes)
	}
	// Other present fields still enriched.
	if !p.MinioKey.Valid || !p.MimeType.Valid {
		t.Errorf("non-size attachment fields not enriched: %+v", p)
	}
}

func TestSummaryDocToUpsertParams_EmptyOptionalsBecomeInvalid(t *testing.T) {
	// A bare note with no optional content: all the optText-backed columns
	// must be invalid (NULL), and the array columns non-nil but empty.
	d := osearch.SummaryDoc{Profile: "p", Vault: "v", SHA: "s", Kind: osearch.KindNote, Title: "t"}
	p := summaryDocToUpsertParams(d, osearch.AttachmentDoc{}, false)

	if p.MemoryType.Valid || p.RawBody.Valid || p.SourceUrl.Valid {
		t.Errorf("empty optionals should be invalid: %+v", p)
	}
	if p.CapturedAt.Valid {
		t.Errorf("nil CapturedAt should be invalid: %+v", p.CapturedAt)
	}
	if p.Source == nil || p.Tags == nil {
		t.Errorf("Source/Tags must be non-nil for NOT NULL DEFAULT '{}': %+v", p)
	}
	if len(p.Source) != 0 || len(p.Tags) != 0 {
		t.Errorf("Source/Tags must be empty: %+v", p)
	}
}

func TestSummaryDocToUpsertParams_NulStrippedFromTitleAndBody(t *testing.T) {
	// Postgres TEXT rejects NUL; SanitizeText must scrub it on the way in.
	d := osearch.SummaryDoc{
		Profile: "p", Vault: "v", SHA: "s", Kind: osearch.KindNote,
		Title:   "ti\x00tle",
		RawBody: "bo\x00dy",
		Source:  []string{"ok", "ba\x00d"},
	}
	p := summaryDocToUpsertParams(d, osearch.AttachmentDoc{}, false)
	if strings.ContainsRune(p.Title, 0) {
		t.Errorf("Title retained NUL: %q", p.Title)
	}
	if p.Title != "title" {
		t.Errorf("Title = %q, want title", p.Title)
	}
	if !p.RawBody.Valid || strings.ContainsRune(p.RawBody.String, 0) {
		t.Errorf("RawBody retained NUL: %+v", p.RawBody)
	}
	if p.Source[1] != "bad" {
		t.Errorf("Source[1] = %q, want bad", p.Source[1])
	}
}

// The opt*/nonNilStrings mappers moved to internal/pgstore (audit set D, D2);
// their unit tests now live in pgstore/mappers_test.go.

func TestVaultStats_noteErr_CapsSampleErrors(t *testing.T) {
	var vs VaultStats
	// Push more than maxSampleErrors; Errors keeps counting but the sample
	// slice is capped at maxSampleErrors.
	total := maxSampleErrors + 3
	for i := 0; i < total; i++ {
		vs.noteErr(fmt.Errorf("err-%d", i))
	}
	if vs.Errors != total {
		t.Errorf("Errors = %d, want %d", vs.Errors, total)
	}
	if len(vs.SampleErrors) != maxSampleErrors {
		t.Errorf("SampleErrors len = %d, want %d", len(vs.SampleErrors), maxSampleErrors)
	}
	// The retained samples are the FIRST ones, in order.
	if vs.SampleErrors[0] != "err-0" || vs.SampleErrors[maxSampleErrors-1] != fmt.Sprintf("err-%d", maxSampleErrors-1) {
		t.Errorf("SampleErrors not the first-N in order: %v", vs.SampleErrors)
	}
}

func TestStats_add_RollsUpEveryCounter(t *testing.T) {
	var s Stats
	a := VaultStats{
		Vault:           "v1",
		RecordsInserted: 1, RecordsDup: 2, RecordsSynthed: 3,
		EntitiesUpserted: 4, AliasesAdded: 5, LinksCreated: 6,
		EntityLinkMisses: 7, Errors: 8,
	}
	b := VaultStats{
		Vault:           "v2",
		RecordsInserted: 10, RecordsDup: 20, RecordsSynthed: 30,
		EntitiesUpserted: 40, AliasesAdded: 50, LinksCreated: 60,
		EntityLinkMisses: 70, Errors: 80,
	}
	s.add(a)
	s.add(b)

	if len(s.PerVault) != 2 || s.PerVault[0].Vault != "v1" || s.PerVault[1].Vault != "v2" {
		t.Fatalf("PerVault = %+v", s.PerVault)
	}
	want := VaultStats{
		RecordsInserted: 11, RecordsDup: 22, RecordsSynthed: 33,
		EntitiesUpserted: 44, AliasesAdded: 55, LinksCreated: 66,
		EntityLinkMisses: 77, Errors: 88,
	}
	if s.Total.RecordsInserted != want.RecordsInserted ||
		s.Total.RecordsDup != want.RecordsDup ||
		s.Total.RecordsSynthed != want.RecordsSynthed ||
		s.Total.EntitiesUpserted != want.EntitiesUpserted ||
		s.Total.AliasesAdded != want.AliasesAdded ||
		s.Total.LinksCreated != want.LinksCreated ||
		s.Total.EntityLinkMisses != want.EntityLinkMisses ||
		s.Total.Errors != want.Errors {
		t.Errorf("Total = %+v, want %+v", s.Total, want)
	}
	// Total carries no Vault label or SampleErrors (it's a counter roll-up).
	if s.Total.Vault != "" {
		t.Errorf("Total.Vault = %q, want empty", s.Total.Vault)
	}
}

// Run's argument-validation guards return BEFORE touching any infra, so they
// are unit-testable. The happy path needs live PG+OS (integration).
func TestRun_ValidationGuards(t *testing.T) {
	realOS := &osearch.Client{} // non-nil sentinel; never dialed on these paths
	tests := []struct {
		name    string
		opts    Options
		wantSub string
	}{
		{"nil OS", Options{Profile: "p", Vaults: []VaultRef{{Vault: "v"}}}, "nil OpenSearch client"},
		{"nil PG non-dry", Options{OS: realOS, Profile: "p", Vaults: []VaultRef{{Vault: "v"}}}, "nil Postgres pool"},
		{"empty profile", Options{OS: realOS, DryRun: true, Vaults: []VaultRef{{Vault: "v"}}}, "empty profile"},
		{"no vaults", Options{OS: realOS, DryRun: true, Profile: "p"}, "no vaults to backfill"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Run(t.Context(), tt.opts)
			if err == nil {
				t.Fatalf("Run() err = nil, want containing %q", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Run() err = %q, want containing %q", err.Error(), tt.wantSub)
			}
		})
	}
}

func TestEmbeddingModelConstant(t *testing.T) {
	// The reused legacy vectors are tagged with the Phase 6 standard model.
	if embeddingModel != "nomic-embed-text" {
		t.Errorf("embeddingModel = %q", embeddingModel)
	}
}
