package server

import (
	"errors"
	"os"
	"path/filepath"
)

// maintenance.flag is a per-vault sentinel file that pauses writes
// (operator-initiated). The flag lives under the vault's locks dir
// so SIGHUP-driven config reloads can't accidentally clobber it.
// Phase 6 kept the maintenance gate even though the file-queue +
// reaper that originally consumed it are gone — the write handlers
// in handlers_write.go still respect it.

const maintenanceFlagName = "maintenance.flag"

// InMaintenance reports whether the vault's maintenance.flag is set.
// Stat-only — never opens the file. Missing file → false, any other
// stat error is treated as "not in maintenance" to fail open (the
// operator can always re-enter if it matters).
func InMaintenance(dataDir DataDir, key VaultKey) bool {
	_, err := os.Stat(filepath.Join(dataDir.VaultLocksDir(key.Profile, key.Vault), maintenanceFlagName))
	return err == nil
}

// SetMaintenance creates the maintenance.flag. Idempotent — repeat
// calls are no-ops. Returns any I/O error so the operator sees why
// the toggle failed (e.g. read-only filesystem).
func SetMaintenance(dataDir DataDir, key VaultKey) error {
	dir := dataDir.VaultLocksDir(key.Profile, key.Vault)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, maintenanceFlagName), []byte("set\n"), 0o644)
}

// ClearMaintenance removes the maintenance.flag. Idempotent.
func ClearMaintenance(dataDir DataDir, key VaultKey) error {
	err := os.Remove(filepath.Join(dataDir.VaultLocksDir(key.Profile, key.Vault), maintenanceFlagName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
