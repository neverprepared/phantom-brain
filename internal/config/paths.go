package config

import "path/filepath"

// VaultBaseDir returns the per-(profile, vault) root on the agent host:
//
//	$XDG_DATA_HOME/phantom-brain/{profile}/{vault}/
//
// This is the prefix every brain and the snapshot cache live under.
// See v5.0 spec §3.
func (a *Agent) VaultBaseDir() string {
	return filepath.Join(a.dataHome, "phantom-brain", a.Profile, a.Vault)
}

// BrainsRoot returns the directory that holds every brain for this
// (profile, vault) on this host. Most directly-relevant for the orphan
// sweep, which walks this directory looking for crashed brains.
//
//	$XDG_DATA_HOME/phantom-brain/{profile}/{vault}/brains/
func (a *Agent) BrainsRoot() string {
	return filepath.Join(a.VaultBaseDir(), "brains")
}

// BrainDir returns the directory for one specific brain.
//
//	$XDG_DATA_HOME/phantom-brain/{profile}/{vault}/brains/{brain_id}/
//
// Caller is responsible for the brain_id (typically a uuid4 allocated
// at birth). Passing an empty string returns BrainsRoot to make
// concatenation mistakes loud rather than silently producing a stray
// trailing slash dir.
func (a *Agent) BrainDir(brainID string) string {
	if brainID == "" {
		return a.BrainsRoot()
	}
	return filepath.Join(a.BrainsRoot(), brainID)
}

// ShipPendingDir returns the local ship-queue directory where brains
// drop their death tarballs while waiting for the daemon to be
// reachable. Bounded by CL_BRAIN_MAX_PENDING_MB in Phase 1.5+.
//
//	$XDG_DATA_HOME/phantom-brain/{profile}/{vault}/_pending/
func (a *Agent) ShipPendingDir() string {
	return filepath.Join(a.VaultBaseDir(), "_pending")
}
