// Package server is the pbrainctl HTTP daemon: per-(profile, vault)
// reaper + synthesizer + snapshot publisher + bearer-authenticated
// API. v5.0 spec §3 (Anatomy) and §8 (API surface) are the canonical
// references.
//
// Phase 2 ships the daemon side; the agent-side cutover (wiring
// internal/brain's snapcache + shipqueue to actually call this
// daemon) is Phase 2.5.
package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// DataDir is the root of every per-(profile, vault) collective and the
// daemon-global state. Production is /var/lib/phantom-brain. Override
// with PHANTOM_BRAIN_DATA_DIR for dev / containers / non-root.
type DataDir string

// DefaultDataDir resolves PHANTOM_BRAIN_DATA_DIR if set, otherwise
// returns the production path. We DO NOT default to $HOME-relative
// paths because that would silently bind the daemon to a user account;
// a missing dir under /var/lib/ is the loud failure operators expect.
func DefaultDataDir() DataDir {
	if v := strings.TrimSpace(os.Getenv("PHANTOM_BRAIN_DATA_DIR")); v != "" {
		return DataDir(v)
	}
	return DataDir("/var/lib/phantom-brain")
}

// String exposes the underlying path for os calls that want a plain
// string.
func (d DataDir) String() string { return string(d) }

// CollectiveDir returns the per-vault collective root. Every other
// per-vault helper composes this with subdirs from v4.4 §3.
//
//	{data}/<profile>/<vault>/collective/
func (d DataDir) CollectiveDir(profile, vault string) string {
	return filepath.Join(string(d), profile, vault, "collective")
}

// VaultDir is the human-readable Wiki/Raw/queue tree inside the
// collective — the analog of an agent's brain_dir/vault/.
//
//	{collective}/vault/
func (d DataDir) VaultDir(profile, vault string) string {
	return filepath.Join(d.CollectiveDir(profile, vault), "vault")
}

// IndexDir is the per-vault SQLite + provenance store.
//
//	{collective}/_index/
func (d DataDir) IndexDir(profile, vault string) string {
	return filepath.Join(d.CollectiveDir(profile, vault), "_index")
}

// BrainsDir owns the death-payload merge lifecycle on the daemon side
// — _uploads/ (presigned multipart prefix), _staging/<brain_id>/
// (during extract), _pending/<brain_id>.tar.zst (waiting for the
// reaper), _merged/<brain_id>.json (merge records).
//
//	{collective}/brains/
func (d DataDir) BrainsDir(profile, vault string) string {
	return filepath.Join(d.CollectiveDir(profile, vault), "brains")
}

// LedgerDir holds merges.sqlite (per-vault, WAL).
//
//	{collective}/ledger/
func (d DataDir) LedgerDir(profile, vault string) string {
	return filepath.Join(d.CollectiveDir(profile, vault), "ledger")
}

// VaultLocksDir is per-vault scratch (NOT flock targets — those live
// under the daemon-global locks dir). Used for the maintenance.flag
// file among other things.
//
//	{collective}/locks/
func (d DataDir) VaultLocksDir(profile, vault string) string {
	return filepath.Join(d.CollectiveDir(profile, vault), "locks")
}

// DaemonLocksDir is the daemon-global lock directory. The
// brain-server.pid flock here is what prevents a second daemon
// instance from coming up against the same data dir.
//
//	{data}/_daemon/locks/
func (d DataDir) DaemonLocksDir() string {
	return filepath.Join(string(d), "_daemon", "locks")
}

// GlobalFlockPath is the file gofrs/flock takes at startup to guard
// the data dir against a second daemon.
//
//	{daemon}/brain-server.pid
func (d DataDir) GlobalFlockPath() string {
	return filepath.Join(d.DaemonLocksDir(), "brain-server.pid")
}

// EnsureCollectiveSkeleton makes every directory the daemon expects
// for a (profile, vault) to exist. Idempotent — safe to call on every
// reaper/synthesizer iteration if needed; called once per vault at
// startup and once during auto-init when a brand-new token first
// authenticates.
func EnsureCollectiveSkeleton(d DataDir, profile, vault string) error {
	if profile == "" || vault == "" {
		return errors.New("server: EnsureCollectiveSkeleton requires non-empty profile and vault")
	}
	dirs := []string{
		filepath.Join(d.VaultDir(profile, vault), "Wiki", "summaries"),
		filepath.Join(d.VaultDir(profile, vault), "Wiki", "entities"),
		filepath.Join(d.VaultDir(profile, vault), "Raw", "curated"),
		filepath.Join(d.VaultDir(profile, vault), "Raw", "gathered"),
		filepath.Join(d.VaultDir(profile, vault), "Raw", "attachments"),
		filepath.Join(d.VaultDir(profile, vault), "Raw", ".staging"),
		filepath.Join(d.VaultDir(profile, vault), "queue", "pending"),
		filepath.Join(d.VaultDir(profile, vault), "queue", "claimed"),
		filepath.Join(d.VaultDir(profile, vault), "queue", "done"),
		filepath.Join(d.VaultDir(profile, vault), "queue", "dead"),
		d.IndexDir(profile, vault),
		filepath.Join(d.BrainsDir(profile, vault), "_uploads"),
		filepath.Join(d.BrainsDir(profile, vault), "_staging"),
		filepath.Join(d.BrainsDir(profile, vault), "_pending"),
		filepath.Join(d.BrainsDir(profile, vault), "_merged"),
		d.LedgerDir(profile, vault),
		d.VaultLocksDir(profile, vault),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// EnsureDaemonSkeleton makes the daemon-global directories (the locks
// dir). Called once at startup before acquiring the global flock.
func EnsureDaemonSkeleton(d DataDir) error {
	return os.MkdirAll(d.DaemonLocksDir(), 0o755)
}
