package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// Phase D1 testability seam: the synth worker reads through synthStore
// (fakeable) and writes through WriteSynth (fakeable), so its full
// behaviour — read raw → gate/distill → extract → write back — is covered
// here against in-memory fakes, no live Postgres. The PG-backed adapter
// (pgSynthStore) is exercised by the integration suite
// (dual_write_integration_test.go).

// waitUntil polls fn until it returns true or the deadline passes.
// Test-only helper for the worker drains — channels in Go don't have a
// "give me a sync point after this enqueue completes" signal without
// explicit coordination, so we poll instead.
func waitUntil(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitUntil: condition still false after %s", d)
}

// fakeSynthStore is the in-memory synthStore the worker reads through.
// recs is keyed by sha → synthRecord. It records SetExtractedText calls
// (extracted) and supports a forced Fetch error (fetchErr) + listErr for
// the resynth list-error path.
type fakeSynthStore struct {
	mu        sync.Mutex
	recs      map[string]*synthRecord
	extracted map[int64]string // recordID → text set via SetExtractedText
	setCalls  int
	fetchErr  error
	listErr   error
}

func newFakeSynthStore() *fakeSynthStore {
	return &fakeSynthStore{
		recs:      map[string]*synthRecord{},
		extracted: map[int64]string{},
	}
}

func (f *fakeSynthStore) put(rec synthRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := rec
	f.recs[rec.Doc.SHA] = &cp
}

func (f *fakeSynthStore) Fetch(_ context.Context, _, _, sha string) (*synthRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	r, ok := f.recs[sha]
	if !ok {
		return nil, nil // delete race / unknown SHA, not an error
	}
	cp := *r
	return &cp, nil
}

func (f *fakeSynthStore) SetExtractedText(_ context.Context, recordID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	f.extracted[recordID] = text
	// Mirror onto the stored record so a re-enqueue sees the persisted
	// text (idempotency: an already-extracted record won't re-extract).
	for _, r := range f.recs {
		if r.RecordID == recordID {
			r.ExtractedText = text
		}
	}
	return nil
}

