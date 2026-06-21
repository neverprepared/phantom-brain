// Package config owns the resolution of pbrainctl's configuration surface.
//
// Phase 0 only loads the AGENT-side configuration — what `pbrainctl mcp`
// needs at startup to bind to a (profile, vault) and locate its brain
// directory. Daemon-side configuration (server.toml + per-vault TOML
// overlays + auth.toml registry) lands with Phase 2 in this same
// package, alongside the FastAPI-replacement HTTP code in internal/server.
//
// The deploy contract is documented in the v5.0 spec §4. Required env
// vars are validated at LoadAgent() time; missing values surface as a
// single error listing every missing field, not a piecemeal series of
// errors during startup.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Agent is the resolved configuration for a single pbrainctl-mcp
// process. Values come from environment variables (the deploy contract);
// defaults match v5.0 §4.
type Agent struct {
	// Required (every process must set these).
	API     string // CL_BRAIN_API           — daemon URL
	Token   string // CL_BRAIN_API_TOKEN     — bearer; daemon resolves to (profile, vault)
	Profile string // CL_WORKSPACE_PROFILE   — must match the token's profile
	Vault   string // CL_BRAIN_VAULT         — must match the token's vault

	// Optional.
	CollectivePath string // CL_BRAIN_COLLECTIVE_PATH — local FS path to collective; enables reflink + zero-API attachment reads
	BrainID        string // CL_BRAIN_ID            — if set, rebind to an existing brain dir; otherwise birth allocates

	// Tunables. Defaults from v5.0 §4. Always resolved (no Optional sentinel) —
	// callers can read them without nil checks.
	CheckpointWrites          int   // CL_BRAIN_CHECKPOINT_WRITES (50)
	CheckpointMinIntervalSecs int   // CL_BRAIN_CHECKPOINT_MIN_INTERVAL_SECS (300)
	CheckpointIdleHours       int   // CL_BRAIN_CHECKPOINT_IDLE_HOURS (6)
	CheckpointMaxAgeDays      int   // CL_BRAIN_CHECKPOINT_MAX_AGE_DAYS (7)
	HeartbeatIntervalSecs     int   // CL_BRAIN_HEARTBEAT_INTERVAL_SECS (30)
	OrphanThresholdSecs       int   // CL_BRAIN_ORPHAN_THRESHOLD_SECS (300)
	MaxPendingMB              int   // CL_BRAIN_MAX_PENDING_MB (5000)
	DiskPreflightCeilingBytes int64 // CL_BRAIN_DISK_PREFLIGHT_CEILING_BYTES (10 GiB)

	// dataHome is the resolved XDG_DATA_HOME (or its HOME-based fallback).
	// Captured at load time so tests can override it deterministically.
	dataHome string
}

// LoadAgent reads the agent contract from the process environment and
// returns a populated Agent. Missing required fields surface as a
// single error enumerating every missing var.
func LoadAgent() (*Agent, error) {
	return loadAgentFrom(getenv)
}

// getenv is a tiny lookup so tests can swap a fake environment.
type lookupFunc func(string) string

func getenv(k string) string { return os.Getenv(k) }

func loadAgentFrom(lookup lookupFunc) (*Agent, error) {
	a := &Agent{
		API:            strings.TrimSpace(lookup("CL_BRAIN_API")),
		Token:          strings.TrimSpace(lookup("CL_BRAIN_API_TOKEN")),
		Profile:        strings.TrimSpace(lookup("CL_WORKSPACE_PROFILE")),
		Vault:          strings.TrimSpace(lookup("CL_BRAIN_VAULT")),
		CollectivePath: strings.TrimSpace(lookup("CL_BRAIN_COLLECTIVE_PATH")),
		BrainID:        strings.TrimSpace(lookup("CL_BRAIN_ID")),
	}

	var missing []string
	if a.API == "" {
		missing = append(missing, "CL_BRAIN_API")
	}
	if a.Token == "" {
		missing = append(missing, "CL_BRAIN_API_TOKEN")
	}
	if a.Profile == "" {
		missing = append(missing, "CL_WORKSPACE_PROFILE")
	}
	if a.Vault == "" {
		missing = append(missing, "CL_BRAIN_VAULT")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("config: missing required env vars: %s", strings.Join(missing, ", "))
	}

	a.CheckpointWrites = lookupInt(lookup, "CL_BRAIN_CHECKPOINT_WRITES", 50)
	a.CheckpointMinIntervalSecs = lookupInt(lookup, "CL_BRAIN_CHECKPOINT_MIN_INTERVAL_SECS", 300)
	a.CheckpointIdleHours = lookupInt(lookup, "CL_BRAIN_CHECKPOINT_IDLE_HOURS", 6)
	a.CheckpointMaxAgeDays = lookupInt(lookup, "CL_BRAIN_CHECKPOINT_MAX_AGE_DAYS", 7)
	a.HeartbeatIntervalSecs = lookupInt(lookup, "CL_BRAIN_HEARTBEAT_INTERVAL_SECS", 30)
	a.OrphanThresholdSecs = lookupInt(lookup, "CL_BRAIN_ORPHAN_THRESHOLD_SECS", 300)
	a.MaxPendingMB = lookupInt(lookup, "CL_BRAIN_MAX_PENDING_MB", 5000)
	a.DiskPreflightCeilingBytes = lookupInt64(lookup, "CL_BRAIN_DISK_PREFLIGHT_CEILING_BYTES", 10*1024*1024*1024)

	// v5.0 invariant: heartbeat threshold must be at least 10x the
	// interval so a delayed touch doesn't false-orphan a live brain.
	if a.OrphanThresholdSecs < 10*a.HeartbeatIntervalSecs {
		return nil, fmt.Errorf(
			"config: CL_BRAIN_ORPHAN_THRESHOLD_SECS (%d) must be >= 10 * CL_BRAIN_HEARTBEAT_INTERVAL_SECS (%d)",
			a.OrphanThresholdSecs, a.HeartbeatIntervalSecs,
		)
	}

	dh, err := resolveDataHome(lookup)
	if err != nil {
		return nil, err
	}
	a.dataHome = dh

	return a, nil
}

func lookupInt(lookup lookupFunc, key string, def int) int {
	raw := strings.TrimSpace(lookup(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func lookupInt64(lookup lookupFunc, key string, def int64) int64 {
	raw := strings.TrimSpace(lookup(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// resolveDataHome follows the XDG Base Directory spec:
//
//	$XDG_DATA_HOME if set
//	$HOME/.local/share otherwise
//
// Refuses to operate without one of these; we'd rather fail loud at
// startup than silently birth a brain into the process CWD.
func resolveDataHome(lookup lookupFunc) (string, error) {
	if xdg := strings.TrimSpace(lookup("XDG_DATA_HOME")); xdg != "" {
		return xdg, nil
	}
	home := strings.TrimSpace(lookup("HOME"))
	if home == "" {
		return "", errors.New("config: neither XDG_DATA_HOME nor HOME is set; cannot resolve data dir")
	}
	return filepath.Join(home, ".local", "share"), nil
}
