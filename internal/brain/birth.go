package brain

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"

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

	// Phase 2.5: try to seed from the daemon. Falls back to the local
	// snapshot cache on network failure, and finally to greenfield if
	// neither is available. seedFromDaemon writes whatever vault/_index
	// content the snapshot contains into the brain dir AND returns the
	// parent_gen / parent_snapshot_sha256 to stamp into the manifest.
	seed := seedResult{Source: SeedGreenfield}
	if ctx := opts.Context; true {
		if ctx == nil {
			// Birth doesn't take a ctx today; use Background so the
			// HTTP call obeys the http.Client timeout.
			ctx = contextBackground()
		}
		if got, serr := seedFromDaemon(ctx, opts.Agent, dir, logger); serr == nil {
			seed = got
		} else {
			logger.Warn("phantom-brain: snapshot fetch failed; birthing greenfield",
				slog.String("err", serr.Error()))
		}
	}

	// Claim the gen so retention doesn't prune it while we're still
	// finishing birth. Best-effort: a claim failure is logged but
	// doesn't block birth — the brain is already seeded on disk.
	if seed.Gen > 0 {
		if cerr := claimGen(opts.Context, opts.Agent, brainID, seed.Gen, logger); cerr != nil {
			logger.Warn("phantom-brain: birth/claim failed (continuing)",
				slog.Uint64("gen", seed.Gen), slog.String("err", cerr.Error()))
		}
	}

	tnow := now().UTC().Format(time.RFC3339)
	m = &Manifest{
		SchemaVersion:        ManifestSchemaVersion,
		BrainID:              brainID,
		ContributorID:        ContributorID(opts.Agent.Profile, opts.Agent.Vault, hostUUID, containerNonce),
		Profile:              opts.Agent.Profile,
		Vault:                opts.Agent.Vault,
		ParentGen:             nilUint64IfZero(seed.Gen),
		ParentSnapshotSHA256:  seed.SHA256,
		ParentSnapshotBuiltAt: formatBuiltAt(seed.BuiltAt),
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
		SeedSource:           seed.Source,
		StaleSeed:            seed.Stale,
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

// seedResult bundles what seedFromDaemon teaches Birth about the
// snapshot used to seed the brain dir. Zero value means greenfield.
type seedResult struct {
	Source  SeedSource
	Gen     uint64
	SHA256  string
	Stale   bool
	BuiltAt time.Time // zero when unknown
}

// seedFromDaemon tries (in order):
//
//   1. Fetch the current snapshot from the daemon and extract it into
//      brain_dir/. On success returns SeedTarball + parent_gen +
//      parent_snapshot_sha256.
//   2. If the daemon is unreachable but a cached snapshot exists from
//      a prior successful fetch, extract that instead and stamp
//      SeedCachedStale + stale_seed = true.
//   3. Otherwise return seedResult{Source: SeedGreenfield} — caller
//      proceeds with an empty vault.
//
// Errors from the daemon (other than connectivity) propagate so the
// caller can branch on them. A daemon that reports gen=0 returns
// success with SeedGreenfield (no snapshot exists yet — birth must
// not stale-cache).
func seedFromDaemon(ctx context.Context, cfg *config.Agent, brainDir string, logger *slog.Logger) (seedResult, error) {
	cs, err := FetchSnapshotFromDaemon(ctx, cfg, logger)
	if err == nil && cs == nil {
		// Daemon returned gen=0 sentinel.
		return seedResult{Source: SeedGreenfield}, nil
	}
	if err != nil {
		// Try the cache as a stale fallback.
		if cached := mostRecentCachedSnapshot(cfg); cached != nil {
			logger.Warn("phantom-brain: daemon unreachable; seeding from stale cache",
				slog.Uint64("gen", cached.Gen), slog.String("err", err.Error()))
			if eerr := extractSnapshotTarball(cached.TarballPath, brainDir); eerr != nil {
				return seedResult{}, fmt.Errorf("brain: extract cached snapshot: %w", eerr)
			}
			return seedResult{Source: SeedCachedStale, Gen: cached.Gen, SHA256: cached.SHA256, Stale: true, BuiltAt: parseRFC3339OrZero(cached.BuiltAt)}, nil
		}
		return seedResult{}, err
	}
	if eerr := extractSnapshotTarball(cs.TarballPath, brainDir); eerr != nil {
		return seedResult{}, fmt.Errorf("brain: extract fresh snapshot: %w", eerr)
	}
	return seedResult{Source: SeedTarball, Gen: cs.Gen, SHA256: cs.SHA256, BuiltAt: parseRFC3339OrZero(cs.BuiltAt)}, nil
}

func formatBuiltAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseRFC3339OrZero(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// mostRecentCachedSnapshot returns the freshest CachedSnapshot from
// disk, or nil when the cache is empty / unreadable.
func mostRecentCachedSnapshot(cfg *config.Agent) *CachedSnapshot {
	list, err := ListCachedSnapshots(cfg)
	if err != nil || len(list) == 0 {
		return nil
	}
	c := list[0] // ListCachedSnapshots returns newest first
	return &c
}

// extractSnapshotTarball decompresses a tar.zst snapshot into dstRoot.
// Mirrors the daemon's writeStagedTarZst output shape: entries are
// relative paths (vault/Wiki/…, _index/…) so extraction reconstructs
// the brain dir layout in-place.
//
// Lightweight safety: rejects ../ entries and absolute paths. Doesn't
// share internal/server.SafeExtract because importing the server
// package into internal/brain would invert the dependency direction.
func extractSnapshotTarball(tarPath, dstRoot string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return fmt.Errorf("brain: open zstd: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		// Reject obvious shenanigans. Snapshots come from our own
		// daemon so this is belt-and-suspenders, not the primary
		// defense.
		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || hasParentEscape(clean) {
			return fmt.Errorf("brain: unsafe snapshot entry %q", hdr.Name)
		}
		target := filepath.Join(dstRoot, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			_ = out.Close()
		default:
			// Symlinks etc. are not produced by writeStagedTarZst —
			// skip silently.
		}
	}
}

// hasParentEscape returns true when a cleaned path begins with ".."
// or contains a ".." segment.
func hasParentEscape(p string) bool {
	if p == ".." || filepathStartsWith(p, "../") {
		return true
	}
	return false
}

// filepathStartsWith is OS-neutral HasPrefix on path strings.
func filepathStartsWith(p, prefix string) bool {
	return len(p) >= len(prefix) && p[:len(prefix)] == prefix
}

// claimGen POSTs /api/brain/birth/claim so retention won't prune the
// gen while we're still finishing birth. Errors bubble up to the
// caller's logger — birth still succeeds with the brain on disk.
func claimGen(ctx context.Context, cfg *config.Agent, brainID string, gen uint64, logger *slog.Logger) error {
	if ctx == nil {
		ctx = contextBackground()
	}
	client, err := NewClient(ClientOpts{BaseURL: cfg.API, Token: cfg.Token})
	if err != nil {
		return err
	}
	return client.ClaimBirth(ctx, brainID, gen, 3600)
}

// nilUint64IfZero returns nil when n is zero (so omitempty in the
// manifest JSON elides the parent_gen field for greenfield brains)
// and a *uint64 otherwise.
func nilUint64IfZero(n uint64) *uint64 {
	if n == 0 {
		return nil
	}
	return &n
}

// contextBackground avoids importing "context" twice across this
// package's helper sites — exists so other helpers can fetch a
// default Context without re-import.
func contextBackground() context.Context { return context.Background() }