func (f *fakeSynthStore) ListUnsynthesised(_ context.Context, profile, vault string) ([]synthRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := []synthRecord{}
	for _, r := range f.recs {
		if r.Doc.Profile == profile && r.Doc.Vault == vault && !r.Synthesised {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeSynthStore) setCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setCalls
}

func (f *fakeSynthStore) extractedFor(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.extracted[id]
}

// synthCapture records the synthResults the worker hands to WriteSynth,
// keyed by sha, so a test can assert on body / reliability / topic /
// EntityNames. writeErr (when set) makes WriteSynth fail, exercising the
// error-propagation path.
type synthCapture struct {
	mu       sync.Mutex
	results  map[string]synthResult
	writeErr error
}

func newSynthCapture() *synthCapture {
	return &synthCapture{results: map[string]synthResult{}}
}

func (c *synthCapture) writeSynth(_ context.Context, _, _, sha string, res synthResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeErr != nil {
		return c.writeErr
	}
	c.results[sha] = res
	return nil
}

func (c *synthCapture) result(sha string) (synthResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.results[sha]
	return r, ok
}

// noteRecord seeds a raw (unsynthesised) note record (the state perceive /
// learn leaves it in).
func noteRecord(profile, vault, sha, title, body string, id int64) synthRecord {
	now := time.Now().UTC()
	return synthRecord{
		Doc: osearch.SummaryDoc{
			Profile: profile, Vault: vault, SHA: sha,
			Kind: osearch.KindNote, Title: title, RawBody: body,
			Tags: []string{"k8s"}, CreatedAt: now, UpdatedAt: now,
			Synthesised: false,
		},
		RecordID: id,
	}
}

// newWorkerWithFakes wires a started worker against an in-memory store +
// attachment store + WriteSynth capture. DisableCLI keeps the pipeline
// deterministic (regex entity extraction, raw-content distill fallback).
func newWorkerWithFakes(t *testing.T, store synthStore, attach AttachmentStore, cap *synthCapture, extractor PDFExtractor) (*SynthWorker, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := NewSynthWorker(SynthWorkerOpts{
		Logger:       slog.New(slog.DiscardHandler),
		BufferSize:   16,
		DisableCLI:   true,
		PDFExtractor: extractor,
	})
	w.Resolve = func(string, string) (synthStore, AttachmentStore, bool) {
		return store, attach, true
	}
	w.WriteSynth = cap.writeSynth
	w.Start(ctx)
	return w, func() {
		w.Stop()
		cancel()
	}
}

// TestSynthWorker_SweeperDrainsBacklogWithoutEnqueue is the C1 durability
// proof: a record left Synthesised=false in the SoR (simulating a synth
// job the lossy Enqueue fast path dropped) is picked up and synthesised by
// the background sweeper ALONE — no Enqueue is ever called. This is what
// makes a dropped fast-path enqueue harmless. Run under -race it also
// exercises the sweeper ↔ (idle) live-loop concurrency through processMu.
func TestSynthWorker_SweeperDrainsBacklogWithoutEnqueue(t *testing.T) {
	store := newFakeSynthStore()
	sha := "sweep-me"
	store.put(noteRecord("p", "v", sha, "Sweeper picks this up",
		"This record was never Enqueued; only the sweeper sees it.", 1))
	cap := newSynthCapture()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewSynthWorker(SynthWorkerOpts{
		Logger:        slog.New(slog.DiscardHandler),
		BufferSize:    16,
		DisableCLI:    true,
		SweepInterval: 20 * time.Millisecond,
	})
	w.Resolve = func(string, string) (synthStore, AttachmentStore, bool) {
		return store, nil, true
	}
	w.WriteSynth = cap.writeSynth
	w.Bindings = func() []VaultKey { return []VaultKey{{Profile: "p", Vault: "v"}} }
	w.Start(ctx)
	defer w.Stop()

	// Deliberately NO w.Enqueue(...) — the sweeper is the only path that can
	// reach this record.
	waitUntil(t, 5*time.Second, func() bool { _, ok := cap.result(sha); return ok })

	res, _ := cap.result(sha)
	if res.Body == "" {
		t.Error("sweeper-synthesised record has empty body")
	}
	if res.Reliability == "" {
		t.Error("sweeper-synthesised record missing reliability")
	}
}

// TestSynthWorker_SweeperNilBindingsIsNoop confirms the nil-safe contract:
// a worker without Bindings wired (the unit-test default) never sweeps, so
// pre-C1 tests are unaffected.
func TestSynthWorker_SweeperNilBindingsIsNoop(t *testing.T) {
	store := newFakeSynthStore()
	store.put(noteRecord("p", "v", "x", "t", "b", 1))
	cap := newSynthCapture()

	_, cleanup := newWorkerWithFakes(t, store, nil, cap, nil)
	defer cleanup()

	// Bindings is nil ⇒ sweeper is a no-op. Give it time to (not) run.
	time.Sleep(80 * time.Millisecond)
	if _, ok := cap.result("x"); ok {
		t.Error("sweeper ran despite nil Bindings — should be a no-op")
	}
}

func TestSynthWorker_EnrichesRawDoc(t *testing.T) {
	store := newFakeSynthStore()
	sha := "abc"
	store.put(noteRecord("p", "v", sha, "Kubernetes pods",
		"Pods are the smallest deployable unit of Kubernetes.", 1))
	cap := newSynthCapture()

	w, cleanup := newWorkerWithFakes(t, store, nil, cap, nil)
	defer cleanup()

	// Phase D2b: the worker's OnComplete hook (snapshot rebuild trigger)
	// was removed; WriteSynth landing in the capture is the completion
	// signal now.
	w.Enqueue("p", "v", sha)
	waitUntil(t, 5*time.Second, func() bool { _, ok := cap.result(sha); return ok })

	res, _ := cap.result(sha)
	if res.Body == "" {
		t.Error("Body empty after synth — should at minimum fall back to raw content")
	}
	if res.Reliability == "" {
		t.Error("Reliability not set after synth")
	}
	if res.Topic == "" {
		t.Error("Topic not set after synth")
	}
}

func TestSynthWorker_SkipsAlreadySynthesised(t *testing.T) {
	store := newFakeSynthStore()
	sha := "abc"
	rec := noteRecord("p", "v", sha, "T", "already done", 1)
	rec.Synthesised = true
	rec.Doc.Synthesised = true
	store.put(rec)
	cap := newSynthCapture()

	w, cleanup := newWorkerWithFakes(t, store, nil, cap, nil)
	defer cleanup()

	// Phase D2b: with OnComplete gone, drive a sentinel job through the
	// single FIFO worker. When the sentinel's WriteSynth lands, the
	// already-synthesised job ahead of it has been processed (and skipped).
	store.put(noteRecord("p", "v", "sentinel", "x", "fresh content", 2))
	w.Enqueue("p", "v", sha)
	w.Enqueue("p", "v", "sentinel")
	waitUntil(t, 5*time.Second, func() bool { _, ok := cap.result("sentinel"); return ok })

	// The worker skips the wasted gate/distill: WriteSynth must NOT fire
	// for an already-synthesised record.
	if _, ok := cap.result(sha); ok {
		t.Error("worker re-synthesised an already-synthesised record")
	}
}

func TestSynthWorker_MissingDocIsNotError(t *testing.T) {
	store := newFakeSynthStore()
	cap := newSynthCapture()

	w, cleanup := newWorkerWithFakes(t, store, nil, cap, nil)
	defer cleanup()

	// A missing-record enqueue is not an error; processJob returns nil
	// WITHOUT calling WriteSynth. Phase D2b: OnComplete is gone, so we
	// verify the worker still drains a subsequent good job — when the
	// "real" job's WriteSynth lands, the ghost ahead of it was processed
	// without poisoning the queue.
	store.put(noteRecord("p", "v", "real", "x", "some content", 2))
	w.Enqueue("p", "v", "ghost")
	w.Enqueue("p", "v", "real")
	waitUntil(t, 5*time.Second, func() bool { _, ok := cap.result("real"); return ok })

	if _, ok := cap.result("ghost"); ok {
		t.Error("WriteSynth fired for a missing record")
	}
}

func TestSynthWorker_EnqueueDropsOnFullQueue(t *testing.T) {
	// Don't Start the worker — backlog the buffer, then overflow.
	w := NewSynthWorker(SynthWorkerOpts{
		Logger:     slog.New(slog.DiscardHandler),
		BufferSize: 2,
	})
	// Push 5; the buffer holds 2, the rest get dropped.
	for i := 0; i < 5; i++ {
		w.Enqueue("p", "v", "sha")
	}
	if got := len(w.queue); got != 2 {
		t.Errorf("queue depth = %d, want 2 (overflow should drop)", got)
	}
}

func TestSynthWorker_ExtractsEntitiesIntoWriteSynth(t *testing.T) {
	store := newFakeSynthStore()
	sha := "abc"
	// ExtractEntities lifts capitalised tokens — give it some.
	store.put(noteRecord("p", "v", sha, "Notes on Kubernetes and Helm",
		"## Kubernetes\n\nA container orchestration platform.\n\n"+
			"## Helm\n\nPackages **Kubernetes** manifests as charts.\n", 1))
	cap := newSynthCapture()

	w, cleanup := newWorkerWithFakes(t, store, nil, cap, nil)
	defer cleanup()
	w.Enqueue("p", "v", sha)
	waitUntil(t, 5*time.Second, func() bool { _, ok := cap.result(sha); return ok })

	res, _ := cap.result(sha)
	if len(res.EntityNames) == 0 {
		t.Fatal("EntityNames empty after synth")
	}
	// Each slug must canonicalise the display name (the SoR upsert keys on
	// slug). Assert Kubernetes round-trips and no empty slugs leak.
	found := false
	for slug, name := range res.EntityNames {
		if slug == "" {
			t.Errorf("empty slug for name %q", name)
		}
		if slug == osearch.EntitySlug("Kubernetes") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a Kubernetes entity in EntityNames, got %v", res.EntityNames)
	}
}

// EntityAccumulatesMentionedBy was an OS-side assertion on the entity
// doc's MentionedBy[] accumulating across two summaries. Post-D1 that
// accumulation is RELATIONAL (record_entities links in Postgres,
// established by writeSynthResult's UpsertEntity + LinkRecordEntity), NOT
// the worker's job — the worker only hands EntityNames to WriteSynth. So
// the worker-level assertion is "both records pass the SAME entity through
// WriteSynth"; the mentioned-by relational accumulation is covered by the
// dual_write integration test (TestSoRWrite_Integration/SynthWrite asserts
// RecordsMentioningEntity).
func TestSynthWorker_PassesSameEntityForBothDocs(t *testing.T) {
	store := newFakeSynthStore()
	for i, sha := range []string{"d1", "d2"} {
		store.put(noteRecord("p", "v", sha, "About Kubernetes",
			"## Kubernetes\n\nOrchestrates containers in production.\n", int64(i+1)))
	}
	cap := newSynthCapture()

	w, cleanup := newWorkerWithFakes(t, store, nil, cap, nil)
	defer cleanup()

	for _, sha := range []string{"d1", "d2"} {
		w.Enqueue("p", "v", sha)
	}
	waitUntil(t, 5*time.Second, func() bool {
		_, a := cap.result("d1")
		_, b := cap.result("d2")
		return a && b
	})

	wantSlug := osearch.EntitySlug("Kubernetes")
	for _, sha := range []string{"d1", "d2"} {
		res, _ := cap.result(sha)
		if _, ok := res.EntityNames[wantSlug]; !ok {
			t.Errorf("record %s did not pass the Kubernetes entity to WriteSynth; got %v", sha, res.EntityNames)
		}
	}
}

// attachStub seeds a raw attachment-stub record (kind attachment_stub) with
// the binary metadata the worker's enrichment path reads off synthRecord.
func attachStub(sha, filename, mime, key, desc string, id int64) synthRecord {
	now := time.Now().UTC()
	return synthRecord{
		Doc: osearch.SummaryDoc{
			Profile: "p", Vault: "v", SHA: sha,
			Kind: osearch.KindAttachmentStub, Title: filename, RawBody: desc,
			CreatedAt: now, UpdatedAt: now, Synthesised: false,
		},
		RecordID:         id,
		MIMEType:         mime,
		OriginalFilename: filename,
		MinIOKey:         key,
	}
}

// v2.5.1 (#48): attachment stub + PDF — synth folds pdftotext output into
// the stub's RawBody so the distill pass (and recall) sees attachment
// content, and persists the extracted text back to the SoR.
func TestSynthWorker_AttachmentStub_PDFEnrichesSummary(t *testing.T) {
	store := newFakeSynthStore()
	attach := newCaptureStore()
	sha := "pdfsha"
	key := "p/v/attachments/" + sha + ".pdf"
	attach.puts[key] = []byte("%PDF-fake")
	store.put(attachStub(sha, "doc.pdf", "application/pdf", key, "kept for billing", 1))
	cap := newSynthCapture()

	extractor := func(_ context.Context, _ []byte) (string, error) {
		return "INVOICE TOTAL $42", nil
	}
	w, cleanup := newWorkerWithFakes(t, store, attach, cap, extractor)
	defer cleanup()

	w.Enqueue("p", "v", sha)
	waitUntil(t, 5*time.Second, func() bool { _, ok := cap.result(sha); return ok })

	res, _ := cap.result(sha)
	if !strings.Contains(res.Body, "kept for billing") {
		t.Errorf("Body dropped description: %q", res.Body)
	}
	if !strings.Contains(res.Body, "INVOICE TOTAL $42") {
		t.Errorf("Body missing pdftotext output: %q", res.Body)
	}
	// Extracted text persisted back to the SoR via SetExtractedText.
	if store.extractedFor(1) != "INVOICE TOTAL $42" {
		t.Errorf("SetExtractedText(recordID=1) = %q, want pdf text", store.extractedFor(1))
	}
	if store.setCallCount() != 1 {
		t.Errorf("SetExtractedText called %d times, want 1", store.setCallCount())
	}
}

// Idempotency: re-enqueueing a stub whose record already carries extracted
// text MUST NOT re-run pdftotext (or re-persist).
func TestSynthWorker_AttachmentStub_Idempotent(t *testing.T) {
	store := newFakeSynthStore()
	attach := newCaptureStore()
	sha := "pdfsha2"
	key := "p/v/attachments/" + sha + ".pdf"
	attach.puts[key] = []byte("%PDF-fake")
	store.put(attachStub(sha, "doc.pdf", "application/pdf", key, "ctx", 7))
	cap := newSynthCapture()

	called := 0
	var cmu sync.Mutex
	extractor := func(_ context.Context, _ []byte) (string, error) {
		cmu.Lock()
		called++
		cmu.Unlock()
		return "extracted", nil
	}
	w, cleanup := newWorkerWithFakes(t, store, attach, cap, extractor)
	defer cleanup()

	w.Enqueue("p", "v", sha)
	waitUntil(t, 5*time.Second, func() bool { _, ok := cap.result(sha); return ok })
	// SetExtractedText mirrored the text onto the stored record; the record
	// is still Synthesised=false in the fake (WriteSynth, which would flip
	// it, is faked out). Re-enqueue must still skip re-extraction because
	// ExtractedText is now non-empty.
	w.Enqueue("p", "v", sha)
	time.Sleep(150 * time.Millisecond)
	cmu.Lock()
	got := called
	cmu.Unlock()
	if got != 1 {
		t.Errorf("extractor called %d times on re-enqueue, want 1 (idempotent via persisted ExtractedText)", got)
	}
}

// Non-PDF stub: synth still completes; the extractor never runs and the
// RawBody stays description-only.
func TestSynthWorker_AttachmentStub_NonPDF(t *testing.T) {
	store := newFakeSynthStore()
	attach := newCaptureStore()
	sha := "imgsha"
	key := "p/v/attachments/" + sha + ".png"
	attach.puts[key] = []byte("fake-png")
	store.put(attachStub(sha, "img.png", "image/png", key, "screenshot", 1))
	cap := newSynthCapture()

	called := 0
	extractor := func(_ context.Context, _ []byte) (string, error) {
		called++
		return "no", nil
	}
	w, cleanup := newWorkerWithFakes(t, store, attach, cap, extractor)
	defer cleanup()

	w.Enqueue("p", "v", sha)
	waitUntil(t, 5*time.Second, func() bool { _, ok := cap.result(sha); return ok })

	if called != 0 {
		t.Errorf("PDF extractor called %d times on non-pdf stub, want 0", called)
	}
	if store.setCallCount() != 0 {
		t.Errorf("SetExtractedText called %d times on non-pdf, want 0", store.setCallCount())
	}
	res, _ := cap.result(sha)
	if !strings.Contains(res.Body, "screenshot") {
		t.Errorf("non-pdf stub lost description: %q", res.Body)
	}
}

func TestSynthWorker_SurvivesWriteSynthFailure(t *testing.T) {
	store := newFakeSynthStore()
	store.put(noteRecord("p", "v", "x", "t", "body", 1))
	store.put(noteRecord("p", "v", "y", "t", "body", 2))
	cap := newSynthCapture()
	cap.writeErr = errors.New("synth write failed")

	w, cleanup := newWorkerWithFakes(t, store, nil, cap, nil)
	defer cleanup()

	// Enqueue a job that fails in WriteSynth. The worker logs + moves on;
	// it must not deadlock or stop draining.
	w.Enqueue("p", "v", "x")
	time.Sleep(100 * time.Millisecond) // give the failure time to log

	done := make(chan struct{})
	go func() {
		w.Enqueue("p", "v", "y")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker appears to have deadlocked after WriteSynth failure")
	}
}
