// Package provision implements the profile-binding provisioning steps —
// Postgres System-of-Record, MinIO bucket, and binding config
// (auth.toml + config.toml) — decoupled from the cobra CLI so a single
// implementation backs both `pbrainctl server profile create` and (later)
// the daemon's admin HTTP handler.
//
// Reload is deliberately NOT part of this package: it is the caller's
// concern (the CLI SIGHUPs a separate daemon process; the daemon calls its
// own in-process reload). Every step here is idempotent, so re-running on a
// partially-provisioned binding heals it.
package provision

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/neverprepared/phantom-brain/internal/pgstore"
	pbserver "github.com/neverprepared/phantom-brain/internal/server"
)

// Spec describes the binding to provision. Bucket and IndexPrefix are
// derived from Profile when empty (`<profile>-archives`, `<profile>_`).
// Token is used only when creating a fresh binding; an existing binding
// keeps its token (which is read back and returned in Result).
type Spec struct {
	Profile     string
	Vault       string
	Bucket      string
	IndexPrefix string
	Token       string
}

// Deps carries the already-resolved runtime dependencies so the core is
// free of CLI/flag concerns. The daemon holds all of these directly
// (d.pgBaseDSN, d.minioBase, d.ConfigDir); the CLI resolves them from
// flags/env/server.toml.
type Deps struct {
	// BaseDSN is the maintenance Postgres DSN. Empty skips the PG step
	// (caller has no Postgres configured).
	BaseDSN string
	// MinIO is the daemon's MinIO backend. Nil skips the bucket step
	// (local blob backend).
	MinIO *pbserver.MinIOBackend
	// ConfigDir is the root under which profiles/<p>/vaults/<v>/ lives.
	ConfigDir string
	// Naming holds the resource-name templates. Zero value → historical
	// defaults, so callers that don't care get today's behavior.
	Naming Naming
}

// Naming makes the per-binding storage-name convention configurable
// instead of hardcoded. Templates support {profile} and {vault}
// placeholders. An empty field falls back to the historical default.
// A per-binding override (Spec.Bucket / Spec.IndexPrefix) always wins
// over the template. NOTE: this governs the MinIO bucket and OpenSearch
// index prefix only — the Postgres database name (pb_<profile>) is
// resolved by convention in both provisioning and the daemon's runtime
// recall path, so templatizing it is a separate, coordinated change.
type Naming struct {
	Bucket      string // default "{profile}-archives"
	IndexPrefix string // default "{profile}_"
}

func (n Naming) bucketFor(profile, vault string) string {
	return expandName(orDefault(n.Bucket, "{profile}-archives"), profile, vault)
}

