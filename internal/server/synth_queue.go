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

// synthRecord is the worker's read view of one SoR record. It carries
// only the fields the synth pipeline reads — the mapped SummaryDoc (so
// CheckCoherence / RunGate / SummarizeContent / extractEntitiesBest keep
// working against their existing shape) plus the relational identity
// (RecordID) and attachment metadata the enrichment path needs. The
// production adapter (pgSynthStore) builds this from a pgdb.Record via
// pgRecordToSummaryDoc; tests build it directly.
type synthRecord struct {
	Doc              osearch.SummaryDoc // mapped summary view (pgRecordToSummaryDoc)
	RecordID         int64
	Synthesised      bool
	MIMEType         string
	OriginalFilename string
	MinIOKey         string
	ExtractedText    string
}

// synthStore is the per-binding Postgres read/extract surface the worker
// depends on, mirroring the WriteSynth callback seam so the read path is
// equally fakeable. The production impl (pgSynthStore) wraps a
// *pgBindingView and calls pgstore.New(view.Pool()); tests inject an
// in-memory fake. Keeping this abstract means processJob + ResynthBacklog
// never touch a live pgx pool directly.
type synthStore interface {
	// Fetch returns the record for (profile, vault, sha) or (nil, nil)
	// when no rows match — a delete race / unknown SHA is not an error.
	Fetch(ctx context.Context, profile, vault, sha string) (*synthRecord, error)
	// SetExtractedText persists newly extracted attachment text back to
	// the SoR record so re-synth is idempotent (it won't re-extract).
	SetExtractedText(ctx context.Context, recordID int64, text string) error
	// ListUnsynthesised returns the Synthesised=false backlog for
	// (profile, vault), ordered for stable sampling. Used by
	// ResynthBacklog; len() doubles as the backlog count.
	ListUnsynthesised(ctx context.Context, profile, vault string) ([]synthRecord, error)
}

// SynthWorker drains an in-memory channel of (profile, vault, sha) jobs.
// For each job it reads the raw record from the Postgres SoR, runs gate +
// distill via the claude CLI, extracts entities, and writes the enriched
// result back to the SoR (MarkRecordSynthesised + entity upserts + a
// re-projection enqueue, all in one tx via writeSynthResult). The
// pb_records OpenSearch projection is updated by the River worker once the
// re-projection job runs — the synth worker never touches OpenSearch
// directly (Phase D1: PG is the sole authoritative store).
//
// Concurrency:
//   - Enqueue is non-blocking with overflow drop (logged at Warn).
//     Operator restart loses queued jobs — durable queueing is a
//     Phase 7 hardening item per the plan.
//   - One worker goroutine processes one job at a time.
//   - Stop drains the in-flight job (bounded by ctx timeout) before
//     returning.
//
// SynthWorker satisfies SynthQueue, so the daemon plugs it straight
// into the field where the noop default used to live.
type SynthWorker struct {
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
	// the backfill goroutine so they never run concurrently. Entity
	// upserts are now per-(profile, vault, slug) ON CONFLICT in the SoR
	// (concurrency-safe), but serialization keeps CLI invocations from
	// stampeding and preserves the single-job-at-a-time contract.
	processMu sync.Mutex

	// backfilling caps the resynth backfill at one at a time. The apply
	// path CompareAndSwaps this to true before spawning its goroutine and
	// Stores false in the goroutine's defer.
	backfilling atomic.Bool

	// OnComplete is fired after each successfully synthesised job.
	// Wires the debounced snapshot rebuild trigger here. Nil-safe; the
	// worker checks before invoking.
	OnComplete func(profile, vault, sha string)

	// cliAvailable is snapshotted at construction so the worker
	// behaves predictably across the run — toggling the CLI in/out
	// of $PATH at runtime would otherwise produce mixed-mode output.
	cliAvailable bool

	// capture wires the raw-source archival path. When the binding's
	// AttachmentStore is non-nil and Capture.Enabled, processJob fetches
	// the doc's source URL and stores response bytes in MinIO before
	// running gate + distill. Failures are logged and non-fatal.
	//
	// Phase D1 NOTE: the resulting CaptureMinIOKey has NO Postgres
	// column, so the capture bytes land in MinIO but the key is logged
	// and dropped — capture-retrieval (handleCaptureGet) will not see it
	// until the SoR grows a capture_minio_key column (follow-up).
	capture CaptureConfig

	// Resolve returns the per-binding synthStore + AttachmentStore for a
	// job's (profile, vault). v3.2 per-binding storage overrides: each
	// binding has its own OS projection prefix + MinIO bucket, and the
	// Postgres pool is per-profile. processJob looks up the binding's
	// store here and uses it for every SoR/MinIO read call.
	//
	// The store is an interface (synthStore), not a concrete
	// *pgBindingView, so the read path is fakeable in unit tests exactly
	// like WriteSynth. Production wraps the resolved view in pgSynthStore.
	//
	// Returns ok=false on cache miss — the worker MUST drop the job
	// rather than fall back to shared infra (tenant-boundary safety).
	Resolve func(profile, vault string) (synthStore, AttachmentStore, bool)

	// WriteSynth persists a job's distilled result into the SoR
	// (MarkRecordSynthesised + entity upserts + re-projection enqueue,
	// transactionally). Wired from Daemon.writeSynthResult, which resolves
	// the binding from (profile, vault). Returning an error leaves the
	// record raw for a later re-enqueue.
	WriteSynth func(ctx context.Context, profile, vault, sha string, res synthResult) error

	// pdfExtractor pulls plain text out of PDF attachments at synth
	// time so they become FTS-searchable. Nil-safe — when the daemon
	// runs in an environment without poppler-utils, the field stays
	// nil and PDF attachments synth as a no-op.
	pdfExtractor PDFExtractor
	pdfAvailable bool

	// ocrAvailable is snapshotted at construction. When true the worker
	// may OCR image attachments and image-only (scanned) PDFs via
	// tesseract. Office (docx/xlsx/pptx) extraction is pure-Go and needs
	// no availability gate. Nil/false in environments without tesseract.
	ocrAvailable bool
}

