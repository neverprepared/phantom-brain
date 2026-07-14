package mart

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
)

// Registry stores mart specs as TOML under <configDir>/marts/. This is
// client-side config (the workstation that runs `mart build`), deliberately
// separate from the daemon's server.toml binding registry.
type Registry struct {
	Dir string // <configDir>/marts
}

// OpenRegistry returns a Registry rooted at <configDir>/marts. It does not
// touch the filesystem until a write; List/Load on a missing dir are benign.
func OpenRegistry(configDir string) *Registry {
	return &Registry{Dir: filepath.Join(configDir, "marts")}
}

// Path is the on-disk location of a mart spec.
func (r *Registry) Path(name string) string {
	return filepath.Join(r.Dir, name+".toml")
}

// Save writes (or overwrites) a spec. The specs hold no secrets, so the file
// is 0644; the parent dir is 0700 to match the config-dir convention.
func (r *Registry) Save(s Spec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return fmt.Errorf("create marts dir: %w", err)
	}
	f, err := os.OpenFile(r.Path(s.Name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open mart spec for write: %w", err)
	}
	defer f.Close()
	if err := toml.NewEncoder(f).Encode(s); err != nil {
		return fmt.Errorf("encode mart spec: %w", err)
	}
	return nil
}

// Load reads one spec by name. A missing spec surfaces as a clear error.
func (r *Registry) Load(name string) (Spec, error) {
	var s Spec
	p := r.Path(name)
	if _, err := toml.DecodeFile(p, &s); err != nil {
		if os.IsNotExist(err) {
			return Spec{}, fmt.Errorf("no mart named %q (looked in %s)", name, r.Dir)
		}
		return Spec{}, fmt.Errorf("decode mart spec %s: %w", p, err)
	}
	return s, nil
}

// List returns every configured spec, sorted by name. A missing marts dir
// returns an empty slice, not an error.
func (r *Registry) List() ([]Spec, error) {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read marts dir: %w", err)
	}
	var out []Spec
	for _, e := range entries {
		// credentials.toml shares the marts dir but is NOT a spec — skip it so
		// it isn't loaded as a bogus empty mart.
		if e.IsDir() || filepath.Ext(e.Name()) != ".toml" || e.Name() == "credentials.toml" {
			continue
		}
		name := e.Name()[:len(e.Name())-len(".toml")]
		s, err := r.Load(name)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Remove deletes a mart's spec file. It deliberately does NOT touch the
// materialized dest directory — dropping a mart's definition is separate from
// deleting its rendered output (the caller prints a hint about the latter).
func (r *Registry) Remove(name string) error {
	if err := os.Remove(r.Path(name)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no mart named %q", name)
		}
		return err
	}
	return nil
}

// CursorPath is where an incremental Sync's resume cursor lives.
func (r *Registry) CursorPath(name string) string {
	return filepath.Join(r.Dir, name+".cursor")
}

// LoadCursor reads a mart's Sync cursor. A missing cursor is not an error —
// it returns the zero Cursor (Sync then reads from the beginning).
func (r *Registry) LoadCursor(name string) (Cursor, error) {
	var c Cursor
	b, err := os.ReadFile(r.CursorPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return Cursor{}, nil
		}
		return Cursor{}, fmt.Errorf("read mart cursor: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Cursor{}, fmt.Errorf("decode mart cursor %s: %w", r.CursorPath(name), err)
	}
	return c, nil
}

// SaveCursor persists a mart's Sync cursor (0644 — it holds no secret).
func (r *Registry) SaveCursor(name string, c Cursor) error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return fmt.Errorf("create marts dir: %w", err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode mart cursor: %w", err)
	}
	if err := os.WriteFile(r.CursorPath(name), b, 0o644); err != nil {
		return fmt.Errorf("write mart cursor: %w", err)
	}
	return nil
}

// RemoveCursor deletes a mart's Sync cursor (idempotent). Used to force the
// next Sync to re-read from the beginning.
func (r *Registry) RemoveCursor(name string) error {
	if err := os.Remove(r.CursorPath(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
