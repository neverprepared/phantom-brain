package brain

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/neverprepared/phantom-brain/internal/config"
)

// RecoverySweepResult enumerates what one Recover() invocation did.
// Exposed for the brain_status MCP tool and operator inspection.
type RecoverySweepResult struct {
	// Inspected is the set of brain dirs the sweep examined (excluding
	// the current brain).
	Inspected []string
	// MarkedDead is the set of brain dirs whose manifests were
	// transitioned alive -> dead by this sweep.
	MarkedDead []string
	// Skipped maps a brain dir to the reason the sweep left it alone
	// (held flock, fresh heartbeat, manifest parse error, etc.). Used
	// for operator diagnostics — every non-MarkedDead inspection lands
	// here so the sweep's behavior is fully accounted for.
	Skipped map[string]string
	// Deleted is the set of brain dirs the GC pass os.RemoveAll'd because
	// their manifests were status=dead and aged past LocalRetentionHours.
	Deleted []string
	// DeleteSkipped maps a brain dir to the reason the GC pass left it
	// on disk despite the brain being a candidate (held flock racing the
	// delete, RemoveAll error, manifest tampered between passes, etc.).
	DeleteSkipped map[string]string
}

// RecoverOpts collects the inputs to a sweep.
type RecoverOpts struct {
	Agent    *config.Agent
	Platform Platform
	// CurrentBrainID is the brain owned by the running process. The
	// sweep never touches its own dir; the current brain's flock is
	// held by the running heartbeat so the freshness check would
	// correctly skip it anyway, but excluding it explicitly avoids
	// even probing.
	CurrentBrainID string
	Logger         *slog.Logger
	Now            func() time.Time
}

// Recover scans BrainsRoot() for siblings and transitions any that
// look crashed to status=dead. A brain is considered crashed if EITHER:
//
//   - its manifest's boot_id is non-empty and differs from the current
//     boot_id (it was alive when the host rebooted), OR
//   - we can take the markers/alive flock (no live process holds it)
//     AND the manifest's last_heartbeat is older than the orphan
//     threshold (the holding process is gone, not just SIGSTOP'd).
//
// Brains whose flock we cannot take are LEFT ALONE — they're live
// (or SIGSTOP'd, which is the same as live as far as we're concerned;
// the v4.4 spec specifically calls this out). Brains with a held
// flock but a stale heartbeat get a freshness re-check via
// Platform.ProcessAlive(pid) to distinguish "live and lagging" from
// "kernel-killed and the lock is being reaped slowly."
//
// The sweep is idempotent — running it twice on the same set produces
// the same MarkedDead outcome. Errors per brain are swallowed (logged
// + Skipped[]) so one corrupted manifest doesn't block the sweep over
// healthy siblings.
func Recover(opts RecoverOpts) (*RecoverySweepResult, error) {
	if opts.Agent == nil {
		return nil, errors.New("brain: Recover requires a non-nil Agent")
	}
	if opts.Platform == nil {
		return nil, errors.New("brain: Recover requires a non-nil Platform")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	res := &RecoverySweepResult{
		Skipped:       map[string]string{},
		DeleteSkipped: map[string]string{},
	}

	currentBoot, err := opts.Platform.BootID()
	if err != nil {
		return nil, fmt.Errorf("brain: detect boot_id during sweep: %w", err)
	}

	root := opts.Agent.BrainsRoot()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return nil, fmt.Errorf("brain: read brains root: %w", err)
	}

	orphanThreshold := time.Duration(opts.Agent.OrphanThresholdSecs) * time.Second

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		brainID := e.Name()
		if brainID == opts.CurrentBrainID {
			continue
		}
		dir := filepath.Join(root, brainID)
		res.Inspected = append(res.Inspected, dir)

		m, err := ReadManifest(dir)
		if err != nil {
			res.Skipped[dir] = fmt.Sprintf("manifest read: %v", err)
			continue
		}
		if m.Status != StatusAlive {
			res.Skipped[dir] = fmt.Sprintf("status=%s", m.Status)
			continue
		}

		// Test #1: previous-boot corpse. Distinct boot_id is
		// unambiguous — the host has rebooted since this brain was
		// born, so no process from that boot can still be running.
		if currentBoot != "" && m.BootID != "" && m.BootID != currentBoot {
			if err := markDead(dir, m); err != nil {
				res.Skipped[dir] = fmt.Sprintf("mark dead (boot mismatch): %v", err)
				continue
			}
			res.MarkedDead = append(res.MarkedDead, dir)
			logger.Warn(
				"phantom-brain: recovered crashed brain from prior boot",
				slog.String("brain_id", m.BrainID),
				slog.String("manifest_boot_id", m.BootID),
				slog.String("current_boot_id", currentBoot),
			)
			continue
		}

		// Test #2: same boot — probe the flock. If we can take it, no
		// process holds the marker. Then double-check via the heartbeat
		// freshness AND ProcessAlive(pid) so a SIGSTOP'd process isn't
		// false-orphaned.
		marker := AliveMarkerPath(dir)
		if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
			// Marker never existed (brain crashed before first
			// heartbeat). Treat as dead.
			if err := markDead(dir, m); err != nil {
				res.Skipped[dir] = fmt.Sprintf("mark dead (no marker): %v", err)
				continue
			}
			res.MarkedDead = append(res.MarkedDead, dir)
			continue
		}

		lk := flock.New(marker)
		took, err := lk.TryLock()
		if err != nil {
			res.Skipped[dir] = fmt.Sprintf("flock probe: %v", err)
			continue
		}
		if !took {
			// Some process has the flock — alive (or SIGSTOP'd, which
			// is fine — its work is paused, not lost).
			res.Skipped[dir] = "flock held"
			continue
		}
		// We took the flock — release immediately so a brain that
		// races us doesn't hit a phantom hold. Then check whether the
		// supposed owner pid is actually gone.
		_ = lk.Unlock()

		if opts.Platform.ProcessAlive(m.PID) {
			// PID is alive but didn't hold the flock — likely lost it
			// to a crash. Skip and log; only mark dead if the
			// heartbeat is also stale.
			res.Skipped[dir] = fmt.Sprintf("pid %d alive without flock (lock leak?)", m.PID)
			continue
		}

		// Heartbeat freshness check. Manifest's LastHeartbeat is
		// authoritative; the marker mtime is a backup.
		last, err := time.Parse(time.RFC3339, m.LastHeartbeat)
		if err == nil && now().Sub(last) < orphanThreshold {
			res.Skipped[dir] = fmt.Sprintf("heartbeat fresh (%s ago)", now().Sub(last).Truncate(time.Second))
			continue
		}

		if err := markDead(dir, m); err != nil {
			res.Skipped[dir] = fmt.Sprintf("mark dead (orphaned): %v", err)
			continue
		}
		res.MarkedDead = append(res.MarkedDead, dir)
		logger.Warn(
			"phantom-brain: recovered orphaned brain",
			slog.String("brain_id", m.BrainID),
			slog.Int("pid", m.PID),
			slog.String("last_heartbeat", m.LastHeartbeat),
		)
	}

	gcSweep(res, root, entries, opts, now, logger)

	return res, nil
}

