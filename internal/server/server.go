package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gofrs/flock"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// Daemon is the in-process state for a running pbrainctl serve
// instance. Lifespan: Start() acquires the global flock, loads the
// registry, spawns per-vault runners, mounts the chi router, and
// returns. Run() blocks on signal-driven shutdown (SIGTERM/SIGINT
// drain, SIGHUP reload).
type Daemon struct {
	Config    *ServerConfig
	ConfigDir string
	DataDir   DataDir
	Logger    *slog.Logger

	registry *Registry
	runners  *runnerSet
	storage  StorageBackend
	router   chi.Router
	srv      *http.Server
	flock    *flock.Flock

	// Phase 6 wiring. osClient is nil when [opensearch] is absent
	// from server.toml; write handlers return 503 in that case.
	// Stored as an interface so tests can inject an in-memory fake.
	// synth defaults to a no-op queue; Day 5 swaps in the real worker.
	// attach is non-nil when storage backend = minio (or test fake).
	//
	// v3.2 per-binding storage overrides: osClient/attach above are
	// the SHARED daemon-default views (no override applied). For
	// each binding the daemon caches a per-binding view in bindings
	// (built at Start + on reload). Handlers / synth resolve through
	// depsForBinding which falls back to the shared views.
	osClient   osWriter
	osBase     *osearch.Client // raw client, used to derive WithPrefix views
	minioBase  *MinIOBackend   // raw MinIO backend, used to derive per-bucket views
	synth      SynthQueue
	attach     AttachmentStore
	bindings   *bindingDepCache

	// Phase A (dormant): per-profile Postgres System-of-Record. pgBaseDSN
	// is the resolved base/maintenance DSN ("" = Postgres disabled).
	// pgProfiles caches the per-PROFILE resources (one *pgxpool.Pool +
	// one River client per pb_<profile> DB, SHARED across that profile's
	// vaults); built by buildPGBindingDeps at Start + on reload, and
	// guarded by pgMu. NOTHING consumes resolvePG yet — purely additive.
	pgBaseDSN  string
	pgMu       sync.Mutex
	pgProfiles map[string]*pgProfileResources

	// dualWriteFailures counts non-fatal Phase B1 dual-write failures
	// (PG disabled errors aside) since daemon start. No Prometheus yet —
	// this counter + the paired Warn logs are the "meter" for dual-write
	// divergence. Read via DualWriteFailureCount.
	dualWriteFailures atomic.Int64

	// allowSharedFallback is the explicit opt-in that lets resolveOS /
	// resolveAttach return the shared d.osClient / d.attach when no
	// per-binding view is registered. Production wiring leaves this
	// false (any cache miss is a tenant-boundary leak); tests that
	// build a Daemon by hand set it to keep the legacy paths working.
	allowSharedFallback bool

	parentCtx context.Context
	parentCancel context.CancelFunc

	mu      sync.Mutex
	started bool
}

// StartOpts groups inputs to Start. All fields required.
type StartOpts struct {
	ConfigDir string
	DataDir   DataDir
	Logger    *slog.Logger
}

