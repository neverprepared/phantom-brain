package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// resolveOS returns the per-binding osWriter view (v3.2 per-binding
// storage overrides). On binding-cache miss returns an error rather
// than silently falling back to the shared d.osClient — a cache miss
// means buildBindingDeps never ran for this binding (configuration
// error / SIGHUP race) and serving the shared infrastructure for a
// binding that has its own [storage_overrides] would write to the
// wrong tenant. Fail loud, log, return 500.
//
// The legacy test path that wires a Daemon by hand without
// buildBindingDeps must call useSharedFallback to opt in explicitly.
func (d *Daemon) resolveOS(b VaultBinding) (osWriter, error) {
	if d.bindings != nil {
		if deps, ok := d.bindings.Get(b.Key); ok && deps != nil && deps.OS != nil {
			return deps.OS, nil
		}
	}
	if d.allowSharedFallback {
		return d.osClient, nil
	}
	return nil, fmt.Errorf("server: no binding view registered for %s — silent fallback to shared infra refused (would leak across tenants)", b.Key)
}

// resolveAttach returns the per-binding AttachmentStore (v3.2). Same
// fail-loud semantics as resolveOS — a cache miss is a configuration
// bug, not a thing to paper over with shared infrastructure.
func (d *Daemon) resolveAttach(b VaultBinding) (AttachmentStore, error) {
	if d.bindings != nil {
		if deps, ok := d.bindings.Get(b.Key); ok && deps != nil && deps.Attach != nil {
			return deps.Attach, nil
		}
	}
	if d.allowSharedFallback {
		return d.attach, nil
	}
	return nil, fmt.Errorf("server: no binding view registered for %s — silent fallback to shared infra refused (would leak across tenants)", b.Key)
}

// appendLogLine writes one newline-terminated record to the
// audit-log file, creating parent dirs as needed. Open/write/close
// per call — _log.md writes are infrequent and locking overhead
// dwarfs the open cost.
func appendLogLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return err
	}
	_, err = f.Write([]byte("\n"))
	return err
}

// Phase 6 write endpoints: agent posts content, daemon indexes it
// into OpenSearch immediately (raw-only doc), enqueues a synth job
// for asynchronous gate + distill, and returns 202. The next
// snapshot pull will see the enriched version once synth completes.
//
// Three endpoints share the same shape:
//   POST /api/brain/perceive  — gathered web content
//   POST /api/brain/learn     — curated document
//   POST /api/brain/attach    — binary blob (bytes inline or referenced)
//   POST /api/brain/trace     — audit-log append; no synth
//
// Plus one read endpoint:
//   GET  /api/brain/attach/{sha} — presigned MinIO GET URL for the blob
//
// All endpoints require the auth middleware to have populated a
// VaultBinding; the (profile, vault) on the binding scopes every
// write — clients cannot cross vault boundaries.

// osWriter is the slice of *osearch.Client that handlers + synth
// worker need. Defined as an interface here so tests can substitute
// an in-memory fake; the production wiring sets it to a real
// *osearch.Client. New methods land here as Day 5+ wire more
// codepaths.
type osWriter interface {
	UpsertSummary(ctx context.Context, doc osearch.SummaryDoc, waitForRefresh bool) error
	GetSummary(ctx context.Context, profile, vault, sha string) (*osearch.SummaryDoc, error)
	UpsertEntity(ctx context.Context, doc osearch.EntityDoc, waitForRefresh bool) error
	GetEntity(ctx context.Context, profile, vault, slug string) (*osearch.EntityDoc, error)
	UpsertAttachment(ctx context.Context, doc osearch.AttachmentDoc, waitForRefresh bool) error
	GetAttachment(ctx context.Context, profile, vault, sha string) (*osearch.AttachmentDoc, error)
}

// SynthQueue is the producer side of the per-vault synthesis worker
// added in Day 5. Day 4 ships a no-op default so the write handlers
// have somewhere to publish without blocking.
type SynthQueue interface {
	Enqueue(profile, vault, sha string)
}

// noopSynthQueue swallows enqueues silently. Used until Day 5 wires
// the real worker.
type noopSynthQueue struct{}

func (noopSynthQueue) Enqueue(string, string, string) {}