// gcSweep deletes brain dirs whose manifests are status=dead and aged
// past Agent.LocalRetentionHours. It runs after the alive->dead pass so
// brains transitioned this cycle have a synthetic dead-time of "now"
// and are still well within retention — they survive at least one cycle
// before becoming GC candidates.
//
// Failures are recorded in res.DeleteSkipped but never abort the sweep.
// GC is best-effort cleanup; a transient RemoveAll failure (read-only
// mount, in-use file on a bind mount, racing sibling birth) is not
// worth blocking daemon startup over.
func gcSweep(res *RecoverySweepResult, root string, entries []os.DirEntry, opts RecoverOpts, now func() time.Time, logger *slog.Logger) {
	retention := time.Duration(opts.Agent.LocalRetentionHours) * time.Hour
	if retention <= 0 {
		return
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		brainID := e.Name()
		if brainID == opts.CurrentBrainID {
			continue
		}
		dir := filepath.Join(root, brainID)

		// Belt-and-braces: refuse to descend out of BrainsRoot even
		// though ReadDir already gave us names rooted there. Defends
		// against a future layout shift that puts snapcache/ or
		// _published/ side-by-side with brain UUIDs.
		if filepath.Dir(dir) != root {
			res.DeleteSkipped[dir] = "path assertion failed"
			continue
		}

		m, err := ReadManifest(dir)
		if err != nil {
			res.DeleteSkipped[dir] = fmt.Sprintf("manifest: %v", err)
			continue
		}
		eligible, reason := IsGCEligible(m, dir, now(), retention)
		if !eligible {
			res.DeleteSkipped[dir] = reason
			continue
		}

		// Take the flock and hold it across RemoveAll. Releasing
		// before deletion would let a sibling birth into the same
		// marker between Unlock and RemoveAll, after which we'd wipe
		// its fresh dir from underneath it. Hold to the end.
		marker := AliveMarkerPath(dir)
		var heldLock *flock.Flock
		if _, err := os.Stat(marker); err == nil {
			lk := flock.New(marker)
			took, lockErr := lk.TryLock()
			if lockErr != nil {
				res.DeleteSkipped[dir] = fmt.Sprintf("flock probe: %v", lockErr)
				continue
			}
			if !took {
				res.DeleteSkipped[dir] = "flock acquired between sweep passes"
				continue
			}
			heldLock = lk
		}

		if err := os.RemoveAll(dir); err != nil {
			res.DeleteSkipped[dir] = fmt.Sprintf("remove: %v", err)
			if heldLock != nil {
				_ = heldLock.Unlock()
			}
			continue
		}
		if heldLock != nil {
			// RemoveAll already nuked the marker file; Unlock is a
			// no-op on a deleted fd path but releases the kernel
			// lock entry for tidiness.
			_ = heldLock.Unlock()
		}
		res.Deleted = append(res.Deleted, dir)
		logger.Info(
			"phantom-brain: garbage-collected dead brain",
			slog.String("brain_id", m.BrainID),
			slog.String("reason", reason),
			slog.Duration("retention", retention),
		)
	}
}

// markDead flips the manifest's status field. Separated for
// testability and so the sweep's bookkeeping doesn't get tangled with
// IO concerns.
func markDead(dir string, m *Manifest) error {
	m.Status = StatusDead
	return WriteManifest(dir, m)
}
