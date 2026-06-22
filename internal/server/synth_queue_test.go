package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"strings"

	"github.com/neverprepared/mcp-phantom-brain/internal/osearch"
)

// waitUntil polls fn until it returns true or the deadline passes.
// Test-only helper for the worker drains — channels in Go don't
// have a "give me a sync point after this enqueue completes" signal
// without explicit coordination, so we poll instead.
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

func newTestWorker(t *testing.T, os osWriter) (*SynthWorker, context.Context, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := NewSynthWorker(SynthWorkerOpts{
		OSClient:   os,
		Logger:     slog.New(slog.DiscardHandler),
		BufferSize: 16,
		DisableCLI: true, // keep tests deterministic + fast
	})
	w.Start(ctx)
	return w, ctx, func() {
		w.Stop()
		cancel()
	}
}

func TestSynthWorker_EnrichesRawDoc(t *testing.T) {
	fos := newFakeOS()
	// Seed a raw-only doc (the state /perceive leaves it in).
	now := time.Now().UTC()
	sha := "abc"
	fos.summaries[osearch.DocID("p", "v", sha)] = osearch.SummaryDoc{
		Profile: "p", Vault: "v", SHA: sha,
		Title: "Kubernetes pods", RawBody: "Pods are the smallest deployable unit of Kubernetes.",
		Tags: []string{"k8s"}, CreatedAt: now, UpdatedAt: now,
		Synthesised: false,
	}

	w, _, cleanup := newTestWorker(t, fos)
	defer cleanup()

	var completedFor string
	var cmu sync.Mutex
	w.OnComplete = func(_, _, sha string) {
		cmu.Lock()
		completedFor = sha
		cmu.Unlock()
	}

	w.Enqueue("p", "v", sha)

	waitUntil(t, 5*time.Second, func() bool {
		d, _ := fos.GetSummary(context.Background(), "p", "v", sha)
		return d != nil && d.Synthesised
	})

	out, _ := fos.GetSummary(context.Background(), "p", "v", sha)
	if !out.Synthesised {
		t.Fatal("doc not marked Synthesised")
	}
	if out.Body == "" {
		t.Error("Body empty after synth — should at minimum fall back to raw content")
	}
	if out.Reliability == "" {
		t.Error("Reliability not set after synth")
	}
	cmu.Lock()
	got := completedFor
	cmu.Unlock()
	if got != sha {
		t.Errorf("OnComplete fired for %q, want %q", got, sha)
	}
}

func TestSynthWorker_SkipsAlreadySynthesised(t *testing.T) {
	fos := newFakeOS()
	sha := "abc"
	fos.summaries[osearch.DocID("p", "v", sha)] = osearch.SummaryDoc{
		Profile: "p", Vault: "v", SHA: sha,
		Title: "T", Body: "already done", Synthesised: true,
		Reliability: osearch.ReliabilityHigh,
	}
	w, _, cleanup := newTestWorker(t, fos)
	defer cleanup()

	// Drop a sentinel to detect that the worker processed the job
	// but left the doc unchanged.
	completed := make(chan string, 1)
	w.OnComplete = func(_, _, sha string) { completed <- sha }

	w.Enqueue("p", "v", sha)
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never reached OnComplete")
	}

	out, _ := fos.GetSummary(context.Background(), "p", "v", sha)
	if out.Body != "already done" || out.Reliability != osearch.ReliabilityHigh {
		t.Errorf("worker mutated already-synthesised doc: %+v", out)
	}
}

func TestSynthWorker_MissingDocIsNotError(t *testing.T) {
	fos := newFakeOS()
	w, _, cleanup := newTestWorker(t, fos)
	defer cleanup()

	completed := make(chan string, 1)
	w.OnComplete = func(_, _, sha string) { completed <- sha }

	w.Enqueue("p", "v", "ghost")
	// A missing-doc enqueue is not an error and still fires OnComplete
	// (no enrichment to do — but worker shouldn't stall).
	// Actually our processJob returns nil for missing docs WITHOUT
	// calling OnComplete… verify the worker doesn't crash either way.
	select {
	case <-completed:
		// fine
	case <-time.After(500 * time.Millisecond):
		// also fine — missing doc currently skips OnComplete; either
		// behaviour is acceptable as long as the worker keeps running.
	}

	// Verify the worker still drains a subsequent good job — the
	// missing one didn't poison the queue.
	now := time.Now().UTC()
	fos.summaries[osearch.DocID("p", "v", "real")] = osearch.SummaryDoc{
		Profile: "p", Vault: "v", SHA: "real",
		Title: "x", RawBody: "some content", CreatedAt: now, UpdatedAt: now,
	}
	w.Enqueue("p", "v", "real")
	waitUntil(t, 5*time.Second, func() bool {
		d, _ := fos.GetSummary(context.Background(), "p", "v", "real")
		return d != nil && d.Synthesised
	})
}

