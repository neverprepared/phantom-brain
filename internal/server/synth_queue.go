package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// SynthWorker is Phase 6's replacement for the file-queue +
// synthesizer-loop pair. It drains an in-memory channel of (profile,
// vault, sha) jobs; for each job it reads the raw OS doc, runs gate
// + distill via the claude CLI, and writes back the enriched
// SummaryDoc plus one EntityDoc per extracted entity.
//
// Concurrency:
//   - Enqueue is non-blocking with overflow drop (logged at Warn).
//     Operator restart loses queued jobs — durable queueing is a
//     Phase 7 hardening item per the plan.
//   - One worker goroutine processes one job at a time. Per-vault
//     parallelism doesn't matter for a single-operator deploy and
//     keeps `claude` CLI invocations from stampeding.
//   - Stop drains the in-flight job (bounded by ctx timeout) before
//     returning.
//
// SynthWorker satisfies SynthQueue, so the daemon plugs it straight
// into the field where the noop default used to live.
type SynthWorker struct {
	os       osWriter
	logger   *slog.Logger
	queue    chan synthJob
	bufSize  int
	stopOnce sync.Once
	stopped  chan struct{}

	// baseCtx is captured in Start. The live run() loop uses the ctx
	// passed to Start directly, but the background backfill goroutine
	// (ResynthBacklog apply path) outlives the HTTP request that kicks
	// it — the request ctx dies when the handler returns, so backfill
	// must run against this longer-lived daemon ctx instead.
	baseCtx context.Context

	// processMu serializes processJob across the live run() loop AND
	// the backfill goroutine so they never run concurrently. upsertEntity
	// is a read-modify-write on the OS entity doc, single-worker-safe
	// only — concurrent processing would corrupt MentionedBy[].
	processMu sync.Mutex

	// backfilling caps the resynth backfill at one at a time. The apply
	// path CompareAndSwaps this to true before spawning its goroutine and
	// Stores false in the goroutine's defer.
	backfilling atomic.Bool

	// OnComplete is fired after each successfully synthesised job.
	// Day 6 wires the debounced snapshot rebuild trigger here.
	// Nil-safe; the worker checks before invoking.
	OnComplete func(profile, vault, sha string)

	// cliAvailable is snapshotted at construction so the worker
	// behaves predictably across the run — toggling the CLI in/out
	// of $PATH at runtime would otherwise produce mixed-mode output.
	cliAvailable bool

	// Capture wires the raw-source archival path. When attach is
	// non-nil and Capture.Enabled, processJob fetches the doc's
	// source URL and stores response bytes in MinIO before running
	// gate + distill. Failures are logged and non-fatal.
	attach  AttachmentStore
	capture CaptureConfig

	// Resolve returns per-binding views for a job's (profile, vault).
	// v3.2 per-binding storage overrides: each binding may have its
	// own OS index prefix + MinIO bucket. processJob looks up the
	// binding's view here and uses it for every OS/MinIO call rather
	// than the worker's struct-level shared os/attach fields.
	//
	// Nil-safe: when Resolve is nil the worker falls back to the
	// shared os/attach fields (legacy / test path).
	Resolve func(profile, vault string) (osWriter, AttachmentStore, bool)

	// pdfExtractor pulls plain text out of PDF attachments at synth
	// time so they become FTS-searchable. Nil-safe — when the daemon
	// runs in an environment without poppler-utils, the field stays
	// nil and PDF attachments synth as a no-op.
	pdfExtractor    PDFExtractor
	pdfAvailable    bool

	// ocrAvailable is snapshotted at construction. When true the worker
	// may OCR image attachments and image-only (scanned) PDFs via
	// tesseract. Office (docx/xlsx/pptx) extraction is pure-Go and needs
	// no availability gate. Nil/false in environments without tesseract
	// — those attachments synth with empty bodies, like before v3.4.
	ocrAvailable bool
}

type synthJob struct {
	Profile string
	Vault   string
	SHA     string
}

