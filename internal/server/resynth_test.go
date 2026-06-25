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

// errScroller embeds fakeOS and forces ScrollSummaries to fail, so the
// scroll-error path of ResynthBacklog is exercised without a live OS.
type errScroller struct {
	*fakeOS
	err error
}

func (e errScroller) ScrollSummaries(context.Context, string, string, int, func(osearch.SummaryDoc) error) error {
	return e.err
}

// newDryRunWorker builds a worker with no Start() — a dry run needs no
// baseCtx and never spawns the backfill goroutine.
func newDryRunWorker(t *testing.T, os osWriter) *SynthWorker {
	t.Helper()
	return NewSynthWorker(SynthWorkerOpts{
		OSClient:   os,
		Logger:     slog.New(slog.DiscardHandler),
		BufferSize: 16,
		DisableCLI: true,
	})
}

func TestResynthBacklog_DryRunReportsAndMutatesNothing(t *testing.T) {
	fos := newFakeOS()
	now := time.Now().UTC()
	// 3 stuck (Synthesised=false), 1 enriched, 1 other-tenant stuck.
	for _, sha := range []string{"s1", "s2", "s3"} {
		fos.summaries[osearch.DocID("p", "v", sha)] = osearch.SummaryDoc{
			Profile: "p", Vault: "v", SHA: sha, Title: "stuck " + sha,
			RawBody: "x", CreatedAt: now, UpdatedAt: now, Synthesised: false,
		}
	}
	fos.summaries[osearch.DocID("p", "v", "ok")] = osearch.SummaryDoc{
		Profile: "p", Vault: "v", SHA: "ok", Title: "done", Synthesised: true,
	}
	fos.summaries[osearch.DocID("other", "v", "z")] = osearch.SummaryDoc{
		Profile: "other", Vault: "v", SHA: "z", Synthesised: false,
	}

	w := newDryRunWorker(t, fos)
	res, err := w.ResynthBacklog(context.Background(), fos, "p", "v", true /*dryRun*/, 0)
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
	// Nothing mutated — the stuck docs stay unsynthesised.
	for _, sha := range []string{"s1", "s2", "s3"} {
		d, _ := fos.GetSummary(context.Background(), "p", "v", sha)
		if d.Synthesised {
			t.Errorf("dry run synthesised %s", sha)
		}
	}
}

func TestResynthBacklog_SampleCappedAt20(t *testing.T) {
	fos := newFakeOS()
	for i := 0; i < 25; i++ {
		sha := fmt.Sprintf("sha-%02d", i)
		fos.summaries[osearch.DocID("p", "v", sha)] = osearch.SummaryDoc{
			Profile: "p", Vault: "v", SHA: sha, Title: "t", Synthesised: false,
		}
	}
	w := newDryRunWorker(t, fos)
	res, err := w.ResynthBacklog(context.Background(), fos, "p", "v", true, 0)
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

func TestResynthBacklog_ScrollErrorWrapped(t *testing.T) {
	sentinel := errors.New("boom")
	osc := errScroller{fakeOS: newFakeOS(), err: sentinel}
	w := newDryRunWorker(t, osc)
	_, err := w.ResynthBacklog(context.Background(), osc, "p", "v", true, 0)
	if err == nil {
		t.Fatal("expected scroll error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error should wrap the scroll error, got %v", err)
	}
}

func TestResynthBacklog_ApplyReprocessesStuckDocs(t *testing.T) {
	fos := newFakeOS()
	now := time.Now().UTC()
	for _, sha := range []string{"a1", "a2"} {
		fos.summaries[osearch.DocID("p", "v", sha)] = osearch.SummaryDoc{
			Profile: "p", Vault: "v", SHA: sha, Title: "stuck " + sha,
			RawBody: "content for " + sha, CreatedAt: now, UpdatedAt: now,
			Synthesised: false,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newDryRunWorker(t, fos)
	w.Start(ctx)
	defer w.Stop()

	res, err := w.ResynthBacklog(ctx, fos, "p", "v", false /*apply*/, 0)
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

	// The background backfill flips both docs to Synthesised over time.
	waitUntil(t, 5*time.Second, func() bool {
		a, _ := fos.GetSummary(context.Background(), "p", "v", "a1")
		b, _ := fos.GetSummary(context.Background(), "p", "v", "a2")
		return a != nil && a.Synthesised && b != nil && b.Synthesised
	})
	// Backfill flag releases when the goroutine drains.
	waitUntil(t, 2*time.Second, func() bool { return !w.backfilling.Load() })
}

func TestResynthBacklog_ApplyRespectsLimit(t *testing.T) {
	fos := newFakeOS()
	now := time.Now().UTC()
	for _, sha := range []string{"l1", "l2", "l3"} {
		fos.summaries[osearch.DocID("p", "v", sha)] = osearch.SummaryDoc{
			Profile: "p", Vault: "v", SHA: sha, Title: "t", RawBody: "c",
			CreatedAt: now, UpdatedAt: now, Synthesised: false,
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newDryRunWorker(t, fos)
	w.Start(ctx)
	defer w.Stop()

	res, err := w.ResynthBacklog(ctx, fos, "p", "v", false, 2 /*limit*/)
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
	fos := newFakeOS()
	now := time.Now().UTC()
	fos.summaries[osearch.DocID("p", "v", "g1")] = osearch.SummaryDoc{
		Profile: "p", Vault: "v", SHA: "g1", Title: "t", RawBody: "c",
		CreatedAt: now, UpdatedAt: now, Synthesised: false,
	}
	// Deterministically simulate an in-flight backfill by holding the
	// flag, then assert a second apply returns ErrResynthInProgress.
	// (Racing two real goroutines on timing would be flaky; the guard's
	// contract is exactly "flag already set → reject".)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newDryRunWorker(t, fos)
	w.Start(ctx)
	defer w.Stop()

	// Simulate an in-flight backfill.
	if !w.backfilling.CompareAndSwap(false, true) {
		t.Fatal("precondition: backfilling already set")
	}
	res, err := w.ResynthBacklog(ctx, fos, "p", "v", false, 0)
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