// AttachmentStore abstracts the blob backend for /attach. MinIO is
// the real impl; tests use an in-memory map. The store is the only
// place that knows where bytes live — Day 8's tear-out of the v5.0
// StorageBackend won't touch it.
// v3.2 Level 2: the store is bound to a single bucket. Production
// callers obtain a per-binding view via Daemon.resolveAttach(binding)
// — a thin wrapper over the shared *MinIOBackend that pins the
// binding's resolved bucket. The shared backend keeps ONE client (one
// credential / endpoint) and the per-binding view routes each call to
// the right bucket without per-request allocation beyond the cache
// lookup. The interface itself does NOT take a per-call bucket —
// callers that need a different bucket get a new view.
type AttachmentStore interface {
	// PutAttachment stores body at the canonical attachment key for
	// (profile, vault, sha, ext) in the view's bucket. Idempotent —
	// repeated puts with identical content are no-ops. Returns the
	// resolved object key.
	PutAttachment(ctx context.Context, profile, vault, sha, ext string, body []byte, contentType string) (key string, err error)
	// PutAttachmentWithTags is PutAttachment plus an index-side tag
	// slice the store mirrors onto the blob (S3 object tags on MinIO).
	// v2.5.1: keeps blob and pb_attachments index in sync at attach
	// time so lifecycle policies + tag-based access can use the same
	// shape brain_recall sees.
	PutAttachmentWithTags(ctx context.Context, profile, vault, sha, ext string, body []byte, contentType string, indexTags []string) (key string, err error)
	// PresignGet returns a short-lived URL the agent can GET to
	// retrieve the blob at key. ttl bounds validity.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (url string, err error)
	// GetAttachmentBytes returns the raw blob at key. Used by the
	// SynthWorker to pull PDFs back out of MinIO for pdftotext at
	// synth time. maxBytes caps the read defensively — pass 0 for
	// the impl's default ceiling.
	GetAttachmentBytes(ctx context.Context, key string, maxBytes int64) ([]byte, error)
}

// ErrAttachmentStoreUnavailable signals that no AttachmentStore is
// wired — endpoints that need it should return 503.
var ErrAttachmentStoreUnavailable = errors.New("attachment store not configured")

// --- request/response shapes --------------------------------------

// MemoryFields are the v2.4 classification fields shared across the
// three write request shapes. Mirrors brain.MemoryFields on the
// agent side. The daemon validates Kind against osearch.Kind's
// closed enum and rejects unknowns at the boundary.
type MemoryFields struct {
	Kind       string     `json:"kind,omitempty"`        // closed enum: see osearch.Kind
	MemoryType string     `json:"memory_type,omitempty"` // semantic | episodic | procedural | ""
	Source     []string   `json:"source,omitempty"`      // provenance: URLs, task IDs, agent IDs, file paths
	References []string   `json:"references,omitempty"`  // SHAs of related summaries
	CapturedAt *time.Time `json:"captured_at,omitempty"` // when the content was authored, not when OS got it; nil = unset
}

// PerceiveRequest mirrors the agent's brain_perceive payload, plus
// the agent-computed embedding so the daemon stays stateless w.r.t.
// Ollama. SHA is canonical and used as the doc-ID half.
type PerceiveRequest struct {
	SHA        string    `json:"sha"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	URL        string    `json:"url,omitempty"`
	SourcePath string    `json:"source_path,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	Embedding  []float32 `json:"embedding,omitempty"`
	MemoryFields
}

// LearnRequest mirrors brain_learn. The only daemon-side difference
// from PerceiveRequest is that learn marks the gate verdict as
// curated-medium (no LLM gate run); URL is optional.
type LearnRequest struct {
	SHA        string    `json:"sha"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	SourcePath string    `json:"source_path,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	Embedding  []float32 `json:"embedding,omitempty"`
	MemoryFields
}

// AttachRequest carries an attachment inline. The agent computes the
// SHA + extracted text + embedding before POSTing — daemon is pass-
// through for those fields and only handles the blob storage and OS
// metadata write.
type AttachRequest struct {
	SHA              string    `json:"sha"`
	OriginalFilename string    `json:"original_filename"`
	Title            string    `json:"title,omitempty"`
	MIMEType         string    `json:"mime_type,omitempty"`
	BytesB64         string    `json:"bytes_b64"`
	Description      string    `json:"description,omitempty"`
	ExtractedText    string    `json:"extracted_text,omitempty"`
	Tags             []string  `json:"tags,omitempty"`
	Embedding        []float32 `json:"embedding,omitempty"`
	MemoryFields
}

