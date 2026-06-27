package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/neverprepared/phantom-brain/internal/osproject"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
	"github.com/neverprepared/phantom-brain/internal/projection"
)

// Phase A — per-binding Postgres resolution (dormant, additive).
//
// This mirrors the v3.2 binding-views pattern (osBindingView /
// minioBindingView) but with a crucial split driven by the storage
// topology:
//
//   - A Postgres DATABASE is per-PROFILE (pb_<profile>). Every vault in
//     a profile shares ONE *pgxpool.Pool and ONE River client →
//     pgProfileResources, keyed by profile string.
//   - The OS projection PREFIX is per-BINDING (profile, vault). So the
//     osproject.Recaller + osproject.Projector are per-binding →
//     pgBindingView, cached on the binding's bindingDeps.
//
// Because one per-profile River client must project records belonging to
// DIFFERENT vaults (different OS prefixes) of that profile, the worker
// cannot be bound to a single Projector. Instead it holds a
// resolvingProjector that looks up the per-binding osproject.Projector
// per record from (rec.Profile, rec.Vault).
//
// NON-FATAL contract: ANY failure building these resources (PG
// unreachable, migrate/start error, OS EnsureIndex/pipeline error) logs a
// warning and DISABLES Postgres for the affected scope — it never aborts
// daemon startup or reload. Nothing depends on PG yet; a misconfigured or
// absent Postgres must not take down the legacy daemon. Phase B tightens
// this once dual-write is gated on.

// ErrPostgresDisabled is returned by resolvePG when Postgres is not
// configured for the daemon, or the binding's PG view failed to build
// (non-fatal disable). There is no shared-fallback concept for PG.
var ErrPostgresDisabled = errors.New("server: postgres not configured for binding")

// pgProfileResources is the per-PROFILE Postgres handle set: one pool +
// one River client over the pb_<profile> database, shared by every vault
// in that profile.
type pgProfileResources struct {
	pool  *pgxpool.Pool
	river *river.Client[pgx.Tx]
}

// pgBindingView is the per-BINDING (profile, vault) Postgres view. It
// borrows the profile-shared pool/River (res) and adds the binding's own
// OS projection prefix via Recaller + Projector.
type pgBindingView struct {
	res       *pgProfileResources
	recaller  *osproject.Recaller
	projector *osproject.Projector
}

// Pool returns the profile-shared connection pool.
func (v *pgBindingView) Pool() *pgxpool.Pool { return v.res.pool }

// River returns the profile-shared River client.
func (v *pgBindingView) River() *river.Client[pgx.Tx] { return v.res.river }

// Recaller returns the per-binding hybrid recall reader.
func (v *pgBindingView) Recaller() *osproject.Recaller { return v.recaller }

// Projector returns the per-binding SoR→OS write projector.
func (v *pgBindingView) Projector() *osproject.Projector { return v.projector }

// pgSynthStore is the production synthStore: it wraps a *pgBindingView and
// satisfies the synth worker's read/extract seam by calling
// pgstore.New(view.Pool()) and mapping pgdb.Record ↔ synthRecord. The
// worker depends only on the synthStore interface so its read path is
// fakeable in unit tests; this adapter is the live pgx-backed impl.
type pgSynthStore struct {
	view *pgBindingView
}

var _ synthStore = (*pgSynthStore)(nil)

// Fetch returns the SoR record for (profile, vault, sha) as a synthRecord,
// or (nil, nil) when no rows match (pgx.ErrNoRows → benign delete race /
// unknown SHA, mirroring the old processJob behaviour).
func (s *pgSynthStore) Fetch(ctx context.Context, profile, vault, sha string) (*synthRecord, error) {
	q := pgstore.New(s.view.Pool())
	rec, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{
		Profile: profile,
		Vault:   vault,
		Sha:     sha,
	})
	if err != nil {
		if errIsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return pgRecordToSynthRecord(rec), nil
}

// SetExtractedText persists newly extracted attachment text back to the
// SoR record so a later re-synth won't re-extract.
func (s *pgSynthStore) SetExtractedText(ctx context.Context, recordID int64, text string) error {
	q := pgstore.New(s.view.Pool())
	return q.SetRecordExtractedText(ctx, pgdb.SetRecordExtractedTextParams{
		ExtractedText: optText(text),
		ID:            recordID,
	})
}

