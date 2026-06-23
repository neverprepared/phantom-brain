package brain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neverprepared/phantom-brain/internal/brain/wqueue"
	"github.com/neverprepared/phantom-brain/internal/config"
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
	writes    atomic.Int64  // bumped by RecordWrite; consumed by the checkpoint ticker + reset on every successful checkpoint
	ckptStop  context.CancelFunc
	ckptDone  chan struct{}
	closed    bool

	// Phase 6: HTTP client to the daemon. Lazily constructed at
	// Start() when the agent contract supplies API + Token; nil
	// otherwise (legacy BRAIN_VAULT_PATH mode). MCP handlers branch
	// on Client() != nil to choose POST-to-daemon vs local-only.
	client *Client

	// v3.1 (#61): write-ahead queue + connectivity state + drainer
	// goroutine. queue and conn are nil in legacy BRAIN_VAULT_PATH
	// mode — call sites MUST nil-guard before use. snapshotBuiltAt
	// records the daemon-side build timestamp of the parent_gen
	// snapshot the agent currently has; refreshed by the drainer.
	queue           *wqueue.Queue
	conn            *Connectivity
	drainStop       context.CancelFunc
	drainDone       chan struct{}
	snapshotBuiltAt time.Time
	snapshotMu      sync.RWMutex
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

	// SkipCheckpointer turns off the background checkpoint ticker.
	// Production callers leave it false; tests that don't want a
	// background goroutine ticking through fixture lifetimes set it.
	SkipCheckpointer bool

	// SkipDrainer disables the write-ahead-queue drainer goroutine.
	// Production callers leave it false; tests that don't want a
	// background ticker firing through fixtures set it. Has no effect
	// in legacy mode (no daemon client → no drainer regardless).
	SkipDrainer bool

	// CheckpointTickInterval is how often the background checkpointer
	// re-evaluates ShouldCheckpoint. Zero falls back to 60s — far
	// smaller than CheckpointMinIntervalSecs so the cadence check
	// is reactive without burning cycles. Bounded below at 1s in
	// case a test passes something silly.
	CheckpointTickInterval time.Duration
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
		Context:  opts.HeartbeatCtx,
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
	// Stamp the daemon-side built-at for the snapshot we just pulled
	// so brain_recall's staleness footer reflects reality from birth.
	if m != nil && m.ParentSnapshotBuiltAt != "" {
		if t, perr := time.Parse(time.RFC3339, m.ParentSnapshotBuiltAt); perr == nil {
			lc.snapshotBuiltAt = t
		}
	}
	// Phase 6: a Lifecycle started under the agent contract has a
	// daemon API + bearer; construct the shared HTTP client once
	// here so MCP handlers don't have to rebuild it per call.
	if opts.Agent.API != "" && opts.Agent.Token != "" {
		c, cerr := NewClient(ClientOpts{BaseURL: opts.Agent.API, Token: opts.Agent.Token})
		if cerr != nil {
			return nil, fmt.Errorf("brain: start: build daemon client: %w", cerr)
		}
		lc.client = c
		q, qerr := wqueue.Open(opts.Agent.VaultBaseDir())
		if qerr != nil {
			return nil, fmt.Errorf("brain: start: open write-ahead queue: %w", qerr)
		}
		lc.queue = q
		lc.conn = NewConnectivity()
		if !opts.SkipDrainer {
			drainParent := opts.HeartbeatCtx
			if drainParent == nil {
				drainParent = context.Background()
			}
			drainCtx, cancel := context.WithCancel(drainParent)
			lc.drainStop = cancel
			lc.drainDone = make(chan struct{})
			go lc.runDrainer(drainCtx)
		}
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
	if !opts.SkipCheckpointer {
		tick := opts.CheckpointTickInterval
		if tick <= 0 {
			tick = 60 * time.Second
		}
		if tick < time.Second {
			tick = time.Second
		}
		ckptParent := opts.HeartbeatCtx
		if ckptParent == nil {
			ckptParent = context.Background()
		}
		ckptCtx, cancel := context.WithCancel(ckptParent)
		lc.ckptStop = cancel
		lc.ckptDone = make(chan struct{})
		go lc.runCheckpointer(ckptCtx, tick)
	}
	return lc, nil
}

// Client returns the agent's daemon HTTP client, or nil when the
// Lifecycle was started without API + Token (legacy mode). MCP
// handlers branch on this to decide local-only vs POST-to-daemon.
func (l *Lifecycle) Client() *Client { return l.client }

// Queue returns the per-Lifecycle write-ahead queue. Nil in legacy
// BRAIN_VAULT_PATH mode.
func (l *Lifecycle) Queue() *wqueue.Queue {
	if l == nil {
		return nil
	}
	return l.queue
}