// SynthWorkerOpts groups construction inputs.
type SynthWorkerOpts struct {
	OSClient osWriter
	Logger   *slog.Logger
	// BufferSize bounds the in-memory queue. 1000 is plenty for a
	// single-operator deploy; bursts that exceed it drop oldest-
	// first (TryEnqueue returns false, caller logs).
	BufferSize int
	// DisableCLI forces the worker to skip both RunGate and
	// SummarizeContent's LLM call paths. Used by tests to keep the
	// pipeline deterministic and fast (no real claude subprocess);
	// production leaves this false and probes the CLI at startup.
	DisableCLI bool
	// Attach + Capture wire raw-source archival. Pass the same
	// AttachmentStore the daemon uses for /attach; if nil, the
	// capture pass is skipped entirely.
	Attach  AttachmentStore
	Capture CaptureConfig
	// PDFExtractor overrides the default pdftotext-backed extractor.
	// Tests inject a deterministic fake; production leaves it nil and
	// NewSynthWorker picks PDFExtractWithPdftotext when the binary is
	// on PATH.
	PDFExtractor PDFExtractor
}

// NewSynthWorker constructs a worker; call Start to spawn the
// goroutine.
func NewSynthWorker(opts SynthWorkerOpts) *SynthWorker {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.BufferSize <= 0 {
		opts.BufferSize = 1000
	}
	cli := ClaudeCLIAvailable()
	if opts.DisableCLI {
		cli = false
	}
	extractor := opts.PDFExtractor
	pdfAvail := PdftotextAvailable()
	if extractor == nil && pdfAvail {
		extractor = PDFExtractWithPdftotext
	}
	return &SynthWorker{
		os:           opts.OSClient,
		logger:       opts.Logger,
		queue:        make(chan synthJob, opts.BufferSize),
		bufSize:      opts.BufferSize,
		stopped:      make(chan struct{}),
		cliAvailable: cli,
		attach:       opts.Attach,
		capture:      opts.Capture,
		pdfExtractor: extractor,
		pdfAvailable: extractor != nil,
		ocrAvailable: OCRAvailable(),
	}
}

// Enqueue tries to publish a job. Returns immediately; never blocks.
// Overflow drops the job and logs at Warn — the operator should see
// queue pressure in logs before agents stop seeing enrichment.
func (w *SynthWorker) Enqueue(profile, vault, sha string) {
	select {
	case w.queue <- synthJob{Profile: profile, Vault: vault, SHA: sha}:
	default:
		w.logger.Warn("phantom-brain: synth queue full; dropping job",
			slog.String("vault", profile+"/"+vault),
			slog.String("sha", sha),
			slog.Int("buf_size", w.bufSize),
		)
	}
}

// Start spawns the worker goroutine. ctx cancellation drains in-
// flight work and exits the loop. Idempotent — repeat Starts are
// no-ops once running.
func (w *SynthWorker) Start(ctx context.Context) {
	w.baseCtx = ctx
	go w.run(ctx)
}

// Stop signals the worker to exit after its current job completes.
// Safe to call multiple times.
func (w *SynthWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stopped) })
}

// ErrResynthInProgress is returned by ResynthBacklog when an apply is
// already running. At most one backfill runs at a time so the
// single-worker entity-upsert invariant holds; callers (handleResynth)
// errors.Is this to map it onto an HTTP 409 Conflict.
var ErrResynthInProgress = errors.New("resynth: a backfill is already in progress")

// ResynthSampleItem is one preview row in a ResynthResult.
type ResynthSampleItem struct {
	SHA   string `json:"sha"`
	Title string `json:"title"`
}

// ResynthResult is the report ResynthBacklog returns. On a dry run only
// the count + sample are populated; on apply Started/Pending describe the
// background work that was kicked off.
type ResynthResult struct {
	BacklogCount int                 `json:"backlog_count"` // total Synthesised=false found
	Sample       []ResynthSampleItem `json:"sample"`        // capped preview (<=20)
	Started      bool                `json:"started"`       // apply kicked off
	Pending      int                 `json:"pending"`       // how many will be processed
}

// resynthSampleCap bounds the preview slice ResynthBacklog returns.
const resynthSampleCap = 20

