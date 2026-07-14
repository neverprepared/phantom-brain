// Package mart projects slices of the phantom-brain System of Record into
// Obsidian-shaped markdown directories ("marts"). A mart is a Tier-6
// materialization: a derived, droppable projection, never truth.
//
// Boundary: this package talks to the brain ONLY through internal/brain (the
// public HTTP client). It must NOT import internal/pgstore or internal/server
// — a mart is an integration, and the memory core stays ignorant of it.
package mart

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// nameRe bounds a mart name to a safe single filesystem segment — it becomes
// both the registry filename (<name>.toml) and appears in user-facing output.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Filters selects which SoR records a mart projects. Empty/zero fields mean
// "no constraint on that facet". Multi-valued facets match a record that
// carries ANY of the listed values (array-overlap semantics daemon-side).
type Filters struct {
	Kinds       []string `toml:"kinds,omitempty"`
	Tags        []string `toml:"tags,omitempty"`
	Sources     []string `toml:"sources,omitempty"`
	Topic       string   `toml:"topic,omitempty"`
	Reliability []string `toml:"reliability,omitempty"`
	// Synthesised is a pointer so an unset value defers to the daemon default
	// (true — marts render distilled bodies). Set false to project the
	// pre-synth backlog.
	Synthesised *bool `toml:"synthesised,omitempty"`
}

// Spec is one mart definition, persisted as <configDir>/marts/<name>.toml.
type Spec struct {
	Name    string `toml:"name"`
	Profile string `toml:"profile"`
	Vault   string `toml:"vault"`
	// Dest is an absolute directory the mart OWNS and may clean-rebuild. It
	// should be a subdirectory reserved for the mart (e.g. .../taxes/_mart),
	// never a shared vault root — the builder writes a .pbrain-mart marker and
	// refuses to wipe any directory lacking it.
	Dest string `toml:"dest"`
	// Ephemeral marts are clean-rebuilt each run (wipe + re-render); non-
	// ephemeral marts overwrite in place by deterministic filename.
	Ephemeral bool    `toml:"ephemeral"`
	Filters   Filters `toml:"filters"`
}

// Validate enforces the invariants the builder and registry rely on.
func (s Spec) Validate() error {
	if !nameRe.MatchString(s.Name) {
		return fmt.Errorf("mart name %q must be lowercase alphanumeric with -/_ (max 64 chars)", s.Name)
	}
	if strings.TrimSpace(s.Profile) == "" {
		return fmt.Errorf("mart %q: profile is required", s.Name)
	}
	if strings.TrimSpace(s.Vault) == "" {
		return fmt.Errorf("mart %q: vault is required", s.Name)
	}
	if strings.TrimSpace(s.Dest) == "" {
		return fmt.Errorf("mart %q: dest is required", s.Name)
	}
	if !filepath.IsAbs(s.Dest) {
		return fmt.Errorf("mart %q: dest %q must be an absolute path", s.Name, s.Dest)
	}
	return nil
}
