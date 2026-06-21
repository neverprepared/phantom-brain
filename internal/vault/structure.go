package vault

import (
	"fmt"
	"os"
	"path/filepath"
)

// Standard directory names inside a brain's vault/ tree.
// Matches v4.x layout so a vault on disk remains the same shape
// before and after the Go cut-over.
const (
	WikiDir            = "Wiki"
	WikiSummariesDir   = "summaries"
	WikiEntitiesDir    = "entities"
	RawDir             = "Raw"
	RawGatheredDir     = "gathered"
	RawCuratedDir      = "curated"
	RawAttachmentsDir  = "attachments"
	QueueDir           = "_queue"
	QueuePendingDir    = "pending"
	QueueDoneDir       = "done"
	IndexDir           = "_index"
)

// EnsureSkeleton creates the standard vault directory tree under
// brainVaultDir (typically <brainDir>/vault/) if any of its
// subdirectories are missing. Idempotent — calling it on an
// already-skeletoned vault is a no-op aside from a few stat()s.
//
// Does NOT create the _index dir; that lives under brainDir/_index/,
// outside the vault tree, and is owned by internal/index.
//
// Does NOT seed any files. File seeding (CLAUDE.md template, Wiki/_log.md,
// etc.) is the caller's concern.
func EnsureSkeleton(brainVaultDir string) error {
	if brainVaultDir == "" {
		return fmt.Errorf("vault: EnsureSkeleton: brainVaultDir is required")
	}
	dirs := []string{
		filepath.Join(brainVaultDir, WikiDir),
		filepath.Join(brainVaultDir, WikiDir, WikiSummariesDir),
		filepath.Join(brainVaultDir, WikiDir, WikiEntitiesDir),
		filepath.Join(brainVaultDir, RawDir),
		filepath.Join(brainVaultDir, RawDir, RawGatheredDir),
		filepath.Join(brainVaultDir, RawDir, RawCuratedDir),
		filepath.Join(brainVaultDir, RawDir, RawAttachmentsDir),
		filepath.Join(brainVaultDir, QueueDir),
		filepath.Join(brainVaultDir, QueueDir, QueuePendingDir),
		filepath.Join(brainVaultDir, QueueDir, QueueDoneDir),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("vault: mkdir %q: %w", d, err)
		}
	}
	return nil
}

// SkeletonPaths returns the list of directories EnsureSkeleton creates
// under brainVaultDir, in dependency order (parents before children).
// Exported for testing and for tools that need to walk the layout.
func SkeletonPaths(brainVaultDir string) []string {
	return []string{
		filepath.Join(brainVaultDir, WikiDir),
		filepath.Join(brainVaultDir, WikiDir, WikiSummariesDir),
		filepath.Join(brainVaultDir, WikiDir, WikiEntitiesDir),
		filepath.Join(brainVaultDir, RawDir),
		filepath.Join(brainVaultDir, RawDir, RawGatheredDir),
		filepath.Join(brainVaultDir, RawDir, RawCuratedDir),
		filepath.Join(brainVaultDir, RawDir, RawAttachmentsDir),
		filepath.Join(brainVaultDir, QueueDir),
		filepath.Join(brainVaultDir, QueueDir, QueuePendingDir),
		filepath.Join(brainVaultDir, QueueDir, QueueDoneDir),
	}
}
