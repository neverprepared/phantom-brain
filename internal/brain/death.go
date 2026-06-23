package brain

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/neverprepared/phantom-brain/internal/config"
)

// DeathOpts collects everything Death() needs. Same shape as BirthOpts
// — explicit dependencies keep the function testable and the call site
// readable.
type DeathOpts struct {
	Agent    *config.Agent
	BrainDir string
	Logger   *slog.Logger
	Now      func() time.Time
}

// DeathResult records the artifacts of a death so the MCP tool can
// echo them to the operator. Phase 6 has no payload — every write
// went to the daemon as it happened — so PayloadPath stays "" and
// PayloadSize stays 0. The shape is preserved so existing callers
// (brain_death MCP tool, ops CLI) don't need to change.
type DeathResult struct {
	BrainID     string
	PayloadPath string
	PayloadSize int64
}

// Death transitions a brain from alive to dead. Phase 6 dropped the
// death-payload tarball — agent writes shipped synchronously via the
// daemon's /api/brain/{perceive,learn,attach} POST endpoints, so
// there is nothing left to pack at shutdown.
//
// Sequence:
//
//  1. Read the manifest. If status != alive, reject — death is not
//     idempotent at the API layer; callers handle the error.
//  2. Flip status alive → shutting_down → dead, persisting between.
//  3. Log a death marker so operators can correlate process exits
//     with manifest state.
//
// Callers MUST have stopped the heartbeat goroutine before calling
// Death — the manifest writes race the alive-marker touches otherwise.
func Death(opts DeathOpts) (*DeathResult, error) {
	if opts.Agent == nil {
		return nil, errors.New("brain: Death requires a non-nil Agent")
	}
	if opts.BrainDir == "" {
		return nil, errors.New("brain: Death requires a brain directory")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	m, err := ReadManifest(opts.BrainDir)
	if err != nil {
		return nil, fmt.Errorf("brain: read manifest before death: %w", err)
	}
	if m.Status != StatusAlive {
		return nil, fmt.Errorf("brain: cannot die from status=%q (only alive)", m.Status)
	}

	m.Status = StatusShuttingDown
	if err := WriteManifest(opts.BrainDir, m); err != nil {
		return nil, fmt.Errorf("brain: mark shutting_down: %w", err)
	}
	m.Status = StatusDead
	if err := WriteManifest(opts.BrainDir, m); err != nil {
		return nil, fmt.Errorf("brain: mark dead: %w", err)
	}

	logger.Info(
		"phantom-brain: brain died (no payload — writes shipped to daemon during life)",
		slog.String("brain_id", m.BrainID),
	)
	return &DeathResult{BrainID: m.BrainID}, nil
}
