package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// Phase D1: ResynthBacklog now reads the Synthesised=false backlog through
// the synthStore seam (fakeSynthStore), so the dry-run / apply / limit /
// in-progress-guard contract is covered here against an in-memory fake —
// no live Postgres. (Pre-D1 the test passed an osWriter directly; the
// worker now resolves the store via w.Resolve.)

// newResynthWorker wires a worker whose Resolve returns the given store +
// WriteSynth capture. limit/apply paths spawn the backfill goroutine, so
// Start (which sets baseCtx) is the caller's responsibility per test.
func newResynthWorker(t *testing.T, store synthStore, cap *synthCapture) *SynthWorker {
	t.Helper()
	w := NewSynthWorker(SynthWorkerOpts{
		Logger:     slog.New(slog.DiscardHandler),
		BufferSize: 16,
		DisableCLI: true,
	})
	w.Resolve = func(string, string) (synthStore, AttachmentStore, bool) {
		return store, nil, true
	}
	if cap != nil {
		w.WriteSynth = cap.writeSynth
	}
	return w
}

// stuckRecord seeds a raw (Synthesised=false) record for the backlog scan.
func stuckRecord(profile, vault, sha, title string, id int64) synthRecord {
	now := time.Now().UTC()
	return synthRecord{
		Doc: osearch.SummaryDoc{
			Profile: profile, Vault: vault, SHA: sha,
			Kind: osearch.KindNote, Title: title, RawBody: "content for " + sha,
			CreatedAt: now, UpdatedAt: now, Synthesised: false,
		},
		RecordID: id,
	}
}

func TestResynthBacklog_DryRunReportsAndMutatesNothing(t *testing.T) {
	store := newFakeSynthStore()
	// 3 stuck (Synthesised=false), 1 enriched, 1 other-tenant stuck.
	for i, sha := range []string{"s1", "s2", "s3"} {
		store.put(stuckRecord("p", "v", sha, "stuck "+sha, int64(i+1)))
	}
	done := stuckRecord("p", "v", "ok", "done", 4)
	done.Synthesised = true
	done.Doc.Synthesised = true
	store.put(done)
	store.put(stuckRecord("other", "v", "z", "other tenant", 5))

	cap := newSynthCapture()
	w := newResynthWorker(t, store, cap)
	res, err := w.ResynthBacklog(context.Background(), "p", "v", true /*dryRun*/, 0)
	if err != nil {
		t.Fatalf("ResynthBacklog dry run: %v", err)
	}
	if res.BacklogCount != 3 {
		t.Errorf("BacklogCount = %d, want 3", res.BacklogCount)
	}
	if len(res.Sample) != 3 {
		t.Errorf("Sample len = %d, want 3", len(res.Sample))
	}
	if res.Started {
		t.Error("dry run set Started=true")
	}
	if res.Pending != 0 {
		t.Errorf("dry run Pending = %d, want 0", res.Pending)
	}
	if w.backfilling.Load() {
		t.Error("dry run flipped the backfilling flag")
	}
	// Nothing mutated — the dry run never invokes WriteSynth.
	if len(cap.results) != 0 {
		t.Errorf("dry run synthesised %d records, want 0", len(cap.results))
	}
}

func TestResynthBacklog_SampleCappedAt20(t *testing.T) {
	store := newFakeSynthStore()
	for i := 0; i < 25; i++ {
		sha := fmt.Sprintf("sha-%02d", i)
		store.put(stuckRecord("p", "v", sha, "t", int64(i+1)))
	}
	w := newResynthWorker(t, store, newSynthCapture())
	res, err := w.ResynthBacklog(context.Background(), "p", "v", true, 0)
	if err != nil {
		t.Fatalf("ResynthBacklog: %v", err)
	}
	if res.BacklogCount != 25 {
		t.Errorf("BacklogCount = %d, want 25", res.BacklogCount)
	}
	if len(res.Sample) != resynthSampleCap {
		t.Errorf("Sample len = %d, want %d (capped)", len(res.Sample), resynthSampleCap)
	}
}

