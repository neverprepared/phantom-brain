// Package brain implements the per-process brain lifecycle described in
// the v5.0 spec §6: birth, life, checkpoint, death, crash recovery. Each
// pbrainctl-mcp process owns exactly one brain directory under
//
//	$XDG_DATA_HOME/phantom-brain/{profile}/{vault}/brains/<brain_id>/
//
// The brain dir holds the manifest, the embedded vault (Wiki/Raw/_index/
// markers/), and the per-process working-memory shard. Manifest writes
// are atomic; markers/alive is held under an advisory flock for the
// brain's lifetime so the orphan sweep can detect crashes.
//
// Phase 1 implements the local mechanics. Daemon-dependent paths
// (snapshot fetch, birth/claim ledger, death tarball ship) are stubbed
// out with warnings — see shipqueue.go and snapcache.go. The MCP server
// can run end-to-end without a Phase 2 daemon, just with greenfield
// brains that never publish their work.
package brain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/neverprepared/mcp-phantom-brain/internal/vault"
)

// ManifestSchemaVersion is bumped any time Manifest's wire schema
// changes in a way that older readers cannot tolerate. Writes always
// stamp the current version; readers reject unknown versions rather
// than silently treating new fields as defaults.
const ManifestSchemaVersion = 1

// ManifestFilename is the on-disk name inside the brain dir.
const ManifestFilename = "manifest.json"

// SeedSource describes where a brain's initial vault state came from.
// Greenfield means "no parent snapshot, blank vault" — the Phase 1
// default when the daemon is unavailable. Local means reflinked from
// the collective on the same host. Tarball means downloaded from the
// daemon. CachedStale means we fell back to the most recent local
// snapshot cache because the daemon was unreachable.
type SeedSource string

const (
	SeedGreenfield  SeedSource = "greenfield"
	SeedLocal       SeedSource = "local"
	SeedTarball     SeedSource = "tarball"
	SeedCachedStale SeedSource = "cached-stale"
)

// Status is the brain lifecycle state. Alive brains run; shutting_down
// brains are mid-death (heartbeat stopped, payload being packaged); dead
// brains have a payload in _pending/ awaiting daemon ship. The recovery
// sweep only ever transitions alive -> dead (for crashed brains).
type Status string

const (
	StatusAlive        Status = "alive"
	StatusShuttingDown Status = "shutting_down"
	StatusDead         Status = "dead"
)

// Manifest is the durable record of one brain's identity and parentage.
// Fields with `omitempty` are absent when the brain was greenfield-born
// without a daemon. The schema matches v4.4 §3 / v5.0 §3; nullable
// parent fields and the schema_version were added in Phase 1.
type Manifest struct {
	SchemaVersion int `json:"schema_version"`

	BrainID       string `json:"brain_id"`
	ContributorID string `json:"contributor_id"`
	Profile       string `json:"profile"`
	Vault         string `json:"vault"`

	// Parentage. Absent on a greenfield birth (no daemon).
	ParentGen            *uint64 `json:"parent_gen,omitempty"`
	ParentSnapshotSHA256 string  `json:"parent_snapshot_sha256,omitempty"`
	ParentSynthesisID    string  `json:"parent_synthesis_id,omitempty"`

	BornAt string `json:"born_at"`            // RFC3339
	Status Status `json:"status"`             // alive | shutting_down | dead
	Host   string `json:"host"`               // host_uuid (machine-id on Linux, IOPlatformExpertDevice on Darwin)
	Hostname        string `json:"hostname"`
	BootID          string `json:"boot_id"`          // /proc/sys/kernel/random/boot_id (Linux) | kern.boottime hash (Darwin)
	PID             int    `json:"pid"`
	ContainerNonce  string `json:"container_nonce,omitempty"` // present only if born inside a container

	LastHeartbeat         string `json:"last_heartbeat,omitempty"`         // RFC3339
	LastCheckpointAt      string `json:"last_checkpoint_at"`               // RFC3339
	LastCheckpointWrites  int    `json:"last_checkpoint_writes"`

	SeedSource SeedSource `json:"seed_source"`
	StaleSeed  bool       `json:"stale_seed,omitempty"`
}

// ManifestPath returns the on-disk location for a brain's manifest
// given its directory. Encapsulated so the rest of the package never
// hard-codes "manifest.json".
func ManifestPath(brainDir string) string {
	return filepath.Join(brainDir, ManifestFilename)
}

// WriteManifest serializes m as pretty JSON and writes it atomically to
// brainDir/manifest.json. Pretty-printed because operators read these
// directly during incidents; the few extra bytes are immaterial.
func WriteManifest(brainDir string, m *Manifest) error {
	if m == nil {
		return errors.New("brain: WriteManifest called with nil manifest")
	}
	if m.SchemaVersion == 0 {
		m.SchemaVersion = ManifestSchemaVersion
	}
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("brain: marshal manifest: %w", err)
	}
	// Trailing newline so the file is well-formed for shell tools.
	buf = append(buf, '\n')
	if err := vault.WriteAtomicFile(ManifestPath(brainDir), buf, 0o644); err != nil {
		return fmt.Errorf("brain: write manifest: %w", err)
	}
	return nil
}

// ReadManifest loads brainDir/manifest.json and returns the parsed
// struct. Returns os.ErrNotExist (wrapped) when the file is missing so
// callers can distinguish "no brain yet" from "corrupt brain".
func ReadManifest(brainDir string) (*Manifest, error) {
	raw, err := os.ReadFile(ManifestPath(brainDir))
	if err != nil {
		return nil, err // os.ErrNotExist passes through; callers errors.Is it
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("brain: parse manifest at %q: %w", brainDir, err)
	}
	if m.SchemaVersion > ManifestSchemaVersion {
		// Refuse to operate on a manifest from a newer pbrainctl. Silent
		// downgrade would discard fields and lose state.
		return nil, fmt.Errorf(
			"brain: manifest schema_version %d at %q is newer than this binary supports (%d)",
			m.SchemaVersion, brainDir, ManifestSchemaVersion,
		)
	}
	return &m, nil
}
