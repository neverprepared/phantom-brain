package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultConfigDir is where the daemon expects server.toml and the
// profiles/ tree. Overridable via PHANTOM_BRAIN_CONFIG_DIR.
//
// Production: ~/.config/phantom-brain-server (mounted read-only into
// the container in deployments).
func DefaultConfigDir() string {
	if v := strings.TrimSpace(os.Getenv("PHANTOM_BRAIN_CONFIG_DIR")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".config/phantom-brain-server" // last-ditch
	}
	return filepath.Join(home, ".config", "phantom-brain-server")
}

// ServerConfig mirrors server.toml. Defaults live in [defaults] and
// are applied to every vault unless overridden by its config.toml.
// Fields are exported because BurntSushi/toml uses reflection.
type ServerConfig struct {
	Server struct {
		Port     int    `toml:"port"`
		Host     string `toml:"host"`
		LogLevel string `toml:"log_level"`
	} `toml:"server"`

	Storage struct {
		// Backend selects the death-payload ship target. "local"
		// (default) stores uploads under {data}/{profile}/{vault}/
		// collective/brains/_pending/; "minio" presigns to a real
		// S3-compatible bucket. Phase 2 ships local; minio is wired
		// behind the flag but not exercised in default smoke tests.
		Backend string `toml:"backend"`

		// MinIO-only fields. Present here so a single TOML covers
		// both backends; ignored when backend = "local".
		MinIOEndpoint  string `toml:"minio_endpoint"`
		MinIOBucket    string `toml:"minio_bucket"`
		MinIOAccessKey string `toml:"minio_access_key"`
		MinIOSecretKey string `toml:"minio_secret_key"`
		MinIOUseSSL    bool   `toml:"minio_use_ssl"`
	} `toml:"storage"`

	// OpenSearch wires Phase 6's canonical content store. When the
	// section is absent the write endpoints (perceive/learn/attach/
	// trace) return 503 — daemon still serves snapshot/health/etc.
	OpenSearch OpenSearchConfig `toml:"opensearch"`

	// Capture controls raw-source archival for brain_perceive calls.
	// When enabled, the daemon's SynthWorker fetches the URL it was
	// given and stores the response bytes in MinIO so the operator
	// can recover the original page content later (web pages change
	// or disappear). See internal/server/capture.go.
	Capture CaptureConfig `toml:"capture"`

	// Postgres wires the (dormant, Phase A) per-profile System-of-Record.
	// Empty DSN ⇒ Postgres disabled and the legacy path is fully
	// untouched. See PostgresConfig + internal/server/pg_binding_views.go.
	Postgres PostgresConfig `toml:"postgres"`

	// Synth selects + configures the daemon-side LLM backend used for the
	// gate verdict, distillation, and entity extraction. Default backend is
	// Ollama (local, zero Claude tokens); backend = "claude" uses the
	// bundled `claude` CLI instead. See SynthConfig + internal/server/llm.go.
	Synth SynthConfig `toml:"synth"`

	Defaults VaultDefaults `toml:"defaults"`
}

// SynthConfig mirrors the [synth] block in server.toml:
//
//	[synth]
//	backend         = "ollama"                  # "ollama" (default) | "claude"
//	ollama_base_url = "http://localhost:11434"  # default DefaultBaseURL
//	ollama_model    = "qwen2.5:7b"              # default DefaultGenModel
//
// Env overrides (win over the TOML): PB_SYNTH_BACKEND, OLLAMA_BASE_URL
// (shared with the embedding client), PB_SYNTH_OLLAMA_MODEL. Empty
// base_url / model resolve to the ollama package defaults at backend
// construction. The chosen Ollama model MUST be pulled locally on the
// Ollama host (`ollama pull <model>`).
type SynthConfig struct {
	Backend       string `toml:"backend"`
	OllamaBaseURL string `toml:"ollama_base_url"`
	OllamaModel   string `toml:"ollama_model"`
	// TimeoutSecs caps each synth LLM call (gate verdict + distill). Applied
	// as a ceiling to both — the gate just finishes faster under it. Zero ⇒
	// defaultSynthTimeoutSecs. The default is generous because the Ollama
	// backend (now the default) is slower than the Claude CLI the original
	// in-code 30s/45s defaults were tuned for, and the FIRST job after a
	// restart pays model cold-load on top of generation. Env override:
	// PB_SYNTH_TIMEOUT_SECS.
	TimeoutSecs int `toml:"timeout_secs"`
}

// defaultSynthTimeoutSecs is the per-call synth LLM ceiling when unset.
// 120s covers an Ollama cold-load (~10–30s for a 7–8B model) plus a
// multi-paragraph distill on modest local hardware.
const defaultSynthTimeoutSecs = 120

// PostgresConfig mirrors the [postgres] block in server.toml. DSN is the
// BASE / maintenance DSN (e.g. postgres://user:pass@host:5432/phantom_brain);
// per-profile databases (pb_<profile>) are derived from it via
// pgstore.DSNForProfile. The env var PB_POSTGRES_DSN overrides this field.
// Empty (after env resolution) ⇒ Postgres disabled (dormant; Phase A is
// additive and must never break the legacy daemon).
type PostgresConfig struct {
	DSN string `toml:"dsn"`
}