// Start builds and primes a Daemon but does not block. Call Run() to
// block on the signal loop, or Shutdown() to stop. Sequence:
//
//  1. EnsureDaemonSkeleton + acquire global flock (fail-fast on
//     second-daemon race).
//  2. LoadServerConfig from {configDir}/server.toml.
//  3. Build registry from {configDir}/profiles/*/vaults/*.
//  4. For each registered vault: EnsureCollectiveSkeleton + spawn
//     vaultRunner.
//  5. Build the chi router (middleware: recoverer, request id,
//     real-ip; auth-mounted endpoints come after).
//  6. Construct *http.Server — Start does NOT call ListenAndServe;
//     Run does. Splits the API so tests can serve via httptest.
func Start(opts StartOpts) (*Daemon, error) {
	if opts.ConfigDir == "" {
		return nil, errors.New("server: Start requires a config dir")
	}
	if opts.DataDir == "" {
		return nil, errors.New("server: Start requires a data dir")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	cfg, err := LoadServerConfig(opts.ConfigDir)
	if err != nil {
		return nil, err
	}

	if err := EnsureDaemonSkeleton(opts.DataDir); err != nil {
		return nil, fmt.Errorf("server: ensure daemon skeleton: %w", err)
	}
	lk := flock.New(opts.DataDir.GlobalFlockPath())
	gotLock, err := lk.TryLock()
	if err != nil {
		return nil, fmt.Errorf("server: acquire global flock: %w", err)
	}
	if !gotLock {
		return nil, fmt.Errorf("server: another pbrainctl serve is holding %s", opts.DataDir.GlobalFlockPath())
	}
	// Write the daemon's PID into the flock file so `pbrainctl vault
	// reload` (and ops scripts) can find us. flock(2) is content-
	// independent — writing inside the held file is safe.
	if err := os.WriteFile(opts.DataDir.GlobalFlockPath(),
		[]byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		opts.Logger.Warn("phantom-brain: write daemon pid sidecar failed (continuing)",
			slog.String("err", err.Error()))
	}

	parentCtx, parentCancel := context.WithCancel(context.Background())

	// Build the storage backend per server.toml. Local is the
	// default and works against the daemon's own URL space.
	baseURL := fmt.Sprintf("http://%s:%d", cfg.Server.Host, cfg.Server.Port)
	if cfg.Server.Host == "0.0.0.0" {
		baseURL = fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port)
	}
	var backend StorageBackend
	switch cfg.Storage.Backend {
	case "minio":
		mb, merr := NewMinIOBackend(MinIOOptions{
			Endpoint:  cfg.Storage.MinIOEndpoint,
			Bucket:    cfg.Storage.MinIOBucket,
			AccessKey: cfg.Storage.MinIOAccessKey,
			SecretKey: cfg.Storage.MinIOSecretKey,
			UseSSL:    cfg.Storage.MinIOUseSSL,
			DataDir:   opts.DataDir,
		})
		if merr != nil {
			_ = lk.Unlock()
			parentCancel()
			return nil, merr
		}
		backend = mb
	default:
		lb, lerr := NewLocalBackend(opts.DataDir, baseURL)
		if lerr != nil {
			_ = lk.Unlock()
			parentCancel()
			return nil, lerr
		}
		backend = lb
	}

	d := &Daemon{
		Config:       cfg,
		ConfigDir:    opts.ConfigDir,
		DataDir:      opts.DataDir,
		Logger:       opts.Logger,
		registry:     NewRegistry(),
		runners:      newRunnerSet(),
		storage:      backend,
		flock:        lk,
		parentCtx:    parentCtx,
		parentCancel: parentCancel,
		synth:        noopSynthQueue{}, // Day 5 swaps in the real worker
		bindings:     newBindingDepCache(),
	}

	// Phase 6: the MinIO backend doubles as the AttachmentStore for
	// /api/brain/attach. LocalBackend has no blob surface, so /attach
	// stays 503 in local-mode deploys (operator should use minio).
	if mb, ok := backend.(*MinIOBackend); ok {
		d.attach = mb
		d.minioBase = mb
	}

	// Phase 6: optional OS client. Daemon still starts without it —
	// snapshot / health / merge routes work; only write endpoints fail.
	if cfg.OpenSearch.Enabled() {
		oc, oerr := osearch.Open(parentCtx, osearch.Config{
			Addresses:          cfg.OpenSearch.Addresses,
			Username:           cfg.OpenSearch.Username,
			Password:           cfg.OpenSearch.Password,
			InsecureSkipVerify: cfg.OpenSearch.InsecureSkipVerify,
			IndexPrefix:        cfg.OpenSearch.IndexPrefix,
			RequestTimeout:     time.Duration(cfg.OpenSearch.RequestTimeoutSecs) * time.Second,
		})
		if oerr != nil {
			_ = lk.Unlock()
			parentCancel()
			return nil, fmt.Errorf("server: opensearch open: %w", oerr)
		}
		d.osClient = oc
		d.osBase = oc
		opts.Logger.Info("phantom-brain: opensearch wired",
			slog.Int("addresses", len(cfg.OpenSearch.Addresses)),
			slog.String("index_prefix", cfg.OpenSearch.IndexPrefix),
		)
	}

	// Phase A (dormant): resolve the base Postgres DSN. PB_POSTGRES_DSN
	// overrides the [postgres] dsn in server.toml. Empty ⇒ disabled; the
	// legacy path is fully untouched and buildPGBindingDeps short-circuits.
	d.pgProfiles = map[string]*pgProfileResources{}
	if v := strings.TrimSpace(os.Getenv("PB_POSTGRES_DSN")); v != "" {
		d.pgBaseDSN = v
	} else {
		d.pgBaseDSN = strings.TrimSpace(cfg.Postgres.DSN)
	}
	if d.pgBaseDSN != "" {
		opts.Logger.Info("phantom-brain: postgres base DSN configured (Phase A — dormant, per-profile SoR)")
	}

	// Load the registry BEFORE eager-ensure so we know which prefixes
	// + buckets every binding resolves to. v3.2: per-binding storage
	// overrides mean shared "pb_*" + default bucket are no longer the
	// only physical targets — every (profile, vault) may carve out
	// its own.
	n, err := d.registry.Load(LoadOpts{
		ConfigDir:          opts.ConfigDir,
		Defaults:           cfg.Defaults,
		DefaultIndexPrefix: cfg.OpenSearch.IndexPrefix,
		DefaultBucket:      cfg.Storage.MinIOBucket,
	})
	if err != nil {
		// Release the flock before returning so a second restart
		// attempt doesn't have to wait on a stuck lock.
		_ = lk.Unlock()
		parentCancel()
		return nil, err
	}
	d.Logger.Info("phantom-brain: registry loaded",
		slog.Int("vaults", n),
		slog.String("config_dir", opts.ConfigDir),
	)

	// v3.2 eager-ensure for MinIO buckets: walk every binding, collect
	// distinct buckets, and ensure each exists BEFORE the HTTP listener
	// opens. Failure aborts startup.
	//
	// Phase D1: the LEGACY OpenSearch indices (pb_summaries/pb_entities/
	// pb_attachments) are NO LONGER written, so they are NOT ensured here.
	// The only OS index that matters now is pb_records, ensured per binding
	// via osproject.EnsureIndex inside buildPGBindingDeps below.
	if d.minioBase != nil {
		seenBucket := map[string]struct{}{}
		buckets := []string{cfg.Storage.MinIOBucket}
		for _, b := range d.registry.Vaults() {
			if b.Storage.Bucket != "" {
				buckets = append(buckets, b.Storage.Bucket)
			}
		}
		for _, bk := range buckets {
			if bk == "" {
				continue
			}
			if _, dup := seenBucket[bk]; dup {
				continue
			}
			seenBucket[bk] = struct{}{}
			if berr := d.minioBase.EnsureBucketExists(parentCtx, bk); berr != nil {
				_ = lk.Unlock()
				parentCancel()
				return nil, berr
			}
		}
	}

	// Phase D1: the v3.2 operator-footgun detector (VerifyStorageOverrides)
	// queried the LEGACY pb_summaries indices, which are no longer
	// ensured or written — so it is dropped here. The pb_records
	// projection is per-binding-prefixed and ensured fresh in
	// buildPGBindingDeps; there is no shared-vs-prefixed straddle to guard
	// against for a store that's authoritative-from-birth.

	// Build per-binding views now that the registry + eager-ensure
	// passed. Handlers + synth resolve through d.depsForBinding —
	// reload() re-runs buildBindingDeps for added bindings. Phase D1:
	// Postgres is mandatory, so a PG build failure here aborts startup.
	if err := d.buildBindingDeps(); err != nil {
		_ = lk.Unlock()
		parentCancel()
		return nil, err
	}

	if d.osBase != nil {
		// Spawn the synth worker now that OS + Postgres are reachable.
		// Phase D1: the worker reads raw records from the Postgres SoR
		// and writes synth results back to it (the pb_records projection
		// follows via River). Resolve threads the per-binding PG view +
		// AttachmentStore in; WriteSynth persists the distilled result.
		// Phase D2b: there is no longer a snapshot rebuild — the
		// projection is the canonical read path, so the worker has no
		// OnComplete hook.
		w := NewSynthWorker(SynthWorkerOpts{
			Logger:  opts.Logger,
			Capture: cfg.Capture,
		})
		w.Resolve = func(profile, vaultName string) (synthStore, AttachmentStore, bool) {
			deps, ok := d.bindings.Get(VaultKey{Profile: profile, Vault: vaultName})
			if !ok || deps == nil || deps.PG == nil {
				return nil, nil, false
			}
			// Wrap the resolved per-binding PG view in the production
			// synthStore adapter so the worker reads through the
			// fakeable seam rather than a concrete pgx pool.
			return &pgSynthStore{view: deps.PG}, deps.Attach, true
		}
		w.WriteSynth = func(ctx context.Context, profile, vaultName, sha string, res synthResult) error {
			b, ok := d.registry.LookupByVault(VaultKey{Profile: profile, Vault: vaultName})
			if !ok {
				return fmt.Errorf("synth: no binding for %s/%s", profile, vaultName)
			}
			return d.writeSynthResult(ctx, b, profile, vaultName, sha, res)
		}
		w.Start(parentCtx)
		d.synth = w
	}

	// Spawn per-vault runners (no-op stubs in day 1; real loops in
	// days 3-4). EnsureCollectiveSkeleton is called for each so the
	// reaper/synthesizer find the dirs they expect.
	for _, b := range d.registry.Vaults() {
		if err := EnsureCollectiveSkeleton(opts.DataDir, b.Key.Profile, b.Key.Vault); err != nil {
			_ = lk.Unlock()
			parentCancel()
			return nil, fmt.Errorf("server: ensure collective for %s: %w", b.Key, err)
		}
		d.runners.Add(newVaultRunner(parentCtx, b, opts.DataDir, d.Logger))
	}

	d.router = d.buildRouter()
	d.srv = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:           d.router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	d.started = true
	return d, nil
}

