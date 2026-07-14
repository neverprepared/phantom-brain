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
