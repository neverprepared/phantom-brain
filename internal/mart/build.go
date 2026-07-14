package mart

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

// MarkerFile is written at the root of every mart directory. Its presence is
// how Build proves it owns a directory before wiping or overwriting it — the
// guard that stops a mart from clobbering hand-authored notes.
const MarkerFile = ".pbrain-mart"

const (
	indexFile      = "index.md"
	attachmentsDir = "attachments"
	// attachmentKind is the kind string the Postgres SoR stores for
	// attachment records — and thus what GET /api/brain/records returns.
	// It is "attachment", NOT the legacy osearch enum "attachment_stub":
	// osearch.SoRKind() collapses KindAttachmentStub to "attachment" on the
	// write path (see internal/osearch/docs.go). Matching "attachment_stub"
	// here (as the first cut did) silently disables blob materialization.
	attachmentKind = "attachment"
	maxAttachmentB = 100 << 20 // 100 MiB — matches the daemon attach ceiling
)

// RecordSource yields one keyset page of records at a time. Build is written
// against this interface so the future mart-daemon (an updated_at change-feed
// source) reuses Build and the renderer verbatim — only the source differs.
type RecordSource interface {
	Page(ctx context.Context, afterID int64) (recs []brain.RecordDTO, next int64, err error)
}

// AttachmentFetcher is an OPTIONAL capability a RecordSource may also
// implement: it returns the raw blob bytes for an attachment record so Build
// can materialize a self-contained mart (embed the PDF/image in Obsidian).
// ok=false means "this record has no blob" (not an attachment / already gone);
// only a genuine transport failure returns a non-nil error.
type AttachmentFetcher interface {
	FetchAttachment(ctx context.Context, rec brain.RecordDTO) (data []byte, filename string, ok bool, err error)
}

// ClientSource is the MVP RecordSource: it pages the daemon's
// GET /api/brain/records over the public HTTP client. It ALSO implements
// AttachmentFetcher, pulling blobs via the presigned-URL attach endpoint.
type ClientSource struct {
	Client   *brain.Client
	Filters  Filters
	PageSize int
}

// Page implements RecordSource.
func (s ClientSource) Page(ctx context.Context, afterID int64) ([]brain.RecordDTO, int64, error) {
	limit := s.PageSize
	if limit <= 0 {
		limit = 100
	}
	resp, err := s.Client.ListRecords(ctx, brain.ListRecordsRequest{
		AfterID:     afterID,
		Limit:       limit,
		Kinds:       s.Filters.Kinds,
		Tags:        s.Filters.Tags,
		Sources:     s.Filters.Sources,
		Topic:       s.Filters.Topic,
		Reliability: s.Filters.Reliability,
		Synthesised: s.Filters.Synthesised,
	})
	if err != nil {
		return nil, 0, err
	}
	return resp.Records, resp.NextAfterID, nil
}

// FetchAttachment implements AttachmentFetcher: resolve the record's presigned
// MinIO URL (GET /api/brain/attach/{sha}) and download the blob. A 404 means
// the record carries no attachment (ok=false, no error).
func (s ClientSource) FetchAttachment(ctx context.Context, rec brain.RecordDTO) ([]byte, string, bool, error) {
	if rec.Kind != attachmentKind {
		return nil, "", false, nil
	}
	ag, err := s.Client.AttachGet(ctx, rec.SHA)
	if err != nil {
		var apiErr *brain.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ag.URL, nil)
	if err != nil {
		return nil, "", false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", false, fmt.Errorf("presigned GET returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentB+1))
	if err != nil {
		return nil, "", false, err
	}
	if len(data) > maxAttachmentB {
		return nil, "", false, fmt.Errorf("attachment exceeds %d-byte cap", maxAttachmentB)
	}
	return data, ag.Original, true, nil
}