// ListUnsynthesised returns the Synthesised=false backlog for
// (profile, vault), capped at resynthScanLimit (the same bound the old
// ResynthBacklog passed to the SoR query).
func (s *pgSynthStore) ListUnsynthesised(ctx context.Context, profile, vault string) ([]synthRecord, error) {
	q := pgstore.New(s.view.Pool())
	recs, err := q.ListUnsynthesised(ctx, pgdb.ListUnsynthesisedParams{
		Profile: profile,
		Vault:   vault,
		Lim:     resynthScanLimit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]synthRecord, 0, len(recs))
	for _, rec := range recs {
		out = append(out, *pgRecordToSynthRecord(rec))
	}
	return out, nil
}

// pgRecordToSynthRecord maps a SoR pgdb.Record into the worker's read
// view, reusing pgRecordToSummaryDoc for the SummaryDoc fields and pulling
// the relational identity + attachment metadata across.
func pgRecordToSynthRecord(rec pgdb.Record) *synthRecord {
	return &synthRecord{
		Doc:              pgRecordToSummaryDoc(rec),
		RecordID:         rec.ID,
		Synthesised:      rec.Synthesised,
		MIMEType:         rec.MimeType.String,
		OriginalFilename: rec.OriginalFilename.String,
		MinIOKey:         rec.MinioKey.String,
		ExtractedText:    rec.ExtractedText.String,
	}
}

// resolvingProjector adapts the per-profile River worker (which is bound
// to one River client serving many vaults) to per-binding OS prefixes. It
// resolves the right osproject.Projector per record from (profile, vault),
// then delegates. Satisfies projection.Projector.
type resolvingProjector struct {
	resolve func(profile, vault string) (*osproject.Projector, error)
}

var _ projection.Projector = (*resolvingProjector)(nil)

// Project resolves the per-binding projector from the record's identity
// and delegates the upsert. A resolve failure returns an error so River
// retries (the binding view may not be registered yet, e.g. mid-reload).
func (p *resolvingProjector) Project(ctx context.Context, rec pgdb.Record) error {
	proj, err := p.resolve(rec.Profile, rec.Vault)
	if err != nil {
		return fmt.Errorf("server: resolve projector for %s/%s: %w", rec.Profile, rec.Vault, err)
	}
	return proj.Project(ctx, rec)
}

// DeleteProjection resolves the per-binding projector from the carried
// identity and delegates the delete.
func (p *resolvingProjector) DeleteProjection(ctx context.Context, profile, vault, sha string) error {
	proj, err := p.resolve(profile, vault)
	if err != nil {
		return fmt.Errorf("server: resolve projector for %s/%s: %w", profile, vault, err)
	}
	return proj.DeleteProjection(ctx, profile, vault, sha)
}