// buildRouter wires the chi tree. Public endpoints (just /api/brain/
// health for now) sit outside the auth middleware; everything else
// goes through AuthMiddleware. Day 2-5 days mount more handlers under
// the authenticated subrouter.
func (d *Daemon) buildRouter() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	r.Route("/api/brain", func(r chi.Router) {
		// /health is intentionally unauthenticated: load balancers
		// and operators need to probe the daemon without holding a
		// vault token.
		r.Get("/health", d.handleHealth)

		// Everything else requires bearer auth.
		r.Group(func(r chi.Router) {
			r.Use(AuthMiddleware(d.registry))
			r.Post("/merge/init", d.handleMergeInit)
			r.Post("/merge/complete/{uploadID}", d.handleMergeComplete)
			r.Get("/merge/{brainID}", d.handleMergeStatus)
			r.Get("/maintenance", d.handleMaintenanceGet)
			r.Post("/maintenance/{action}", d.handleMaintenance)

			// Phase 6 write endpoints. Return 503 when opensearch is
			// not configured; otherwise upsert + enqueue synth.
			r.Post("/perceive", d.handlePerceive)
			r.Post("/learn", d.handleLearn)
			// Phase C: always-online recall over the pb_records
			// Postgres projection. 503 when PG isn't enabled for the
			// binding — the agent falls back to its local snapshot.
			r.Post("/recall", d.handleRecall)
			// Phase D2a: always-online fetch — full record body by SHA
			// from the Postgres SoR. Companion to /recall.
			r.Get("/fetch/{sha}", d.handleFetch)
			r.Post("/attach", d.handleAttach)
			r.Post("/trace", d.handleTrace)
			r.Get("/attach/{sha}", d.handleAttachGet)
			r.Get("/capture/{sha}", d.handleCaptureGet)

			// v3.3 brain_reflect maintenance cycle (issue #72 Phase 1).
			// reflect REPORTS forget-candidates; forget APPLIES one.
			r.Get("/reflect", d.handleReflect)
			r.Post("/forget", d.handleForget)
			// v3.4 re-synthesis backfill (issue #82): re-process docs
			// stuck at Synthesised=false (dropped synth jobs).
			r.Post("/resynth", d.handleResynth)
		})

		// Upload route is local-backend only and uses its own
		// HMAC-token auth rather than bearer (so brains can use
		// the presigned URL with curl without forwarding their
		// vault token).
		r.Put("/merge/upload/{uploadID}", d.handleMergeUpload)
	})
	return r
}