// Connectivity returns the per-Lifecycle daemon-reachability tracker.
// Nil in legacy mode. The returned holder's methods are nil-receiver
// tolerant.
func (l *Lifecycle) Connectivity() *Connectivity {
	if l == nil {
		return nil
	}
	return l.conn
}

// SetSnapshotBuiltAt records the daemon-side built_at timestamp for
// the snapshot this brain was birthed from (refreshed by the drainer
// each cycle so the recall footer stays accurate after rebuilds).
// Zero means unknown.
func (l *Lifecycle) SetSnapshotBuiltAt(t time.Time) {
	if l == nil {
		return
	}
	l.snapshotMu.Lock()
	defer l.snapshotMu.Unlock()
	l.snapshotBuiltAt = t
}

// SnapshotAge returns how long ago the parent snapshot was built on
// the daemon. Returns 0 when unknown.
func (l *Lifecycle) SnapshotAge(now time.Time) time.Duration {
	if l == nil {
		return 0
	}
	l.snapshotMu.RLock()
	defer l.snapshotMu.RUnlock()
	if l.snapshotBuiltAt.IsZero() {
		return 0
	}
	d := now.Sub(l.snapshotBuiltAt)
	if d < 0 {
		return 0
	}
	return d
}


// RecordWrite is the hook ingest handlers (brain_perceive,
// brain_learn, brain_attach) call after a successful write so the
// checkpoint cadence's writes-threshold has real input. Cheap
// (single atomic add); safe to call from any goroutine.
func (l *Lifecycle) RecordWrite() {
	l.writes.Add(1)
}

// WriteCount returns the current write counter. Exposed for tests
// and the manual brain_checkpoint MCP path that wants the same
// value the automatic ticker would have seen.
func (l *Lifecycle) WriteCount() int64 { return l.writes.Load() }

// runCheckpointer is the background goroutine that re-evaluates
// ShouldCheckpoint on every tick and fires Lifecycle.Checkpoint
// when the predicate clears. Resets the write counter on every
// successful (non-skipped) checkpoint so the next cadence cycle
// counts fresh activity.
//
// Exits on ctx.Done. Errors are logged but do not terminate the
// loop — a transient disk error shouldn't kill the cadence forever.
func (l *Lifecycle) runCheckpointer(ctx context.Context, interval time.Duration) {
	defer close(l.ckptDone)
	t := time.NewTicker(interval)
	defer t.Stop()
	// Drop to Debug — operators don't need to see the ticker boot/exit
	// in normal logs. The Phase 1 INFO was useful when the cadence was
	// new; in production it just generates a paired start/exit on
	// every short-lived agent (MCP servers that init + immediately
	// shutdown produce both lines back-to-back).
	l.logger.Debug("phantom-brain: checkpoint ticker started",
		slog.String("interval", interval.String()),
		slog.Int("writes_threshold", l.agent.CheckpointWrites),
	)
	for {
		select {
		case <-ctx.Done():
			l.logger.Debug("phantom-brain: checkpoint ticker exiting")
			return
		case <-t.C:
			n := int(l.writes.Load())
			res, err := l.Checkpoint(n, false)
			if err != nil {
				l.logger.Warn("phantom-brain: auto-checkpoint failed",
					slog.String("err", err.Error()))
				continue
			}
			if res != nil && !res.Skipped {
				// Reset the counter atomically: subtract the value we
				// just used so concurrent RecordWrite calls during
				// Checkpoint() aren't lost.
				l.writes.Add(-int64(n))
			}
		}
	}
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

// Agent returns the deploy-contract config the Lifecycle was started
// with. Useful for tools that need vault paths (ShipPendingDir,
// SnapshotCacheDir) without having those threaded separately into
// ServerDeps.
func (l *Lifecycle) Agent() *config.Agent { return l.agent }

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
	// Stop the drainer before Death — it would otherwise race the
	// queue close + try to POST through a half-torn-down lifecycle.
	if l.drainStop != nil {
		l.drainStop()
		l.mu.Unlock()
		<-l.drainDone
		l.mu.Lock()
	}
	if l.queue != nil {
		if err := l.queue.Close(); err != nil {
			l.logger.Warn("phantom-brain: wqueue close returned error", slog.String("err", err.Error()))
		}
	}
	// Stop the checkpoint ticker before Death() so a tick mid-pack
	// doesn't try to take the mutex we already hold.
	if l.ckptStop != nil {
		l.ckptStop()
		// Drop the mutex briefly so the goroutine can finish its
		// in-flight Checkpoint() call — otherwise it deadlocks
		// waiting on the mutex we hold here.
		l.mu.Unlock()
		<-l.ckptDone
		l.mu.Lock()
	}
	// Close the write-ahead queue before Death so the underlying
	// sqlite file is released cleanly. The drainer goroutine (Stream
	// B) will already have shut down via its own ctx; closing here
	// is just the handle release.
	if l.queue != nil {
		_ = l.queue.Close()
		l.queue = nil
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
