package mart

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Credential is a workstation-side daemon credential for one (profile, vault)
// binding — the API URL + bearer token a mart uses to reach the daemon.
type Credential struct {
	Profile string `toml:"profile"`
	Vault   string `toml:"vault"`
	API     string `toml:"api"`
	Token   string `toml:"token"`
}

// Credentials is the workstation credential store: it lets a mart resolve its
// daemon token from its own (profile, vault) instead of ambient env, so marts
// across multiple profiles can be built/synced without env-juggling and a
// launchd job needs no baked-in secret. Persisted at
// <configDir>/marts/credentials.toml (0600 — it holds bearer tokens).
type Credentials struct {
	Bindings []Credential `toml:"binding"`
}

// CredentialsPath is where the store lives.
func CredentialsPath(configDir string) string {
	return filepath.Join(configDir, "marts", "credentials.toml")
}

// LoadCredentials reads the store. A missing file is not an error — it returns
// an empty store (callers then fall back to ambient env).
func LoadCredentials(configDir string) (Credentials, error) {
	var c Credentials
	p := CredentialsPath(configDir)
	if _, err := toml.DecodeFile(p, &c); err != nil {
		if os.IsNotExist(err) {
			return Credentials{}, nil
		}
		return Credentials{}, fmt.Errorf("decode credentials %s: %w", p, err)
	}
	return c, nil
}

// SaveCredentials writes the store 0600 (bearer secrets), marts dir 0700.
func SaveCredentials(configDir string, c Credentials) error {
	dir := filepath.Join(configDir, "marts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create marts dir: %w", err)
	}
	f, err := os.OpenFile(CredentialsPath(configDir), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open credentials for write: %w", err)
	}
	defer f.Close()
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	return nil
}

// Lookup returns the credential for a binding, if present.
func (c Credentials) Lookup(profile, vault string) (Credential, bool) {
	for _, b := range c.Bindings {
		if b.Profile == profile && b.Vault == vault {
			return b, true
		}
	}
	return Credential{}, false
}

// Set upserts a credential by (profile, vault).
func (c *Credentials) Set(cred Credential) {
	for i := range c.Bindings {
		if c.Bindings[i].Profile == cred.Profile && c.Bindings[i].Vault == cred.Vault {
			c.Bindings[i] = cred
			return
		}
	}
	c.Bindings = append(c.Bindings, cred)
}

// Remove deletes a credential by (profile, vault); reports whether one was
// removed.
func (c *Credentials) Remove(profile, vault string) bool {
	for i := range c.Bindings {
		if c.Bindings[i].Profile == profile && c.Bindings[i].Vault == vault {
			c.Bindings = append(c.Bindings[:i], c.Bindings[i+1:]...)
			return true
		}
	}
	return false
}

// AgentEnv is the ambient agent-contract binding (CL_BRAIN_*), used as a
// fallback credential source when it matches the mart's (profile, vault). Kept
// as a plain struct so this package stays free of internal/config — the caller
// fills it (the CLI from config.LoadAgent, the MCP server from ServerDeps).
type AgentEnv struct {
	API, Token, Profile, Vault string
}

// ResolveCredential returns the daemon (api, token) for a spec's binding so a
// mart can reach the right tenant WITHOUT relying on ambient env. Order:
//  1. the workstation credentials store, keyed by (profile, vault);
//  2. env — but ONLY when it is bound to the same (profile, vault) and has a
//     token (a mismatched env would project the wrong tenant, so it is ignored,
//     not trusted);
//  3. a clear error. The store wins over env.
func ResolveCredential(configDir string, spec Spec, env AgentEnv) (api, token string, err error) {
	store, lerr := LoadCredentials(configDir)
	if lerr != nil {
		return "", "", lerr
	}
	if cred, ok := store.Lookup(spec.Profile, spec.Vault); ok {
		return cred.API, cred.Token, nil
	}
	if env.Profile == spec.Profile && env.Vault == spec.Vault && env.API != "" && env.Token != "" {
		return env.API, env.Token, nil
	}
	return "", "", fmt.Errorf("no credentials for %s/%s: run `pbrainctl client mart cred add` with that profile's CL_BRAIN_* exported, or export CL_BRAIN_* matching it",
		spec.Profile, spec.Vault)
}
