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

// Kind classifies the shape of memory a SummaryDoc represents. Distinct
// from Topic (subject matter) — Kind says how the doc came into being
// and is queryable when you want "all task summaries" or "everything
// scraped from the web". Closed enum, application-validated; adding a
// value is a one-line addition + daemon redeploy (no schema migration).
type Kind string

const (
	KindNote           Kind = "note"            // brain_learn: an operator-curated note
	KindWebScrape      Kind = "web_scrape"      // brain_perceive: web content
	KindTaskSummary    Kind = "task_summary"    // task_complete: promoted task findings
	KindAttachmentStub Kind = "attachment_stub" // sidecar summary for an attachment
	KindEmailImport    Kind = "email_import"    // bulk loader: legacy email-scrape doc
	KindManualCurate   Kind = "manual_curate"   // bulk loader: future/non-email formats
)

// IsValid reports whether k is one of the recognised Kind values.
// Daemon write handlers reject unknown kinds to catch typos at the
// boundary. To allow new kinds in a rolling-deploy scenario, relax
// this check to a warn-and-store at the call site.
func (k Kind) IsValid() bool {
	switch k {
	case KindNote, KindWebScrape, KindTaskSummary,
		KindAttachmentStub, KindEmailImport, KindManualCurate:
		return true
	}
	return false
}

// MemoryType is the Tulving taxonomy (semantic / episodic / procedural)
// already used in working memory findings. Optional on long-term docs
// — empty means "undecided / not applicable". When set it lets recall
// say "show me procedural memories about <X>".
type MemoryType string

const (
	MemorySemantic   MemoryType = "semantic"   // facts / concepts
	MemoryEpisodic   MemoryType = "episodic"   // events / what happened
	MemoryProcedural MemoryType = "procedural" // how-to / steps
)

// IsValid reports whether m is one of the recognised MemoryType values
// OR the empty string (which signals "undecided"). Empty is allowed
// because the caller may not know which bucket a given doc belongs to.
func (m MemoryType) IsValid() bool {
	switch m {
	case "", MemorySemantic, MemoryEpisodic, MemoryProcedural:
		return true
	}
	return false
}

// SummaryDoc is one Wiki summary (one synthesised raw source). One
// doc per content SHA — re-perceiving the same bytes upserts in place.
type SummaryDoc struct {
	// Identity (denormalised onto the doc for filtering + recovery)
	Profile string `json:"profile"`
	Vault   string `json:"vault"`
	SHA     string `json:"sha"`

	// Memory classification (v2.4: enables faceted queries beyond
	// just topic/reliability — "give me all task summaries from last
	// week" or "all procedural memories about Kubernetes").
	Kind       Kind       `json:"kind,omitempty"`        // shape of memory: note / web_scrape / task_summary / ...
	MemoryType MemoryType `json:"memory_type,omitempty"` // Tulving: semantic / episodic / procedural

	// Provenance
	SourcePath string    `json:"source_path,omitempty"` // Raw/curated/foo.md or Raw/gathered/...
	SourceURL  string    `json:"source_url,omitempty"`  // for gathered sources
	Source     []string  `json:"source,omitempty"`      // multi-valued provenance: URLs, task IDs, agent IDs, file paths
	CreatedAt  time.Time `json:"created_at"`            // when OS first received this doc
	UpdatedAt  time.Time `json:"updated_at"`            // when OS last touched it
	CapturedAt time.Time `json:"captured_at,omitempty"` // when the underlying content was authored / captured

	// Content
	Title   string   `json:"title"`
	Body    string   `json:"body"`              // distilled summary prose (post-synth)
	RawBody string   `json:"raw_body,omitempty"` // original text (pre-synth fallback)
	Tags    []string `json:"tags,omitempty"`     // free-form labels (legacy vendor/category/type fold in here)
	Topic   string   `json:"topic,omitempty"`    // agents|memory|governance|tools|...

	// Gate verdict
	Reliability Reliability `json:"reliability,omitempty"`
	GateReason  string      `json:"gate_reason,omitempty"`

	// Cross-references (denormalised; entity docs hold the inverse)
	Entities    []string `json:"entities,omitempty"`    // names of extracted entities
	Attachments []string `json:"attachments,omitempty"` // SHAs of related attachments
	References  []string `json:"references,omitempty"`  // SHAs of related SummaryDocs (graph hook)

	// Raw-source capture: MinIO key of the captured page bytes, if
	// the daemon was configured with [capture].enabled and the fetch
	// succeeded. Empty when capture is off, the URL was unreachable,
	// or the doc wasn't from a URL source (brain_learn, task_summary).
	CaptureMinIOKey  string `json:"capture_minio_key,omitempty"`
	CaptureSizeBytes int64  `json:"capture_size_bytes,omitempty"`

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
	CapturedAt       time.Time `json:"captured_at,omitempty"` // when the file was authored (PDF /Title metadata, file mtime, etc.)

	// MinIOKey is the object key under the daemon's bucket. Format:
	//   <profile>/<vault>/attachments/<sha256><ext>
	MinIOKey string `json:"minio_key"`

	// ExtractedText is whatever pdftotext / textutil / tesseract /
	// plaintext-read produced from the binary. Indexed for FTS so
	// brain_recall can hit attachments by content.
	ExtractedText string `json:"extracted_text,omitempty"`

	// Memory classification (v2.4). Kind is typically KindAttachmentStub
	// but agents are free to pass a different value (e.g. KindEmailImport
	// for the migration). MemoryType defaults to MemorySemantic.
	Kind       Kind       `json:"kind,omitempty"`
	MemoryType MemoryType `json:"memory_type,omitempty"`
	Source     []string   `json:"source,omitempty"`     // provenance — URL, original filename, email-thread ID
	References []string   `json:"references,omitempty"` // SHAs of related summaries

	// SummarySHA links to the SummaryDoc that wraps this attachment's
	// extracted text (empty when extraction failed or yielded nothing).
	SummarySHA string `json:"summary_sha,omitempty"`

	// Tags mirror SummaryDoc.Tags — free-form labels for faceting.
	Tags []string `json:"tags,omitempty"`

	Embedding []float32 `json:"embedding,omitempty"`
}

// DocID composes the canonical OS document ID from (profile, vault,
// key). Used for all three indices — within a given index, the key
// part is a SHA256 (summaries/attachments) or an entity slug.
//
//	<profile>:<vault>:<key>
//
// Slashes are deliberately avoided: opensearch-go interpolates the
// doc ID into URL paths without encoding, so a slash would silently
// produce a 404 ("no handler found for uri"). Colons are URL-safe
// within /_doc/{id}.
func DocID(profile, vault, key string) string {
	return profile + ":" + vault + ":" + key
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
