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
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gofrs/flock"
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
	}

	n, err := d.registry.Load(LoadOpts{ConfigDir: opts.ConfigDir, Defaults: cfg.Defaults})
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
			r.Get("/snapshot/current", d.handleSnapshotCurrent)
			r.Get("/snapshot/{gen}", d.handleSnapshotByGen)
			r.Get("/snapshot/{gen}/tarball", d.handleSnapshotTarball)
			r.Post("/birth/claim", d.handleBirthClaim)
			r.Post("/merge/init", d.handleMergeInit)
			r.Post("/merge/complete/{uploadID}", d.handleMergeComplete)
			r.Get("/merge/{brainID}", d.handleMergeStatus)
			r.Get("/maintenance", d.handleMaintenanceGet)
			r.Post("/maintenance/{action}", d.handleMaintenance)
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
	if _, err := d.registry.Load(LoadOpts{ConfigDir: d.ConfigDir, Defaults: d.Config.Defaults}); err != nil {
		return err
	}
	added, removed := d.registry.Diff(prior)
	for _, k := range removed {
		d.Logger.Info("phantom-brain: SIGHUP draining vault", slog.String("vault", k.String()))
		d.runners.Remove(k)
	}
	for _, k := range added {
		b, ok := d.registry.LookupByVault(k)
		if !ok {
			continue // racing remove — skip
		}
		if err := EnsureCollectiveSkeleton(d.DataDir, k.Profile, k.Vault); err != nil {
			d.Logger.Warn("phantom-brain: SIGHUP collective-skeleton failed", slog.String("vault", k.String()), slog.String("err", err.Error()))
			continue
		}
		d.Logger.Info("phantom-brain: SIGHUP starting vault", slog.String("vault", k.String()))
		d.runners.Add(newVaultRunner(d.parentCtx, b, d.DataDir, d.Logger))
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