// buildPGBindingDeps fills the PG view on each staged *bindingDeps and
// publishes the fully-built deps into the shared cache. `fresh` carries
// the OS/MinIO views already assembled by buildBindingDeps but NOT yet
// published — this function adds PG and Set()s the complete struct, so a
// reader never observes a half-built deps (C3 race fix).
//
// C3 diff-only rebuild: per-profile Postgres resources (pool + River
// client) are keyed by profile and depend only on the profile name (the
// DSN is derived deterministically). On reload we therefore:
//   - KEEP every profile that is still present (its pool + River client
//     keep running untouched — no cross-tenant disruption);
//   - BUILD only newly-added profiles (and Start their River client AFTER
//     their bindings are published, so a projection worker never resolves
//     through an unpublished deps);
//   - CLOSE only removed profiles (River Stop + pool Close).
// The lightweight per-binding view (Recaller/Projector over the binding's
// OS prefix) is always rebuilt — that is how a storage_overrides prefix
// edit takes effect — but it borrows the (possibly unchanged) profile
// resources, so rebuilding it is cheap and non-disruptive.
//
// Phase D1: Postgres is MANDATORY. This returns an error when PG is
// unconfigured or any binding's PG view fails to build. Start propagates
// the error (fail-loud at boot); reload logs + tolerates partial failure
// per the documented reload semantics.
func (d *Daemon) buildPGBindingDeps(fresh map[VaultKey]*bindingDeps) error {
	d.pgMu.Lock()
	defer d.pgMu.Unlock()

	if d.pgProfiles == nil {
		d.pgProfiles = map[string]*pgProfileResources{}
	}

	// Disabled: PG is mandatory in D1 — refuse to build. Still publish the
	// OS/Attach views (PG nil) so non-PG paths/tests work, close any prior
	// PG resources, then return the error.
	if d.pgBaseDSN == "" {
		d.closePGProfilesLocked()
		d.pgProfiles = map[string]*pgProfileResources{}
		for k, deps := range fresh {
			d.bindings.Set(k, deps) // PG nil
		}
		return errors.New("server: postgres DSN not configured — the Postgres SoR is mandatory (set [postgres] dsn or PB_POSTGRES_DSN)")
	}
	// OS projection needs the base client to ensure indices + pipeline.
	if d.osBase == nil {
		d.closePGProfilesLocked()
		d.pgProfiles = map[string]*pgProfileResources{}
		for k, deps := range fresh {
			d.bindings.Set(k, deps) // PG nil
		}
		return errors.New("server: postgres requires opensearch for the pb_records projection target, but opensearch is not configured")
	}

	ctx := d.parentCtx
	if ctx == nil {
		ctx = context.Background()
	}

	// Distinct profiles the new registry needs.
	needed := map[string]struct{}{}
	for k := range fresh {
		needed[k.Profile] = struct{}{}
	}

	// Build ADDED profiles (present in needed, absent from the live map).
	// Do NOT Start their River clients yet — Start happens after the
	// bindings that resolve through them are published.
	var newlyAdded []string
	for profile := range needed {
		if _, ok := d.pgProfiles[profile]; ok {
			continue // unchanged — keep the running pool + River client
		}
		res, err := d.buildPGProfileResources(ctx, profile)
		if err != nil {
			// Non-fatal disable contract: publish the OS/Attach views with
			// PG nil so legacy/non-PG read paths still resolve, then fail
			// loud (Start aborts boot; reload logs + tolerates).
			d.publishFreshLocked(fresh)
			return fmt.Errorf("server: postgres profile resources for %q: %w", profile, err)
		}
		d.pgProfiles[profile] = res
		newlyAdded = append(newlyAdded, profile)
	}

	// Hybrid search pipeline is a CLUSTER resource — ensure once (idempotent).
	if err := osproject.EnsureSearchPipeline(ctx, d.osBase); err != nil {
		d.publishFreshLocked(fresh)
		return fmt.Errorf("server: postgres search pipeline ensure: %w", err)
	}

	// Assemble each binding's PG view on its staged deps, then publish the
	// COMPLETE deps via a single atomic Set (never mutate after publish).
	for _, b := range d.registry.Vaults() {
		deps, ok := fresh[b.Key]
		if !ok || deps == nil {
			continue
		}
		res, ok := d.pgProfiles[b.Key.Profile]
		if !ok {
			d.publishFreshLocked(fresh)
			return fmt.Errorf("server: postgres profile resources missing for %s", b.Key)
		}
		if err := osproject.EnsureIndex(ctx, d.osBase, b.Storage.IndexPrefix); err != nil {
			d.publishFreshLocked(fresh)
			return fmt.Errorf("server: postgres projection index ensure for %s (prefix %q): %w", b.Key, b.Storage.IndexPrefix, err)
		}
		deps.PG = &pgBindingView{
			res:       res,
			recaller:  osproject.NewRecaller(d.osBase, b.Storage.IndexPrefix),
			projector: osproject.New(d.osBase, b.Storage.IndexPrefix),
		}
		d.bindings.Set(b.Key, deps) // atomic full publish
	}

	// Start newly-added River clients now that the bindings their workers
	// resolve through are published (audit: don't Start before publish).
	// The deps are already published and valid — even if Start fails, the
	// pool is open and InsertTx still durably queues projection jobs (they
	// just won't drain until a working client starts), so we keep the
	// resource rather than closing it out from under published readers. We
	// still return the error so boot fails loud; reload logs + tolerates.
	for _, profile := range newlyAdded {
		res := d.pgProfiles[profile]
		if res == nil || res.river == nil {
			continue
		}
		if err := res.river.Start(ctx); err != nil {
			return fmt.Errorf("server: start river client for profile %q: %w", profile, err)
		}
	}

	// Close REMOVED profiles (live but no longer needed). No binding deps
	// reference them anymore, so it is safe to Stop + Close.
	for profile, res := range d.pgProfiles {
		if _, ok := needed[profile]; ok {
			continue
		}
		d.closePGProfileResourceLocked(profile, res)
		delete(d.pgProfiles, profile)
	}
	return nil
}

