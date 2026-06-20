package server

import (
	"context"
	"net/http"
	"strings"
)

// authCtxKey scopes the binding stash so it cannot collide with
// caller-provided values. Unexported per Go context conventions.
type authCtxKey struct{}

// BindingFromContext retrieves the VaultBinding the auth middleware
// stashed on the request context. Handlers MUST call this rather than
// re-parsing the Authorization header — the middleware has already
// validated the token, and handlers running below the middleware can
// assume the binding is present.
func BindingFromContext(ctx context.Context) (VaultBinding, bool) {
	b, ok := ctx.Value(authCtxKey{}).(VaultBinding)
	return b, ok
}

// AuthMiddleware enforces bearer-token auth backed by the registry.
// Returns 401 INVALID_TOKEN for missing or unknown tokens, with the
// standard error envelope. On success, stashes the resolved
// VaultBinding on the request context for downstream handlers.
//
// Bearer tokens are looked up in constant time against the registry's
// internal map — there's no string-compare loop, so timing attacks
// against the token set don't get you the per-vault token (you can
// still confirm presence/absence via timing, but that's an acceptable
// trade given the tokens are 32+ bytes of random anyway).
func AuthMiddleware(registry *Registry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerFromHeader(r.Header.Get("Authorization"))
			if !ok {
				WriteErrorEnvelope(w, http.StatusUnauthorized, ErrCodeInvalidToken,
					"missing or malformed Authorization header (expected: Bearer <token>)", nil)
				return
			}
			binding, ok := registry.LookupByToken(token)
			if !ok {
				WriteErrorEnvelope(w, http.StatusUnauthorized, ErrCodeInvalidToken,
					"unknown bearer token", nil)
				return
			}
			ctx := context.WithValue(r.Context(), authCtxKey{}, binding)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerFromHeader extracts the token from "Bearer <token>".
// Tolerates extra whitespace and case-insensitive "bearer"; rejects
// empty tokens. Returns (token, true) on success.
func bearerFromHeader(h string) (string, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", false
	}
	// Split into at-most-2 parts so a token containing a space
	// (shouldn't happen, but) doesn't get truncated.
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 {
		return "", false
	}
	if !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	tok := strings.TrimSpace(parts[1])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// RequireVaultMatch enforces that the caller's URL-path (profile,
// vault) matches the token's binding. Used by handlers that take a
// profile/vault in the path AND need belt-and-suspenders confirmation
// the caller isn't trying to cross vault boundaries. Returns
// http.StatusForbidden with VAULT_MISMATCH on mismatch.
//
// Not used in Phase 2's read endpoints (they derive (profile, vault)
// from the binding directly); reserved for future routes that take an
// explicit path.
func RequireVaultMatch(binding VaultBinding, profile, vault string) (int, string) {
	if binding.Key.Profile != profile || binding.Key.Vault != vault {
		return http.StatusForbidden, "token does not grant access to " + profile + "/" + vault
	}
	return 0, ""
}
