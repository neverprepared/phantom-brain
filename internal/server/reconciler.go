package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/osproject"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// The projection reconciler (design-review item #5) is the drift-repair
// backstop for the pb_records OpenSearch projection, the mirror image of the
// SynthWorker's SoR-backed durability sweeper. River projects each written
// record at-least-once, but a project or delete job that exhausts its retries
// leaves the projection out of sync with the Postgres SoR in one of two ways:
//
//   - Missing (SoR→OS): a SoR record whose doc never landed in pb_records. Its
//     recall hits vanish. Repair: re-project it (Project).
//   - Orphan  (OS→SoR): a pb_records doc whose SHA has no SoR record — a
//     brain_forget delete whose River delete job was exhausted. Recall surfaces
//     a hit whose brain_fetch 404s. Repair: delete the doc (DeleteProjection).
//
// The reconciler periodically diffs each binding's SoR record SHAs against its
// projection SHAs (keyed on records.sha == the OS doc `sha` field) and repairs
// both directions. Every repair is idempotent (and, once the version-guard
// lands, version-guarded), so re-projecting or re-deleting is always safe.

// reconcileSoR is the reconciler's per-binding Postgres surface: enumerate the
// SoR record SHAs and fetch one full record for re-projection. Abstract so the
// diff/repair loop is fakeable in unit tests exactly like synthStore.
type reconcileSoR interface {
	// ListSHAs returns ONE keyset page of (id, sha) pairs for (profile,
	// vault) with id > after, ordered by id, capped at limit. BOTH
	// synthesised states — the reconciler diffs the ENTIRE SoR.
	ListSHAs(ctx context.Context, profile, vault string, after int64, limit int) ([]recordSHA, error)
	// Fetch returns the full record for (profile, vault, sha) for
	// re-projection, or (nil, nil) when the row vanished mid-cycle (a
	// concurrent brain_forget) — a benign skip, not an error.
	Fetch(ctx context.Context, profile, vault, sha string) (*pgdb.Record, error)
}

// reconcileProjection is the reconciler's per-binding OpenSearch surface:
// enumerate the projected doc SHAs and repair drift in either direction.
type reconcileProjection interface {
	// ScrollSHAs invokes fn for every projected doc's sha (scoped to the
	// binding via the resolved prefix), pulling only the sha field. An
	// error from fn aborts the scroll.
	ScrollSHAs(ctx context.Context, profile, vault string, fn func(sha string) error) error
	// Project re-projects a full record into pb_records (missing repair).
	Project(ctx context.Context, rec pgdb.Record) error
	// DeleteProjection removes an orphan doc by (profile, vault, sha).
	DeleteProjection(ctx context.Context, profile, vault, sha string) error
}

// recordSHA is one (id, sha) row from the SoR keyset enumeration; id is the
// keyset cursor, sha the diff key.
type recordSHA struct {
	ID  int64
	SHA string
}

// reconcileMaxPerBinding caps how many SHAs the reconciler will scan from EACH
// side (SoR and OS) per binding per cycle. A binding that exceeds this on
// either side is reconciled PARTIALLY this cycle and logs a clear warning —
// there is no silent truncation. 100k SHAs is far past any single-operator
// vault; the cap exists only so a runaway binding can't pin the reconciler for
// an unbounded stretch.
const reconcileMaxPerBinding = 100000

// reconcileMaxRepairsPerBinding caps the repairs (re-projects + deletes)
// applied to one binding per cycle. Exceeding it stops repairs early and logs
// a partial-cycle warning; the next cycle picks up the remainder. Keeps a
// large drift event from turning one cycle into a multi-minute repair storm.
const reconcileMaxRepairsPerBinding = 5000

// reconcileSoRPageSize is the keyset page size ListSHAs is walked in — small
// enough to bound each round-trip, large enough to drain a healthy vault in a
// few pages.
const reconcileSoRPageSize = 1000

// Reconciler diffs each binding's SoR record set against its pb_records
// projection on a fixed interval and repairs drift both directions. It mirrors
// SynthWorker.runSweeper: a ticking loop, per-binding errors logged and never
// aborting the loop, nil-safe when disabled.
type Reconciler struct {
	logger   *slog.Logger
	interval time.Duration

	// Bindings enumerates the (profile, vault) pairs to reconcile, wired from
	// the daemon registry. Nil-safe: a nil func makes reconcileOnce a no-op.
	Bindings func() []VaultKey

	// Resolve returns the per-binding SoR + projection surfaces for one
	// (profile, vault). Returns ok=false on a cache miss (binding view not
	// built) — reconcileBinding then skips the binding rather than touching
	// shared infra, the same tenant-boundary posture as the synth worker.
	Resolve func(profile, vault string) (reconcileSoR, reconcileProjection, bool)

	stopOnce sync.Once
	stopped  chan struct{}
}

// NewReconciler constructs a Reconciler. interval <= 0 leaves it disabled: Start
// then returns without spawning the loop (Enabled reports false).
func NewReconciler(logger *slog.Logger, interval time.Duration) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{
		logger:   logger,
		interval: interval,
		stopped:  make(chan struct{}),
	}
}