// ResynthBacklog scrolls Synthesised=false summaries for (profile, vault).
// dryRun: report count+sample, mutate nothing. apply: spawn ONE background
// goroutine that re-processes each stuck doc serialized with the live
// worker (non-lossy — bypasses the lossy Enqueue channel). limit<=0 means
// all; limit>0 caps how many are processed (count still reflects the true total).
//
// This is the fix-it apply-companion to brain_reflect (issue #82): a bulk
// ingest can outrun the single CLI-bound worker and overflow Enqueue's
// buffer, leaving docs stuck at Synthesised=false. Re-pushing them through
// Enqueue would risk dropping them again; instead the backfill calls
// w.handle directly, which takes processMu and therefore can never run
// concurrently with the live worker (preserving the entity-upsert invariant).
func (w *SynthWorker) ResynthBacklog(ctx context.Context, osc osWriter, profile, vault string, dryRun bool, limit int) (ResynthResult, error) {
	var stuck []synthJob
	var sample []ResynthSampleItem
	err := osc.ScrollSummaries(ctx, profile, vault, 0, func(doc osearch.SummaryDoc) error {
		if doc.Synthesised {
			return nil
		}
		stuck = append(stuck, synthJob{Profile: profile, Vault: vault, SHA: doc.SHA})
		if len(sample) < resynthSampleCap {
			sample = append(sample, ResynthSampleItem{SHA: doc.SHA, Title: doc.Title})
		}
		return nil
	})
	if err != nil {
		return ResynthResult{}, fmt.Errorf("resynth: scroll: %w", err)
	}

	res := ResynthResult{BacklogCount: len(stuck), Sample: sample}
	if dryRun || len(stuck) == 0 {
		return res, nil
	}

	toProcess := stuck
	if limit > 0 && limit < len(stuck) {
		toProcess = stuck[:limit]
	}

	// At most one backfill at a time — the live worker + backfill already
	// serialize on processMu, but two backfills would each spin a goroutine
	// re-scanning the same backlog. CompareAndSwap rejects the second.
	if !w.backfilling.CompareAndSwap(false, true) {
		return res, ErrResynthInProgress
	}

	go func() {
		defer w.backfilling.Store(false)
		bctx := w.baseCtx
		if bctx == nil {
			bctx = context.Background()
		}
		for _, job := range toProcess {
			select {
			case <-bctx.Done():
				return
			default:
			}
			w.handle(bctx, job)
		}
	}()

	res.Started = true
	res.Pending = len(toProcess)
	return res, nil
}

func (w *SynthWorker) run(ctx context.Context) {
	w.logger.Info("phantom-brain: synth worker started",
		slog.Int("buf_size", w.bufSize),
		slog.Bool("claude_cli", w.cliAvailable),
		slog.Bool("pdftotext", w.pdfAvailable),
		slog.Bool("tesseract_ocr", w.ocrAvailable),
	)
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("phantom-brain: synth worker exiting (ctx done)")
			return
		case <-w.stopped:
			w.logger.Info("phantom-brain: synth worker exiting (stop)")
			return
		case job := <-w.queue:
			w.handle(ctx, job)
		}
	}
}

// handle runs one job under processMu (serialized with backfill) and
// fires OnComplete on success. Shared by the live loop and backfill.
func (w *SynthWorker) handle(ctx context.Context, job synthJob) {
	w.processMu.Lock()
	err := w.processJob(ctx, job)
	w.processMu.Unlock()
	if err != nil {
		w.logger.Warn("phantom-brain: synth job failed",
			slog.String("vault", job.Profile+"/"+job.Vault),
			slog.String("sha", job.SHA), slog.String("err", err.Error()))
		return
	}
	if w.OnComplete != nil {
		w.OnComplete(job.Profile, job.Vault, job.SHA)
	}
}

// resolveForJob returns the per-binding views (osWriter +
// AttachmentStore) for a job's (profile, vault). v3.2 per-binding
// storage overrides: each (profile, vault) may resolve to its own
// OS index prefix + MinIO bucket. The worker calls this once per
// job and uses the result instead of the struct-level os/attach
// fields.
//
// Returns ok=false on cache miss. Callers MUST drop the job rather
// than fall back to shared infra — synthesising a doc into the
// wrong tenant's indices/bucket is a worse failure than dropping
// the job and waiting for a re-enqueue. The Resolve callback uses
// the same fail-loud contract as Daemon.resolveOS/resolveAttach;
// the shared fallback fields (w.os/w.attach) remain only for the
// legacy single-tenant path where Resolve is nil.
func (w *SynthWorker) resolveForJob(job synthJob) (osWriter, AttachmentStore, bool) {
	if w.Resolve != nil {
		if oc, at, ok := w.Resolve(job.Profile, job.Vault); ok {
			return oc, at, true
		}
		return nil, nil, false
	}
	if w.os == nil {
		return nil, nil, false
	}
	return w.os, w.attach, true
}