// Run blocks on the signal loop until SIGINT/SIGTERM. SIGHUP triggers
// a registry reload (add new vaults, drain removed ones). Returns nil
// on graceful exit; returns the error from ListenAndServe if the
// server died unexpectedly.
func (d *Daemon) Run() error {
	d.mu.Lock()
	if !d.started {
		d.mu.Unlock()
		return errors.New("server: Daemon.Run called before Start")
	}
	d.mu.Unlock()

	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(hupCh)

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stopCh)

	srvErr := make(chan error, 1)
	go func() {
		d.Logger.Info("phantom-brain: HTTP server listening", slog.String("addr", d.srv.Addr))
		err := d.srv.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
			return
		}
		srvErr <- nil
	}()

	for {
		select {
		case sig := <-hupCh:
			d.Logger.Info("phantom-brain: received signal — reloading registry", slog.String("signal", sig.String()))
			if err := d.reload(); err != nil {
				d.Logger.Warn("phantom-brain: SIGHUP reload failed (continuing with prior registry)", slog.String("err", err.Error()))
			}
		case sig := <-stopCh:
			d.Logger.Info("phantom-brain: received shutdown signal", slog.String("signal", sig.String()))
			return d.Shutdown(context.Background())
		case err := <-srvErr:
			if err != nil {
				return fmt.Errorf("server: http listener: %w", err)
			}
			return nil
		}
	}
}

