package brain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// AliveMarkerFilename is the on-disk name of the heartbeat marker
// inside markers/. Exposed for the recovery sweep, which probes the
// flock on this file to detect crashed brains.
const AliveMarkerFilename = "alive"

// AliveMarkerPath returns markers/alive inside the given brain dir.
func AliveMarkerPath(brainDir string) string {
	return filepath.Join(brainDir, "markers", AliveMarkerFilename)
}

// Heartbeat owns the per-brain liveness signal. Lifetime: created at
// Lifecycle.Start, stopped at Lifecycle.Shutdown. While running it:
//
//   - holds an exclusive advisory flock on markers/alive so the
//     recovery sweep on this or any other host can probe with TryLock
//     to tell us apart from a crashed brain, and
//   - touches the marker's mtime every interval so the sweep's
//     freshness check (last_heartbeat older than orphan threshold)
//     has something to read.
//
// Heartbeat is paranoid about cancellation: every Tick respects ctx.Done
// so Shutdown returns promptly, and Stop() is idempotent.
type Heartbeat struct {
	brainDir string
	interval time.Duration
	logger   *slog.Logger

	mu     sync.Mutex
	lock   *flock.Flock
	cancel context.CancelFunc
	done   chan struct{}
	stopped bool
}

// HeartbeatOpts is the constructor surface. Interval comes from the
// agent's CL_BRAIN_HEARTBEAT_INTERVAL_SECS — passed explicitly so
// tests can shorten it without touching the global env.
type HeartbeatOpts struct {
	BrainDir string
	Interval time.Duration
	Logger   *slog.Logger
}

// StartHeartbeat takes the alive marker's flock and launches the
// ticker goroutine. Returns an error if the flock is already held by
// another process — that signals either a stuck-but-live sibling
// brain (don't double-attach) or a corrupted lock file. The caller
// must call Stop() during shutdown.
func StartHeartbeat(ctx context.Context, opts HeartbeatOpts) (*Heartbeat, error) {
	if opts.BrainDir == "" {
		return nil, errors.New("brain: StartHeartbeat requires a brain dir")
	}
	if opts.Interval <= 0 {
		return nil, errors.New("brain: StartHeartbeat requires a positive interval")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	markerPath := AliveMarkerPath(opts.BrainDir)
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		return nil, fmt.Errorf("brain: mkdir markers/: %w", err)
	}
	// Touch the file so flock can take a hold on it. WriteFile is
	// atomic enough here — concurrent recovery sweeps that race the
	// create will either observe an empty file (fine) or our pid
	// stamp from the first tick.
	if err := os.WriteFile(markerPath, []byte{}, 0o644); err != nil {
		return nil, fmt.Errorf("brain: touch alive marker: %w", err)
	}

	lock := flock.New(markerPath)
	acquired, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("brain: flock alive marker: %w", err)
	}
	if !acquired {
		return nil, fmt.Errorf("brain: alive marker %s is already locked (another brain attached?)", markerPath)
	}

	hbCtx, cancel := context.WithCancel(ctx)
	h := &Heartbeat{
		brainDir: opts.BrainDir,
		interval: opts.Interval,
		logger:   logger,
		lock:     lock,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	// First touch happens synchronously so callers can rely on
	// last_heartbeat being current as soon as StartHeartbeat returns.
	if err := h.touch(); err != nil {
		_ = lock.Unlock()
		cancel()
		return nil, fmt.Errorf("brain: initial heartbeat touch: %w", err)
	}
	go h.run(hbCtx)
	return h, nil
}

// run is the ticker loop. Exits on ctx.Done. Each tick that fails to
// write the marker is logged but does not terminate the loop — a
// transient ENOSPC on the heartbeat shouldn't take down the whole
// brain.
func (h *Heartbeat) run(ctx context.Context) {
	defer close(h.done)
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := h.touch(); err != nil {
				h.logger.Warn(
					"phantom-brain: heartbeat touch failed",
					slog.String("brain_dir", h.brainDir),
					slog.String("err", err.Error()),
				)
			}
		}
	}
}

// touch rewrites markers/alive with the current pid + RFC3339 time so
// (a) the file's mtime advances even on filesystems where flock
// doesn't bump it, and (b) operators can `cat markers/alive` to see
// what's holding it. Manifest's LastHeartbeat is updated as a side
// effect so the recovery sweep can do freshness math without parsing
// the marker file.
func (h *Heartbeat) touch() error {
	now := time.Now().UTC().Format(time.RFC3339)
	payload := fmt.Sprintf("pid=%d\nat=%s\n", os.Getpid(), now)
	if err := os.WriteFile(AliveMarkerPath(h.brainDir), []byte(payload), 0o644); err != nil {
		return err
	}
	// Refresh manifest's LastHeartbeat. Best-effort — the marker write
	// above is what the recovery sweep actually checks; the manifest
	// field is for operators reading the manifest in-flight.
	m, err := ReadManifest(h.brainDir)
	if err != nil {
		return nil
	}
	m.LastHeartbeat = now
	return WriteManifest(h.brainDir, m)
}

// Stop terminates the ticker and releases the flock. Idempotent.
// Returns the first error encountered while releasing the lock so the
// caller can log it; the goroutine is always stopped regardless.
func (h *Heartbeat) Stop() error {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return nil
	}
	h.stopped = true
	h.cancel()
	lock := h.lock
	h.mu.Unlock()

	<-h.done
	if lock != nil {
		if err := lock.Unlock(); err != nil {
			return fmt.Errorf("brain: release alive flock: %w", err)
		}
	}
	return nil
}
