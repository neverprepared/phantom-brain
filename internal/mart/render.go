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
		Title:       rec.Title,
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
	out.WriteString(strings.TrimRight(rec.Body, "\n"))
	out.WriteString("\n")
	return out.Bytes(), nil
}

var (
	slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
	slugTrim     = regexp.MustCompile(`^-+|-+$`)
)

// Filename returns a stable, collision-free filename for a record:
// slug(title)-<sha[:12]>.md. The sha suffix guarantees uniqueness even for
// duplicate or empty titles, and makes re-render idempotent (same record →
// same filename).
func Filename(rec brain.RecordDTO) string {
	slug := slugTrim.ReplaceAllString(slugNonAlnum.ReplaceAllString(strings.ToLower(rec.Title), "-"), "")
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