// reload re-scans the config dir and diffs against the live runners.
// Added vaults get an EnsureCollectiveSkeleton + new runner; removed
// vaults get their runner drained. Existing vaults are NOT restarted
// — operator-edited config.toml takes effect only on full restart
// (Phase 5 will add a finer-grained reload if the need arises).
func (d *Daemon) reload() error {
	prior := d.runners.Keys()
	if _, err := d.registry.Load(LoadOpts{
		ConfigDir:          d.ConfigDir,
		Defaults:           d.Config.Defaults,
		DefaultIndexPrefix: d.Config.OpenSearch.IndexPrefix,
		DefaultBucket:      d.Config.Storage.MinIOBucket,
	}); err != nil {
		return err
	}
	added, removed := d.registry.Diff(prior)
	for _, k := range removed {
		d.Logger.Info("phantom-brain: SIGHUP draining vault", slog.String("vault", k.String()))
		d.runners.Remove(k)
		d.bindings.Delete(k)
	}
	// v3.2: ensure per-binding storage targets for any added bindings
	// before they start serving. Failure logs + skips that binding —
	// preserves the prior behavior of "partial reload tolerated, log
	// errors on the offenders".
	for _, k := range added {
		b, ok := d.registry.LookupByVault(k)
		if !ok {
			continue // racing remove — skip
		}
		// Phase D1: legacy pb_* index ensure + VerifyStorageOverrides are
		// dropped — those indices are no longer written. The pb_records
		// projection index is ensured for added bindings inside
		// buildBindingDeps → buildPGBindingDeps below.
		if d.minioBase != nil && b.Storage.Bucket != "" {
			if err := d.minioBase.EnsureBucketExists(d.parentCtx, b.Storage.Bucket); err != nil {
				d.Logger.Warn("phantom-brain: SIGHUP ensure-bucket failed",
					slog.String("vault", k.String()),
					slog.String("bucket", b.Storage.Bucket),
					slog.String("err", err.Error()))
				continue
			}
		}
		if err := EnsureCollectiveSkeleton(d.DataDir, k.Profile, k.Vault); err != nil {
			d.Logger.Warn("phantom-brain: SIGHUP collective-skeleton failed", slog.String("vault", k.String()), slog.String("err", err.Error()))
			continue
		}
		d.Logger.Info("phantom-brain: SIGHUP starting vault", slog.String("vault", k.String()))
		d.runners.Add(newVaultRunner(d.parentCtx, b, d.DataDir, d.Logger))
	}
	// Rebuild deps for every binding (added + existing) so any
	// operator-edited config.toml (e.g. switched buckets) takes
	// effect. Existing runners are NOT restarted per the documented
	// reload semantics. Phase D1: a PG rebuild failure is logged but
	// tolerated (consistent with reload's "continue with prior
	// registry" posture) — resolvePG then fails loud per-request for
	// any binding whose view didn't rebuild.
	if err := d.buildBindingDeps(); err != nil {
		d.Logger.Warn("phantom-brain: SIGHUP postgres binding rebuild failed (some bindings may have no PG view until fixed)",
			slog.String("err", err.Error()))
	}
	return nil
}

// Shutdown gracefully drains the HTTP server, stops every runner,
// and releases the global flock. ctx bounds the HTTP drain; runner
// drain runs unbounded because the loops have their own ctx wired
// from parentCtx (cancelled here).
func (d *Daemon) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.started {
		return nil
	}
	d.started = false

	shutdownCtx := ctx
	if _, deadlineSet := ctx.Deadline(); !deadlineSet {
		var cancel context.CancelFunc
		shutdownCtx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	if err := d.srv.Shutdown(shutdownCtx); err != nil {
		d.Logger.Warn("phantom-brain: HTTP shutdown error", slog.String("err", err.Error()))
	}

	d.parentCancel()
	d.runners.StopAll()
	// Phase A: stop every per-profile River client + close its pool. Logs
	// warns on error; never blocks shutdown.
	d.closePGProfiles()
	if d.flock != nil {
		if err := d.flock.Unlock(); err != nil {
			d.Logger.Warn("phantom-brain: release global flock", slog.String("err", err.Error()))
		}
	}
	d.Logger.Info("phantom-brain: shutdown complete")
	return nil
}

