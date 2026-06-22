package server

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/neverprepared/mcp-phantom-brain/internal/osearch"
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
	return &SynthWorker{
		os:           opts.OSClient,
		logger:       opts.Logger,
		queue:        make(chan synthJob, opts.BufferSize),
		bufSize:      opts.BufferSize,
		stopped:      make(chan struct{}),
		cliAvailable: cli,
		attach:       opts.Attach,
		capture:      opts.Capture,
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
	go w.run(ctx)
}

// Stop signals the worker to exit after its current job completes.
// Safe to call multiple times.
func (w *SynthWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stopped) })
}

func (w *SynthWorker) run(ctx context.Context) {
	w.logger.Info("phantom-brain: synth worker started",
		slog.Int("buf_size", w.bufSize),
		slog.Bool("claude_cli", w.cliAvailable),
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
			if err := w.processJob(ctx, job); err != nil {
				w.logger.Warn("phantom-brain: synth job failed",
					slog.String("vault", job.Profile+"/"+job.Vault),
					slog.String("sha", job.SHA),
					slog.String("err", err.Error()),
				)
				continue
			}
			if w.OnComplete != nil {
				w.OnComplete(job.Profile, job.Vault, job.SHA)
			}
		}
	}
}

// processJob runs one (profile, vault, sha) item through the
// pipeline. Returns nil on success; non-nil errors get logged at
// Warn — the doc stays in raw-only state and a future re-enqueue
// (operator-driven, e.g. brain_reflect) can retry.
func (w *SynthWorker) processJob(ctx context.Context, job synthJob) error {
	doc, err := w.os.GetSummary(ctx, job.Profile, job.Vault, job.SHA)
	if err != nil {
		return err
	}
	if doc == nil {
		// Race with a delete or the synth ran before the upsert
		// committed (eventually-consistent OS refresh). Not an error.
		return nil
	}
	if doc.Synthesised {
		// Idempotent: re-enqueueing an already-synthed doc is fine
		// and may even be useful (re-rate after a gate model upgrade),
		// but for Day 5's "first cut" we skip the wasted work.
		return nil
	}

	content := doc.RawBody
	if content == "" {
		content = doc.Body // shouldn't happen for handler-fed docs but defensible
	}

	// Raw-source capture (v2.4+): when capture is wired and the doc
	// has a URL, fetch the page bytes and stash them in MinIO. Best-
	// effort — fetch failures (URL unreachable, oversize, non-2xx)
	// are logged and DON'T block gate/distill/index writes.
	if w.attach != nil && w.capture.Enabled && doc.SourceURL != "" && doc.CaptureMinIOKey == "" {
		ua := w.capture.UserAgent
		timeout := time.Duration(w.capture.TimeoutSecs) * time.Second
		res, cerr := CaptureURL(ctx, w.attach, job.Profile, job.Vault, job.SHA,
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
		slug, err := w.upsertEntity(ctx, job, doc, ent, content, verdict)
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
	return w.os.UpsertSummary(ctx, *doc, false)
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
func (w *SynthWorker) upsertEntity(ctx context.Context, job synthJob, src *osearch.SummaryDoc, name, body string, v GateVerdict) (string, error) {
	slug := osearch.EntitySlug(name)
	if slug == "" {
		return "", nil
	}
	now := time.Now().UTC()
	existing, err := w.os.GetEntity(ctx, job.Profile, job.Vault, slug)
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
	if err := w.os.UpsertEntity(ctx, doc, false); err != nil {
		return "", err
	}
	return slug, nil
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
