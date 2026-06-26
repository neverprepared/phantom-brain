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

// buildPGBindingDeps (re)builds the per-profile Postgres resources and the
// per-binding pgBindingView entries on every cached bindingDeps. Called
// from buildBindingDeps AFTER the OS/MinIO views are set, so the
// bindingDeps pointers already exist in the cache.
//
// Reload safety: any EXISTING d.pgProfiles are fully closed (River Stop +
// pool Close) before rebuilding, so reload never leaks pools/goroutines.
// The cache is then rebuilt fresh from the current registry.
//
// NON-FATAL: every failure path logs a warning and leaves PG nil for the
// affected scope; the daemon keeps serving the legacy path.
func (d *Daemon) buildPGBindingDeps() {
	d.pgMu.Lock()
	defer d.pgMu.Unlock()

	// Tear down any prior resources first (reload). Idempotent on first
	// build (empty map).
	d.closePGProfilesLocked()
	d.pgProfiles = map[string]*pgProfileResources{}

	// Disabled: ensure every binding's PG view is nil and return.
	if d.pgBaseDSN == "" {
		for _, b := range d.registry.Vaults() {
			if deps, ok := d.bindings.Get(b.Key); ok && deps != nil {
				deps.PG = nil
			}
		}
		return
	}

	// OS projection needs the base client to ensure indices + pipeline.
	// Without it we can't project — disable PG entirely.
	if d.osBase == nil {
		d.Logger.Warn("phantom-brain: postgres configured but opensearch is not — postgres disabled (no projection target)")
		for _, b := range d.registry.Vaults() {
			if deps, ok := d.bindings.Get(b.Key); ok && deps != nil {
				deps.PG = nil
			}
		}
		return
	}

	ctx := d.parentCtx
	if ctx == nil {
		ctx = context.Background()
	}

	// The hybrid search pipeline is a CLUSTER resource — ensure it once
	// total, not per binding.
	pipelineEnsured := false
	ensurePipeline := func() error {
		if pipelineEnsured {
			return nil
		}
		if err := osproject.EnsureSearchPipeline(ctx, d.osBase); err != nil {
			return err
		}
		pipelineEnsured = true
		return nil
	}

	// Build per-PROFILE resources once. Collect distinct profiles across
	// all bindings.
	profiles := map[string]struct{}{}
	for _, b := range d.registry.Vaults() {
		profiles[b.Key.Profile] = struct{}{}
	}
	for profile := range profiles {
		res, err := d.buildPGProfileResources(ctx, profile)
		if err != nil {
			d.Logger.Warn("phantom-brain: postgres profile resources failed — postgres disabled for profile (non-fatal)",
				slog.String("profile", profile),
				slog.String("err", err.Error()))
			continue // leave this profile's bindings with PG nil
		}
		d.pgProfiles[profile] = res
	}

	// Build per-BINDING views, attaching to the profile-shared resources.
	for _, b := range d.registry.Vaults() {
		deps, ok := d.bindings.Get(b.Key)
		if !ok || deps == nil {
			continue
		}
		res, ok := d.pgProfiles[b.Key.Profile]
		if !ok {
			deps.PG = nil // profile resources failed to build
			continue
		}
		if err := osproject.EnsureIndex(ctx, d.osBase, b.Storage.IndexPrefix); err != nil {
			d.Logger.Warn("phantom-brain: postgres projection index ensure failed — postgres disabled for binding (non-fatal)",
				slog.String("vault", b.Key.String()),
				slog.String("prefix", b.Storage.IndexPrefix),
				slog.String("err", err.Error()))
			deps.PG = nil
			continue
		}
		if err := ensurePipeline(); err != nil {
			d.Logger.Warn("phantom-brain: postgres search pipeline ensure failed — postgres disabled for binding (non-fatal)",
				slog.String("vault", b.Key.String()),
				slog.String("err", err.Error()))
			deps.PG = nil
			continue
		}
		deps.PG = &pgBindingView{
			res:       res,
			recaller:  osproject.NewRecaller(d.osBase, b.Storage.IndexPrefix),
			projector: osproject.New(d.osBase, b.Storage.IndexPrefix),
		}
	}
}

// buildPGProfileResources opens the per-profile pool, migrates River,
// builds the projection worker (with the daemon's resolvingProjector so a
// single client can project to many vault prefixes), and starts the River
// client. On ANY error it cleanly closes whatever it opened and returns
// the error (caller logs + disables — non-fatal).
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
	if err := client.Start(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("server: start river client for profile %q: %w", profile, err)
	}

	return &pgProfileResources{pool: pool, river: client}, nil
}

// closePGProfilesLocked stops every River client (bounded ctx) then closes
// each pool. Caller MUST hold d.pgMu. Errors are logged, never fatal.
func (d *Daemon) closePGProfilesLocked() {
	for profile, res := range d.pgProfiles {
		if res == nil {
			continue
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