type synthJob struct {
	Profile string
	Vault   string
	SHA     string
}

// SynthWorkerOpts groups construction inputs.
type SynthWorkerOpts struct {
	Logger *slog.Logger
	// BufferSize bounds the in-memory queue. 1000 is plenty for a
	// single-operator deploy; bursts that exceed it drop oldest-
	// first (TryEnqueue returns false, caller logs).
	BufferSize int
	// DisableCLI forces the worker to skip both RunGate and
	// SummarizeContent's LLM call paths. Used by tests to keep the
	// pipeline deterministic and fast (no real claude subprocess);
	// production leaves this false and probes the CLI at startup.
	DisableCLI bool
	// Capture wires raw-source archival. The per-binding AttachmentStore
	// (from Resolve) is the blob target; this carries the enable flag +
	// limits.
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
		logger:       opts.Logger,
		queue:        make(chan synthJob, opts.BufferSize),
		bufSize:      opts.BufferSize,
		stopped:      make(chan struct{}),
		cliAvailable: cli,
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

// resynthScanLimit bounds the SoR ListUnsynthesised scan. The true total
// comes from CountUnsynthesised; this caps how many rows we pull to
// process + sample in one apply.
const resynthScanLimit = 10000

// ResynthBacklog reports + (on apply) re-processes Synthesised=false
// records for (profile, vault) from the Postgres SoR. dryRun: report
// count+sample, mutate nothing. apply: spawn ONE background goroutine that
// re-processes each stuck record serialized with the live worker. limit<=0
// means all (up to resynthScanLimit); limit>0 caps how many are processed
// (count still reflects the true total).
//
// This is the fix-it apply-companion to brain_reflect (issue #82): a bulk
// ingest can outrun the single CLI-bound worker and overflow Enqueue's
// buffer, leaving records stuck at Synthesised=false. Re-pushing them
// through Enqueue would risk dropping them again; instead the backfill
// calls w.handle directly, which takes processMu and therefore can never
// run concurrently with the live worker.
func (w *SynthWorker) ResynthBacklog(ctx context.Context, profile, vault string, dryRun bool, limit int) (ResynthResult, error) {
	store, _, ok := w.resolveForJob(synthJob{Profile: profile, Vault: vault})
	if !ok {
		return ResynthResult{}, fmt.Errorf("resynth: no binding view registered for %s/%s", profile, vault)
	}

	// ListUnsynthesised returns the full backlog; its length IS the
	// count (the SoR query caps at resynthScanLimit internally), so we no
	// longer need a separate CountUnsynthesised round-trip.
	recs, err := store.ListUnsynthesised(ctx, profile, vault)
	if err != nil {
		return ResynthResult{}, fmt.Errorf("resynth: list: %w", err)
	}

	stuck := make([]synthJob, 0, len(recs))
	var sample []ResynthSampleItem
	for _, rec := range recs {
		stuck = append(stuck, synthJob{Profile: profile, Vault: vault, SHA: rec.Doc.SHA})
		if len(sample) < resynthSampleCap {
			sample = append(sample, ResynthSampleItem{SHA: rec.Doc.SHA, Title: rec.Doc.Title})
		}
	}

	res := ResynthResult{BacklogCount: len(recs), Sample: sample}
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

// resolveForJob returns the per-binding synthStore + AttachmentStore for
// a job's (profile, vault). v3.2 per-binding storage overrides: each
// (profile, vault) resolves to its own OS prefix + MinIO bucket; the PG
// pool is per-profile. The worker calls this once per job.
//
// Returns ok=false on cache miss. Callers MUST drop the job rather than
// fall back to shared infra — synthesising into the wrong tenant's store
// is a worse failure than dropping the job and waiting for a re-enqueue.
func (w *SynthWorker) resolveForJob(job synthJob) (synthStore, AttachmentStore, bool) {
	if w.Resolve == nil {
		return nil, nil, false
	}
	return w.Resolve(job.Profile, job.Vault)
}

// processJob runs one (profile, vault, sha) item through the pipeline.
// Returns nil on success; non-nil errors get logged at Warn by handle —
// the record stays in raw-only state and a future re-enqueue
// (operator-driven, e.g. brain_resynth) can retry.
//
// Phase D1: reads the raw record from the Postgres SoR (GetRecordBySHA),
// maps it into an in-memory SummaryDoc so the existing pipeline logic
// (CheckCoherence / RunGate / SummarizeContent / extractEntitiesBest)
// keeps working unchanged, then persists results via WriteSynth.
func (w *SynthWorker) processJob(ctx context.Context, job synthJob) error {
	store, attach, ok := w.resolveForJob(job)
	if !ok {
		// Dropping the job is the correct response: we cannot synthesise
		// without knowing which binding the record belongs to, and
		// writing to a shared default would leak across tenants. A
		// re-enqueue (e.g. brain_resynth) retries once the binding view
		// is registered.
		w.logger.Error("phantom-brain: synth job dropped — no binding view registered",
			slog.String("profile", job.Profile),
			slog.String("vault", job.Vault),
			slog.String("sha", job.SHA))
		return nil
	}

	rec, err := store.Fetch(ctx, job.Profile, job.Vault, job.SHA)
	if err != nil {
		return err
	}
	if rec == nil {
		// Delete race / unknown SHA — nothing to synthesise.
		return nil
	}
	if rec.Synthesised {
		// Idempotent: re-enqueueing an already-synthed record is fine but
		// we skip the wasted work.
		return nil
	}

	doc := rec.Doc

	// Attachment records (kind "attachment" → KindAttachmentStub): before
	// gate/distill, attempt text extraction (PDF/OCR/office) and fold it
	// into the RawBody so the downstream distill pass sees real content
	// rather than just the caller's description. Non-fatal — a failure
	// leaves the description-only RawBody, which is still recall-visible.
	if doc.Kind == osearch.KindAttachmentStub {
		if err := w.enrichAttachmentRecord(ctx, job, &doc, rec, store, attach); err != nil {
			w.logger.Warn("phantom-brain: attachment enrichment failed (non-fatal)",
				slog.String("sha", job.SHA), slog.String("err", err.Error()))
		}
	}

	content := doc.RawBody
	if content == "" {
		content = doc.Body
	}

	// Raw-source capture (v2.4+): when capture is wired and the doc has a
	// URL, fetch the page bytes and stash them in MinIO. Best-effort —
	// fetch failures are logged and DON'T block gate/distill.
	//
	// Phase D1 limitation: the resulting CaptureMinIOKey has no SoR column,
	// so we log it and drop it. The bytes are in MinIO but unreachable via
	// handleCaptureGet until the SoR grows a capture_minio_key column.
	if attach != nil && w.capture.Enabled && doc.SourceURL != "" {
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
			w.logger.Debug("phantom-brain: capture stored in MinIO (key dropped — no SoR column yet)",
				slog.String("sha", job.SHA),
				slog.String("capture_key", res.Key))
		}
	}

	// Coherence first — free and rejects obviously-broken input before
	// paying for the LLM.
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
			SourceType: gateSourceType(&doc),
		})
	}

	// Distill. If the CLI is unavailable or fails we fall back to the raw
	// content so the record still becomes searchable as a summary.
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

	// Extract entities from RAW content. LLM-driven when claude is
	// available; falls back to the regex extractor otherwise.
	entities := extractEntitiesBest(ctx, doc.Title, content, w.cliAvailable, w.logger)
	// entityNames maps slug → display name for the SoR entity upserts.
	entityNames := make(map[string]string, len(entities))
	for _, ent := range entities {
		slug := osearch.EntitySlug(ent)
		if slug == "" {
			continue
		}
		entityNames[slug] = ent
	}

	if w.WriteSynth == nil {
		return errors.New("synth: WriteSynth not wired")
	}
	// Embedding carried here is the record's agent-computed vector (the
	// daemon does not recompute) — empty for records ingested without one.
	return w.WriteSynth(ctx, job.Profile, job.Vault, job.SHA, synthResult{
		Body:           summary,
		Reliability:    string(verdict.Reliability),
		Topic:          string(verdict.Topic),
		GateReason:     verdict.Reason,
		Embedding:      doc.Embedding,
		EmbeddingModel: synthEmbeddingModel,
		EntityNames:    entityNames,
	})
}

