package brain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/neverprepared/mcp-phantom-brain/internal/config"
)

// Lifecycle owns a single brain for the duration of an MCP server
// process. Construct via Start(); pass to MCP tools that need to read
// the manifest or trigger a checkpoint/death; call Shutdown() during
// graceful exit. Concurrency-safe: every method takes the internal
// mutex briefly to read/swap state.
//
// What Lifecycle does NOT do:
//
//   - Start the heartbeat goroutine — that's Day 3 work. For now
//     Start() just births the brain and returns; the heartbeat field
//     is reserved for Day 3 wiring without changing the surface.
//   - Recovery sweep — also Day 3.
//   - Ship the death payload over HTTP — Phase 2.
type Lifecycle struct {
	agent    *config.Agent
	platform Platform
	logger   *slog.Logger

	mu        sync.Mutex
	brainDir  string
	manifest  *Manifest
	heartbeat *Heartbeat
	closed    bool
}

// StartOpts narrows what callers must supply to instantiate a
// Lifecycle. Logger is required even if it's slog.New(slog.DiscardHandler)
// — we'd rather fail loud at construction than at the first warning.
//
// HeartbeatCtx scopes the heartbeat goroutine; pass the process root
// context so the goroutine dies with the process. SkipHeartbeat is for
// tests that don't want a background ticker firing through their
// fixture lifetimes — production callers leave it false.
type StartOpts struct {
	Agent         *config.Agent
	Platform      Platform
	Logger        *slog.Logger
	HeartbeatCtx  context.Context
	SkipHeartbeat bool
}

// Start births (or rebinds) a brain and returns the running Lifecycle.
// The brain dir and a copy of the manifest are exposed via getters
// for the MCP server to wire into its tool deps (specifically VaultDir
// points at brainDir/vault).
func Start(opts StartOpts) (*Lifecycle, error) {
	if opts.Agent == nil {
		return nil, errors.New("brain: Start requires a non-nil Agent")
	}
	if opts.Platform == nil {
		return nil, errors.New("brain: Start requires a non-nil Platform")
	}
	if opts.Logger == nil {
		return nil, errors.New("brain: Start requires a non-nil Logger")
	}

	dir, m, err := Birth(BirthOpts{
		Agent:    opts.Agent,
		Platform: opts.Platform,
		Logger:   opts.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("brain: start: %w", err)
	}
	lc := &Lifecycle{
		agent:    opts.Agent,
		platform: opts.Platform,
		logger:   opts.Logger,
		brainDir: dir,
		manifest: m,
	}
	if !opts.SkipHeartbeat {
		hbCtx := opts.HeartbeatCtx
		if hbCtx == nil {
			hbCtx = context.Background()
		}
		hb, hberr := StartHeartbeat(hbCtx, HeartbeatOpts{
			BrainDir: dir,
			Interval: time.Duration(opts.Agent.HeartbeatIntervalSecs) * time.Second,
			Logger:   opts.Logger,
		})
		if hberr != nil {
			return nil, fmt.Errorf("brain: start heartbeat: %w", hberr)
		}
		lc.heartbeat = hb
	}
	return lc, nil
}

// BrainDir returns the on-disk root of the running brain. Callers
// (notably the MCP server bootstrap) compose brainDir/vault as the
// VaultDir injected into tool deps. Stable for the Lifecycle's
// lifetime so MCP tools may cache it.
func (l *Lifecycle) BrainDir() string { return l.brainDir }

// VaultDir returns brainDir/vault — the path the MCP tools should
// treat as their vault root. Exposed so call sites don't have to know
// the internal layout.
func (l *Lifecycle) VaultDir() string { return l.brainDir + "/vault" }

// Snapshot returns a defensive copy of the current manifest. Used by
// brain_status (Day 4) and any tool that wants to report identity.
func (l *Lifecycle) Snapshot() Manifest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return *l.manifest
}

// Checkpoint runs the checkpoint flow and updates the in-memory
// manifest if it succeeded. Skipped checkpoints (cadence not met) are
// not an error.
func (l *Lifecycle) Checkpoint(writeCount int, force bool) (*CheckpointResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, errors.New("brain: lifecycle already shut down")
	}
	res, err := Checkpoint(CheckpointOpts{
		Agent:      l.agent,
		BrainDir:   l.brainDir,
		WriteCount: writeCount,
		Force:      force,
		Logger:     l.logger,
	})
	if err != nil {
		return nil, err
	}
	if !res.Skipped {
		// Reload manifest so future Snapshot() calls reflect the new
		// LastCheckpointAt without us having to mirror the field math.
		m, rerr := ReadManifest(l.brainDir)
		if rerr == nil {
			l.manifest = m
		}
	}
	return res, nil
}

// Shutdown transitions the brain to dead and writes the payload to
// the local ship queue. Idempotent: calling twice returns
// errAlreadyShutDown the second time so MCP signal handlers can call
// it freely. Heartbeat goroutine teardown lands Day 3 — for now
// Shutdown only runs the death flow.
func (l *Lifecycle) Shutdown(ctx context.Context) (*DeathResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, errAlreadyShutDown
	}
	l.closed = true
	// Stop the heartbeat before packing the death payload so the
	// alive-marker touches don't race the tar walk and so the flock is
	// released before this brain becomes a sweep candidate.
	if l.heartbeat != nil {
		if err := l.heartbeat.Stop(); err != nil {
			l.logger.Warn("phantom-brain: heartbeat stop returned error", slog.String("err", err.Error()))
		}
	}
	res, err := Death(DeathOpts{
		Agent:    l.agent,
		BrainDir: l.brainDir,
		Logger:   l.logger,
		Now:      func() time.Time { return time.Now() },
	})
	if err != nil {
		// Best-effort: even on partial failure (e.g. manifest write
		// after payload pack) we keep closed=true so the next signal
		// doesn't retry and create duplicate payloads.
		return nil, err
	}
	return res, nil
}

// errAlreadyShutDown signals a duplicate Shutdown call. Exposed via
// IsAlreadyShutDown so callers can suppress the warning at the call
// site instead of string-matching.
var errAlreadyShutDown = errors.New("brain: lifecycle already shut down")

// IsAlreadyShutDown reports whether err means the lifecycle has
// already been shut down. Useful in signal handlers that may fire
// more than once during graceful exit.
func IsAlreadyShutDown(err error) bool { return errors.Is(err, errAlreadyShutDown) }
