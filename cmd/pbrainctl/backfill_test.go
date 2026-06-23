package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/neverprepared/mcp-phantom-brain/internal/osearch"
)

// fakeBackfillClient is an in-memory implementation of
// backfillStubClient sufficient for the backfill unit tests. Mirrors
// what ingest-bulk does with its embedder fake — keep tests offline.
type fakeBackfillClient struct {
	mu          sync.Mutex
	attachments []osearch.AttachmentDoc
	summaries   map[string]osearch.SummaryDoc // key: profile:vault:sha
	upsertErr   error
	getErr      error
}

func newFakeBackfillClient(atts []osearch.AttachmentDoc) *fakeBackfillClient {
	return &fakeBackfillClient{
		attachments: atts,
		summaries:   map[string]osearch.SummaryDoc{},
	}
}

func (f *fakeBackfillClient) ScrollAttachments(_ context.Context, profile, vault string, _ int, fn func(osearch.AttachmentDoc) error) error {
	for _, a := range f.attachments {
		if a.Profile != profile || a.Vault != vault {
			continue
		}
		if err := fn(a); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeBackfillClient) GetSummary(_ context.Context, profile, vault, sha string) (*osearch.SummaryDoc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if d, ok := f.summaries[osearch.DocID(profile, vault, sha)]; ok {
		c := d
		return &c, nil
	}
	return nil, nil
}

func (f *fakeBackfillClient) UpsertSummary(_ context.Context, doc osearch.SummaryDoc, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.summaries[osearch.DocID(doc.Profile, doc.Vault, doc.SHA)] = doc
	return nil
}

func seedAttachments(n int) []osearch.AttachmentDoc {
	out := make([]osearch.AttachmentDoc, n)
	for i := 0; i < n; i++ {
		out[i] = osearch.AttachmentDoc{
			Profile:          "p",
			Vault:            "v",
			SHA:              fmt.Sprintf("sha%03d", i),
			OriginalFilename: fmt.Sprintf("file-%d.pdf", i),
			Title:            fmt.Sprintf("doc %d", i),
			MIMEType:         "application/pdf",
			ExtractedText:    fmt.Sprintf("description body %d", i),
			Tags:             []string{"vendor:acme"},
			Source:           []string{"ingest-bulk:Raw/attachments/x.pdf"},
		}
	}
	return out
}

func TestBackfill_CreatesStubsForUnmatchedAttachments(t *testing.T) {
	atts := seedAttachments(5)
	client := newFakeBackfillClient(atts)

	res, err := runBackfillAttachmentStubs(context.Background(), client, backfillOpts{
		Profile: "p", Vault: "v", BatchSize: 10, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.Walked != 5 || res.Created != 5 || res.Skipped != 0 || res.Errors != 0 {
		t.Fatalf("counts: %+v", res)
	}
	if len(client.summaries) != 5 {
		t.Fatalf("want 5 stubs, got %d", len(client.summaries))
	}

	stub := client.summaries[osearch.DocID("p", "v", "sha000")]
	if stub.Kind != osearch.KindAttachmentStub {
		t.Errorf("kind = %q, want attachment_stub", stub.Kind)
	}
	if stub.Title != "doc 0" {
		t.Errorf("title = %q", stub.Title)
	}
	if stub.RawBody != "description body 0" {
		t.Errorf("raw_body = %q (expected ExtractedText-seeded)", stub.RawBody)
	}
	if stub.Reliability != osearch.ReliabilityMedium {
		t.Errorf("reliability = %q", stub.Reliability)
	}
	if stub.GateReason == "" {
		t.Errorf("gate_reason empty")
	}
	if len(stub.Attachments) != 1 || stub.Attachments[0] != "sha000" {
		t.Errorf("attachments back-link = %v", stub.Attachments)
	}
	if stub.SourcePath != "attachment://sha000" {
		t.Errorf("source_path = %q", stub.SourcePath)
	}
	want := map[string]bool{"vendor:acme": false, "attachment": false, "mime:application/pdf": false}
	for _, tag := range stub.Tags {
		if _, ok := want[tag]; ok {
			want[tag] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing tag %q in %v", k, stub.Tags)
		}
	}
}

func TestBackfill_Idempotent(t *testing.T) {
	atts := seedAttachments(3)
	client := newFakeBackfillClient(atts)

	if _, err := runBackfillAttachmentStubs(context.Background(), client, backfillOpts{
		Profile: "p", Vault: "v", BatchSize: 10, Concurrency: 2,
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	res, err := runBackfillAttachmentStubs(context.Background(), client, backfillOpts{
		Profile: "p", Vault: "v", BatchSize: 10, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Created != 0 || res.Skipped != 3 || res.Walked != 3 {
		t.Fatalf("re-run not idempotent: %+v", res)
	}
}

func TestBackfill_DryRunWritesNothing(t *testing.T) {
	atts := seedAttachments(4)
	client := newFakeBackfillClient(atts)

	res, err := runBackfillAttachmentStubs(context.Background(), client, backfillOpts{
		Profile: "p", Vault: "v", BatchSize: 10, Concurrency: 2, DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if res.Created != 4 || res.Walked != 4 {
		t.Fatalf("counts: %+v", res)
	}
	if len(client.summaries) != 0 {
		t.Fatalf("dry-run wrote %d stubs", len(client.summaries))
	}
}

func TestBackfill_FallsBackToTitleWhenExtractedTextEmpty(t *testing.T) {
	atts := []osearch.AttachmentDoc{{
		Profile: "p", Vault: "v", SHA: "abc",
		OriginalFilename: "nameless.bin",
		Title:            "human title",
	}}
	client := newFakeBackfillClient(atts)
	if _, err := runBackfillAttachmentStubs(context.Background(), client, backfillOpts{
		Profile: "p", Vault: "v", BatchSize: 10, Concurrency: 1,
	}); err != nil {
		t.Fatal(err)
	}
	stub := client.summaries[osearch.DocID("p", "v", "abc")]
	if stub.RawBody != "human title" {
		t.Errorf("raw_body = %q; want title fallback", stub.RawBody)
	}
}

func TestBackfill_FallsBackToFilenameWhenTitleEmpty(t *testing.T) {
	atts := []osearch.AttachmentDoc{{
		Profile: "p", Vault: "v", SHA: "abc",
		OriginalFilename: "only-filename.bin",
	}}
	client := newFakeBackfillClient(atts)
	if _, err := runBackfillAttachmentStubs(context.Background(), client, backfillOpts{
		Profile: "p", Vault: "v", BatchSize: 10, Concurrency: 1,
	}); err != nil {
		t.Fatal(err)
	}
	stub := client.summaries[osearch.DocID("p", "v", "abc")]
	if stub.Title != "only-filename.bin" || stub.RawBody != "only-filename.bin" {
		t.Errorf("title/body = %q / %q", stub.Title, stub.RawBody)
	}
}

func TestBackfill_UpsertErrorCounted(t *testing.T) {
	atts := seedAttachments(2)
	client := newFakeBackfillClient(atts)
	client.upsertErr = errors.New("os down")

	res, err := runBackfillAttachmentStubs(context.Background(), client, backfillOpts{
		Profile: "p", Vault: "v", BatchSize: 10, Concurrency: 1,
	})
	if err == nil {
		t.Fatal("expected error surfaced")
	}
	if res.Errors == 0 {
		t.Fatalf("errors not counted: %+v", res)
	}
	if len(client.summaries) != 0 {
		t.Fatalf("wrote despite upsert err: %d", len(client.summaries))
	}
}