// CaptureConfig mirrors the [capture] block in server.toml. All
// fields optional; missing/zero values fall through to sensible
// defaults so an operator can `[capture]\nenabled = true` and get
// everything else automatic.
type CaptureConfig struct {
	Enabled     bool   `toml:"enabled"`
	MaxBytes    int64  `toml:"max_bytes"`     // default 10 MB
	TimeoutSecs int    `toml:"timeout_secs"`  // default 30
	UserAgent   string `toml:"user_agent"`    // default phantom-brain/2
}

// OpenSearchConfig mirrors the [opensearch] block in server.toml.
type OpenSearchConfig struct {
	Addresses          []string `toml:"addresses"`
	Username           string   `toml:"username"`
	Password           string   `toml:"password"`
	InsecureSkipVerify bool     `toml:"insecure_skip_verify"`
	IndexPrefix        string   `toml:"index_prefix"`
	RequestTimeoutSecs int      `toml:"request_timeout_secs"`
}

// Enabled reports whether the operator wired any OS addresses.
// Absent block / empty addresses → write endpoints disabled.
func (c OpenSearchConfig) Enabled() bool { return len(c.Addresses) > 0 }

// VaultDefaults are the per-vault knobs. Same shape lives in
// profiles/{p}/vaults/{v}/config.toml; nonzero fields there override
// the global defaults.
type VaultDefaults struct {
	ReaperPollIntervalSecs        int   `toml:"reaper_poll_interval_secs"`
	MaxTarballBytes               int64 `toml:"max_tarball_bytes"`
	MaxUncompressedBytes          int64 `toml:"max_uncompressed_bytes"`
	ContributorQuotaBytesPerHour  int64 `toml:"contributor_quota_bytes_per_hour"`
}

// LoadServerConfig reads {configDir}/server.toml. Missing file is an
// error — daemon refuses to start without an explicit config so
// operators don't end up with surprising port/host defaults.
func LoadServerConfig(configDir string) (*ServerConfig, error) {
	path := filepath.Join(configDir, "server.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("server: read %s: %w", path, err)
	}
	var cfg ServerConfig
	if _, err := toml.Decode(string(raw), &cfg); err != nil {
		return nil, fmt.Errorf("server: parse %s: %w", path, err)
	}
	applyServerDefaults(&cfg)
	return &cfg, nil
}

// applyServerDefaults fills in the values the spec requires when the
// operator leaves a knob unset. Mirrors v4.4 §4 defaults verbatim so
// behavior matches the spec's quoted examples.
func applyServerDefaults(cfg *ServerConfig) {
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 9998
	}
	if cfg.Server.Host == "" {
		cfg.Server.Host = "0.0.0.0"
	}
	if cfg.Server.LogLevel == "" {
		cfg.Server.LogLevel = "info"
	}
	if cfg.Storage.Backend == "" {
		cfg.Storage.Backend = "local"
	}

	// Synth backend selection. Env overrides win over the TOML; an unset
	// backend defaults to Ollama. Empty base_url / model are left empty
	// here and resolved to the ollama package defaults in NewLLMBackend.
	s := &cfg.Synth
	if v := strings.TrimSpace(os.Getenv("PB_SYNTH_BACKEND")); v != "" {
		s.Backend = v
	}
	if s.Backend == "" {
		s.Backend = "ollama"
	}
	if v := strings.TrimSpace(os.Getenv("OLLAMA_BASE_URL")); v != "" {
		s.OllamaBaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("PB_SYNTH_OLLAMA_MODEL")); v != "" {
		s.OllamaModel = v
	}
	if v := strings.TrimSpace(os.Getenv("PB_SYNTH_TIMEOUT_SECS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			s.TimeoutSecs = n
		}
	}
	if s.TimeoutSecs <= 0 {
		s.TimeoutSecs = defaultSynthTimeoutSecs
	}

	d := &cfg.Defaults
	if d.ReaperPollIntervalSecs == 0 {
		d.ReaperPollIntervalSecs = 5
	}
	if d.MaxTarballBytes == 0 {
		d.MaxTarballBytes = 5 * 1024 * 1024 * 1024 // 5 GB
	}
	if d.MaxUncompressedBytes == 0 {
		d.MaxUncompressedBytes = 30 * 1024 * 1024 * 1024 // 30 GB
	}
	if d.ContributorQuotaBytesPerHour == 0 {
		d.ContributorQuotaBytesPerHour = 10 * 1024 * 1024 * 1024 // 10 GB
	}
}