// validateMemoryFields runs the closed-enum checks at the boundary.
// Unknown Kind rejected; empty Kind allowed (caller didn't classify).
// MemoryType has its own "empty is OK" rule baked into IsValid.
// Returns "" on success, a user-facing message on failure.
func validateMemoryFields(m MemoryFields) string {
	if m.Kind != "" && !osearch.Kind(m.Kind).IsValid() {
		return "unknown kind: " + m.Kind + " (closed enum — see docs)"
	}
	if !osearch.MemoryType(m.MemoryType).IsValid() {
		return "unknown memory_type: " + m.MemoryType
	}
	return ""
}

// applyMemoryFields copies the request's memory fields onto a
// SummaryDoc. Called by both perceive and learn handlers so the
// pattern stays consistent.
func applyMemoryFields(doc *osearch.SummaryDoc, m MemoryFields) {
	doc.Kind = osearch.Kind(m.Kind)
	doc.MemoryType = osearch.MemoryType(m.MemoryType)
	doc.Source = m.Source
	doc.References = m.References
	doc.CapturedAt = m.CapturedAt
}

// TraceRequest is an append-only audit-log line. No synth, no OS
// content doc — the daemon just persists the entry. Phase 6 keeps
// the v5.0 _log.md format (line-per-event); the wiring lives in the
// existing trace logic, which Day 8 will rehome.
type TraceRequest struct {
	Kind    string         `json:"kind"`
	Message string         `json:"message"`
	Meta    map[string]any `json:"meta,omitempty"`
}

// WriteResponse is what perceive/learn/attach return after a
// successful raw-doc write. SynthEnqueued means synth is durably
// pending: the record is persisted Synthesised=false in the Postgres
// SoR, the worker's Enqueue fast path was tried for low latency, and —
// crucially — the background sweeper guarantees the record is processed
// even if that lossy fast path dropped it. It is therefore true whenever
// the record was written (synth is guaranteed-eventually), NOT a report
// of the in-memory channel's accept/drop. False only when the daemon has
// no synth worker running at all (e.g. OpenSearch unconfigured).
type WriteResponse struct {
	SHA           string `json:"sha"`
	IndexedAt     int64  `json:"indexed_at"`
	SynthEnqueued bool   `json:"synth_enqueued"`
}

// --- handlers -----------------------------------------------------

func (d *Daemon) handlePerceive(w http.ResponseWriter, r *http.Request) {
	binding, _ := BindingFromContext(r.Context())

	var req PerceiveRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validateWriteContent(w, req.SHA, req.Title, req.Body, req.MemoryFields) {
		return
	}

	now := time.Now().UTC()
	doc := osearch.SummaryDoc{
		Profile:     binding.Key.Profile,
		Vault:       binding.Key.Vault,
		SHA:         req.SHA,
		SourcePath:  req.SourcePath,
		SourceURL:   req.URL,
		Title:       req.Title,
		RawBody:     req.Body,
		Tags:        req.Tags,
		CreatedAt:   now,
		UpdatedAt:   now,
		Synthesised: false,
		Embedding:   req.Embedding,
	}
	applyMemoryFields(&doc, req.MemoryFields)
	// Phase D1: the Postgres SoR is THE write. On error finishRecordWrite
	// returns 502 so the agent's write-ahead queue retries (the daemon
	// SHA-dedups, so retries are safe). The pb_records projection follows
	// asynchronously via River.
	d.finishRecordWrite(w, r, binding, doc)
}

