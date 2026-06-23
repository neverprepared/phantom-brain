package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// osExporter is the slice of *osearch.Client the snapshot builder
// needs. Defined as an interface so the debouncer can be unit-tested
// with a fake exporter that records calls instead of touching OS.
type osExporter interface {
	Export(ctx context.Context, opts osearch.ExportOptions) (osearch.ExportManifest, error)
}

// BuildSnapshotFromOS is Phase 6's snapshot builder. It replaces the
// reflink-the-Wiki-tree pipeline (BuildSnapshot) with an OS bulk
// scroll into a sqlite-vec + FTS5 tarball — the daemon no longer
// maintains a local filesystem copy of the canonical content.
//
// Steps (mirrors BuildSnapshot's atomicity ordering):
//
//  1. Allocate next gen = current + 1.
//  2. Call osearch.Export targeted at _published/snapshot-<gen>.tar.zst
//     directly. Export does its own temp + rename so a crashed build
//     leaves no half-written tarball under the published name.
//  3. Compose a SnapshotManifest from Export's return + write the
//     .sha256 + .manifest.json sidecars.
//  4. Atomically bump .gen-counter so the new snapshot becomes the
//     advertised current.
//  5. Prune snapshots older than retentionGens.
//
// Returns (nil, nil) when the export found zero docs — there is
// nothing useful to publish and bumping the gen counter would just
// make every agent re-download an empty cache. Caller treats that
// as a benign "nothing to do".
func BuildSnapshotFromOS(ctx context.Context, dataDir DataDir, exp osExporter, profile, vaultName string, retentionGens int) (*SnapshotInfo, error) {
	if exp == nil {
		return nil, errors.New("server: BuildSnapshotFromOS requires an exporter")
	}
	if retentionGens <= 0 {
		retentionGens = 30
	}

	current, err := ReadGenCounter(dataDir, profile, vaultName)
	if err != nil {
		return nil, err
	}
	next := current + 1

	publishedDir := dataDir.PublishedDir(profile, vaultName)
	if err := os.MkdirAll(publishedDir, 0o755); err != nil {
		return nil, fmt.Errorf("server: mkdir published: %w", err)
	}
	tarballPath := filepath.Join(publishedDir, fmt.Sprintf("snapshot-%d.tar.zst", next))

	manifest, err := exp.Export(ctx, osearch.ExportOptions{
		Profile:    profile,
		Vault:      vaultName,
		OutputPath: tarballPath,
		// IncludeRawOnly stays false: the snapshot is for agent
		// recall, and raw-only docs miss the synthesised body /
		// reliability / topic — including them muddies recall results.
	})
	if err != nil {
		return nil, fmt.Errorf("server: osearch export: %w", err)
	}
	if manifest.NumDocs == 0 {
		// Nothing to publish — clean up the (possibly empty) tarball
		// Export still wrote and return without bumping the counter.
		_ = os.Remove(tarballPath)
		return nil, nil
	}

	sideManifest := SnapshotManifest{
		Profile:   profile,
		Vault:     vaultName,
		Gen:       next,
		SHA256:    manifest.SHA256,
		SizeBytes: manifest.SizeBytes,
		BuiltAt:   manifest.GeneratedAt.UTC().Format(time.RFC3339),
	}
	if err := writeSnapshotSidecars(publishedDir, next, sideManifest); err != nil {
		return nil, fmt.Errorf("server: snapshot sidecars: %w", err)
	}
	if err := writeGenCounter(dataDir, profile, vaultName, next); err != nil {
		return nil, fmt.Errorf("server: bump gen counter: %w", err)
	}

	if err := pruneSnapshots(dataDir, profile, vaultName, retentionGens); err != nil {
		return &SnapshotInfo{Manifest: sideManifest, TarballPath: tarballPath},
			fmt.Errorf("server: snapshot prune: %w", err)
	}
	return &SnapshotInfo{Manifest: sideManifest, TarballPath: tarballPath}, nil
}

// SnapshotDebouncer batches snapshot rebuild triggers so a burst of
// synth-job completions produces ONE rebuild, not N. Per-vault state
// — a Trigger for (profile A, vault X) doesn't reset the timer for
// (profile B, vault Y).
//
// Lifecycle: NewSnapshotDebouncer constructs; Start(ctx) spawns the
// per-vault timers as needed; Trigger publishes a request; ctx
// cancellation stops every in-flight timer.
type SnapshotDebouncer struct {
	build  func(ctx context.Context, profile, vault string) error
	delay  time.Duration
	logger snapshotLogger

	ctx   context.Context

	pending chan VaultKey
}

// snapshotLogger is a tiny interface so the debouncer doesn't import
// log/slog directly — keeps the test setup minimal and lets callers
// pass any logger that quacks like slog.Default.
type snapshotLogger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// NewSnapshotDebouncer constructs a debouncer. `delay` is the
// quiet-window — Trigger calls within delay of each other for the
// same vault collapse into a single rebuild fired `delay` after the
// LAST trigger.
func NewSnapshotDebouncer(build func(ctx context.Context, profile, vault string) error, delay time.Duration, logger snapshotLogger) *SnapshotDebouncer {
	if delay <= 0 {
		delay = 60 * time.Second
	}
	return &SnapshotDebouncer{
		build:   build,
		delay:   delay,
		logger:  logger,
		pending: make(chan VaultKey, 256),
	}
}

// Start spawns the dispatcher goroutine. ctx cancellation drains
// pending triggers and exits.
func (d *SnapshotDebouncer) Start(ctx context.Context) {
	d.ctx = ctx
	go d.dispatch(ctx)
}

// Trigger requests a snapshot rebuild for (profile, vault). Non-
// blocking; overflow drops the trigger (the next Trigger will catch
// it). Idempotent — the debouncer collapses repeats.
func (d *SnapshotDebouncer) Trigger(profile, vault string) {
	select {
	case d.pending <- VaultKey{Profile: profile, Vault: vault}:
	default:
		// Channel full — caller's a synth worker firing faster than
		// we can route. Drop is fine because the dispatcher's per-
		// vault timer will pick up the next one within delay.
	}
}

func (d *SnapshotDebouncer) dispatch(ctx context.Context) {
	// Per-vault timer registry. A new Trigger resets the timer; on
	// fire the dispatcher spawns the rebuild and clears the entry.
	timers := map[VaultKey]*time.Timer{}
	defer func() {
		for _, t := range timers {
			t.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			if d.logger != nil {
				d.logger.Info("phantom-brain: snapshot debouncer exiting")
			}
			return
		case k := <-d.pending:
			if t, ok := timers[k]; ok {
				t.Reset(d.delay)
				continue
			}
			key := k
			timers[k] = time.AfterFunc(d.delay, func() {
				if err := d.build(ctx, key.Profile, key.Vault); err != nil {
					if d.logger != nil {
						d.logger.Warn("phantom-brain: snapshot rebuild failed",
							"vault", key.String(), "err", err.Error())
					}
				}
			})
		}
	}
}