// Enabled reports whether the reconciler will actually run (interval > 0).
func (r *Reconciler) Enabled() bool { return r != nil && r.interval > 0 }

// Start spawns the reconcile loop unless disabled (interval <= 0) or wiring is
// incomplete (nil Bindings/Resolve). Idempotent-safe to call once at daemon
// start; ctx cancellation or Stop exits the loop.
func (r *Reconciler) Start(ctx context.Context) {
	if !r.Enabled() {
		r.logger.Info("phantom-brain: projection reconciler disabled (interval <= 0)")
		return
	}
	if r.Bindings == nil || r.Resolve == nil {
		r.logger.Warn("phantom-brain: projection reconciler not wired (nil Bindings/Resolve); skipping")
		return
	}
	go r.runLoop(ctx)
}

// Stop signals the loop to exit. Safe to call multiple times.
func (r *Reconciler) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() { close(r.stopped) })
}

// runLoop ticks on the configured interval, running reconcileOnce each tick.
// ctx cancellation or Stop exits.
func (r *Reconciler) runLoop(ctx context.Context) {
	r.logger.Info("phantom-brain: projection reconciler started",
		slog.Duration("interval", r.interval))
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("phantom-brain: projection reconciler exiting (ctx done)")
			return
		case <-r.stopped:
			r.logger.Info("phantom-brain: projection reconciler exiting (stop)")
			return
		case <-t.C:
			r.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce reconciles every binding once. Per-binding errors are logged
// and never abort the sweep — same posture as SynthWorker.sweepOnce.
func (r *Reconciler) reconcileOnce(ctx context.Context) {
	if r.Bindings == nil {
		return
	}
	for _, k := range r.Bindings() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := r.reconcileBinding(ctx, k); err != nil {
			r.logger.Warn("phantom-brain: projection reconcile failed",
				slog.String("vault", k.Profile+"/"+k.Vault),
				slog.String("err", err.Error()))
		}
	}
}

// reconcileBinding diffs one binding's SoR SHAs against its projection SHAs and
// repairs both drift directions. It builds both bounded SHA sets, computes
// missing = SoR−OS and orphan = OS−SoR, re-projects each missing SHA (loading
// the full record; a mid-cycle-vanished record is skipped) and deletes each
// orphan doc. A binding that hits reconcileMaxPerBinding on either side, or
// reconcileMaxRepairsPerBinding on repairs, is reconciled PARTIALLY this cycle
// and logs a clear warning — no silent truncation. Logs a per-binding summary.
func (r *Reconciler) reconcileBinding(ctx context.Context, k VaultKey) error {
	sor, proj, ok := r.Resolve(k.Profile, k.Vault)
	if !ok {
		// Binding view not registered — skip rather than touch shared infra.
		return nil
	}

	// SoR side: keyset-paginated SHA set, capped at reconcileMaxPerBinding.
	sorSHAs := make(map[string]struct{})
	sorTruncated := false
	var cursor int64
	for {
		page := reconcileSoRPageSize
		if remaining := reconcileMaxPerBinding - len(sorSHAs); remaining < page {
			page = remaining
		}
		if page <= 0 {
			sorTruncated = true
			break
		}
		rows, err := sor.ListSHAs(ctx, k.Profile, k.Vault, cursor, page)
		if err != nil {
			return err
		}
		for _, row := range rows {
			sorSHAs[row.SHA] = struct{}{}
			cursor = row.ID
		}
		if len(rows) < page {
			break
		}
	}

	// OS side: scroll the projection's SHAs, capped at reconcileMaxPerBinding.
	// A cap hit aborts the scroll via a sentinel error from fn.
	osSHAs := make(map[string]struct{})
	osTruncated := false
	scanErr := proj.ScrollSHAs(ctx, k.Profile, k.Vault, func(sha string) error {
		if len(osSHAs) >= reconcileMaxPerBinding {
			osTruncated = true
			return errReconcileCapped
		}
		osSHAs[sha] = struct{}{}
		return nil
	})
	if scanErr != nil && !errors.Is(scanErr, errReconcileCapped) {
		return scanErr
	}

	missing, orphan := diffProjection(sorSHAs, osSHAs)

	repaired, deleted, repairTruncated := 0, 0, false
	budget := reconcileMaxRepairsPerBinding

	// Missing (SoR→OS): re-project each. A record that vanished mid-cycle
	// (ErrNoRows) is skipped — a concurrent brain_forget will (or already
	// did) enqueue the delete.
	for _, sha := range missing {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if budget <= 0 {
			repairTruncated = true
			break
		}
		rec, err := sor.Fetch(ctx, k.Profile, k.Vault, sha)
		if err != nil {
			r.logger.Warn("phantom-brain: reconcile fetch-for-reproject failed",
				slog.String("vault", k.Profile+"/"+k.Vault),
				slog.String("sha", sha), slog.String("err", err.Error()))
			continue
		}
		if rec == nil {
			continue // vanished mid-cycle — skip
		}
		if err := proj.Project(ctx, *rec); err != nil {
			r.logger.Warn("phantom-brain: reconcile re-project failed",
				slog.String("vault", k.Profile+"/"+k.Vault),
				slog.String("sha", sha), slog.String("err", err.Error()))
			continue
		}
		repaired++
		budget--
	}

	// Orphan (OS→SoR): delete each doc whose SHA has no SoR record.
	for _, sha := range orphan {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if budget <= 0 {
			repairTruncated = true
			break
		}
		if err := proj.DeleteProjection(ctx, k.Profile, k.Vault, sha); err != nil {
			r.logger.Warn("phantom-brain: reconcile delete-orphan failed",
				slog.String("vault", k.Profile+"/"+k.Vault),
				slog.String("sha", sha), slog.String("err", err.Error()))
			continue
		}
		deleted++
		budget--
	}

	if sorTruncated || osTruncated || repairTruncated {
		r.logger.Warn("phantom-brain: projection reconcile PARTIAL this cycle (cap hit)",
			slog.String("vault", k.Profile+"/"+k.Vault),
			slog.Bool("sor_truncated", sorTruncated),
			slog.Bool("os_truncated", osTruncated),
			slog.Bool("repairs_truncated", repairTruncated),
			slog.Int("scan_cap", reconcileMaxPerBinding),
			slog.Int("repair_cap", reconcileMaxRepairsPerBinding))
	}

	r.logger.Info("phantom-brain: projection reconcile complete",
		slog.String("vault", k.Profile+"/"+k.Vault),
		slog.Int("sor_scanned", len(sorSHAs)),
		slog.Int("os_scanned", len(osSHAs)),
		slog.Int("missing_reprojected", repaired),
		slog.Int("orphans_deleted", deleted))
	return nil
}