func (n Naming) indexPrefixFor(profile, vault string) string {
	return expandName(orDefault(n.IndexPrefix, "{profile}_"), profile, vault)
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func expandName(tmpl, profile, vault string) string {
	return strings.NewReplacer("{profile}", profile, "{vault}", vault).Replace(tmpl)
}

// StepResult is one step's outcome. Result is one of:
// created | ok | exists | skipped | failed.
type StepResult struct {
	Name   string
	Result string
	Detail string
	Err    error
}

// Result is the outcome of ProfileCreate. Token is the binding's
// EFFECTIVE bearer token — freshly generated, caller-supplied, or read
// back from an existing binding — so a caller (e.g. the provisioning
// orchestrator) can always recover it and a re-run is safe. This is the
// deliberate fix for the "mint-once-and-hide" token-orphan hazard: an
// existing binding never hides its secret from the caller. TokenCreated
// reports whether the token was newly written this call.
type Result struct {
	Key          pbserver.VaultKey
	Bucket       string
	IndexPrefix  string
	Token        string
	TokenCreated bool
	Steps        []StepResult
}

// Failed reports whether any step failed.
func (r Result) Failed() bool {
	for _, s := range r.Steps {
		if s.Err != nil {
			return true
		}
	}
	return false
}

// ProfileCreate provisions a complete binding (Postgres SoR + MinIO
// bucket + binding config), idempotently. It does NOT reload the daemon.
// A non-nil error is returned only for a precondition failure (invalid
// name/prefix/token); per-step failures are collected in Result.Steps —
// check Result.Failed().
func ProfileCreate(ctx context.Context, deps Deps, spec Spec) (Result, error) {
	if err := validateSegment("profile", spec.Profile); err != nil {
		return Result{}, err
	}
	if err := validateSegment("vault", spec.Vault); err != nil {
		return Result{}, err
	}
	if strings.ContainsAny(spec.Token, " \t\r\n\f\v") {
		return Result{}, errors.New("provision: token must contain no whitespace")
	}

	key := pbserver.VaultKey{Profile: spec.Profile, Vault: spec.Vault}
	// Per-binding override wins; otherwise derive from the (configurable)
	// naming template.
	bucket := spec.Bucket
	if bucket == "" {
		bucket = deps.Naming.bucketFor(spec.Profile, spec.Vault)
	}
	indexPrefix := spec.IndexPrefix
	if indexPrefix == "" {
		indexPrefix = deps.Naming.indexPrefixFor(spec.Profile, spec.Vault)
	}
	if err := pbserver.ValidateStorageOverridePrefix(indexPrefix); err != nil {
		return Result{}, err
	}

	res := Result{Key: key, Bucket: bucket, IndexPrefix: indexPrefix}

	// 1. Postgres SoR (pb_<profile>)
	res.Steps = append(res.Steps, provisionPostgres(ctx, deps.BaseDSN, spec.Profile))
	// 2. MinIO bucket (<profile>-archives)
	res.Steps = append(res.Steps, provisionBucket(ctx, deps.MinIO, bucket))
	// 3. Binding config (auth.toml + config.toml) — resolves the effective token
	cfgStep, token, created := writeBindingConfig(deps.ConfigDir, key, indexPrefix, bucket, spec.Token)
	res.Steps = append(res.Steps, cfgStep)
	res.Token, res.TokenCreated = token, created
	// 4. Validate the on-disk config loads and the binding is visible
	res.Steps = append(res.Steps, validateConfig(deps.ConfigDir, key))

	return res, nil
}

func provisionPostgres(ctx context.Context, baseDSN, profile string) StepResult {
	s := StepResult{Name: "postgres db (pb_" + profile + ")"}
	if strings.TrimSpace(baseDSN) == "" {
		s.Result, s.Detail = "skipped", "no base Postgres DSN provided"
		return s
	}
	if err := pgstore.Provision(ctx, baseDSN, profile); err != nil {
		s.Result, s.Err = "failed", err
		return s
	}
	s.Result, s.Detail = "ok", "created or already present + migrated"
	return s
}

func provisionBucket(ctx context.Context, mb *pbserver.MinIOBackend, bucket string) StepResult {
	s := StepResult{Name: "minio bucket (" + bucket + ")"}
	if mb == nil {
		s.Result, s.Detail = "skipped", "storage backend is not minio"
		return s
	}
	if err := mb.CreateBucket(ctx, bucket); err != nil {
		s.Result, s.Err = "failed", err
		return s
	}
	s.Result = "ok"
	return s
}

// writeBindingConfig writes auth.toml (only when absent — never clobber a
// live token) and config.toml (idempotent — holds no secret). It returns
// the effective bearer token: on a fresh binding the supplied or generated
// one; on an existing binding it is read BACK from auth.toml so the caller
// can always recover it.
func writeBindingConfig(configDir string, key pbserver.VaultKey, indexPrefix, bucket, token string) (StepResult, string, bool) {
	s := StepResult{Name: "binding config"}
	bindingDir := filepath.Join(configDir, "profiles", key.Profile, "vaults", key.Vault)
	authPath := filepath.Join(bindingDir, "auth.toml")
	cfgPath := filepath.Join(bindingDir, "config.toml")

	existed := false
	if _, err := os.Stat(authPath); err == nil {
		existed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		s.Result, s.Err = "failed", fmt.Errorf("stat %s: %w", authPath, err)
		return s, "", false
	}

	if err := os.MkdirAll(bindingDir, 0o700); err != nil {
		s.Result, s.Err = "failed", fmt.Errorf("mkdir %s: %w", bindingDir, err)
		return s, "", false
	}

	var effective string
	created := false
	if existed {
		tok, err := readBearerToken(authPath)
		if err != nil {
			s.Result, s.Err = "failed", err
			return s, "", false
		}
		effective = tok
	} else {
		effective = token
		if effective == "" {
			gen, err := newBearerToken()
			if err != nil {
				s.Result, s.Err = "failed", err
				return s, "", false
			}
			effective = gen
		}
		authBody := fmt.Sprintf("bearer_token = %q\n", effective)
		if err := os.WriteFile(authPath, []byte(authBody), 0o600); err != nil {
			s.Result, s.Err = "failed", fmt.Errorf("write %s: %w", authPath, err)
			return s, "", false
		}
		created = true
	}

	if err := os.WriteFile(cfgPath, []byte(buildBindingConfigTOML(indexPrefix, bucket)), 0o644); err != nil {
		s.Result, s.Err = "failed", fmt.Errorf("write %s: %w", cfgPath, err)
		return s, "", false
	}

	if created {
		s.Result, s.Detail = "created", "auth.toml (token) + config.toml"
	} else {
		s.Result, s.Detail = "exists", "kept token, refreshed config.toml"
	}
	return s, effective, created
}

func validateConfig(configDir string, key pbserver.VaultKey) StepResult {
	s := StepResult{Name: "config validate"}
	cfg, err := pbserver.LoadServerConfig(configDir)
	if err != nil {
		s.Result, s.Err = "failed", err
		return s
	}
	r := pbserver.NewRegistry()
	if _, err := r.Load(pbserver.LoadOpts{
		ConfigDir:          configDir,
		Defaults:           cfg.Defaults,
		DefaultIndexPrefix: cfg.OpenSearch.IndexPrefix,
		DefaultBucket:      cfg.Storage.MinIOBucket,
	}); err != nil {
		s.Result, s.Err = "failed", err
		return s
	}
	if _, ok := r.LookupByVault(key); !ok {
		s.Result, s.Err = "failed", fmt.Errorf("binding %s not visible in the loaded registry after write", key)
		return s
	}
	s.Result, s.Detail = "ok", "registry loads; binding visible"
	return s
}

// readBearerToken reads a binding's live token from auth.toml so an
// existing binding can hand its secret back to the caller.
func readBearerToken(authPath string) (string, error) {
	var a struct {
		BearerToken string `toml:"bearer_token"`
	}
	if _, err := toml.DecodeFile(authPath, &a); err != nil {
		return "", fmt.Errorf("read %s: %w", authPath, err)
	}
	if strings.TrimSpace(a.BearerToken) == "" {
		return "", fmt.Errorf("%s has no bearer_token", authPath)
	}
	return a.BearerToken, nil
}

// newBearerToken returns 32 bytes of crypto/rand hex-encoded (a
// 64-character token).
func newBearerToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate bearer token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// buildBindingConfigTOML writes the minimal [storage_overrides] body by
// hand so the file stays human-readable + diff-friendly.
func buildBindingConfigTOML(indexPrefix, bucket string) string {
	var b strings.Builder
	b.WriteString("[storage_overrides]\n")
	if indexPrefix != "" {
		fmt.Fprintf(&b, "index_prefix = %q\n", indexPrefix)
	}
	if bucket != "" {
		fmt.Fprintf(&b, "bucket = %q\n", bucket)
	}
	return b.String()
}

func validateSegment(kind, v string) error {
	if v == "" {
		return fmt.Errorf("provision: %s must not be empty", kind)
	}
	if strings.ContainsAny(v, "/\\") || v == "." || v == ".." {
		return fmt.Errorf("provision: %s %q contains path separators", kind, v)
	}
	return nil
}
