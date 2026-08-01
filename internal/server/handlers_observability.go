package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/neverprepared/phantom-brain/internal/osproject"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// Observability surface (design-review item #2):
//   - /api/brain/readyz — per-binding readiness: pings Postgres, OpenSearch,
//     and MinIO for EVERY binding. 200 when all pass, 503 when any fails.
//     Distinct from /api/brain/health (pure liveness — registry loaded).
//   - /metrics — Prometheus text-format gauges/counters for the synth
//     backlog + dead-letter depth + processed/failed/dual-write totals. No
//     Prometheus client dependency: the exposition format is emitted by hand.

// readyDep is one dependency's probe result within a binding.
type readyDep struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// readyBinding rolls up a binding's three dependency probes.
type readyBinding struct {
	Profile    string   `json:"profile"`
	Vault      string   `json:"vault"`
	Postgres   readyDep `json:"postgres"`
	OpenSearch readyDep `json:"opensearch"`
	MinIO      readyDep `json:"minio"`
}

// readyResponse is the /readyz body. Status is "ready" (200) or
// "not_ready" (503); Bindings carries the per-binding per-dependency detail.
type readyResponse struct {
	Status   string         `json:"status"`
	Bindings []readyBinding `json:"bindings"`
}

// readyProbeTimeout bounds each dependency probe so a hung dependency can't
// wedge the readiness handler past the router's own 60s timeout.
const readyProbeTimeout = 5 * time.Second

// handleReadyz is the unauthenticated readiness probe. For each binding it
// pings Postgres (pool.Ping), OpenSearch (IndexExists on the binding's
// pb_records projection — reachability + index present), and MinIO
// (EnsureBucketExists — probe only, never create). Returns 200 when every
// probe passes; 503 with per-dependency detail when any fails. Load balancers
// gate traffic on this; /health stays pure liveness.
func (d *Daemon) handleReadyz(w http.ResponseWriter, r *http.Request) {
	out := readyResponse{Status: "ready"}
	allOK := true

	for _, b := range d.registry.Vaults() {
		rb := readyBinding{Profile: b.Key.Profile, Vault: b.Key.Vault}

		rb.Postgres = d.probePostgres(r.Context(), b)
		rb.OpenSearch = d.probeOpenSearch(r.Context(), b)
		rb.MinIO = d.probeMinIO(r.Context(), b)

		if !rb.Postgres.OK || !rb.OpenSearch.OK || !rb.MinIO.OK {
			allOK = false
		}
		out.Bindings = append(out.Bindings, rb)
	}

	status := http.StatusOK
	if !allOK {
		out.Status = "not_ready"
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, out)
}

// probePostgres pings the binding's per-profile pool. A resolvePG failure
// (PG disabled / view not built) reports the dependency as not ready.
func (d *Daemon) probePostgres(ctx context.Context, b VaultBinding) readyDep {
	view, err := d.resolvePG(b)
	if err != nil {
		return readyDep{Error: err.Error()}
	}
	pctx, cancel := context.WithTimeout(ctx, readyProbeTimeout)
	defer cancel()
	if err := view.Pool().Ping(pctx); err != nil {
		return readyDep{Error: err.Error()}
	}
	return readyDep{OK: true}
}

// probeOpenSearch probes the binding's pb_records projection index via a
// lightweight exists call (also confirms cluster reachability). A missing
// index or an unreachable cluster reports not ready.
func (d *Daemon) probeOpenSearch(ctx context.Context, b VaultBinding) readyDep {
	if d.osBase == nil {
		return readyDep{Error: "opensearch not configured"}
	}
	pctx, cancel := context.WithTimeout(ctx, readyProbeTimeout)
	defer cancel()
	client := d.osBase.WithPrefix(b.Storage.IndexPrefix)
	ok, err := client.IndexExists(pctx, osproject.LogicalRecords)
	if err != nil {
		return readyDep{Error: err.Error()}
	}
	if !ok {
		return readyDep{Error: fmt.Sprintf("projection index %s missing", client.IndexName(osproject.LogicalRecords))}
	}
	return readyDep{OK: true}
}

// probeMinIO probes the binding's bucket via the shared backend's
// EnsureBucketExists (a HEAD, never a create). A missing bucket or an
// unreachable MinIO reports not ready.
func (d *Daemon) probeMinIO(ctx context.Context, b VaultBinding) readyDep {
	if d.minioBase == nil {
		return readyDep{Error: "minio not configured"}
	}
	pctx, cancel := context.WithTimeout(ctx, readyProbeTimeout)
	defer cancel()
	if err := d.minioBase.EnsureBucketExists(pctx, b.Storage.Bucket); err != nil {
		return readyDep{Error: err.Error()}
	}
	return readyDep{OK: true}
}