// Result summarises a build.
type Result struct {
	RecordsWritten     int
	AttachmentsWritten int
	AttachmentsSkipped int // attachments a consumer wanted but could not fetch
	DestPath           string
}

// Build renders the mart described by spec into spec.Dest, paging through src.
//
// Ownership safety (critical): Build refuses to wipe or write into a non-empty
// directory that lacks the MarkerFile — a mart only ever touches directories
// it created. An ephemeral mart clean-rebuilds (wipe → recreate → marker); a
// non-ephemeral mart overwrites in place by the record's deterministic
// filename. index.md is written last as the Obsidian entry point.
//
// Attachments: unless spec.SkipAttachments is set and src implements
// AttachmentFetcher, each attachment record's blob is downloaded into
// <dest>/attachments/ and embedded in its note (![[...]]), making the mart
// self-contained. Blob fetch is best-effort — a failure is counted, not fatal.
func Build(ctx context.Context, spec Spec, src RecordSource) (Result, error) {
	if err := spec.Validate(); err != nil {
		return Result{}, err
	}
	if err := prepareDest(spec); err != nil {
		return Result{}, err
	}

	var fetcher AttachmentFetcher
	if !spec.SkipAttachments {
		if af, ok := src.(AttachmentFetcher); ok {
			fetcher = af
		}
	}

	type indexRow struct {
		base, title, kind, topic string
	}
	var rows []indexRow
	res := Result{DestPath: spec.Dest}

	afterID := int64(0)
	for {
		recs, next, err := src.Page(ctx, afterID)
		if err != nil {
			return Result{}, fmt.Errorf("list records: %w", err)
		}
		for _, rec := range recs {
			data, err := Render(rec)
			if err != nil {
				return Result{}, fmt.Errorf("render %s: %w", rec.SHA, err)
			}

			// Best-effort: materialize the blob and embed it in the note.
			if fetcher != nil && rec.Kind == attachmentKind {
				embed, ok, aerr := materializeAttachment(ctx, spec, fetcher, rec)
				switch {
				case aerr != nil:
					res.AttachmentsSkipped++
					data = append(data, []byte("\n> [!warning] attachment could not be materialized: "+aerr.Error()+"\n")...)
				case ok:
					res.AttachmentsWritten++
					data = append(data, []byte("\n## File\n\n"+embed+"\n")...)
				}
			}

			name := Filename(rec)
			if err := os.WriteFile(filepath.Join(spec.Dest, name), data, 0o644); err != nil {
				return Result{}, fmt.Errorf("write %s: %w", name, err)
			}
			rows = append(rows, indexRow{
				base:  strings.TrimSuffix(name, ".md"),
				title: rec.Title,
				kind:  rec.Kind,
				topic: rec.Topic,
			})
			res.RecordsWritten++
		}
		if next == 0 || next <= afterID || len(recs) == 0 {
			break
		}
		afterID = next
	}

	// index.md — a table of wikilinks so Obsidian has an entry point.
	var idx strings.Builder
	fmt.Fprintf(&idx, "# %s\n\n", spec.Name)
	fmt.Fprintf(&idx, "Projected from phantom-brain (%s/%s). %d record(s), %d attachment(s). Generated by `pbrainctl mart build`; do not edit — this directory is rebuilt.\n\n",
		spec.Profile, spec.Vault, res.RecordsWritten, res.AttachmentsWritten)
	idx.WriteString("| Note | Kind | Topic |\n|------|------|-------|\n")
	for _, r := range rows {
		title := r.title
		if strings.TrimSpace(title) == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&idx, "| [[%s\\|%s]] | %s | %s |\n", r.base, title, r.kind, r.topic)
	}
	if err := os.WriteFile(filepath.Join(spec.Dest, indexFile), []byte(idx.String()), 0o644); err != nil {
		return Result{}, fmt.Errorf("write index.md: %w", err)
	}

	return res, nil
}