// VaultOverrides is parsed from profiles/{p}/vaults/{v}/config.toml.
// Every field is optional — only nonzero values override the global
// defaults via MergedDefaults.
type VaultOverrides struct {
	ReaperPollIntervalSecs       int   `toml:"reaper_poll_interval_secs"`
	MaxTarballBytes              int64 `toml:"max_tarball_bytes"`
	MaxUncompressedBytes         int64 `toml:"max_uncompressed_bytes"`
	ContributorQuotaBytesPerHour int64 `toml:"contributor_quota_bytes_per_hour"`

	// StorageOverrides optionally re-routes this binding to its own
	// OS index set and/or MinIO bucket. Both fields optional; missing
	// fields fall through to the daemon-global defaults. See
	// StorageOverrides for the per-field contract.
	//
	// Phase D1: the Postgres SoR is the sole authoritative store and is
	// always on; the former per-binding `dual_write` flag was removed. A
	// stray `dual_write = true` in an old config.toml is now silently
	// ignored by the TOML decoder (no matching field), which is benign.
	StorageOverrides StorageOverrides `toml:"storage_overrides"`
}

// StorageOverrides is the per-binding override block parsed from
// profiles/<profile>/vaults/<vault>/config.toml under [storage_overrides].
// Both fields optional. When unset the binding uses the shared daemon
// defaults (cfg.OpenSearch.IndexPrefix + cfg.Storage.MinIOBucket).
//
// IndexPrefix is APPENDED to the daemon-global cfg.OpenSearch.IndexPrefix
// when constructing physical index names — final shape:
//
//	<daemon_global_prefix> + <binding_storage_override_prefix> + <logical>
//
// e.g. IndexPrefix="client_x_" + logical="pb_summaries" yields
// "<global>client_x_pb_summaries". Global stays first so dev/test
// sandbox prefixes still wrap every binding the same way.
//
// Bucket replaces the daemon-default MinIO bucket for this binding only.
// MinIO credentials + endpoint are NOT overridable — those stay global
// (Level 2 contract). The bucket must exist before daemon start; the
// daemon will not create it.
//
// Allowed characters in IndexPrefix: lowercase ASCII letters, digits,
// and underscore. Anything else is rejected at registry Load — typos
// and shell metacharacters in index names tend to produce confusing
// 4xx from OpenSearch much later.
type StorageOverrides struct {
	IndexPrefix string `toml:"index_prefix"`
	Bucket      string `toml:"bucket"`
}

// VaultAuth is parsed from profiles/{p}/vaults/{v}/auth.toml.
// BearerToken is the only required field; Description is operator-
// facing only and never surfaces in API responses.
type VaultAuth struct {
	BearerToken string `toml:"bearer_token"`
	Description string `toml:"description"`
}

// LoadVaultFiles reads config.toml + auth.toml for one vault.
// Returns errors for missing/unreadable auth.toml (a vault without
// auth is unusable) but tolerates a missing config.toml (the vault
// inherits global defaults).
func LoadVaultFiles(configDir, profile, vault string) (VaultOverrides, VaultAuth, error) {
	base := filepath.Join(configDir, "profiles", profile, "vaults", vault)
	var overrides VaultOverrides
	if raw, err := os.ReadFile(filepath.Join(base, "config.toml")); err == nil {
		if _, err := toml.Decode(string(raw), &overrides); err != nil {
			return overrides, VaultAuth{}, fmt.Errorf("server: parse %s/config.toml: %w", base, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return overrides, VaultAuth{}, fmt.Errorf("server: read %s/config.toml: %w", base, err)
	}

	authPath := filepath.Join(base, "auth.toml")
	authRaw, err := os.ReadFile(authPath)
	if err != nil {
		return overrides, VaultAuth{}, fmt.Errorf("server: read %s: %w", authPath, err)
	}
	var auth VaultAuth
	if _, err := toml.Decode(string(authRaw), &auth); err != nil {
		return overrides, VaultAuth{}, fmt.Errorf("server: parse %s: %w", authPath, err)
	}
	if strings.TrimSpace(auth.BearerToken) == "" {
		return overrides, VaultAuth{}, fmt.Errorf("server: %s missing bearer_token", authPath)
	}
	return overrides, auth, nil
}

// MergedDefaults applies overrides over the global defaults. Zero
// values in overrides leave the global default in place — that's the
// only signal we have for "operator left this knob unset" since TOML
// doesn't distinguish missing from zero. Operators who want to set a
// limit to zero on purpose can't (acceptable — no such limit makes
// sense in this domain).
func MergedDefaults(global VaultDefaults, overrides VaultOverrides) VaultDefaults {
	out := global
	if overrides.ReaperPollIntervalSecs != 0 {
		out.ReaperPollIntervalSecs = overrides.ReaperPollIntervalSecs
	}
	if overrides.MaxTarballBytes != 0 {
		out.MaxTarballBytes = overrides.MaxTarballBytes
	}
	if overrides.MaxUncompressedBytes != 0 {
		out.MaxUncompressedBytes = overrides.MaxUncompressedBytes
	}
	if overrides.ContributorQuotaBytesPerHour != 0 {
		out.ContributorQuotaBytesPerHour = overrides.ContributorQuotaBytesPerHour
	}
	return out
}
