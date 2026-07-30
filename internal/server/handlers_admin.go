package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
)

// --- provisioning injection seam -------------------------------------------
//
// The admin endpoint provisions bindings by calling internal/provision, but
// that package imports THIS one (for VaultKey, the registry, MinIOBackend,
// etc.), so importing it here would be a cycle. Instead the daemon holds an
// injected ProvisionFn (wired by cmd to provision.ProfileCreate). These
// structs are the server-side mirror of provision's Deps/Spec/Result so the
// seam carries no provision types.

type ProvisionRequest struct {
	Profile     string
	Vault       string
	Token       string
	Bucket      string
	IndexPrefix string
}

type ProvisionDeps struct {
	BaseDSN           string
	MinIO             *MinIOBackend
	ConfigDir         string
	NamingBucket      string
	NamingIndexPrefix string
}

type ProvisionStep struct {
	Name   string
	Result string
	Detail string
	Err    string
}

type ProvisionResult struct {
	Profile      string
	Vault        string
	Bucket       string
	IndexPrefix  string
	Token        string
	TokenCreated bool
	Steps        []ProvisionStep
}

// Failed reports whether any step failed.
func (r ProvisionResult) Failed() bool {
	for _, s := range r.Steps {
		if s.Err != "" {
			return true
		}
	}
	return false
}

// ProvisionFunc provisions a binding (Postgres SoR + MinIO bucket + config),
// idempotently, without reloading the daemon (that's the caller's job).
type ProvisionFunc func(ctx context.Context, deps ProvisionDeps, req ProvisionRequest) (ProvisionResult, error)

// --- admin auth ------------------------------------------------------------

// adminAuthMiddleware guards the operator/admin surface with a single
// operator key (PB_ADMIN_KEY), distinct from the per-binding bearer tokens.
// Admin ops act ACROSS profiles (they provision a binding that does not exist
// yet), so they must never accept a profile/vault token — hence they mount
// OUTSIDE AuthMiddleware. Fail closed: an unset PB_ADMIN_KEY disables the
// surface entirely (every request 403s).
func (d *Daemon) adminAuthMiddleware() func(http.Handler) http.Handler {
	key := strings.TrimSpace(os.Getenv("PB_ADMIN_KEY"))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if key == "" {
				WriteErrorEnvelope(w, http.StatusForbidden, ErrCodeInvalidToken,
					"admin surface disabled (PB_ADMIN_KEY not configured)", nil)
				return
			}
			got := strings.TrimSpace(r.Header.Get("X-API-Key"))
			if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
				WriteErrorEnvelope(w, http.StatusForbidden, ErrCodeInvalidToken,
					"invalid or missing operator key", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// --- handlers --------------------------------------------------------------

// adminProfileCreateReq is the POST /admin/profiles body. Token is optional
// and caller-supplied — the orchestrator generates it and persists it (to
// phantom-credentials) BEFORE calling, so the durable side effect carries a
// known value. Empty ⇒ the binding generates one (and an existing binding's
// token is read back and returned regardless).
type adminProfileCreateReq struct {
	Profile     string `json:"profile"`
	Vault       string `json:"vault"`
	Token       string `json:"token,omitempty"`
	Bucket      string `json:"bucket,omitempty"`
	IndexPrefix string `json:"index_prefix,omitempty"`
}

type adminStepView struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
}

type adminProfileResp struct {
	Profile      string          `json:"profile"`
	Vault        string          `json:"vault"`
	Bucket       string          `json:"bucket"`
	IndexPrefix  string          `json:"index_prefix"`
	Token        string          `json:"token"`
	TokenCreated bool            `json:"token_created"`
	OK           bool            `json:"ok"`
	Reloaded     bool            `json:"reloaded"`
	Live         bool            `json:"live"`
	Steps        []adminStepView `json:"steps"`
}

// handleAdminProfileCreate provisions a binding and reloads the daemon
// in-process so it goes live. Idempotent. Serialized behind provisionMu so
// concurrent calls (and the SIGHUP loop) don't race the reload.
func (d *Daemon) handleAdminProfileCreate(w http.ResponseWriter, r *http.Request) {
	if d.ProvisionFn == nil {
		WriteErrorEnvelope(w, http.StatusNotImplemented, ErrCodeInternal, "provisioning not wired", nil)
		return
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // version skew: an unknown field is a loud 400, not a silent drop
	var req adminProfileCreateReq
	if err := dec.Decode(&req); err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "bad request body: "+err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.Vault) == "" {
		req.Vault = "memory"
	}

	d.provisionMu.Lock()
	defer d.provisionMu.Unlock()

	deps := ProvisionDeps{BaseDSN: d.pgBaseDSN, MinIO: d.minioBase, ConfigDir: d.ConfigDir}
	if d.Config != nil {
		deps.NamingBucket = d.Config.Naming.Bucket
		deps.NamingIndexPrefix = d.Config.Naming.IndexPrefix
	}
	res, err := d.ProvisionFn(r.Context(), deps, ProvisionRequest{
		Profile:     req.Profile,
		Vault:       req.Vault,
		Token:       req.Token,
		Bucket:      req.Bucket,
		IndexPrefix: req.IndexPrefix,
	})
	if err != nil {
		// Precondition failure (invalid name/prefix/token) — nothing ran.
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, err.Error(), nil)
		return
	}

	// Reload in-process so the binding goes live, then verify it resolved.
	reloaded := true
	if rerr := d.reload(); rerr != nil {
		reloaded = false
		d.Logger.Warn("admin.reload_after_provision_failed", slog.String("error", rerr.Error()))
	}
	_, live := d.registry.LookupByVault(VaultKey{Profile: req.Profile, Vault: req.Vault})

	resp := adminProfileResp{
		Profile:      res.Profile,
		Vault:        res.Vault,
		Bucket:       res.Bucket,
		IndexPrefix:  res.IndexPrefix,
		Token:        res.Token,
		TokenCreated: res.TokenCreated,
		OK:           !res.Failed(),
		Reloaded:     reloaded,
		Live:         live,
	}
	for _, s := range res.Steps {
		resp.Steps = append(resp.Steps, adminStepView{Name: s.Name, Result: s.Result, Detail: s.Detail, Error: s.Err})
	}

	// Honest status: 500 if a step failed; 202 if provisioned but not yet live
	// (the caller marks it drifted); 200 when live.
	status := http.StatusOK
	switch {
	case !resp.OK:
		status = http.StatusInternalServerError
	case !resp.Live:
		status = http.StatusAccepted
	}
	writeJSON(w, status, resp)
}

// handleAdminProfileGet reports a binding's observed state (no secret): its
// derived bucket + index prefix, and whether it exists/is live.
func (d *Daemon) handleAdminProfileGet(w http.ResponseWriter, r *http.Request) {
	key := VaultKey{Profile: chi.URLParam(r, "profile"), Vault: chi.URLParam(r, "vault")}
	b, ok := d.registry.LookupByVault(key)
	if !ok {
		WriteErrorEnvelope(w, http.StatusNotFound, ErrCodeVaultNotFound, "no binding for "+key.String(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"profile":      key.Profile,
		"vault":        key.Vault,
		"bucket":       b.Storage.Bucket,
		"index_prefix": b.Storage.IndexPrefix,
		"exists":       true,
	})
}
