//go:build integration

// Phase A integration coverage for per-binding Postgres resolution.
// Build-tagged OFF by default so `make test` neither compiles this file
// nor needs Docker. Run with:
//
//	GOFLAGS="-tags=sqlite_fts5,integration" go test ./internal/server/ -run PG -count=1 -v
//
// Brings up pgvector/pgvector:pg17 (per-profile SoR + River) and
// opensearchproject/opensearch:2.18.0 (per-binding projection index /
// Recaller). Proves the per-profile pool/River sharing, reload teardown,
// shutdown teardown, and the NON-FATAL disable-on-error contract.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcopensearch "github.com/testcontainers/testcontainers-go/modules/opensearch"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/osproject"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
)

const (
	pgTestImage = "pgvector/pgvector:pg17"
	osTestImage = "opensearchproject/opensearch:2.18.0"
)

// startPGForServer boots a pgvector container and returns the base
// (maintenance) DSN.
func startPGForServer(ctx context.Context, t *testing.T) string {
	t.Helper()
	const (
		dbUser = "pbrain"
		dbPass = "pbrain"
		dbName = "phantom_brain"
	)
	req := testcontainers.ContainerRequest{
		Image:        pgTestImage,
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     dbUser,
			"POSTGRES_PASSWORD": dbPass,
			"POSTGRES_DB":       dbName,
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(2 * time.Minute),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start pgvector container: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("terminate pg container: %v", err)
		}
	})
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("pg host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("pg mapped port: %v", err)
	}
	return "postgres://" + dbUser + ":" + dbPass + "@" + host + ":" + port.Port() + "/" + dbName + "?sslmode=disable"
}

// startOSForServer boots a single-node OpenSearch and returns a Client.
func startOSForServer(ctx context.Context, t *testing.T) *osearch.Client {
	t.Helper()
	ctr, err := tcopensearch.Run(ctx, osTestImage)
	if err != nil {
		t.Fatalf("start opensearch container: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("terminate os container: %v", err)
		}
	})
	addr, err := ctr.Address(ctx)
	if err != nil {
		t.Fatalf("os address: %v", err)
	}
	cfg := osearch.DefaultConfig()
	cfg.Addresses = []string{addr}
	cfg.RequestTimeout = 15 * time.Second
	cfg.Username = ctr.User
	cfg.Password = ctr.Password
	openCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c, err := osearch.Open(openCtx, cfg)
	if err != nil {
		t.Fatalf("osearch.Open: %v", err)
	}
	return c
}

// newPGTestDaemon builds a Daemon by hand (mirroring the storage_overrides
// tests) with a registry seeded from the supplied bindings and parentCtx
// wired so buildPGBindingDeps has a live context.
func newPGTestDaemon(t *testing.T, bindings ...VaultBinding) *Daemon {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := NewRegistry()
	for _, b := range bindings {
		reg.byVault[b.Key] = b
		if b.Auth.BearerToken != "" {
			reg.byToken[b.Auth.BearerToken] = b
		}
	}
	return &Daemon{
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		registry:     reg,
		bindings:     newBindingDepCache(),
		pgProfiles:   map[string]*pgProfileResources{},
		parentCtx:    ctx,
		parentCancel: cancel,
	}
}

// binding is a tiny constructor for a VaultBinding with a resolved prefix.
func binding(profile, vault, prefix string) VaultBinding {
	return VaultBinding{
		Key:     VaultKey{Profile: profile, Vault: vault},
		Storage: ResolvedStorage{IndexPrefix: prefix},
	}
}