func (d *Daemon) handleLearn(w http.ResponseWriter, r *http.Request) {
	binding, _ := BindingFromContext(r.Context())

	var req LearnRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validateWriteContent(w, req.SHA, req.Title, req.Body, req.MemoryFields) {
		return
	}

	now := time.Now().UTC()
	// Curated sources skip the LLM gate per Phase 6 plan; verdict is
	// fixed medium. Day 5's synth worker still runs the distillation
	// pass to produce the synthesised body + entity extraction.
	doc := osearch.SummaryDoc{
		Profile:     binding.Key.Profile,
		Vault:       binding.Key.Vault,
		SHA:         req.SHA,
		SourcePath:  req.SourcePath,
		Title:       req.Title,
		RawBody:     req.Body,
		Tags:        req.Tags,
		CreatedAt:   now,
		UpdatedAt:   now,
		Reliability: osearch.ReliabilityMedium,
		GateReason:  "curated (brain_learn)",
		Synthesised: false,
		Embedding:   req.Embedding,
	}
	applyMemoryFields(&doc, req.MemoryFields)
	// Phase D1: the Postgres SoR is THE write. finishRecordWrite returns 502
	// on error so the agent's write-ahead queue retries (SHA-dedup makes
	// retries safe).
	d.finishRecordWrite(w, r, binding, doc)
}

func (d *Daemon) handleAttach(w http.ResponseWriter, r *http.Request) {
	if d.attach == nil {
		WriteErrorEnvelope(w, http.StatusServiceUnavailable, ErrCodeStorageBackendErr,
			"attachment store not configured; attach disabled", nil)
		return
	}
	binding, _ := BindingFromContext(r.Context())
	attach, err := d.resolveAttach(binding)
	if err != nil {
		d.writeBindingConfigError(w, err)
		return
	}

	var req AttachRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if err := validateSHA(req.SHA); err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.OriginalFilename) == "" {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "original_filename required", nil)
		return
	}
	if strings.TrimSpace(req.BytesB64) == "" {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "bytes_b64 required", nil)
		return
	}
	if msg := validateMemoryFields(req.MemoryFields); msg != "" {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, msg, nil)
		return
	}

	bytes, err := base64.StdEncoding.DecodeString(req.BytesB64)
	if err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "bytes_b64 decode failed: "+err.Error(), nil)
		return
	}
	// Guard against accidental SHA/byte mismatches — the SHA is the
	// content-addressed identity; a mismatch usually means the agent
	// hashed before mutation or the wire was corrupted.
	if got := osearch.SHA256Hex(bytes); got != req.SHA {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest,
			fmt.Sprintf("sha mismatch: declared %s, computed %s", req.SHA, got), nil)
		return
	}

	ext := extFromFilename(req.OriginalFilename)
	key, err := attach.PutAttachmentWithTags(r.Context(), binding.Key.Profile, binding.Key.Vault, req.SHA, ext, bytes, req.MIMEType, req.Tags)
	if err != nil {
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"attachment put failed: "+err.Error(), nil)
		return
	}

	now := time.Now().UTC()
	// Default Kind to attachment_stub when caller didn't classify —
	// preserves the agent's explicit choice (e.g. KindEmailImport for
	// migration ingest) when it's set.
	kind := osearch.Kind(req.Kind)
	if kind == "" {
		kind = osearch.KindAttachmentStub
	}
	doc := osearch.AttachmentDoc{
		Profile:          binding.Key.Profile,
		Vault:            binding.Key.Vault,
		SHA:              req.SHA,
		OriginalFilename: req.OriginalFilename,
		Title:            req.Title,
		MIMEType:         req.MIMEType,
		SizeBytes:        int64(len(bytes)),
		CreatedAt:        now,
		CapturedAt:       req.CapturedAt,
		MinIOKey:         key,
		Description:      req.Description,
		ExtractedText:    req.ExtractedText,
		Kind:             kind,
		MemoryType:       osearch.MemoryType(req.MemoryType),
		Source:           req.Source,
		References:       req.References,
		Tags:             req.Tags,
		Embedding:        req.Embedding,
		SummarySHA:       req.SHA, // companion stub lives at the same SHA in pb_summaries
	}
	// Preserve existing extracted text on re-attach. Without this guard
	// every re-attach (same SHA, same bytes) blows away the synth pass's
	// ExtractedText until the next synth pass re-runs extraction. Phase D1:
	// the attachment IS the same record in the SoR — read it back by SHA.
	if req.ExtractedText == "" {
		if view, perr := d.resolvePG(binding); perr == nil {
			if existing, gerr := d.getRecordBySHA(r.Context(), view, binding, req.SHA); gerr == nil && existing.ExtractedText.Valid && existing.ExtractedText.String != "" {
				doc.ExtractedText = existing.ExtractedText.String
			}
		}
	}

	// Companion stub identity — the attachment record's recall fields. The
	// attachment's binary metadata (minio_key/mime/size/filename) rides on
	// the same SoR record via writeAttachRecord. Same SHA, one record.
	stubTitle := req.Title
	if strings.TrimSpace(stubTitle) == "" {
		stubTitle = req.OriginalFilename
	}
	stubTags := append([]string(nil), req.Tags...)
	stubTags = append(stubTags, "attachment")
	if req.MIMEType != "" {
		stubTags = append(stubTags, "mime:"+req.MIMEType)
	}
	stub := osearch.SummaryDoc{
		Profile:     binding.Key.Profile,
		Vault:       binding.Key.Vault,
		SHA:         req.SHA,
		Kind:        osearch.KindAttachmentStub,
		MemoryType:  osearch.MemoryType(req.MemoryType),
		SourcePath:  "attachment://" + req.SHA,
		Source:      req.Source,
		References:  req.References,
		CapturedAt:  req.CapturedAt,
		Title:       stubTitle,
		RawBody:     req.Description, // extraction fills this in synth
		Tags:        stubTags,
		Attachments: []string{req.SHA},
		Reliability: osearch.ReliabilityMedium,
		GateReason:  "curated (attachment)",
		CreatedAt:   now,
		UpdatedAt:   now,
		Synthesised: false,
		Embedding:   req.Embedding,
	}
	// Phase D1: the Postgres SoR is THE write. 502 on error so the agent's
	// write-ahead queue retries (the bytes are already in MinIO; SHA-dedup
	// makes the record write retry-safe).
	if err := d.writeAttachRecord(r.Context(), binding, stub, doc); err != nil {
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"attachment record write failed: "+err.Error(), nil)
		return
	}
	d.synth.Enqueue(binding.Key.Profile, binding.Key.Vault, req.SHA)

	writeWriteResponse(w, http.StatusAccepted, req.SHA, true)
}