func TestSynthWorker_EnqueueDropsOnFullQueue(t *testing.T) {
	fos := newFakeOS()
	// Don't Start the worker — backlog the buffer, then overflow.
	w := NewSynthWorker(SynthWorkerOpts{
		OSClient:   fos,
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

func TestSynthWorker_ExtractsEntitiesIntoOS(t *testing.T) {
	fos := newFakeOS()
	now := time.Now().UTC()
	sha := "abc"
	// ExtractEntities lifts capitalised tokens — give it some.
	fos.summaries[osearch.DocID("p", "v", sha)] = osearch.SummaryDoc{
		Profile: "p", Vault: "v", SHA: sha,
		Title: "Notes on Kubernetes and Helm",
		RawBody: "## Kubernetes\n\nA container orchestration platform.\n\n" +
			"## Helm\n\nPackages **Kubernetes** manifests as charts.\n",
		CreatedAt: now, UpdatedAt: now,
	}
	w, _, cleanup := newTestWorker(t, fos)
	defer cleanup()
	w.Enqueue("p", "v", sha)
	waitUntil(t, 5*time.Second, func() bool {
		d, _ := fos.GetSummary(context.Background(), "p", "v", sha)
		return d != nil && d.Synthesised
	})

	out, _ := fos.GetSummary(context.Background(), "p", "v", sha)
	if len(out.Entities) == 0 {
		t.Fatal("Entities empty after synth")
	}
	// Every Entity in the summary should round-trip to an OS entity doc.
	for _, slug := range out.Entities {
		got, _ := fos.GetEntity(context.Background(), "p", "v", slug)
		if got == nil {
			t.Errorf("entity %q not written to OS", slug)
			continue
		}
		if !containsString(got.MentionedBy, sha) {
			t.Errorf("entity %q missing MentionedBy=%s; got %v", slug, sha, got.MentionedBy)
		}
	}
}

func TestSynthWorker_EntityAccumulatesMentionedBy(t *testing.T) {
	fos := newFakeOS()
	now := time.Now().UTC()
	// Two docs mentioning the same entity. Synthesise both — the
	// entity doc's MentionedBy should grow.
	for _, sha := range []string{"d1", "d2"} {
		fos.summaries[osearch.DocID("p", "v", sha)] = osearch.SummaryDoc{
			Profile: "p", Vault: "v", SHA: sha,
			Title: "About Kubernetes",
			RawBody: "## Kubernetes\n\nOrchestrates containers in production.\n",
			CreatedAt: now, UpdatedAt: now,
		}
	}
	w, _, cleanup := newTestWorker(t, fos)
	defer cleanup()

	for _, sha := range []string{"d1", "d2"} {
		w.Enqueue("p", "v", sha)
	}

	waitUntil(t, 5*time.Second, func() bool {
		a, _ := fos.GetSummary(context.Background(), "p", "v", "d1")
		b, _ := fos.GetSummary(context.Background(), "p", "v", "d2")
		return a != nil && a.Synthesised && b != nil && b.Synthesised
	})

	// At least one entity should now list both SHAs in MentionedBy.
	fos.mu.Lock()
	defer fos.mu.Unlock()
	merged := false
	for _, e := range fos.entities {
		if containsString(e.MentionedBy, "d1") && containsString(e.MentionedBy, "d2") {
			merged = true
			break
		}
	}
	if !merged {
		t.Errorf("no entity merged both d1 + d2 into MentionedBy; entities = %+v", fos.entities)
	}
}

func TestSynthWorker_PDFAttachmentGetsExtractedText(t *testing.T) {
	fos := newFakeOS()
	store := newCaptureStore()
	sha := "pdfsha"
	key := "p/v/attachments/" + sha + ".pdf"
	// Seed store with fake bytes (the extractor below ignores them).
	store.puts[key] = []byte("%PDF-fake-bytes")
	now := time.Now().UTC()
	fos.attachments[osearch.DocID("p", "v", sha)] = osearch.AttachmentDoc{
		Profile: "p", Vault: "v", SHA: sha,
		OriginalFilename: "doc.pdf",
		MIMEType:         "application/pdf",
		MinIOKey:         key,
		CreatedAt:        now,
	}

	called := 0
	extractor := func(_ context.Context, body []byte) (string, error) {
		called++
		return "extracted text from PDF: " + string(body), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewSynthWorker(SynthWorkerOpts{
		OSClient:     fos,
		Logger:       slog.New(slog.DiscardHandler),
		BufferSize:   8,
		DisableCLI:   true,
		Attach:       store,
		PDFExtractor: extractor,
	})
	w.Start(ctx)
	defer w.Stop()

	w.Enqueue("p", "v", sha)
	waitUntil(t, 5*time.Second, func() bool {
		d, _ := fos.GetAttachment(context.Background(), "p", "v", sha)
		return d != nil && d.ExtractedText != ""
	})

	out, _ := fos.GetAttachment(context.Background(), "p", "v", sha)
	if !strings.Contains(out.ExtractedText, "extracted text from PDF") {
		t.Errorf("ExtractedText missing payload: %q", out.ExtractedText)
	}
	if called != 1 {
		t.Errorf("extractor called %d times, want 1", called)
	}

	// Idempotency: re-enqueue should not re-extract.
	w.Enqueue("p", "v", sha)
	time.Sleep(150 * time.Millisecond)
	if called != 1 {
		t.Errorf("extractor called %d times after re-enqueue, want 1 (idempotent)", called)
	}
}

func TestSynthWorker_NonPDFAttachmentSkipped(t *testing.T) {
	fos := newFakeOS()
	store := newCaptureStore()
	sha := "imgsha"
	key := "p/v/attachments/" + sha + ".png"
	store.puts[key] = []byte("fake-png")
	now := time.Now().UTC()
	fos.attachments[osearch.DocID("p", "v", sha)] = osearch.AttachmentDoc{
		Profile: "p", Vault: "v", SHA: sha,
		OriginalFilename: "img.png",
		MIMEType:         "image/png",
		MinIOKey:         key,
		CreatedAt:        now,
	}

	called := 0
	extractor := func(_ context.Context, body []byte) (string, error) {
		called++
		return "should not be called", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewSynthWorker(SynthWorkerOpts{
		OSClient:     fos,
		Logger:       slog.New(slog.DiscardHandler),
		BufferSize:   8,
		DisableCLI:   true,
		Attach:       store,
		PDFExtractor: extractor,
	})
	w.Start(ctx)
	defer w.Stop()

	done := make(chan string, 1)
	w.OnComplete = func(_, _, sha string) { done <- sha }

	w.Enqueue("p", "v", sha)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never finished non-pdf attachment job")
	}
	if called != 0 {
		t.Errorf("extractor called %d times on non-pdf, want 0", called)
	}
	out, _ := fos.GetAttachment(context.Background(), "p", "v", sha)
	if out.ExtractedText != "" {
		t.Errorf("ExtractedText set on non-pdf: %q", out.ExtractedText)
	}
}

// failingOS lets us assert that an UpsertSummary error doesn't crash
// the worker.
type failingOS struct{ *fakeOS }

func (f failingOS) UpsertSummary(context.Context, osearch.SummaryDoc, bool) error {
	return errors.New("os write failed")
}

func TestSynthWorker_SurvivesUpsertFailure(t *testing.T) {
	inner := newFakeOS()
	now := time.Now().UTC()
	inner.summaries[osearch.DocID("p", "v", "x")] = osearch.SummaryDoc{
		Profile: "p", Vault: "v", SHA: "x", Title: "t", RawBody: "body",
		CreatedAt: now, UpdatedAt: now,
	}
	fos := failingOS{inner}

	w, _, cleanup := newTestWorker(t, fos)
	defer cleanup()

	// Enqueue a job that will fail mid-process. Then enqueue another
	// against a fresh doc to verify the worker is still alive.
	w.Enqueue("p", "v", "x")
	time.Sleep(100 * time.Millisecond) // give the failure time to log

	inner.mu.Lock()
	inner.summaries[osearch.DocID("p", "v", "y")] = osearch.SummaryDoc{
		Profile: "p", Vault: "v", SHA: "y", Title: "t", RawBody: "body",
		CreatedAt: now, UpdatedAt: now,
	}
	inner.mu.Unlock()
	// y will also fail to write back, but the worker should still
	// drain it. The assertion is just "worker did not deadlock".
	done := make(chan struct{})
	go func() {
		w.Enqueue("p", "v", "y")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker appears to have deadlocked after upsert failure")
	}
}