// errReconcileCapped is the sentinel fn returns to abort the OS scroll once the
// per-binding scan cap is reached. reconcileBinding treats it as "stop, not an
// error" (errors.Is).
var errReconcileCapped = errors.New("reconcile: per-binding scan cap reached")

// diffProjection computes the two drift sets from the SoR and OS SHA sets:
// missing = SoR−OS (records absent from the projection, need re-project) and
// orphan = OS−SoR (projected docs with no record, need delete). Pure — the
// unit-testable core of the reconciler.
func diffProjection(sorSHAs, osSHAs map[string]struct{}) (missing, orphan []string) {
	for sha := range sorSHAs {
		if _, ok := osSHAs[sha]; !ok {
			missing = append(missing, sha)
		}
	}
	for sha := range osSHAs {
		if _, ok := sorSHAs[sha]; !ok {
			orphan = append(orphan, sha)
		}
	}
	return missing, orphan
}

// ── production adapters ────────────────────────────────────────────────────

// pgReconcileSoR is the production reconcileSoR: it wraps a *pgBindingView and
// serves both the keyset SHA enumeration (ListRecordSHAs) and the full-record
// fetch (GetRecordBySHA), mapping pgx.ErrNoRows → (nil, nil) so a mid-cycle
// delete is a benign skip.
type pgReconcileSoR struct {
	view *pgBindingView
}

var _ reconcileSoR = (*pgReconcileSoR)(nil)

func (s *pgReconcileSoR) ListSHAs(ctx context.Context, profile, vault string, after int64, limit int) ([]recordSHA, error) {
	rows, err := pgstore.New(s.view.Pool()).ListRecordSHAs(ctx, pgdb.ListRecordSHAsParams{
		Profile: profile, Vault: vault, After: after, Lim: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]recordSHA, 0, len(rows))
	for _, row := range rows {
		out = append(out, recordSHA{ID: row.ID, SHA: row.Sha})
	}
	return out, nil
}

func (s *pgReconcileSoR) Fetch(ctx context.Context, profile, vault, sha string) (*pgdb.Record, error) {
	rec, err := pgstore.New(s.view.Pool()).GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{
		Profile: profile, Vault: vault, Sha: sha,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // vanished mid-cycle
		}
		return nil, err
	}
	return &rec, nil
}

// osReconcileProjection is the production reconcileProjection: it wraps the
// binding's resolved *osearch.Client prefix (SHA scroll) + *osproject.Projector
// (Project / DeleteProjection).
type osReconcileProjection struct {
	client    *osearch.Client
	prefix    string
	projector *osproject.Projector
}

var _ reconcileProjection = (*osReconcileProjection)(nil)

func (p *osReconcileProjection) ScrollSHAs(ctx context.Context, profile, vault string, fn func(sha string) error) error {
	return p.client.ScrollRecordSHAsWithPrefix(ctx, p.prefix, profile, vault, 0, fn)
}

func (p *osReconcileProjection) Project(ctx context.Context, rec pgdb.Record) error {
	return p.projector.Project(ctx, rec)
}

func (p *osReconcileProjection) DeleteProjection(ctx context.Context, profile, vault, sha string) error {
	return p.projector.DeleteProjection(ctx, profile, vault, sha)
}