func (d *Daemon) handleTrace(w http.ResponseWriter, r *http.Request) {
	binding, _ := BindingFromContext(r.Context())

	var req TraceRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Kind) == "" || strings.TrimSpace(req.Message) == "" {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "kind and message required", nil)
		return
	}

	// Phase 6 keeps the v5.0 collective Wiki/_log.md as the audit
	// surface. A dedicated OS index lands in a later phase if the
	// volume warrants it. For Day 4 we just append a structured line
	// to the existing log path — same writer used by the synthesizer.
	line := map[string]any{
		"ts":      time.Now().UTC().Format(time.RFC3339Nano),
		"profile": binding.Key.Profile,
		"vault":   binding.Key.Vault,
		"kind":    req.Kind,
		"message": req.Message,
		"meta":    req.Meta,
	}
	out, _ := json.Marshal(line)

	logPath := filepath.Join(d.DataDir.VaultDir(binding.Key.Profile, binding.Key.Vault), "Wiki", "_log.md")
	if err := appendLogLine(logPath, out); err != nil {
		WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeStorageBackendErr,
			"trace append failed: "+err.Error(), nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleAttachGet / handleCaptureGet read the Postgres SoR (records) for
// the presign metadata (Phase D2a). The blob bytes still live in MinIO;
// these handlers look up the record by (profile, vault, sha), then presign
// the stored MinIO key. Post-cutover docs are written ONLY to the SoR, so
// these reads cover them; the legacy OpenSearch read paths were retired.
func (d *Daemon) handleAttachGet(w http.ResponseWriter, r *http.Request) {
	d.servePresignedRecordKey(w, r, "attach get", "attachment not found",
		func(rec pgdb.Record) (string, bool) {
			if rec.MinioKey.Valid && rec.MinioKey.String != "" {
				return rec.MinioKey.String, true
			}
			return "", false
		},
		func(rec pgdb.Record, url string) map[string]any {
			return map[string]any{
				"sha":        rec.Sha,
				"original":   rec.OriginalFilename.String,
				"mime_type":  rec.MimeType.String,
				"size_bytes": rec.SizeBytes.Int64,
				"url":        url,
				"expires_in": 600,
			}
		})
}

// handleCaptureGet returns a presigned MinIO URL for the raw-source
// capture associated with a summary doc. Empty/404 when capture is
// off, the URL was unreachable at synth time, or the doc isn't
// from a URL source (brain_learn, task_summary).
func (d *Daemon) handleCaptureGet(w http.ResponseWriter, r *http.Request) {
	const notFound = "no capture stored for this doc (capture disabled, URL absent, or fetch failed)"
	d.servePresignedRecordKey(w, r, "capture get", notFound,
		func(rec pgdb.Record) (string, bool) {
			if rec.CaptureMinioKey.Valid && rec.CaptureMinioKey.String != "" {
				return rec.CaptureMinioKey.String, true
			}
			return "", false
		},
		func(rec pgdb.Record, url string) map[string]any {
			return map[string]any{
				"sha":        rec.Sha,
				"source_url": rec.SourceUrl.String,
				"size_bytes": rec.CaptureSizeBytes.Int64,
				"url":        url,
				"expires_in": 600,
			}
		})
}

// --- helpers ------------------------------------------------------

// writeBindingConfigError emits the one canonical 500 envelope for a
// binding-view resolve failure (resolveOS / resolveAttach, or resolvePG's
// non-disabled branch). A cache miss here means buildBindingDeps never ran
// for the binding — a configuration error, not a storage outage.
func (d *Daemon) writeBindingConfigError(w http.ResponseWriter, err error) {
	d.Logger.Error("phantom-brain: binding configuration error", slog.String("err", err.Error()))
	WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeStorageBackendErr, "binding configuration error", nil)
}

