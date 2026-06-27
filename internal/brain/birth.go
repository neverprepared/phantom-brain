package brain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/neverprepared/phantom-brain/internal/config"
)

// ErrDaemonUnavailable is returned by any Phase 1 path that would
// normally contact the daemon. Callers handle it by degrading
// gracefully — greenfield seed instead of snapshot, local payload
// retention instead of upload. Once Phase 2 ships the daemon this
// error will only surface during genuine outages.
var ErrDaemonUnavailable = errors.New("brain: daemon unavailable (Phase 2 not yet implemented)")

// ErrDaemonUnreachable wraps transient network failures from any
// client HTTP call (timeout, EOF, connection refused, DNS). Used by
// `pbrainctl client queue drain-now` to distinguish "tried, daemon's
// down" (exit 0) from genuine internal errors (exit 1).
var ErrDaemonUnreachable = errors.New("brain: daemon unreachable")

// BirthOpts narrows the inputs Birth() needs to a small struct so the
// signature stays stable as we add knobs in later phases.
type BirthOpts struct {
	// Agent is the resolved deploy contract (profile, vault, API URL,
	// tunables). Cannot be nil.
	Agent *config.Agent

	// Platform is the host-detection implementation. Pass NewPlatform()
	// in production; tests inject a fake.
	Platform Platform

	// Logger receives operator-facing messages. Stderr-targeted by
	// caller because MCP uses stdout for JSON-RPC. If nil, the default
	// slog logger is used.
	Logger *slog.Logger

	// Now lets tests freeze time. If nil, time.Now is used.
	Now func() time.Time

	// Context bounds the daemon snapshot fetch / claim calls during
	// birth. Nil falls back to a Background context — the http.Client
	// timeout still applies, but cancellation from the caller's
	// shutdown signal would be lost.
	Context context.Context
}