// processJob runs one (profile, vault, sha) item through the
// pipeline. Returns nil on success; non-nil errors get logged at
// Warn — the doc stays in raw-only state and a future re-enqueue
// (operator-driven, e.g. brain_reflect) can retry.
func (w *SynthWorker) processJob(ctx context.Context, job synthJob) error {
	osc, attach, ok := w.resolveForJob(job)
	if !ok {
		// Dropping the job is the correct response: we cannot synthesise
		// without knowing which prefix/bucket the binding resolves to,
		// and writing to the shared default would leak across tenants.
		// A re-enqueue (e.g. brain_reflect) will retry once the binding
		// view is registered.
		w.logger.Error("phantom-brain: synth job dropped — no binding view registered",
			slog.String("profile", job.Profile),
			slog.String("vault", job.Vault),
			slog.String("sha", job.SHA))
		return nil
	}
	doc, err := osc.GetSummary(ctx, job.Profile, job.Vault, job.SHA)
	if err != nil {
		return err
	}
	if doc == nil {
		// No summary doc — could be a delete race, or this SHA belongs
		// to an attachment doc instead. Try the attachment path before
		// silently returning so PDFs get their daemon-side extraction.
		return w.processAttachmentJob(ctx, job, osc, attach)
	}
	if doc.Synthesised {
		// Idempotent: re-enqueueing an already-synthed doc is fine
		// and may even be useful (re-rate after a gate model upgrade),
		// but for Day 5's "first cut" we skip the wasted work.
		return nil
	}

	// Attachment stubs (v2.5.1, #48): before gate/distill, attempt PDF
	// text extraction and fold it into the stub's RawBody so the
	// downstream distill pass sees real attachment content rather than
	// just the caller's description. Non-fatal — a failure leaves the
	// stub with description-only RawBody, which is still recall-visible.
	if doc.Kind == osearch.KindAttachmentStub {
		if err := w.enrichAttachmentStub(ctx, job, doc, osc, attach); err != nil {
			w.logger.Warn("phantom-brain: attachment stub enrichment failed (non-fatal)",
				slog.String("sha", job.SHA), slog.String("err", err.Error()))
		}
	}

	content := doc.RawBody
	if content == "" {
		content = doc.Body // shouldn't happen for handler-fed docs but defensible
	}

	// Raw-source capture (v2.4+): when capture is wired and the doc
	// has a URL, fetch the page bytes and stash them in MinIO. Best-
	// effort — fetch failures (URL unreachable, oversize, non-2xx)
	// are logged and DON'T block gate/distill/index writes.
	if attach != nil && w.capture.Enabled && doc.SourceURL != "" && doc.CaptureMinIOKey == "" {
		ua := w.capture.UserAgent
		timeout := time.Duration(w.capture.TimeoutSecs) * time.Second
		res, cerr := CaptureURL(ctx, attach, job.Profile, job.Vault, job.SHA,
			doc.SourceURL, w.capture.MaxBytes, ua, timeout)
		if cerr != nil {
			w.logger.Warn("phantom-brain: capture failed (non-fatal)",
				slog.String("sha", job.SHA),
				slog.String("url", doc.SourceURL),
				slog.String("err", cerr.Error()))
		} else {
			doc.CaptureMinIOKey = res.Key
			doc.CaptureSizeBytes = res.SizeBytes
		}
	}

	// Coherence first — free and rejects obviously-broken input
	// before paying for the LLM. Same shape as v5.0's pipeline.
	verdict := GateVerdict{Topic: TopicGeneral, Reliability: ReliabilityMedium}
	if cr := CheckCoherence(content); !cr.Passed {
		verdict = GateVerdict{
			Reliability: ReliabilityLow,
			Category:    CategoryInformal,
			Topic:       TopicGeneral,
			Reason:      "coherence-fail: " + cr.Reason,
		}
	} else if doc.Reliability == osearch.ReliabilityMedium && doc.GateReason != "" &&
		strings.HasPrefix(doc.GateReason, "curated") {
		// Learn() already stamped curated-medium; skip the LLM gate.
	} else if w.cliAvailable {
		verdict = RunGate(ctx, GateOpts{
			Title:      doc.Title,
			SourceURL:  doc.SourceURL,
			Content:    content,
			Format:     "markdown",
			SourceType: gateSourceType(doc),
		})
	}

	// Distill. If the CLI is unavailable or fails we fall back to the
	// raw content so the doc still becomes searchable as a summary —
	// matches v5.0 behaviour.
	summary := ""
	if w.cliAvailable {
		s, sErr := SummarizeContent(ctx, doc.Title, content, "", 0)
		if sErr != nil {
			w.logger.Warn("phantom-brain: summarize failed; using raw content",
				slog.String("sha", job.SHA), slog.String("err", sErr.Error()))
		} else {
			summary = s
		}
	}
	if summary == "" {
		summary = content
	}

	// Extract entities from RAW content per v5.0 rationale — entity
	// coverage is more faithful on the original text than the LLM
	// distillation.
	// v2.4: LLM-driven entity extraction when claude is available —
	// falls back to the regex extractor (heading + bold heuristics)
	// otherwise. The LLM version filters out section labels +
	// descriptive list items that the regex can't distinguish from
	// real named entities; the regex stays as a resilient fallback.
	entities := extractEntitiesBest(ctx, doc.Title, content, w.cliAvailable, w.logger)
	entitySlugs := make([]string, 0, len(entities))
	for _, ent := range entities {
		slug, err := w.upsertEntity(ctx, job, doc, ent, content, verdict, osc)
		if err != nil {
			w.logger.Warn("phantom-brain: entity upsert failed (continuing)",
				slog.String("entity", ent), slog.String("err", err.Error()))
			continue
		}
		entitySlugs = append(entitySlugs, slug)
	}

	now := time.Now().UTC()
	doc.Body = summary
	doc.Synthesised = true
	doc.UpdatedAt = now
	doc.Reliability = osearch.Reliability(verdict.Reliability)
	doc.Topic = string(verdict.Topic)
	doc.GateReason = verdict.Reason
	doc.Entities = entitySlugs
	return osc.UpsertSummary(ctx, *doc, false)
}