// publishFreshLocked Sets every staged deps into the shared cache as-is.
// Used on the failure paths so OS/Attach views still resolve (with PG nil)
// even when the PG build fails — the non-fatal disable contract. Each Set
// publishes a complete struct (PG nil is a valid "disabled" state), so
// readers never observe a torn deps. Caller MUST hold d.pgMu.
func (d *Daemon) publishFreshLocked(fresh map[VaultKey]*bindingDeps) {
	for k, deps := range fresh {
		d.bindings.Set(k, deps)
	}
}

// buildPGProfileResources opens the per-profile pool, migrates River, and
// builds the projection worker + River client (with the daemon's
// resolvingProjector so a single client can project to many vault
// prefixes). It does NOT Start the River client — the caller Starts it
// only after the bindings it resolves through are published. On ANY error
// it cleanly closes whatever it opened and returns the error.
func (d *Daemon) buildPGProfileResources(ctx context.Context, profile string) (*pgProfileResources, error) {
	dsn, err := pgstore.DSNForProfile(d.pgBaseDSN, profile)
	if err != nil {
		return nil, fmt.Errorf("server: derive dsn for profile %q: %w", profile, err)
	}
	pool, err := pgstore.Open(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("server: open pool for profile %q: %w", profile, err)
	}

	if err := projection.MigrateRiver(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("server: river migrate for profile %q: %w", profile, err)
	}

	q := pgstore.New(pool)
	worker := projection.NewProjectRecordWorker(q, &resolvingProjector{resolve: d.resolvePGProjector})
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)

	client, err := projection.NewClient(pool, workers)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("server: new river client for profile %q: %w", profile, err)
	}

	return &pgProfileResources{pool: pool, river: client}, nil
}

// closePGProfileResourceLocked stops one profile's River client (bounded
// ctx) then closes its pool. Caller MUST hold d.pgMu. Errors are logged,
// never fatal.
func (d *Daemon) closePGProfileResourceLocked(profile string, res *pgProfileResources) {
	if res == nil {
		return
	}
	if res.river != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := res.river.Stop(stopCtx); err != nil {
			d.Logger.Warn("phantom-brain: postgres river client stop error",
				slog.String("profile", profile),
				slog.String("err", err.Error()))
		}
		cancel()
	}
	if res.pool != nil {
		res.pool.Close()
	}
}

// closePGProfilesLocked stops every River client (bounded ctx) then closes
// each pool. Caller MUST hold d.pgMu. Errors are logged, never fatal.
// Used by Shutdown and by the PG-disabled reload path.
func (d *Daemon) closePGProfilesLocked() {
	for profile, res := range d.pgProfiles {
		d.closePGProfileResourceLocked(profile, res)
	}
	d.pgProfiles = nil
}

// closePGProfiles is the locking wrapper used by Shutdown.
func (d *Daemon) closePGProfiles() {
	d.pgMu.Lock()
	defer d.pgMu.Unlock()
	d.closePGProfilesLocked()
}

// resolvePG returns the per-binding Postgres view, mirroring resolveOS's
// fail-loud shape. Returns ErrPostgresDisabled when Postgres is not
// configured or the binding's view failed to build. NOTHING consumes this
// yet (Phase A is dormant).
func (d *Daemon) resolvePG(b VaultBinding) (*pgBindingView, error) {
	if d.bindings != nil {
		if deps, ok := d.bindings.Get(b.Key); ok && deps != nil && deps.PG != nil {
			return deps.PG, nil
		}
	}
	return nil, ErrPostgresDisabled
}

// resolvePGProjector resolves the per-binding osproject.Projector from a
// (profile, vault) pair. Used by resolvingProjector so a per-profile River
// worker projects each record to the correct per-binding OS prefix.
func (d *Daemon) resolvePGProjector(profile, vault string) (*osproject.Projector, error) {
	b, ok := d.registry.LookupByVault(VaultKey{Profile: profile, Vault: vault})
	if !ok {
		return nil, fmt.Errorf("server: no binding for %s/%s", profile, vault)
	}
	view, err := d.resolvePG(b)
	if err != nil {
		return nil, err
	}
	return view.projector, nil
}
