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

// buildPGBindingDeps (re)builds the per-profile Postgres resources and the
// per-binding pgBindingView entries on every cached bindingDeps. Called
// from buildBindingDeps AFTER the OS/MinIO views are set, so the
// bindingDeps pointers already exist in the cache.
//
// Reload safety: any EXISTING d.pgProfiles are fully closed (River Stop +
// pool Close) before rebuilding, so reload never leaks pools/goroutines.
// The cache is then rebuilt fresh from the current registry.
//
// Phase D1: Postgres is MANDATORY. This returns an error when PG is
// unconfigured or any binding's PG view fails to build. Start propagates
// the error (fail-loud at boot); reload logs + tolerates partial failure
// per the documented reload semantics. Returning an error leaves the
// affected binding's PG view nil — resolvePG then fails loud per request.
func (d *Daemon) buildPGBindingDeps() error {
	d.pgMu.Lock()
	defer d.pgMu.Unlock()

	// Tear down any prior resources first (reload). Idempotent on first
	// build (empty map).
	d.closePGProfilesLocked()
	d.pgProfiles = map[string]*pgProfileResources{}

	// Disabled: PG is mandatory in D1 — refuse to build.
	if d.pgBaseDSN == "" {
		for _, b := range d.registry.Vaults() {
			if deps, ok := d.bindings.Get(b.Key); ok && deps != nil {
				deps.PG = nil
			}
		}
		return errors.New("server: postgres DSN not configured — the Postgres SoR is mandatory (set [postgres] dsn or PB_POSTGRES_DSN)")
	}

	// OS projection needs the base client to ensure indices + pipeline.
	// Without it we can't project — fail loud.
	if d.osBase == nil {
		for _, b := range d.registry.Vaults() {
			if deps, ok := d.bindings.Get(b.Key); ok && deps != nil {
				deps.PG = nil
			}
		}
		return errors.New("server: postgres requires opensearch for the pb_records projection target, but opensearch is not configured")
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
			return fmt.Errorf("server: postgres profile resources for %q: %w", profile, err)
		}
		d.pgProfiles[profile] = res
	}

	// Build per-BINDING views, attaching to the profile-shared resources.
	for _, b := range d.registry.Vaults() {
		deps, ok := d.bindings.Get(b.Key)
		if !ok || deps == nil {
			return fmt.Errorf("server: no binding deps cached for %s (cannot attach postgres view)", b.Key)
		}
		res, ok := d.pgProfiles[b.Key.Profile]
		if !ok {
			deps.PG = nil
			return fmt.Errorf("server: postgres profile resources missing for %s", b.Key)
		}
		if err := osproject.EnsureIndex(ctx, d.osBase, b.Storage.IndexPrefix); err != nil {
			deps.PG = nil
			return fmt.Errorf("server: postgres projection index ensure for %s (prefix %q): %w", b.Key, b.Storage.IndexPrefix, err)
		}
		if err := ensurePipeline(); err != nil {
			deps.PG = nil
			return fmt.Errorf("server: postgres search pipeline ensure for %s: %w", b.Key, err)
		}
		deps.PG = &pgBindingView{
			res:       res,
			recaller:  osproject.NewRecaller(d.osBase, b.Storage.IndexPrefix),
			projector: osproject.New(d.osBase, b.Storage.IndexPrefix),
		}
	}
	return nil
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
