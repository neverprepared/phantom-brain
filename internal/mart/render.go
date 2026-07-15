package mart

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

// frontmatter is the YAML block emitted at the top of every mart file. Field
// order here is the emitted key order (yaml.v3 honours struct order). The
// shape mirrors the SoR record + the Obsidian/OKF mapping: type (from kind),
// tags, source, captured_at, reliability, topic, sha. It is canonicaliser-
// compatible (a `---` block at byte 0) so mart output re-ingests cleanly.
type frontmatter struct {
	Type        string     `yaml:"type"`
	Title       string     `yaml:"title"`
	MemoryType  string     `yaml:"memory_type,omitempty"`
	Tags        []string   `yaml:"tags,omitempty"`
	Topic       string     `yaml:"topic,omitempty"`
	Reliability string     `yaml:"reliability,omitempty"`
	Source      []string   `yaml:"source,omitempty"`
	SourceURL   string     `yaml:"source_url,omitempty"`
	CapturedAt  *time.Time `yaml:"captured_at,omitempty"`
	UpdatedAt   time.Time  `yaml:"updated_at"`
	SHA         string     `yaml:"sha"`
}

// Render turns one record into a full markdown document: a YAML frontmatter
// block followed by the body. Uses yaml.v3 for correct quoting/escaping; the
// `---` delimiters are written manually.
func Render(rec brain.RecordDTO) ([]byte, error) {
	fm := frontmatter{
		Type:        rec.Kind, // kind is already a clean lowercase enum
		Title:       displayTitle(rec),
		MemoryType:  rec.MemoryType,
		Tags:        rec.Tags,
		Topic:       rec.Topic,
		Reliability: rec.Reliability,
		Source:      rec.Source,
		SourceURL:   rec.SourceURL,
		CapturedAt:  rec.CapturedAt,
		UpdatedAt:   rec.UpdatedAt,
		SHA:         rec.SHA,
	}
	var y bytes.Buffer
	enc := yaml.NewEncoder(&y)
	enc.SetIndent(2)
	if err := enc.Encode(fm); err != nil {
		return nil, fmt.Errorf("encode frontmatter: %w", err)
	}
	_ = enc.Close()

	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(y.Bytes())
	out.WriteString("---\n\n")
	out.WriteString(strings.TrimRight(displayBody(rec), "\n"))
	out.WriteString("\n")
	return out.Bytes(), nil
}

// displayTitle picks a human-friendly title. Attachment records carry their
// content SHA as the title; prefer the original filename (kept with its
// extension — it signals the file type) when the title is empty or is just the
// SHA. Normal notes keep their own title.
func displayTitle(rec brain.RecordDTO) string {
	t := strings.TrimSpace(rec.Title)
	if (t == "" || t == rec.SHA) && strings.TrimSpace(rec.OriginalFilename) != "" {
		return rec.OriginalFilename
	}
	if t == "" {
		return "untitled"
	}
	return t
}

// apologyPhrases are signatures of the "there's nothing to summarize" boilerplate
// synth emits for an attachment with no extracted text (root fix is #86 OCR).
// Deliberately narrow so displayBody can't suppress a real document summary.
var apologyPhrases = []string{
	"no text to summarize",
	"content provided is empty",
	"nothing in the content",
	"please paste the document",
	"provide the document",
	"document text and i will",
}

// looksLikeApology reports whether body is the empty-attachment apology.
func looksLikeApology(body string) bool {
	b := strings.ToLower(body)
	for _, p := range apologyPhrases {
		if strings.Contains(b, p) {
			return true
		}
	}
	return false
}

// displayBody suppresses the empty/apology body for ATTACHMENT records only —
// the embedded file is the real content, so a hash-title apology just adds
// noise. Attachments that do have extracted text, and all non-attachment
// records, are returned unchanged.
func displayBody(rec brain.RecordDTO) string {
	if rec.Kind == attachmentKind {
		if b := strings.TrimSpace(rec.Body); b == "" || looksLikeApology(b) {
			return "_(attachment — no extractable text)_"
		}
	}
	return rec.Body
}

var (
	slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
	slugTrim     = regexp.MustCompile(`^-+|-+$`)
)

// Filename returns a stable, collision-free filename for a record:
// slug(displayTitle)-<sha[:12]>.md. Using the friendly title means attachments
// get a readable name (return-2025-<sha>.md) instead of a hash-of-a-hash. The
// sha suffix guarantees uniqueness even for duplicate/empty titles and makes
// re-render idempotent (same record → same filename).
func Filename(rec brain.RecordDTO) string {
	slug := slugTrim.ReplaceAllString(slugNonAlnum.ReplaceAllString(strings.ToLower(displayTitle(rec)), "-"), "")
	if slug == "" {
		slug = "untitled"
	}
	if len(slug) > 60 {
		slug = strings.TrimRight(slug[:60], "-")
	}
	sha := rec.SHA
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return slug + "-" + sha + ".md"
}