// indexExists issues a HEAD against the prefixed index name; 200 ⇒ exists.
func indexExists(ctx context.Context, t *testing.T, c *osearch.Client, prefix, logical string) bool {
	t.Helper()
	name := osearch.IndexNameWithPrefix(prefix, logical)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "/"+name, nil)
	if err != nil {
		t.Fatalf("build HEAD request: %v", err)
	}
	resp, err := c.API().Client.Perform(req)
	if err != nil {
		t.Fatalf("index HEAD: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// riverJobTableExists checks the per-profile DB for the river_job table.
func riverJobTableExists(ctx context.Context, t *testing.T, view *pgBindingView) bool {
	t.Helper()
	var reg *string
	if err := view.Pool().QueryRow(ctx, "SELECT to_regclass('public.river_job')::text").Scan(&reg); err != nil {
		t.Fatalf("query river_job regclass: %v", err)
	}
	return reg != nil
}

// TestPGBindingResolution_Disabled proves the disabled path needs no
// containers: empty pgBaseDSN ⇒ PG nil, resolvePG ⇒ ErrPostgresDisabled,
// legacy OS/MinIO views untouched.
func TestPGBindingResolution_Disabled(t *testing.T) {
	b := binding("tctest", "main", "pba_disabled_")
	d := newPGTestDaemon(t, b)
	// No pgBaseDSN set ⇒ disabled.
	d.buildBindingDeps()

	if _, err := d.resolvePG(b); err == nil {
		t.Fatal("expected ErrPostgresDisabled when pgBaseDSN empty")
	} else if err != ErrPostgresDisabled {
		t.Fatalf("expected ErrPostgresDisabled, got %v", err)
	}
	deps, ok := d.bindings.Get(b.Key)
	if !ok || deps == nil {
		t.Fatal("binding deps should still exist (legacy views)")
	}
	if deps.PG != nil {
		t.Fatal("PG view should be nil when disabled")
	}
}

// TestPGBindingResolution_NonFatal proves an unreachable DSN does NOT
// panic or abort: the build logs + disables, resolvePG returns
// ErrPostgresDisabled, and the legacy OS view still resolves.
func TestPGBindingResolution_NonFatalUnreachable(t *testing.T) {
	ctx := context.Background()
	osc := startOSForServer(ctx, t)

	b := binding("tctest", "main", "pba_nonfatal_")
	d := newPGTestDaemon(t, b)
	d.osBase = osc
	d.osClient = osc
	// Unreachable Postgres (port 1).
	d.pgBaseDSN = "postgres://pbrain:pbrain@127.0.0.1:1/phantom_brain?sslmode=disable"

	// Must not panic / abort.
	d.buildBindingDeps()

	if _, err := d.resolvePG(b); err != ErrPostgresDisabled {
		t.Fatalf("expected ErrPostgresDisabled after unreachable PG, got %v", err)
	}
	// Legacy OS view still resolves.
	if _, err := d.resolveOS(b); err != nil {
		t.Fatalf("legacy OS view must still resolve: %v", err)
	}
}

// TestPGBindingResolution_Integration is the full happy-path + sharing +
// reload + shutdown suite against real containers.
func TestPGBindingResolution_Integration(t *testing.T) {
	ctx := context.Background()
	baseDSN := startPGForServer(ctx, t)
	osc := startOSForServer(ctx, t)

	// Provision the per-profile DB (Phase A requires `db provision` to
	// have run).
	if err := pgstore.Provision(ctx, baseDSN, "tctest"); err != nil {
		t.Fatalf("provision tctest db: %v", err)
	}

	t.Run("HappyPath", func(t *testing.T) {
		b := binding("tctest", "main", "pba_happy_")
		d := newPGTestDaemon(t, b)
		d.osBase = osc
		d.osClient = osc
		d.pgBaseDSN = baseDSN

		d.buildBindingDeps()
		t.Cleanup(d.closePGProfiles)

		view, err := d.resolvePG(b)
		if err != nil {
			t.Fatalf("resolvePG: %v", err)
		}
		if view == nil {
			t.Fatal("nil view")
		}
		if err := view.Pool().Ping(ctx); err != nil {
			t.Fatalf("pool ping: %v", err)
		}
		if !riverJobTableExists(ctx, t, view) {
			t.Fatal("river_job table missing — MigrateRiver did not run")
		}
		if !indexExists(ctx, t, osc, b.Storage.IndexPrefix, osproject.LogicalRecords) {
			t.Fatal("pb_records index missing — EnsureIndex did not run")
		}
		if view.Recaller() == nil {
			t.Fatal("nil Recaller")
		}
		if view.Projector() == nil {
			t.Fatal("nil Projector")
		}
	})

	t.Run("PerProfileSharing", func(t *testing.T) {
		b1 := binding("tctest", "main", "pba_share_main_")
		b2 := binding("tctest", "other", "pba_share_other_")
		d := newPGTestDaemon(t, b1, b2)
		d.osBase = osc
		d.osClient = osc
		d.pgBaseDSN = baseDSN

		d.buildBindingDeps()
		t.Cleanup(d.closePGProfiles)

		v1, err := d.resolvePG(b1)
		if err != nil {
			t.Fatalf("resolvePG b1: %v", err)
		}
		v2, err := d.resolvePG(b2)
		if err != nil {
			t.Fatalf("resolvePG b2: %v", err)
		}
		// Same profile ⇒ same pool + same River client pointer.
		if v1.Pool() != v2.Pool() {
			t.Error("same-profile vaults must share ONE *pgxpool.Pool")
		}
		if v1.River() != v2.River() {
			t.Error("same-profile vaults must share ONE River client")
		}
		// Different prefixes ⇒ distinct, non-nil Recaller/Projector.
		if v1.Recaller() == nil || v2.Recaller() == nil {
			t.Fatal("recallers must be non-nil")
		}
		if v1.Projector() == nil || v2.Projector() == nil {
			t.Fatal("projectors must be non-nil")
		}
		if v1.Recaller() == v2.Recaller() {
			t.Error("per-binding recallers should be distinct instances")
		}
	})

	t.Run("ShutdownClosesPools", func(t *testing.T) {
		b := binding("tctest", "main", "pba_shutdown_")
		d := newPGTestDaemon(t, b)
		d.osBase = osc
		d.osClient = osc
		d.pgBaseDSN = baseDSN

		d.buildBindingDeps()

		view, err := d.resolvePG(b)
		if err != nil {
			t.Fatalf("resolvePG: %v", err)
		}
		if err := view.Pool().Ping(ctx); err != nil {
			t.Fatalf("pool ping before close: %v", err)
		}
		// closePGProfiles mirrors what Shutdown does after StopAll.
		d.closePGProfiles()
		if err := view.Pool().Ping(ctx); err == nil {
			t.Fatal("pool should error after close")
		}
	})

	// C3 (audit): reload is now DIFF-ONLY. A second buildBindingDeps with an
	// UNCHANGED registry must NOT tear down + rebuild the profile's pool +
	// River client — the unchanged tenant keeps running untouched. This is
	// the inverse of the pre-C3 behaviour (which closed every pool on every
	// reload, disrupting all tenants on any config edit).
	t.Run("ReloadKeepsUnchangedPool", func(t *testing.T) {
		b := binding("tctest", "main", "pba_reload_")
		d := newPGTestDaemon(t, b)
		d.osBase = osc
		d.osClient = osc
		d.pgBaseDSN = baseDSN

		// First build.
		d.buildBindingDeps()
		t.Cleanup(d.closePGProfiles)
		v1, err := d.resolvePG(b)
		if err != nil {
			t.Fatalf("resolvePG (build 1): %v", err)
		}
		oldPool := v1.Pool()
		oldRiver := v1.River()
		if err := oldPool.Ping(ctx); err != nil {
			t.Fatalf("old pool ping: %v", err)
		}

		// Second build (simulating SIGHUP reload with no config change).
		d.buildBindingDeps()

		v2, err := d.resolvePG(b)
		if err != nil {
			t.Fatalf("resolvePG (build 2): %v", err)
		}
		// Unchanged profile ⇒ SAME pool + SAME River client (not rebuilt),
		// still live.
		if v2.Pool() != oldPool {
			t.Fatal("reload rebuilt an unchanged profile's pool — should be kept")
		}
		if v2.River() != oldRiver {
			t.Fatal("reload rebuilt an unchanged profile's River client — should be kept")
		}
		if err := oldPool.Ping(ctx); err != nil {
			t.Fatalf("unchanged pool should still be live after reload: %v", err)
		}
	})

	// C3: an ADDED profile is built (and its River started) on reload; a
	// REMOVED profile is closed; throughout, an unchanged profile's pool is
	// left running.
	t.Run("ReloadAddsAndRemovesProfiles", func(t *testing.T) {
		if err := pgstore.Provision(ctx, baseDSN, "tctest2"); err != nil {
			t.Fatalf("provision tctest2 db: %v", err)
		}
		b1 := binding("tctest", "main", "pba_addrm_p1_")
		d := newPGTestDaemon(t, b1)
		d.osBase = osc
		d.osClient = osc
		d.pgBaseDSN = baseDSN

		d.buildBindingDeps()
		t.Cleanup(d.closePGProfiles)
		v1, err := d.resolvePG(b1)
		if err != nil {
			t.Fatalf("resolvePG b1: %v", err)
		}
		pool1 := v1.Pool()

		// Reload: ADD a binding in a brand-new profile (tctest2).
		b2 := binding("tctest2", "main", "pba_addrm_p2_")
		d.registry.byVault[b2.Key] = b2
		d.buildBindingDeps()

		if d.pgProfiles["tctest2"] == nil {
			t.Fatal("added profile tctest2 was not built on reload")
		}
		v2, err := d.resolvePG(b2)
		if err != nil {
			t.Fatalf("resolvePG b2 after add: %v", err)
		}
		pool2 := v2.Pool()
		if err := pool2.Ping(ctx); err != nil {
			t.Fatalf("added pool2 ping: %v", err)
		}
		// Unchanged profile kept.
		if v, _ := d.resolvePG(b1); v.Pool() != pool1 {
			t.Fatal("adding a profile rebuilt the unchanged profile's pool")
		}
		if err := pool1.Ping(ctx); err != nil {
			t.Fatalf("unchanged pool1 ping after add: %v", err)
		}

		// Reload: REMOVE the tctest2 binding (mirrors reload()'s registry
		// delete). The removed profile must be closed.
		delete(d.registry.byVault, b2.Key)
		d.bindings.Delete(b2.Key)
		d.buildBindingDeps()

		if d.pgProfiles["tctest2"] != nil {
			t.Fatal("removed profile tctest2 was not dropped on reload")
		}
		if err := pool2.Ping(ctx); err == nil {
			t.Fatal("removed profile's pool should be closed after reload")
		}
		// Unchanged profile STILL kept across the remove.
		if v, _ := d.resolvePG(b1); v.Pool() != pool1 {
			t.Fatal("removing a profile rebuilt the unchanged profile's pool")
		}
		if err := pool1.Ping(ctx); err != nil {
			t.Fatalf("unchanged pool1 ping after remove: %v", err)
		}
	})
}