// synthEmbeddingModel labels the agent-computed embedding the daemon
// passes through. Phase 6 standardises on nomic-embed-text (768-dim); the
// SoR records the model name so a future re-embed can detect drift.
const synthEmbeddingModel = "nomic-embed-text"

// gateSourceType maps the SummaryDoc shape onto the GateOpts SourceType
// field. Curated docs are stamped by handleLearn with reliability=medium +
// a "curated" reason; everything else is gathered.
func gateSourceType(doc *osearch.SummaryDoc) string {
	if doc.Reliability == osearch.ReliabilityMedium && strings.HasPrefix(doc.GateReason, "curated") {
		return "curated"
	}
	return "gathered"
}

// extractAttachmentText dispatches on the attachment's MIME type (with a
// filename-extension fallback for the empty/octet-stream case) and runs
// the matching extractor. Every branch is availability-gated and
// soft-fail: a fetch error, a missing tool, or an extractor failure logs
// at Warn and returns "" so the caller leaves ExtractedText empty.
//
// Dispatch table:
//
//	application/pdf              → pdftotext; empty result + OCR available
//	                               → OCRExtractScannedPDF (scanned PDF)
//	image/{png,jpeg,gif,bmp,     → OCRExtractImage (tesseract)
//	       tiff,webp}
//	docx/xlsx/pptx OOXML mimes   → OfficeExtract (pure-Go, always on)
//	application/msword, …        → OfficeExtract → unsupported-legacy log
func (w *SynthWorker) extractAttachmentText(ctx context.Context, job synthJob, att *osearch.AttachmentDoc, attach AttachmentStore) string {
	mime := att.MIMEType
	ext := strings.ToLower(filepath.Ext(att.OriginalFilename))

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

// enrichAttachmentRecord folds an attachment record's extracted text into
// the in-memory SummaryDoc's RawBody so the existing gate + distill tail
// handles it like any other summary. Runs the MIME-dispatch extractor on
// demand when the record carries no extracted text yet, and persists newly
// extracted text back to the SoR via synthStore.SetExtractedText.
//
// Phase D1: the attachment IS the same record (same SHA) in the SoR — there
// is no separate AttachmentDoc, and no SummarySHA cross-link concept. The
// attachment metadata travels on the synthRecord (MIMEType / MinIOKey /
// OriginalFilename / ExtractedText) so this path needs no pgx types.
//
// Mutates `doc` in place — caller continues with the standard pipeline
// using the updated RawBody.
func (w *SynthWorker) enrichAttachmentRecord(ctx context.Context, job synthJob, doc *osearch.SummaryDoc, rec *synthRecord, store synthStore, attach AttachmentStore) error {
	// Build the AttachmentDoc shape the extractor dispatch expects from
	// the synthRecord's attachment fields. Description carries the
	// curator intent (the record's RawBody before enrichment).
	att := osearch.AttachmentDoc{
		Profile:          doc.Profile,
		Vault:            doc.Vault,
		SHA:              doc.SHA,
		OriginalFilename: rec.OriginalFilename,
		Title:            doc.Title,
		MIMEType:         rec.MIMEType,
		MinIOKey:         rec.MinIOKey,
		Description:      doc.RawBody,
		ExtractedText:    rec.ExtractedText,
	}

	// Extract searchable text when the record carries none yet and its
	// type is one we can handle (PDF, image via OCR, OOXML office).
	// Failures here are soft — the doc falls back to description-only.
	if att.ExtractedText == "" && attach != nil {
		if text := w.extractAttachmentText(ctx, job, &att, attach); text != "" {
			att.ExtractedText = text
			if err := store.SetExtractedText(ctx, rec.RecordID, text); err != nil {
				return err
			}
		}
	}

	// Compose the RawBody. Description first (curator intent), extracted
	// text after (machine signal). Either alone is fine.
	desc := strings.TrimSpace(att.Description)
	extracted := strings.TrimSpace(att.ExtractedText)
	switch {
	case desc != "" && extracted != "":
		doc.RawBody = desc + "\n\n---\n\n" + extracted
	case extracted != "":
		doc.RawBody = extracted
	case desc != "":
		doc.RawBody = desc
		// else: leave whatever the record already carried (likely empty).
	}
	return nil
}
