package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// VaultKey identifies one (profile, vault) pair across the registry,
// the runner map, and log lines. Exported so reaper/synthesizer code
// can use it as a goroutine label.
type VaultKey struct {
	Profile string
	Vault   string
}

// String formats as profile/vault. Used in log fields and operator
// output where a single string is more readable than a struct.
func (k VaultKey) String() string { return k.Profile + "/" + k.Vault }

// VaultBinding is everything the daemon knows about a single
// authenticated vault: its identity, the merged defaults, the
// bearer token operators issued for it, and the resolved storage
// handle (OS index prefix + MinIO bucket).
type VaultBinding struct {
	Key      VaultKey
	Auth     VaultAuth
	Defaults VaultDefaults
	Storage  ResolvedStorage
}

// ResolvedStorage carries the binding's final OS index prefix + MinIO
// bucket after applying StorageOverrides over the daemon defaults.
// Filled in once by Registry.Load — every write path reads this
// directly; there is no "look at overrides if set, else default"
// branch outside Load.
//
// IndexPrefix is the FULL prefix callers prepend to a logical name
// like "pb_summaries"; it already includes the daemon-global prefix.
// Bucket is the absolute MinIO bucket name.
type ResolvedStorage struct {
	IndexPrefix string
	Bucket      string
}

// Registry is the live view of every vault the daemon serves. The
// auth middleware looks up bearer tokens here; SIGHUP triggers a
// re-scan that diffs the new state against the old.
//
// Concurrency: every method takes an internal RWMutex. The auth path
// is read-heavy so RLock is the common case; SIGHUP takes a write
// lock briefly to swap the map.
type Registry struct {
	mu        sync.RWMutex
	byToken   map[string]VaultBinding
	byVault   map[VaultKey]VaultBinding
}

// NewRegistry returns an empty registry. Use Load to populate.
func NewRegistry() *Registry {
	return &Registry{
		byToken: map[string]VaultBinding{},
		byVault: map[VaultKey]VaultBinding{},
	}
}

// LookupByToken returns the binding for a bearer token. The bool
// reports presence so the auth middleware can distinguish "token
// missing" (401) from "token belongs to a vault we just removed via
// SIGHUP" (also 401 — same outcome, but logged differently).
func (r *Registry) LookupByToken(token string) (VaultBinding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.byToken[token]
	return b, ok
}

// LookupByVault returns the binding for a (profile, vault) — used by
// internal callers (reaper, synthesizer) that already know which
// vault they're working on but want the merged defaults.
func (r *Registry) LookupByVault(k VaultKey) (VaultBinding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.byVault[k]
	return b, ok
}

// Vaults returns every binding sorted by VaultKey for stable iteration
// (registry tests + /api/brain/health output benefit from determinism).
func (r *Registry) Vaults() []VaultBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]VaultBinding, 0, len(r.byVault))
	for _, b := range r.byVault {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Profile != out[j].Key.Profile {
			return out[i].Key.Profile < out[j].Key.Profile
		}
		return out[i].Key.Vault < out[j].Key.Vault
	})
	return out
}

// LoadOpts narrows what Registry.Load needs from outside this file.
//
// DefaultIndexPrefix + DefaultBucket are the daemon-global fallbacks
// applied to every binding that omits the matching field in its
// [storage_overrides] block. Empty values are valid (shared-mode
// daemon with no global prefix / no MinIO bucket configured).
type LoadOpts struct {
	ConfigDir string
	Defaults  VaultDefaults

	// DefaultIndexPrefix + DefaultBucket are the daemon-global
	// storage targets. Bindings with no [storage_overrides] inherit
	// these; bindings with an override block APPEND IndexPrefix and
	// REPLACE Bucket. Both may be empty (shared OS prefix / no MinIO).
	DefaultIndexPrefix string
	DefaultBucket      string
}