// gateSourceType maps the SummaryDoc shape onto the v5.0 GateOpts
// SourceType field. Curated docs are stamped by handleLearn with
// reliability=medium + a "curated" reason; everything else is gathered.
func gateSourceType(doc *osearch.SummaryDoc) string {
	if doc.Reliability == osearch.ReliabilityMedium && strings.HasPrefix(doc.GateReason, "curated") {
		return "curated"
	}
	return "gathered"
}

// upsertEntity merges this synthesis into the (profile, vault, slug)
// entity doc. Read-modify-write under the daemon's single-worker
// constraint; with multiple workers we'd need OS's painless update
// script for atomicity, which can land in Phase 7 if it ever matters.
func (w *SynthWorker) upsertEntity(ctx context.Context, job synthJob, src *osearch.SummaryDoc, name, body string, v GateVerdict, osc osWriter) (string, error) {
	slug := osearch.EntitySlug(name)
	if slug == "" {
		return "", nil
	}
	now := time.Now().UTC()
	existing, err := osc.GetEntity(ctx, job.Profile, job.Vault, slug)
	if err != nil {
		return "", err
	}
	snippet := EntitySnippet(body, name)

	var doc osearch.EntityDoc
	if existing != nil {
		doc = *existing
		doc.UpdatedAt = now
		if !containsString(doc.MentionedBy, src.SHA) {
			doc.MentionedBy = append(doc.MentionedBy, src.SHA)
		}
		if snippet != "" && !strings.Contains(doc.Body, snippet) {
			doc.Body = strings.TrimSpace(doc.Body + "\n\n" + snippet)
		}
	} else {
		doc = osearch.EntityDoc{
			Profile:     job.Profile,
			Vault:       job.Vault,
			Slug:        slug,
			Name:        name,
			Body:        snippet,
			Topic:       string(v.Topic),
			MentionedBy: []string{src.SHA},
			CreatedAt:   now,
			UpdatedAt:   now,
		}
	}
	if err := osc.UpsertEntity(ctx, doc, false); err != nil {
		return "", err
	}
	return slug, nil
}