// resolvePGOrError resolves the per-binding Postgres view and, on failure,
// emits the canonical envelope and returns ok=false (the common Go
// `if !ok { return }` handler idiom). ErrPostgresDisabled → 503
// "<op> not enabled for this binding"; any other resolve failure → the
// canonical 500 binding-config envelope. op names the feature for the 503
// message (e.g. "online recall", "online fetch", "reflect").
func (d *Daemon) resolvePGOrError(w http.ResponseWriter, b VaultBinding, op string) (*pgBindingView, bool) {
	view, err := d.resolvePG(b)
	if err != nil {
		if errors.Is(err, ErrPostgresDisabled) {
			WriteErrorEnvelope(w, http.StatusServiceUnavailable, ErrCodeStorageBackendErr,
				op+" not enabled for this binding", nil)
			return nil, false
		}
		d.writeBindingConfigError(w, err)
		return nil, false
	}
	return view, true
}

// errRecordNotFound is the sentinel getRecordBySHA returns when no record
// matches (profile, vault, sha). Callers map it to 404; any other error is a
// 502 Postgres-query failure.
var errRecordNotFound = errors.New("server: record not found")

// getRecordBySHA looks up one SoR record for the binding by SHA, returning
// errRecordNotFound (not pgx.ErrNoRows) when nothing matches. Centralises the
// params-from-binding construction and the no-rows classification shared by
// the read handlers.
func (d *Daemon) getRecordBySHA(ctx context.Context, view *pgBindingView, b VaultBinding, sha string) (pgdb.Record, error) {
	rec, err := pgstore.New(view.Pool()).GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{
		Profile: b.Key.Profile,
		Vault:   b.Key.Vault,
		Sha:     sha,
	})
	if err != nil {
		if errIsNoRows(err) {
			return pgdb.Record{}, errRecordNotFound
		}
		return pgdb.Record{}, err
	}
	return rec, nil
}