// materializeAttachment downloads rec's blob and writes it under
// <dest>/attachments/, returning the Obsidian embed string. ok=false means the
// record had no blob (skip silently).
func materializeAttachment(ctx context.Context, spec Spec, fetcher AttachmentFetcher, rec brain.RecordDTO) (embed string, ok bool, err error) {
	data, filename, ok, err := fetcher.FetchAttachment(ctx, rec)
	if err != nil || !ok {
		return "", false, err
	}
	ext := filepath.Ext(filename)
	if ext == "" {
		ext = extFromMIME(rec.MimeType)
	}
	base := strings.TrimSuffix(Filename(rec), ".md") // shares the note's slug+sha
	attName := base + ext
	dir := filepath.Join(spec.Dest, attachmentsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("create attachments dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, attName), data, 0o644); err != nil {
		return "", false, fmt.Errorf("write attachment: %w", err)
	}
	return fmt.Sprintf("![[%s/%s]]", attachmentsDir, attName), true, nil
}

// extFromMIME maps a MIME type to a file extension when the original filename
// lacked one. Falls back to ".bin".
func extFromMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "application/pdf":
		return ".pdf"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "text/plain":
		return ".txt"
	case "text/markdown":
		return ".md"
	case "text/html":
		return ".html"
	case "text/csv":
		return ".csv"
	case "application/json":
		return ".json"
	}
	return ".bin"
}

// ensureOwnedDir makes spec.Dest exist and proves the mart owns it (drops the
// .pbrain-mart marker), WITHOUT ever wiping. Fresh or empty dir → create +
// claim. Already-marked dir → ok. Unmarked non-empty dir → refuse (never touch
// a directory that might hold hand-authored notes). Shared by Build (full) and
// Sync (incremental).
func ensureOwnedDir(spec Spec) error {
	info, err := os.Stat(spec.Dest)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(spec.Dest, 0o755); err != nil {
			return fmt.Errorf("create mart dir: %w", err)
		}
		return writeMarker(spec)
	case err != nil:
		return fmt.Errorf("stat mart dest: %w", err)
	case !info.IsDir():
		return fmt.Errorf("mart dest %q exists and is not a directory", spec.Dest)
	}
	if fileExists(filepath.Join(spec.Dest, MarkerFile)) {
		return nil // already own it
	}
	empty, derr := dirEmpty(spec.Dest)
	if derr != nil {
		return derr
	}
	if !empty {
		return fmt.Errorf("refusing to use non-mart directory %q: it is non-empty and lacks the %s marker (point --dest at a dedicated subdirectory the mart can own)",
			spec.Dest, MarkerFile)
	}
	return writeMarker(spec) // empty dir — safe to adopt
}

// prepareDest readies spec.Dest for a full Build. For an ephemeral mart it
// clean-rebuilds: prove ownership (or refuse), then wipe + recreate + reclaim.
// Non-ephemeral overwrites in place.
func prepareDest(spec Spec) error {
	if err := ensureOwnedDir(spec); err != nil {
		return err
	}
	if spec.Ephemeral {
		// Safe — ensureOwnedDir proved we own it (marker present) or just
		// created/adopted an empty dir.
		if err := os.RemoveAll(spec.Dest); err != nil {
			return fmt.Errorf("clean mart dir: %w", err)
		}
		if err := os.MkdirAll(spec.Dest, 0o755); err != nil {
			return fmt.Errorf("recreate mart dir: %w", err)
		}
		return writeMarker(spec)
	}
	return nil
}

func writeMarker(spec Spec) error {
	content := fmt.Sprintf("phantom-brain mart: %s\nprofile: %s\nvault: %s\n", spec.Name, spec.Profile, spec.Vault)
	if err := os.WriteFile(filepath.Join(spec.Dest, MarkerFile), []byte(content), 0o644); err != nil {
		return fmt.Errorf("write mart marker: %w", err)
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func dirEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("read mart dest: %w", err)
	}
	return len(entries) == 0, nil
}