// Router returns the underlying chi router. Exposed for tests that
// want to mount the daemon under httptest.NewServer without running
// the full ListenAndServe path.
func (d *Daemon) Router() chi.Router { return d.router }

// LocalBackendForTest returns the underlying LocalBackend if the
// daemon was configured with local storage. Test-only accessor so
// integration tests can rewrite the baseURL after httptest assigns
// a port. Returns (nil, false) when running against MinIO.
func (d *Daemon) LocalBackendForTest() (*LocalBackend, bool) {
	lb, ok := d.storage.(*LocalBackend)
	return lb, ok
}

// buildBindingDeps walks the registry and refreshes the per-binding
// view cache. v3.2: each VaultBinding resolves to its own
// ResolvedStorage{IndexPrefix, Bucket}; we derive a per-binding
// *osearch.Client (via WithPrefix) and a per-binding AttachmentStore
// (minioBindingView) once at startup / reload so the request path is
// a cache lookup, not a clone.
func (d *Daemon) buildBindingDeps() error {
	for _, b := range d.registry.Vaults() {
		deps := &bindingDeps{}
		if d.osBase != nil {
			view := newOSBindingView(d.osBase, b.Storage.IndexPrefix)
			deps.OS = view
		}
		if d.minioBase != nil {
			deps.Attach = newMinIOBindingView(d.minioBase, b.Storage.Bucket)
		} else if d.attach != nil {
			// LocalBackend / test fake — share the daemon-default
			// AttachmentStore. StorageOverrides.Bucket is meaningless
			// in local-backend mode; log a warn at reload if set.
			deps.Attach = d.attach
		}
		d.bindings.Set(b.Key, deps)
	}
	// Phase D1: build per-profile Postgres resources + per-binding PG
	// views AFTER the OS/MinIO views are cached (the PG view borrows the
	// just-set bindingDeps pointers). Postgres is MANDATORY — a build
	// failure is returned so Start fails loud; reload logs + tolerates it.
	return d.buildPGBindingDeps()
}

// depsForBinding returns the per-binding view bundle. Handlers call
// this once per request. Falls back to a synthesised view using the
// daemon-default fields when the binding isn't cached (shouldn't
// happen in normal operation — buildBindingDeps populates every
// binding the registry knows about — but the fallback keeps tests
// that construct a Daemon by hand from panicking).
func (d *Daemon) depsForBinding(b VaultBinding) *bindingDeps {
	if deps, ok := d.bindings.Get(b.Key); ok && deps != nil {
		return deps
	}
	deps := &bindingDeps{}
	if d.osBase != nil {
		view := newOSBindingView(d.osBase, b.Storage.IndexPrefix)
		deps.OS = view
	}
	if d.minioBase != nil {
		deps.Attach = newMinIOBindingView(d.minioBase, b.Storage.Bucket)
	} else if d.attach != nil {
		deps.Attach = d.attach
	}
	return deps
}

// LookupBindingForTest exposes the registry's vault-keyed lookup so
// integration tests can drive ReapOnce / SynthesizeOne directly.
// Production code goes through the chi middleware (binding from
// context).
func (d *Daemon) LookupBindingForTest(k VaultKey) (VaultBinding, bool) {
	return d.registry.LookupByVault(k)
}

// healthResponse keeps the shape consistent with what Phase 2.5 brain
// agents will parse out of /api/brain/health. Vaults are listed
// alphabetically; tokens are NEVER returned.
type healthResponse struct {
	Status string         `json:"status"`
	Vaults []healthVault  `json:"vaults"`
}

type healthVault struct {
	Profile string `json:"profile"`
	Vault   string `json:"vault"`
}

// handleHealth is the unauthenticated liveness probe. Returns 200 +
// JSON listing every configured vault (no auth state). Operators use
// this to confirm the registry loaded what they expected.
func (d *Daemon) handleHealth(w http.ResponseWriter, _ *http.Request) {
	out := healthResponse{Status: "ok"}
	for _, b := range d.registry.Vaults() {
		out.Vaults = append(out.Vaults, healthVault{Profile: b.Key.Profile, Vault: b.Key.Vault})
	}
	writeJSON(w, http.StatusOK, out)
}

// writeJSON encodes body as pretty JSON with HTML escaping disabled.
// Encoding errors are swallowed: headers + status are already written
// by the time Encode fails, so there's nothing useful to return; chi's
// Recoverer middleware surfaces the underlying io error.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}
