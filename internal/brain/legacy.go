package brain

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/neverprepared/mcp-phantom-brain/internal/config"
)

// LegacyMigrationResult summarises what MigrateLegacyVault did.
// Reported back to the operator so the one-time migration is
// auditable.
type LegacyMigrationResult struct {
	BrainID     string
	BrainDir    string
	CopiedFiles int
}

// MigrateLegacyVault copies an existing TS-era vault (the layout the
// v4.x BRAIN_VAULT_PATH produced — Wiki/, Raw/, _index/ at the top
// level) into a new brain dir under the agent's v5.0 data layout,
// stamping a manifest with seed_source = "legacy-migration".
//
// Intentionally NOT called from Birth: legacy migration is a one-time
// operator action. Auto-migrating at birth would run on every fresh
// agent process and a partial failure mid-walk could leave the brain
// in an ambiguous state.
//
// Idempotency: if a brain dir for the agent already exists, returns
// (nil, ErrLegacyMigrationAlreadyDone). Operators can force a fresh
// migration by deleting the old brain dir.
//
// The source legacy vault is NOT deleted — operators may want it
// around for verification. Recommend manual rm after the daemon's
// next snapshot picks up the migrated content.
func MigrateLegacyVault(legacyVaultPath string, cfg *config.Agent, logger *slog.Logger) (*LegacyMigrationResult, error) {
	if legacyVaultPath == "" {
		return nil, errors.New("brain: MigrateLegacyVault requires legacyVaultPath")
	}
	if cfg == nil {
		return nil, errors.New("brain: MigrateLegacyVault requires a non-nil Agent")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if _, err := os.Stat(legacyVaultPath); err != nil {
		return nil, fmt.Errorf("brain: legacy vault path not accessible: %w", err)
	}

	// Refuse if a brain already exists for this (profile, vault). The
	// operator should resolve the conflict explicitly.
	if entries, err := os.ReadDir(cfg.BrainsRoot()); err == nil && len(entries) > 0 {
		for _, e := range entries {
			if e.IsDir() {
				return nil, fmt.Errorf("%w: brain already at %s/%s",
					ErrLegacyMigrationAlreadyDone, cfg.BrainsRoot(), e.Name())
			}
		}
	}

	brainID := uuid.NewString()
	dir := cfg.BrainDir(brainID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := ensureVaultSkeleton(dir); err != nil {
		return nil, err
	}

	copied, err := copyLegacyTree(legacyVaultPath, dir, logger)
	if err != nil {
		// Roll back the partial brain dir so the next attempt starts
		// clean. Without this a failed migration would trip the
		// "brain already exists" guard above.
		_ = os.RemoveAll(dir)
		return nil, err
	}

	// Stamp the manifest. host_uuid / boot_id come from the real
	// platform — same caller can detect a fresh boot vs a re-run.
	plat := NewPlatform()
	hostUUID, _ := plat.HostUUID()
	bootID, _ := plat.BootID()
	hostname, _ := plat.Hostname()
	tnow := time.Now().UTC().Format(time.RFC3339)
	m := &Manifest{
		SchemaVersion:        ManifestSchemaVersion,
		BrainID:              brainID,
		ContributorID:        ContributorID(cfg.Profile, cfg.Vault, hostUUID, ""),
		Profile:              cfg.Profile,
		Vault:                cfg.Vault,
		BornAt:               tnow,
		Status:               StatusAlive,
		Host:                 hostUUID,
		Hostname:             hostname,
		BootID:               bootID,
		PID:                  os.Getpid(),
		LastHeartbeat:        tnow,
		LastCheckpointAt:     tnow,
		LastCheckpointWrites: 0,
		SeedSource:           SeedSource("legacy-migration"),
	}
	if err := WriteManifest(dir, m); err != nil {
		return nil, err
	}

	logger.Info(
		"phantom-brain: legacy vault migrated",
		slog.String("brain_id", brainID),
		slog.String("from", legacyVaultPath),
		slog.String("to", dir),
		slog.Int("files", copied),
	)
	return &LegacyMigrationResult{
		BrainID:     brainID,
		BrainDir:    dir,
		CopiedFiles: copied,
	}, nil
}

// ErrLegacyMigrationAlreadyDone signals a brain dir already exists
// for the (profile, vault). Callers errors.Is it to distinguish
// "you've already migrated" from "something went wrong".
var ErrLegacyMigrationAlreadyDone = errors.New("brain: legacy migration target already populated")

// copyLegacyTree walks the legacy vault and reflinks (or copies)
// every regular file into the brain's vault dir, mirroring the
// subdirectory structure. The legacy layout has Wiki/, Raw/, and
// _queue/ at the top level; we slot them under brain_dir/vault/
// without rearrangement so the daemon's reaper sees expected paths
// after the next ship.
func copyLegacyTree(srcRoot, brainDir string, logger *slog.Logger) (int, error) {
	dstVault := filepath.Join(brainDir, "vault")
	var copied int
	err := filepath.Walk(srcRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(srcRoot, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		// Skip TS-specific scratch dirs that would just clutter the
		// migrated brain. Daemon will rebuild them on demand.
		base := filepath.Base(rel)
		if base == "node_modules" || base == "dist" || base == ".git" {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dst := filepath.Join(dstVault, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := ReflinkOrCopyFile(path, dst); err != nil {
			return fmt.Errorf("copy %s -> %s: %w", path, dst, err)
		}
		copied++
		return nil
	})
	return copied, err
}