// extractAttachmentText dispatches on the attachment's MIME type (with
// a filename-extension fallback for the empty/octet-stream case the
// bulk loader assigns) and runs the matching extractor. Every branch
// is availability-gated and soft-fail: a fetch error, a missing tool,
// or an extractor failure logs at Warn and returns "" so the caller
// leaves ExtractedText empty exactly as the pre-v3.4 PDF path did.
//
// Dispatch table:
//
//	application/pdf              → pdftotext; empty result + OCR available
//	                               → OCRExtractScannedPDF (scanned PDF)
//	image/{png,jpeg,gif,bmp,     → OCRExtractImage (tesseract)
//	       tiff,webp}
//	docx/xlsx/pptx OOXML mimes   → OfficeExtract (pure-Go, always on)
//	application/msword, …ms-     → OfficeExtract → unsupported-legacy log
//	excel, …ms-powerpoint
//
// When MIMEType is empty or application/octet-stream, dispatch keys off
// the OriginalFilename extension instead (the loader tags xlsx/pptx/doc/
// xls as octet-stream — see guessAttachmentMIME).
func (w *SynthWorker) extractAttachmentText(ctx context.Context, job synthJob, att *osearch.AttachmentDoc, attach AttachmentStore) string {
	mime := att.MIMEType
	ext := strings.ToLower(filepath.Ext(att.OriginalFilename))

	// fetch lazily — every branch needs the bytes, but office/legacy
	// dispatch can short-circuit before we pay the MinIO round-trip only
	// in the unsupported-legacy case (handled inside OfficeExtract).
	fetch := func() ([]byte, bool) {
		body, ferr := attach.GetAttachmentBytes(ctx, att.MinIOKey, 0)
		if ferr != nil {
			w.logger.Warn("phantom-brain: attachment fetch failed (non-fatal)",
				slog.String("sha", job.SHA), slog.String("key", att.MinIOKey),
				slog.String("err", ferr.Error()))
			return nil, false
		}
		return body, true
	}

	switch {
	case mime == "application/pdf" || (isGenericMIME(mime) && ext == ".pdf"):
		if w.pdfExtractor == nil {
			return ""
		}
		body, ok := fetch()
		if !ok {
			return ""
		}
		text, eerr := w.pdfExtractor(ctx, body)
		if eerr != nil {
			w.logger.Warn("phantom-brain: pdf extract failed (non-fatal)",
				slog.String("sha", job.SHA), slog.String("err", eerr.Error()))
			text = ""
		}
		if strings.TrimSpace(text) != "" {
			return text
		}
		// Image-only (scanned) PDF: pdftotext found nothing. Fall back to
		// rasterize-then-OCR when tesseract is available.
		if w.ocrAvailable {
			ocrText, oerr := OCRExtractScannedPDF(ctx, body)
			if oerr != nil {
				w.logger.Warn("phantom-brain: scanned-pdf OCR failed (non-fatal)",
					slog.String("sha", job.SHA), slog.String("err", oerr.Error()))
				return ""
			}
			return ocrText
		}
		return ""

	case isImageMIME(mime) || (isGenericMIME(mime) && isImageExt(ext)):
		if !w.ocrAvailable {
			return ""
		}
		body, ok := fetch()
		if !ok {
			return ""
		}
		text, oerr := OCRExtractImage(ctx, body)
		if oerr != nil {
			w.logger.Warn("phantom-brain: image OCR failed (non-fatal)",
				slog.String("sha", job.SHA), slog.String("err", oerr.Error()))
			return ""
		}
		return text

	case officeKind(mime, att.OriginalFilename) != "":
		body, ok := fetch()
		if !ok {
			return ""
		}
		text, oerr := OfficeExtract(mime, att.OriginalFilename, body)
		if errors.Is(oerr, ErrUnsupportedLegacyOffice) {
			w.logger.Info("phantom-brain: skipping legacy binary office format",
				slog.String("sha", job.SHA), slog.String("mime", mime),
				slog.String("filename", att.OriginalFilename))
			return ""
		}
		if oerr != nil {
			w.logger.Warn("phantom-brain: office extract failed (non-fatal)",
				slog.String("sha", job.SHA), slog.String("err", oerr.Error()))
			return ""
		}
		return text
	}
	return ""
}

// isGenericMIME reports whether the stored MIME is absent or the
// catch-all octet-stream, in which case dispatch should fall back to
// the filename extension.
func isGenericMIME(mime string) bool {
	return mime == "" || mime == "application/octet-stream"
}

