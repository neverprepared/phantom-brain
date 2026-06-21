package brain

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/neverprepared/mcp-phantom-brain/internal/config"
)

// CheckpointOpts captures the inputs to Checkpoint(). WriteCount is
// the number of mutations since the last checkpoint — Phase 1 takes it
// from the caller because the working-memory + ingest layers don't yet
// emit a write counter we can subscribe to. Phase 4 will wire that up
// properly; for now the MCP brain_checkpoint tool can pass 0 to force
// an unconditional checkpoint or a real number to participate in the
// threshold check.
type CheckpointOpts struct {
	Agent      *config.Agent
	BrainDir   string
	WriteCount int
	Force      bool
	Logger     *slog.Logger
	Now        func() time.Time
}

// CheckpointResult tells the caller whether a checkpoint was actually
// taken and where its directory landed. Skipped=true with no error
// means the should_checkpoint() predicate said no — not a failure.
type CheckpointResult struct {
	Skipped       bool
	Reason        string
	CheckpointDir string
}

// Checkpoint advances the brain's checkpoint state. In Phase 1 we
// don't actually CoW-fork the brain (reflink lands Day 4); we only
// update the manifest's last_checkpoint_at / last_checkpoint_writes
// fields and log that the daemon publish is stubbed. This is enough
// for the checkpoint cadence to be exercisable end-to-end via the
// brain_checkpoint MCP tool; the data-shipping side waits for Phase 2.
//
// ShouldCheckpoint encodes the v4.4 thresholds. Force=true bypasses
// them — wired to the MCP tool so an operator can demand a checkpoint
// for incident response without satisfying the cadence.
func Checkpoint(opts CheckpointOpts) (*CheckpointResult, error) {
	if opts.Agent == nil {
		return nil, errors.New("brain: Checkpoint requires a non-nil Agent")
	}
	if opts.BrainDir == "" {
		return nil, errors.New("brain: Checkpoint requires a brain directory")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	m, err := ReadManifest(opts.BrainDir)
	if err != nil {
		return nil, fmt.Errorf("brain: read manifest before checkpoint: %w", err)
	}
	if m.Status != StatusAlive {
		return nil, fmt.Errorf("brain: cannot checkpoint from status=%q", m.Status)
	}

	if !opts.Force {
		ok, reason := ShouldCheckpoint(m, opts.Agent, opts.WriteCount, now())
		if !ok {
			return &CheckpointResult{Skipped: true, Reason: reason}, nil
		}
	}

	tnow := now().UTC()
	cpDir := filepath.Join(opts.BrainDir, "_checkpoints", tnow.Format("20060102T150405Z"))
	if err := os.MkdirAll(cpDir, 0o755); err != nil {
		return nil, fmt.Errorf("brain: mkdir checkpoint: %w", err)
	}
	// Drop a marker file with the manifest state so an operator
	// inspecting _checkpoints/ can tell what was active at this point.
	// Real CoW snapshotting (reflink) is Day 4 work; for now this is
	// the breadcrumb. The marker is intentionally small — the daemon
	// publish is the real persistence story.
	if err := WriteManifest(cpDir, &Manifest{
		SchemaVersion:        ManifestSchemaVersion,
		BrainID:              m.BrainID,
		ContributorID:        m.ContributorID,
		Profile:              m.Profile,
		Vault:                m.Vault,
		BornAt:               m.BornAt,
		Status:               StatusAlive,
		Host:                 m.Host,
		Hostname:             m.Hostname,
		BootID:               m.BootID,
		PID:                  m.PID,
		ContainerNonce:       m.ContainerNonce,
		LastHeartbeat:        m.LastHeartbeat,
		LastCheckpointAt:     tnow.Format(time.RFC3339),
		LastCheckpointWrites: opts.WriteCount,
		SeedSource:           m.SeedSource,
		StaleSeed:            m.StaleSeed,
	}); err != nil {
		return nil, fmt.Errorf("brain: write checkpoint marker: %w", err)
	}

	m.LastCheckpointAt = tnow.Format(time.RFC3339)
	m.LastCheckpointWrites = opts.WriteCount
	if err := WriteManifest(opts.BrainDir, m); err != nil {
		return nil, fmt.Errorf("brain: persist checkpoint state: %w", err)
	}

	logger.Warn(
		"phantom-brain: checkpoint marker written locally; daemon publish is no-op until Phase 2",
		slog.String("brain_id", m.BrainID),
		slog.String("checkpoint_dir", cpDir),
	)
	return &CheckpointResult{CheckpointDir: cpDir}, nil
}

// ShouldCheckpoint implements the v4.4 §6 mtime-cutoff predicate:
//
//   - writes >= CL_BRAIN_CHECKPOINT_WRITES, AND
//   - now - last_checkpoint_at >= CL_BRAIN_CHECKPOINT_MIN_INTERVAL_SECS
//
//   - OR idle gap (now - last_checkpoint_at >= CHECKPOINT_IDLE_HOURS)
//
//   - OR age gap (now - last_checkpoint_at >= CHECKPOINT_MAX_AGE_DAYS)
//
// Any of the three age-based predicates suffices; the writes threshold
// is the steady-state trigger, the others are safety valves. Returns
// (true, "") when a checkpoint should run, (false, reason) otherwise
// so callers can surface the reason to operators.
func ShouldCheckpoint(m *Manifest, cfg *config.Agent, writes int, now time.Time) (bool, string) {
	last, err := time.Parse(time.RFC3339, m.LastCheckpointAt)
	if err != nil {
		// Manifest parse error means we're past the safety horizon —
		// the brain has no record of ever checkpointing. Take one.
		return true, ""
	}
	age := now.Sub(last)
	minInterval := time.Duration(cfg.CheckpointMinIntervalSecs) * time.Second
	idleGap := time.Duration(cfg.CheckpointIdleHours) * time.Hour
	maxAge := time.Duration(cfg.CheckpointMaxAgeDays) * 24 * time.Hour

	if age >= maxAge {
		return true, ""
	}
	if age >= idleGap {
		return true, ""
	}
	if writes >= cfg.CheckpointWrites && age >= minInterval {
		return true, ""
	}
	return false, fmt.Sprintf("writes=%d/%d age=%s/%s", writes, cfg.CheckpointWrites, age.Truncate(time.Second), minInterval)
}