func TestResynthBacklog_ListErrorWrapped(t *testing.T) {
	sentinel := errors.New("boom")
	store := newFakeSynthStore()
	store.listErr = sentinel
	w := newResynthWorker(t, store, newSynthCapture())
	_, err := w.ResynthBacklog(context.Background(), "p", "v", true, 0)
	if err == nil {
		t.Fatal("expected list error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error should wrap the list error, got %v", err)
	}
}

func TestResynthBacklog_ApplyReprocessesStuckDocs(t *testing.T) {
	store := newFakeSynthStore()
	for i, sha := range []string{"a1", "a2"} {
		store.put(stuckRecord("p", "v", sha, "stuck "+sha, int64(i+1)))
	}
	cap := newSynthCapture()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newResynthWorker(t, store, cap)
	w.Start(ctx)
	defer w.Stop()

	res, err := w.ResynthBacklog(ctx, "p", "v", false /*apply*/, 0)
	if err != nil {
		t.Fatalf("ResynthBacklog apply: %v", err)
	}
	if !res.Started {
		t.Error("apply did not set Started=true")
	}
	if res.Pending != 2 {
		t.Errorf("Pending = %d, want 2", res.Pending)
	}
	if res.BacklogCount != 2 {
		t.Errorf("BacklogCount = %d, want 2", res.BacklogCount)
	}

	// The background backfill pushes both records through WriteSynth.
	waitUntil(t, 5*time.Second, func() bool {
		_, a := cap.result("a1")
		_, b := cap.result("a2")
		return a && b
	})
	// Backfill flag releases when the goroutine drains.
	waitUntil(t, 2*time.Second, func() bool { return !w.backfilling.Load() })
}

func TestResynthBacklog_ApplyRespectsLimit(t *testing.T) {
	store := newFakeSynthStore()
	for i, sha := range []string{"l1", "l2", "l3"} {
		store.put(stuckRecord("p", "v", sha, "t", int64(i+1)))
	}
	cap := newSynthCapture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newResynthWorker(t, store, cap)
	w.Start(ctx)
	defer w.Stop()

	res, err := w.ResynthBacklog(ctx, "p", "v", false, 2 /*limit*/)
	if err != nil {
		t.Fatalf("ResynthBacklog apply with limit: %v", err)
	}
	// Count reflects the true total; Pending reflects the cap.
	if res.BacklogCount != 3 {
		t.Errorf("BacklogCount = %d, want 3 (true total)", res.BacklogCount)
	}
	if res.Pending != 2 {
		t.Errorf("Pending = %d, want 2 (limited)", res.Pending)
	}
	waitUntil(t, 2*time.Second, func() bool { return !w.backfilling.Load() })
}

func TestResynthBacklog_InProgressGuard(t *testing.T) {
	store := newFakeSynthStore()
	store.put(stuckRecord("p", "v", "g1", "t", 1))

	// Deterministically simulate an in-flight backfill by holding the
	// flag, then assert a second apply returns ErrResynthInProgress.
	// (Racing two real goroutines on timing would be flaky; the guard's
	// contract is exactly "flag already set → reject".)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newResynthWorker(t, store, newSynthCapture())
	w.Start(ctx)
	defer w.Stop()

	if !w.backfilling.CompareAndSwap(false, true) {
		t.Fatal("precondition: backfilling already set")
	}
	res, err := w.ResynthBacklog(ctx, "p", "v", false, 0)
	if !errors.Is(err, ErrResynthInProgress) {
		t.Fatalf("second apply error = %v, want ErrResynthInProgress", err)
	}
	// The report fields are still populated even when the guard rejects.
	if res.BacklogCount != 1 {
		t.Errorf("BacklogCount = %d, want 1", res.BacklogCount)
	}
	if res.Started {
		t.Error("rejected apply set Started=true")
	}
	// Release the simulated in-flight backfill.
	w.backfilling.Store(false)
}