// isImageMIME matches the raster image types we OCR.
func isImageMIME(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif",
		"image/bmp", "image/tiff", "image/webp":
		return true
	}
	return false
}

// isImageExt is the extension-fallback companion to isImageMIME.
func isImageExt(ext string) bool {
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".tif", ".tiff", ".webp":
		return true
	}
	return false
}

// enrichAttachmentStub is the v2.5.1 attachment path: fold the
// AttachmentDoc's extracted text into the companion stub SummaryDoc's
// RawBody so the existing gate + distill + UpsertSummary tail handles
// the stub like any other summary. Runs pdftotext on demand when the
// attachment hasn't been extracted yet.
//
// Mutates `summary` in place — caller continues with the standard
// pipeline using the updated RawBody.
func (w *SynthWorker) enrichAttachmentStub(ctx context.Context, job synthJob, summary *osearch.SummaryDoc, osc osWriter, attach AttachmentStore) error {
	att, err := osc.GetAttachment(ctx, job.Profile, job.Vault, job.SHA)
	if err != nil {
		return err
	}
	if att == nil {
		// Stub without companion AttachmentDoc — legacy or torn-write.
		// Nothing to enrich from; stub still gets synthesised on its
		// description-only RawBody.
		return nil
	}

	// Extract searchable text when the attachment carries none yet and
	// its type is one we can handle (PDF, image via OCR, OOXML office).
	// Failures here are soft — the stub falls back to description-only
	// RawBody.
	if att.ExtractedText == "" && attach != nil {
		if text := w.extractAttachmentText(ctx, job, att, attach); text != "" {
			att.ExtractedText = text
		}
	}

	// Backfill the AttachmentDoc <-> SummaryDoc cross-link when missing.
	dirty := false
	if att.SummarySHA == "" {
		att.SummarySHA = summary.SHA
		dirty = true
	}
	if att.ExtractedText != "" {
		dirty = true // persist newly-extracted text
	}
	if dirty {
		if err := osc.UpsertAttachment(ctx, *att, false); err != nil {
			return err
		}
	}

	// Compose the stub's RawBody. Description first (curator intent),
	// extracted text after (machine signal). Either alone is fine.
	desc := strings.TrimSpace(att.Description)
	extracted := strings.TrimSpace(att.ExtractedText)
	switch {
	case desc != "" && extracted != "":
		summary.RawBody = desc + "\n\n---\n\n" + extracted
	case extracted != "":
		summary.RawBody = extracted
	case desc != "":
		summary.RawBody = desc
		// else: leave whatever the stub already carried (likely empty).
	}
	return nil
}

// processAttachmentJob runs the attachment-specific synth pass for an
// AttachmentDoc with no companion SummaryDoc. v3.4 (#86) generalized
// the enrichment from PDF-only to a MIME dispatch (PDF + scanned-PDF
// OCR, image OCR, OOXML office) via extractAttachmentText.
//
// Failure modes are all soft — log + return nil — because an
// attachment with no extracted text is still a valid recall hit on
// title/filename and re-enqueue via brain_reflect is the operator
// escape hatch.
func (w *SynthWorker) processAttachmentJob(ctx context.Context, job synthJob, osc osWriter, attach AttachmentStore) error {
	doc, err := osc.GetAttachment(ctx, job.Profile, job.Vault, job.SHA)
	if err != nil {
		return err
	}
	if doc == nil {
		// True delete race / unknown SHA — nothing to do.
		return nil
	}
	if doc.ExtractedText != "" {
		// Idempotent: agent-side extraction already populated this, or
		// a previous synth pass did. Don't pay the subprocess cost twice.
		return nil
	}
	if attach == nil {
		w.logger.Info("phantom-brain: skipping attachment extraction (no store)",
			slog.String("sha", job.SHA))
		return nil
	}
	// Shared MIME dispatch (PDF/pdftotext+OCR, image OCR, OOXML office).
	// Soft-fail: an unhandled type or a tool failure returns "" and we
	// leave the doc alone rather than writing an empty body that signals
	// "we tried" when a later re-enrich could succeed.
	text := w.extractAttachmentText(ctx, job, doc, attach)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	doc.ExtractedText = text
	return osc.UpsertAttachment(ctx, *doc, false)
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