// servePresignedRecordKey is the shared body of handleAttachGet /
// handleCaptureGet: both look up a SoR record by SHA, presign a MinIO key
// stored on it, and return a small JSON envelope. They differ only in which
// key field is presigned (keySel), the not-found message, and the response
// shape (respBuilder). op labels the feature for the disabled-store / 503
// messages.
func (d *Daemon) servePresignedRecordKey(
	w http.ResponseWriter, r *http.Request, op, notFoundMsg string,
	keySel func(pgdb.Record) (string, bool),
	respBuilder func(rec pgdb.Record, url string) map[string]any,
) {
	if d.attach == nil {
		WriteErrorEnvelope(w, http.StatusServiceUnavailable, ErrCodeStorageBackendErr,
			op+" disabled (attachment store missing)", nil)
		return
	}
	binding, _ := BindingFromContext(r.Context())
	view, ok := d.resolvePGOrError(w, binding, op)
	if !ok {
		return
	}
	attach, err := d.resolveAttach(binding)
	if err != nil {
		d.writeBindingConfigError(w, err)
		return
	}
	sha := chi.URLParam(r, "sha")
	if err := validateSHA(sha); err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, err.Error(), nil)
		return
	}
	rec, err := d.getRecordBySHA(r.Context(), view, binding, sha)
	if err != nil {
		if errors.Is(err, errRecordNotFound) {
			WriteErrorEnvelope(w, http.StatusNotFound, ErrCodeNotFound, notFoundMsg, nil)
			return
		}
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"postgres get failed: "+err.Error(), nil)
		return
	}
	key, ok := keySel(rec)
	if !ok {
		WriteErrorEnvelope(w, http.StatusNotFound, ErrCodeNotFound, notFoundMsg, nil)
		return
	}
	url, err := attach.PresignGet(r.Context(), key, 10*time.Minute)
	if err != nil {
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"presign failed: "+err.Error(), nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(respBuilder(rec, url))
}

// decodeJSONBody decodes the request body into dst, emitting the canonical
// 400 "invalid JSON body" envelope and returning false on failure.
func decodeJSONBody[T any](w http.ResponseWriter, r *http.Request, dst *T) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "invalid JSON body", nil)
		return false
	}
	return true
}

// validateWriteContent runs the perceive/learn SHA + title/body +
// memory-field checks, emitting the matching 400 envelope and returning false
// on the first failure. Preserves every message verbatim.
func validateWriteContent(w http.ResponseWriter, sha, title, body string, m MemoryFields) bool {
	if err := validateSHA(sha); err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, err.Error(), nil)
		return false
	}
	if strings.TrimSpace(title) == "" || strings.TrimSpace(body) == "" {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "title and body required", nil)
		return false
	}
	if msg := validateMemoryFields(m); msg != "" {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, msg, nil)
		return false
	}
	return true
}

// finishRecordWrite is the shared tail of handlePerceive / handleLearn:
// write the raw record to the SoR (502 on failure so the agent's
// write-ahead queue retries — SHA-dedup makes that safe), fire the synth
// enqueue, and return 202.
func (d *Daemon) finishRecordWrite(w http.ResponseWriter, r *http.Request, binding VaultBinding, doc osearch.SummaryDoc) {
	if err := d.writeRecordRaw(r.Context(), binding, doc); err != nil {
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"record write failed: "+err.Error(), nil)
		return
	}
	d.synth.Enqueue(binding.Key.Profile, binding.Key.Vault, doc.SHA)
	writeWriteResponse(w, http.StatusAccepted, doc.SHA, true)
}

func writeWriteResponse(w http.ResponseWriter, status int, sha string, enqueued bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(WriteResponse{
		SHA:           sha,
		IndexedAt:     time.Now().UTC().Unix(),
		SynthEnqueued: enqueued,
	})
}

// validateSHA accepts only lowercase hex SHA256 strings (64 chars).
// Returning a typed error here means handlers can show the operator
// the bad input verbatim without leaking internals.
func validateSHA(sha string) error {
	if len(sha) != 64 {
		return fmt.Errorf("sha must be 64 hex chars; got %d", len(sha))
	}
	for _, ch := range sha {
		switch {
		case ch >= '0' && ch <= '9', ch >= 'a' && ch <= 'f':
			// ok
		default:
			return fmt.Errorf("sha must be lowercase hex; bad char %q", ch)
		}
	}
	return nil
}

// extFromFilename returns the dot-prefixed extension, or empty when
// the filename has none. Used to build attachment object keys.
func extFromFilename(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 || i == len(name)-1 {
		return ""
	}
	ext := name[i:]
	// Reject anything that would create a weird path component.
	for _, ch := range ext {
		if ch == '/' || ch == '\\' || ch == 0 {
			return ""
		}
	}
	return ext
}
