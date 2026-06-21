package osearch

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Logical index names. Resolve to physical names via Client.IndexName
// so the IndexPrefix can sandbox tests.
const (
	IndexSummaries   = "pb_summaries"
	IndexEntities    = "pb_entities"
	IndexAttachments = "pb_attachments"
)

// EmbeddingDim is the vector dimension for the daemon's embedding
// model. Phase 6 standardises on nomic-embed-text via Ollama, which
// emits 768-dim vectors. Agent and daemon MUST agree; the schema's
// knn_vector mapping pins this value.
const EmbeddingDim = 768

// Reliability mirrors the gate verdict written by internal/server/gate.go.
// Stored as a keyword field for term filters in recall.
type Reliability string

const (
	ReliabilityHigh      Reliability = "high"
	ReliabilityMedium    Reliability = "medium"
	ReliabilityLow       Reliability = "low"
	ReliabilityContested Reliability = "contested"
)

// SummaryDoc is one Wiki summary (one synthesised raw source). One
// doc per content SHA — re-perceiving the same bytes upserts in place.
type SummaryDoc struct {
	// Identity (denormalised onto the doc for filtering + recovery)
	Profile string `json:"profile"`
	Vault   string `json:"vault"`
	SHA     string `json:"sha"`

	// Provenance
	SourcePath string    `json:"source_path,omitempty"` // Raw/curated/foo.md or Raw/gathered/...
	SourceURL  string    `json:"source_url,omitempty"`  // for gathered sources
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	// Content
	Title   string   `json:"title"`
	Body    string   `json:"body"`              // distilled summary prose (post-synth)
	RawBody string   `json:"raw_body,omitempty"` // original text (pre-synth fallback)
	Tags    []string `json:"tags,omitempty"`
	Topic   string   `json:"topic,omitempty"`   // agents|memory|governance|tools|...

	// Gate verdict
	Reliability Reliability `json:"reliability,omitempty"`
	GateReason  string      `json:"gate_reason,omitempty"`

	// Cross-references (denormalised; entity docs hold the inverse)
	Entities    []string `json:"entities,omitempty"`     // names of extracted entities
	Attachments []string `json:"attachments,omitempty"` // SHAs of related attachments

	// Synth status — agents may see raw-only docs that the daemon's
	// synth queue hasn't processed yet. Empty/false = not yet synthed.
	Synthesised bool `json:"synthesised"`

	// Embedding: 768-dim vector from the agent's local Ollama. Daemon
	// is a pass-through; it doesn't recompute. Omit on synth-only updates.
	Embedding []float32 `json:"embedding,omitempty"`
}

// EntityDoc is one extracted entity, appended across sources. The
// MentionedBy[] array grows over time as new summaries reference it.
type EntityDoc struct {
	Profile string `json:"profile"`
	Vault   string `json:"vault"`
	Slug    string `json:"slug"` // canonicalised entity name; doc ID base

	Name       string   `json:"name"`
	Aliases    []string `json:"aliases,omitempty"`
	Body       string   `json:"body,omitempty"` // accumulated description across mentions
	Tags       []string `json:"tags,omitempty"`
	Topic      string   `json:"topic,omitempty"`

	// MentionedBy is the list of summary SHAs that reference this
	// entity. Updated atomically via OS painless script on each new
	// summary write.
	MentionedBy []string `json:"mentioned_by,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Embedding []float32 `json:"embedding,omitempty"`
}

// AttachmentDoc is one binary blob's metadata + extracted text. The
// original bytes live in MinIO at MinIOKey; this doc carries
// everything searchable about the attachment.
type AttachmentDoc struct {
	Profile string `json:"profile"`
	Vault   string `json:"vault"`
	SHA     string `json:"sha"` // SHA256 of the binary; also the doc ID base

	OriginalFilename string    `json:"original_filename"`
	Title            string    `json:"title,omitempty"`
	MIMEType         string    `json:"mime_type,omitempty"`
	SizeBytes        int64     `json:"size_bytes"`
	CreatedAt        time.Time `json:"created_at"`

	// MinIOKey is the object key under the daemon's bucket. Format:
	//   <profile>/<vault>/attachments/<sha256><ext>
	MinIOKey string `json:"minio_key"`

	// ExtractedText is whatever pdftotext / textutil / tesseract /
	// plaintext-read produced from the binary. Indexed for FTS so
	// brain_recall can hit attachments by content.
	ExtractedText string `json:"extracted_text,omitempty"`

	// SummarySHA links to the SummaryDoc that wraps this attachment's
	// extracted text (empty when extraction failed or yielded nothing).
	SummarySHA string `json:"summary_sha,omitempty"`

	Embedding []float32 `json:"embedding,omitempty"`
}

// DocID composes the canonical OS document ID from (profile, vault,
// key). Used for all three indices — within a given index, the key
// part is a SHA256 (summaries/attachments) or an entity slug.
//
//	<profile>/<vault>/<key>
//
// Doc IDs that look like paths are safe in OS; the only constraint
// is they're <= 512 bytes and don't contain '\n' or '\r'.
func DocID(profile, vault, key string) string {
	return profile + "/" + vault + "/" + key
}

// EntitySlug canonicalises an entity name into a stable slug suitable
// for use as the key half of a doc ID. Lowercased; whitespace and
// slashes collapsed to hyphens; trimmed.
func EntitySlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == '_':
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			// Whitespace, punctuation, slashes — all collapse to '-'.
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// SHA256Hex computes the SHA-256 of a byte slice and returns the
// lowercase hex digest. Used for content-addressed doc IDs.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