// handleMetrics emits the daemon's synth + dual-write meters in Prometheus
// text exposition format, by hand (no client_golang dependency). Per-binding
// gauges (pb_synth_backlog / pb_synth_dead) are queried live from the SoR;
// the process-wide counters read the SynthWorker atomics + the daemon's
// dual-write counter. Unauthenticated — the same posture as /health.
func (d *Daemon) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	fmt.Fprint(w, "# HELP pb_synth_backlog Unsynthesised records eligible for the synth sweeper (excludes dead-lettered).\n")
	fmt.Fprint(w, "# TYPE pb_synth_backlog gauge\n")
	fmt.Fprint(w, "# HELP pb_synth_dead Unsynthesised records that exhausted synth retries (dead-lettered).\n")
	fmt.Fprint(w, "# TYPE pb_synth_dead gauge\n")

	// Per-binding gauges. Order deterministically so scrapes diff cleanly.
	bindings := d.registry.Vaults()
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].Key.Profile != bindings[j].Key.Profile {
			return bindings[i].Key.Profile < bindings[j].Key.Profile
		}
		return bindings[i].Key.Vault < bindings[j].Key.Vault
	})
	for _, b := range bindings {
		backlog, dead, err := d.synthBacklogCounts(r.Context(), b)
		if err != nil {
			// A per-binding count failure must not blank the whole scrape;
			// skip this binding's gauges (Prometheus treats an absent series
			// as stale) rather than emitting a lie.
			continue
		}
		labels := fmt.Sprintf("{profile=%q,vault=%q}", b.Key.Profile, b.Key.Vault)
		fmt.Fprintf(w, "pb_synth_backlog%s %d\n", labels, backlog)
		fmt.Fprintf(w, "pb_synth_dead%s %d\n", labels, dead)
	}

	// Process-wide counters.
	var processed, failed int64
	if sw, ok := d.synth.(*SynthWorker); ok {
		processed = sw.SynthProcessedCount()
		failed = sw.SynthFailedCount()
	}
	fmt.Fprint(w, "# HELP pb_synth_processed_total Synth jobs that completed successfully since daemon start.\n")
	fmt.Fprint(w, "# TYPE pb_synth_processed_total counter\n")
	fmt.Fprintf(w, "pb_synth_processed_total %d\n", processed)

	fmt.Fprint(w, "# HELP pb_synth_failed_total Synth jobs that returned an error since daemon start.\n")
	fmt.Fprint(w, "# TYPE pb_synth_failed_total counter\n")
	fmt.Fprintf(w, "pb_synth_failed_total %d\n", failed)

	fmt.Fprint(w, "# HELP pb_dual_write_failures_total Non-fatal SoR dual-write failures since daemon start.\n")
	fmt.Fprint(w, "# TYPE pb_dual_write_failures_total counter\n")
	fmt.Fprintf(w, "pb_dual_write_failures_total %d\n", d.DualWriteFailureCount())
}

// synthBacklogCounts returns (backlog, dead) for a binding: backlog is the
// sweeper-eligible count (CountUnsynthesised minus dead), dead is the
// dead-lettered count (synth_attempts >= MaxSynthAttempts). Resolves the
// binding's PG view; returns an error when PG is disabled for it.
func (d *Daemon) synthBacklogCounts(ctx context.Context, b VaultBinding) (backlog, dead int64, err error) {
	view, err := d.resolvePG(b)
	if err != nil {
		return 0, 0, err
	}
	pctx, cancel := context.WithTimeout(ctx, readyProbeTimeout)
	defer cancel()
	q := pgstore.New(view.Pool())
	total, err := q.CountUnsynthesised(pctx, pgdb.CountUnsynthesisedParams{
		Profile: b.Key.Profile, Vault: b.Key.Vault,
	})
	if err != nil {
		return 0, 0, err
	}
	dead, err = q.CountSynthDead(pctx, pgdb.CountSynthDeadParams{
		Profile: b.Key.Profile, Vault: b.Key.Vault, MaxAttempts: MaxSynthAttempts,
	})
	if err != nil {
		return 0, 0, err
	}
	return total - dead, dead, nil
}
