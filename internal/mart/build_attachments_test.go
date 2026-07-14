package mart

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

// fakeFetchSource is a RecordSource that ALSO implements AttachmentFetcher, so
// Build's blob-materialization path can be tested without a daemon or MinIO.
type fakeFetchSource struct {
	*fakeSource
	blobs map[string][]byte // sha -> bytes (absent => ok=false, no blob)
	names map[string]string // sha -> original filename
	fail  map[string]bool   // sha -> return a transport error
}

func (f fakeFetchSource) FetchAttachment(_ context.Context, rec brain.RecordDTO) ([]byte, string, bool, error) {
	if f.fail[rec.SHA] {
		return nil, "", false, errors.New("boom")
	}
	b, ok := f.blobs[rec.SHA]
	if !ok {
		return nil, "", false, nil
	}
	return b, f.names[rec.SHA], true, nil
}

func attachmentRec(sha, title, filename, mime string) brain.RecordDTO {
	return brain.RecordDTO{
		SHA:              sha,
		Kind:             attachmentKind,
		Title:            title,
		Body:             "stub summary for " + title,
		OriginalFilename: filename,
		MimeType:         mime,
	}
}

func TestBuild_MaterializesAttachment(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "_mart")
	rec := attachmentRec("aa11000000000000", "Return 2025", "return.pdf", "application/pdf")
	src := fakeFetchSource{
		fakeSource: &fakeSource{recs: []brain.RecordDTO{rec}},
		blobs:      map[string][]byte{rec.SHA: []byte("%PDF-1.7 fake")},
		names:      map[string]string{rec.SHA: "return.pdf"},
	}
	spec := Spec{Name: "m", Profile: "p", Vault: "v", Dest: dest, Ephemeral: true}

	res, err := Build(context.Background(), spec, src)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.AttachmentsWritten != 1 || res.AttachmentsSkipped != 0 {
		t.Fatalf("attachments written=%d skipped=%d, want 1/0", res.AttachmentsWritten, res.AttachmentsSkipped)
	}
	blob := filepath.Join(dest, attachmentsDir, "return-2025-aa1100000000.pdf")
	if b, err := os.ReadFile(blob); err != nil || string(b) != "%PDF-1.7 fake" {
		t.Fatalf("blob not written correctly at %s: err=%v", blob, err)
	}
	note, err := os.ReadFile(filepath.Join(dest, Filename(rec)))
	if err != nil {
		t.Fatalf("note missing: %v", err)
	}
	if !strings.Contains(string(note), "![[attachments/return-2025-aa1100000000.pdf]]") {
		t.Errorf("note missing embed:\n%s", note)
	}
}

func TestBuild_SkipAttachments(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "_mart")
	rec := attachmentRec("bb22000000000000", "W2", "w2.pdf", "application/pdf")
	src := fakeFetchSource{
		fakeSource: &fakeSource{recs: []brain.RecordDTO{rec}},
		blobs:      map[string][]byte{rec.SHA: []byte("bytes")},
		names:      map[string]string{rec.SHA: "w2.pdf"},
	}
	spec := Spec{Name: "m", Profile: "p", Vault: "v", Dest: dest, Ephemeral: true, SkipAttachments: true}

	res, err := Build(context.Background(), spec, src)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.AttachmentsWritten != 0 {
		t.Errorf("AttachmentsWritten = %d, want 0 with SkipAttachments", res.AttachmentsWritten)
	}
	if _, err := os.Stat(filepath.Join(dest, attachmentsDir)); !os.IsNotExist(err) {
		t.Errorf("attachments dir should not exist when skipping")
	}
	// The stub note is still projected.
	if _, err := os.Stat(filepath.Join(dest, Filename(rec))); err != nil {
		t.Errorf("stub note should still be written: %v", err)
	}
}

func TestBuild_AttachmentFetchFailureIsBestEffort(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "_mart")
	rec := attachmentRec("cc33000000000000", "Broken", "x.pdf", "application/pdf")
	src := fakeFetchSource{
		fakeSource: &fakeSource{recs: []brain.RecordDTO{rec}},
		fail:       map[string]bool{rec.SHA: true},
	}
	spec := Spec{Name: "m", Profile: "p", Vault: "v", Dest: dest, Ephemeral: true}

	res, err := Build(context.Background(), spec, src)
	if err != nil {
		t.Fatalf("Build must not fail on a blob error: %v", err)
	}
	if res.AttachmentsWritten != 0 || res.AttachmentsSkipped != 1 {
		t.Fatalf("written=%d skipped=%d, want 0/1", res.AttachmentsWritten, res.AttachmentsSkipped)
	}
	note, _ := os.ReadFile(filepath.Join(dest, Filename(rec)))
	if !strings.Contains(string(note), "[!warning]") {
		t.Errorf("failed attachment should leave a warning callout:\n%s", note)
	}
}

func TestBuild_NonAttachmentRecordIgnoredByFetcher(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "_mart")
	// A plain note whose SHA happens to have a blob entry — must be ignored
	// because Kind != attachment.
	note := brain.RecordDTO{SHA: "dd44000000000000", Kind: "note", Title: "plain", Body: "b"}
	src := fakeFetchSource{
		fakeSource: &fakeSource{recs: []brain.RecordDTO{note}},
		blobs:      map[string][]byte{note.SHA: []byte("should-not-be-used")},
	}
	spec := Spec{Name: "m", Profile: "p", Vault: "v", Dest: dest, Ephemeral: true}

	res, err := Build(context.Background(), spec, src)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.AttachmentsWritten != 0 {
		t.Errorf("non-attachment record must not materialize a blob")
	}
	if _, err := os.Stat(filepath.Join(dest, attachmentsDir)); !os.IsNotExist(err) {
		t.Errorf("no attachments dir expected")
	}
}

func TestExtFromMIME(t *testing.T) {
	cases := map[string]string{
		"application/pdf":          ".pdf",
		"image/png":                ".png",
		"image/jpeg":               ".jpg",
		"text/csv":                 ".csv",
		"application/octet-stream": ".bin",
		"":                         ".bin",
	}
	for mime, want := range cases {
		if got := extFromMIME(mime); got != want {
			t.Errorf("extFromMIME(%q) = %q, want %q", mime, got, want)
		}
	}
}