// Birth allocates a new brain or rebinds to an existing one and
// returns the brain directory + manifest. Side effects:
//
//   - Creates BrainDir(brain_id) and its vault/Raw/{gathered,curated,
//     attachments} subtree.
//   - Writes manifest.json atomically.
//   - Writes a container nonce if applicable.
//
// What Birth does NOT do in Phase 1:
//
//   - Fetch a snapshot from the daemon (stubbed; logs warning).
//   - POST to the birth/claim ledger (stubbed; greenfield never claims).
//   - Reflink from a local collective (deferred to Phase 2 + Day 4).
//   - Start the heartbeat — caller does this after Birth returns so the
//     heartbeat ctx lifetime is tied to the MCP server's, not Birth's.
//
// If opts.Agent.BrainID is set and the brain dir already exists with a
// valid manifest, Birth rebinds: returns the existing manifest, no new
// directory is created. The PID is updated to the current process.
func Birth(opts BirthOpts) (brainDir string, m *Manifest, err error) {
	if opts.Agent == nil {
		return "", nil, errors.New("brain: Birth requires a non-nil Agent")
	}
	if opts.Platform == nil {
		return "", nil, errors.New("brain: Birth requires a non-nil Platform")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	// Rebind shortcut: if CL_BRAIN_ID points at an existing brain dir,
	// we reuse it rather than birthing a new one. This is how a process
	// restart re-attaches to its working set instead of leaking corpses.
	if opts.Agent.BrainID != "" {
		dir := opts.Agent.BrainDir(opts.Agent.BrainID)
		existing, rerr := ReadManifest(dir)
		if rerr == nil {
			rebound, werr := rebind(dir, existing, opts, logger, now)
			if werr != nil {
				return "", nil, fmt.Errorf("brain: rebind to %s: %w", opts.Agent.BrainID, werr)
			}
			return dir, rebound, nil
		}
		if !errors.Is(rerr, os.ErrNotExist) {
			return "", nil, fmt.Errorf("brain: rebind read manifest at %s: %w", dir, rerr)
		}
		// Manifest absent — fall through to fresh birth, but honor the
		// caller-supplied brain id so external systems holding the id
		// can still find us.
	}

	hostUUID, herr := opts.Platform.HostUUID()
	if herr != nil {
		return "", nil, fmt.Errorf("brain: detect host_uuid: %w", herr)
	}
	bootID, berr := opts.Platform.BootID()
	if berr != nil {
		return "", nil, fmt.Errorf("brain: detect boot_id: %w", berr)
	}
	hostname, _ := opts.Platform.Hostname() // empty hostname is non-fatal

	brainID := opts.Agent.BrainID
	if brainID == "" {
		brainID = uuid.NewString()
	}
	dir := opts.Agent.BrainDir(brainID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("brain: mkdir %s: %w", dir, err)
	}
	if err := ensureVaultSkeleton(dir); err != nil {
		return "", nil, err
	}

	// Container nonce: only allocate when we're actually inside a
	// container. Two containers on the same host with the same machine-
	// id otherwise produce colliding contributor_ids.
	var containerNonce string
	if opts.Platform.InContainer() {
		nonce, nerr := readOrCreateContainerNonce(dir)
		if nerr != nil {
			return "", nil, fmt.Errorf("brain: container nonce: %w", nerr)
		}
		containerNonce = nonce
	}

	// Phase D2b: snapshots are gone. Recall + fetch are online-only
	// against the daemon's Postgres SoR, so a brain has no local read
	// cache to seed — birth is ALWAYS greenfield. No snapshot fetch, no
	// snapcache fallback, no gen claim.
	tnow := now().UTC().Format(time.RFC3339)
	m = &Manifest{
		SchemaVersion:        ManifestSchemaVersion,
		BrainID:              brainID,
		ContributorID:        ContributorID(opts.Agent.Profile, opts.Agent.Vault, hostUUID, containerNonce),
		Profile:              opts.Agent.Profile,
		Vault:                opts.Agent.Vault,
		BornAt:               tnow,
		Status:               StatusAlive,
		Host:                 hostUUID,
		Hostname:             hostname,
		BootID:               bootID,
		PID:                  os.Getpid(),
		ContainerNonce:       containerNonce,
		LastHeartbeat:        tnow,
		LastCheckpointAt:     tnow,
		LastCheckpointWrites: 0,
		SeedSource:           SeedGreenfield,
	}
	if err := WriteManifest(dir, m); err != nil {
		return "", nil, err
	}
	return dir, m, nil
}

// rebind updates an existing manifest with the new PID + boot_id and
// returns the refreshed copy. Cross-boot rebinds are allowed (the
// brain's vault content survives reboots) but logged because they
// indicate either deliberate carry-over or stale env var configuration.
func rebind(dir string, m *Manifest, opts BirthOpts, logger *slog.Logger, now func() time.Time) (*Manifest, error) {
	if m.Status == StatusDead {
		return nil, fmt.Errorf("manifest status=dead; brain %s cannot rebind", m.BrainID)
	}
	currentBoot, _ := opts.Platform.BootID()
	if currentBoot != "" && m.BootID != "" && currentBoot != m.BootID {
		logger.Warn(
			"phantom-brain: rebinding to brain born under a different boot_id",
			slog.String("brain_id", m.BrainID),
			slog.String("manifest_boot_id", m.BootID),
			slog.String("current_boot_id", currentBoot),
		)
		m.BootID = currentBoot
	}
	m.PID = os.Getpid()
	m.Status = StatusAlive
	m.LastHeartbeat = now().UTC().Format(time.RFC3339)
	if err := WriteManifest(dir, m); err != nil {
		return nil, err
	}
	return m, nil
}

// ensureVaultSkeleton makes the standard vault directory tree inside
// the brain dir. Idempotent — safe to call on every Birth (rebind or
// fresh). Mirrors vault.EnsureSkeleton but is rooted at brainDir/vault
// rather than a top-level vault path; we don't reuse vault.EnsureSkeleton
// because it writes seed files we don't want for greenfield brains
// (those come from the daemon snapshot in Phase 2).
func ensureVaultSkeleton(brainDir string) error {
	dirs := []string{
		filepath.Join(brainDir, "vault", "Raw", "curated"),
		filepath.Join(brainDir, "vault", "Raw", "gathered"),
		filepath.Join(brainDir, "vault", "Raw", "attachments"),
		filepath.Join(brainDir, "vault", "Wiki", "summaries"),
		filepath.Join(brainDir, "vault", "Wiki", "entities"),
		filepath.Join(brainDir, "_index"),
		filepath.Join(brainDir, "markers"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("brain: mkdir %s: %w", d, err)
		}
	}
	return nil
}

// readOrCreateContainerNonce returns the per-container ID stored at
// brainDir/container.nonce, creating it (16 random bytes hex) on first
// call. The nonce is part of contributor_id so two containers on the
// same host don't share an identity. Stored inside the brain dir so a
// brain dir copied to another container still records its origin.
func readOrCreateContainerNonce(brainDir string) (string, error) {
	noncePath := filepath.Join(brainDir, "container.nonce")
	if raw, err := os.ReadFile(noncePath); err == nil {
		s := string(raw)
		if s != "" {
			return s, nil
		}
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(buf[:])
	if err := os.WriteFile(noncePath, []byte(nonce), 0o644); err != nil {
		return "", err
	}
	return nonce, nil
}