// Load walks {configDir}/profiles/*/vaults/*/auth.toml, parses each,
// merges the per-vault overrides over the global defaults, and atomically
// swaps the in-memory maps. Returns the number of vaults loaded and
// any error encountered — partial-load failure returns the error AND
// leaves the previous registry state intact (no in-place mutation).
//
// Errors during a single vault's parse stop the whole load: a typo'd
// auth.toml is operator error, and starting up with a partial vault
// set would lose tokens silently. Better to fail loud at SIGHUP / boot.
func (r *Registry) Load(opts LoadOpts) (int, error) {
	if opts.ConfigDir == "" {
		return 0, errors.New("server: Registry.Load requires a config dir")
	}
	newByToken := map[string]VaultBinding{}
	newByVault := map[VaultKey]VaultBinding{}

	profilesRoot := filepath.Join(opts.ConfigDir, "profiles")
	profiles, err := os.ReadDir(profilesRoot)
	if errors.Is(err, os.ErrNotExist) {
		// No profiles configured yet — that's an empty but valid
		// registry. The daemon serves /health and returns 401 on
		// every other endpoint.
		r.swap(newByToken, newByVault)
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("server: read %s: %w", profilesRoot, err)
	}

	for _, pe := range profiles {
		if !pe.IsDir() {
			continue
		}
		profile := pe.Name()
		vaultsRoot := filepath.Join(profilesRoot, profile, "vaults")
		vaults, err := os.ReadDir(vaultsRoot)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("server: read %s: %w", vaultsRoot, err)
		}
		for _, ve := range vaults {
			if !ve.IsDir() {
				continue
			}
			vault := ve.Name()
			overrides, auth, err := LoadVaultFiles(opts.ConfigDir, profile, vault)
			if err != nil {
				return 0, err
			}
			key := VaultKey{Profile: profile, Vault: vault}
			if _, dup := newByToken[auth.BearerToken]; dup {
				return 0, fmt.Errorf("server: duplicate bearer_token across vaults; conflict at %s", key)
			}
			if err := validateStorageOverridePrefix(overrides.StorageOverrides.IndexPrefix); err != nil {
				return 0, fmt.Errorf("server: %s: %w", key, err)
			}
			storage := ResolvedStorage{
				IndexPrefix: opts.DefaultIndexPrefix + overrides.StorageOverrides.IndexPrefix,
				Bucket:      opts.DefaultBucket,
			}
			if overrides.StorageOverrides.Bucket != "" {
				storage.Bucket = overrides.StorageOverrides.Bucket
			}
			binding := VaultBinding{
				Key:      key,
				Auth:     auth,
				Defaults: MergedDefaults(opts.Defaults, overrides),
				Storage:  storage,
			}
			newByToken[auth.BearerToken] = binding
			newByVault[key] = binding
		}
	}
	r.swap(newByToken, newByVault)
	return len(newByVault), nil
}

func (r *Registry) swap(byToken map[string]VaultBinding, byVault map[VaultKey]VaultBinding) {
	r.mu.Lock()
	r.byToken = byToken
	r.byVault = byVault
	r.mu.Unlock()
}

// Diff compares the registry against a previous snapshot of vault
// keys and returns the added / removed sets. The runner manager uses
// this on SIGHUP reload to spawn new vaultRunners or drain removed
// ones. Caller provides the prior keys explicitly so Diff is pure.
func (r *Registry) Diff(prior []VaultKey) (added, removed []VaultKey) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	priorSet := map[VaultKey]bool{}
	for _, k := range prior {
		priorSet[k] = true
	}
	currentSet := map[VaultKey]bool{}
	for k := range r.byVault {
		currentSet[k] = true
		if !priorSet[k] {
			added = append(added, k)
		}
	}
	for k := range priorSet {
		if !currentSet[k] {
			removed = append(removed, k)
		}
	}
	return added, removed
}

// ValidateStorageOverridePrefix is the exported alias of the internal
// validator. The CLI (`pbrainctl server binding create --index-prefix`)
// calls this so the CLI and the daemon's registry agree on what's a
// legal prefix.
func ValidateStorageOverridePrefix(p string) error {
	return validateStorageOverridePrefix(p)
}

// validateStorageOverridePrefix enforces the allowed character set for
// per-binding index_prefix. Empty is fine (means "no override"). Else
// only lowercase ASCII letters, digits, and underscore are permitted —
// OpenSearch tolerates a wider set but typos / shell metacharacters
// here surface as confusing 4xx much later.
func validateStorageOverridePrefix(p string) error {
	if p == "" {
		return nil
	}
	for i, r := range p {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_':
		default:
			return fmt.Errorf("storage_overrides.index_prefix %q invalid at byte %d (allowed: a-z 0-9 _)", p, i)
		}
	}
	return nil
}
