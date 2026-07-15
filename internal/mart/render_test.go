package mart

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

func sampleRecord() brain.RecordDTO {
	captured := time.Date(2026, 5, 28, 14, 30, 0, 0, time.UTC)
	return brain.RecordDTO{
		SHA:         "abcdef0123456789abcdef",
		Kind:        "note",
		MemoryType:  "semantic",
		Title:       "Weekly Active Users",
		Body:        "One row per completed order.\n",
		SourceURL:   "https://example.com/x",
		Source:      []string{"task:42"},
		Tags:        []string{"sales", "revenue"},
		Topic:       "memory",
		Reliability: "high",
		CapturedAt:  &captured,
		UpdatedAt:   time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
	}
}

func TestRender_FrontmatterRoundTrips(t *testing.T) {
	rec := sampleRecord()
	out, err := Render(rec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("---\n")) {
		t.Fatalf("output must start with frontmatter delimiter, got:\n%s", out)
	}
	// Split the two `---` fences and parse the frontmatter back.
	s := string(out)
	rest := strings.TrimPrefix(s, "---\n")
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("no closing frontmatter fence in:\n%s", s)
	}
	var fm map[string]any
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		t.Fatalf("frontmatter did not parse as yaml: %v", err)
	}
	if fm["type"] != "note" {
		t.Errorf("type = %v, want note", fm["type"])
	}
	if fm["sha"] != rec.SHA {
		t.Errorf("sha = %v, want %s", fm["sha"], rec.SHA)
	}
	if fm["title"] != rec.Title {
		t.Errorf("title = %v, want %s", fm["title"], rec.Title)
	}
	if fm["reliability"] != "high" {
		t.Errorf("reliability = %v, want high", fm["reliability"])
	}
	// Body follows the closing fence.
	body := rest[end+len("\n---\n"):]
	if !strings.Contains(body, "One row per completed order.") {
		t.Errorf("body missing from output:\n%s", body)
	}
}

func TestRender_OmitsEmptyOptionalFields(t *testing.T) {
	rec := brain.RecordDTO{SHA: "deadbeef0000", Kind: "web_scrape", Title: "x", Body: "b"}
	out, err := Render(rec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, k := range []string{"memory_type", "reliability", "topic", "source_url", "captured_at"} {
		if strings.Contains(string(out), k+":") {
			t.Errorf("empty optional field %q should be omitted, got:\n%s", k, out)
		}
	}
}

func TestFilename_DeterministicAndShaSuffixed(t *testing.T) {
	rec := sampleRecord()
	got := Filename(rec)
	want := "weekly-active-users-abcdef012345.md"
	if got != want {
		t.Errorf("Filename = %q, want %q", got, want)
	}
	if Filename(rec) != got {
		t.Error("Filename is not deterministic")
	}
}

func TestFilename_EmptyTitleFallsBack(t *testing.T) {
	rec := brain.RecordDTO{SHA: "0123456789abcdef", Title: "   "}
	got := Filename(rec)
	if got != "untitled-0123456789ab.md" {
		t.Errorf("Filename = %q, want untitled-0123456789ab.md", got)
	}
}

func TestDisplayTitle(t *testing.T) {
	sha := "abcdef0123456789abcdef"
	// Attachment whose title IS the sha → use the original filename.
	att := brain.RecordDTO{Kind: "attachment", SHA: sha, Title: sha, OriginalFilename: "Return 2025.pdf"}
	if got := displayTitle(att); got != "Return 2025.pdf" {
		t.Errorf("sha-title attachment: %q, want Return 2025.pdf", got)
	}
	// Empty title, no filename → untitled.
	if got := displayTitle(brain.RecordDTO{SHA: sha, Title: "  "}); got != "untitled" {
		t.Errorf("empty title: %q, want untitled", got)
	}
	// A real title is kept (never overridden).
	note := brain.RecordDTO{Kind: "note", SHA: sha, Title: "Weekly Active Users", OriginalFilename: "x.pdf"}
	if got := displayTitle(note); got != "Weekly Active Users" {
		t.Errorf("real title should be kept: %q", got)
	}
}

func TestDisplayBody_SuppressesAttachmentApology(t *testing.T) {
	apology := "The document content provided is empty — there is no text to summarize. Please paste the document text."
	att := brain.RecordDTO{Kind: "attachment", Body: apology}
	if got := displayBody(att); got != "_(attachment — no extractable text)_" {
		t.Errorf("attachment apology should be suppressed, got: %q", got)
	}
	// Empty attachment body → placeholder.
	if got := displayBody(brain.RecordDTO{Kind: "attachment", Body: "  "}); got != "_(attachment — no extractable text)_" {
		t.Errorf("empty attachment body should be placeholder, got: %q", got)
	}
	// Attachment WITH real extracted text → kept.
	real := brain.RecordDTO{Kind: "attachment", Body: "Invoice #4021 for $1,240 due 2026-07-01."}
	if got := displayBody(real); got != real.Body {
		t.Errorf("real attachment body should be kept, got: %q", got)
	}
	// A NOTE that happens to contain apology-like text is NOT touched (only
	// attachments are suppressed).
	noteApology := brain.RecordDTO{Kind: "note", Body: "Reminder: please paste the document into the shared drive."}
	if got := displayBody(noteApology); got != noteApology.Body {
		t.Errorf("non-attachment body must never be suppressed, got: %q", got)
	}
}

func TestLooksLikeApology(t *testing.T) {
	if !looksLikeApology("There is no text to summarize.") {
		t.Error("should flag the apology")
	}
	if looksLikeApology("Weekly active users are computed from the event stream.") {
		t.Error("a normal summary must not be flagged")
	}
}

func TestRender_AttachmentGetsFriendlyTitleAndCleanBody(t *testing.T) {
	sha := "0132562e5f842266f205688342ebb012e8c5822bf441499b7c260c54f1cb18f4"
	att := brain.RecordDTO{
		Kind: "attachment", SHA: sha, Title: sha,
		OriginalFilename: "Berrydale-survey.pdf",
		Body:             "The document content provided is empty — nothing in the content field.",
		UpdatedAt:        time.Now().UTC(),
	}
	out, err := Render(att)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "title: Berrydale-survey.pdf") {
		t.Errorf("friendly title missing:\n%s", s)
	}
	if strings.Contains(s, "nothing in the content field") {
		t.Errorf("apology body should be suppressed:\n%s", s)
	}
	if !strings.Contains(s, "no extractable text") {
		t.Errorf("placeholder missing:\n%s", s)
	}
	// Filename uses the friendly slug, not the hash.
	if fn := Filename(att); fn != "berrydale-survey-pdf-0132562e5f84.md" {
		t.Errorf("Filename = %q, want berrydale-survey-pdf-<sha12>.md", fn)
	}
}
