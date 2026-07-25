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

// AuthMiddleware enforces bearer auth for the daemon. Two token shapes are
// accepted, distinguished by structure:
//
//   - a **phantom-auth JWT** (three dot-separated parts) — verified locally
//     against the JWKS when a verifier is configured. Its `profile` claim
//     selects the binding; a caller with several vaults picks one via the
//     `X-Brain-Vault` header. This is the unified platform identity.
//   - a **legacy per-vault bearer token** — looked up in constant time in the
//     registry's map. Kept so existing vault tokens keep working.
//
// verifier may be nil (no [auth] block): only legacy tokens are then accepted.
// Returns 401 INVALID_TOKEN for missing/unknown/invalid tokens; on success,
// stashes the resolved VaultBinding on the request context.
func AuthMiddleware(registry *Registry, verifier *Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerFromHeader(r.Header.Get("Authorization"))
			if !ok {
				WriteErrorEnvelope(w, http.StatusUnauthorized, ErrCodeInvalidToken,
					"missing or malformed Authorization header (expected: Bearer <token>)", nil)
				return
			}

			var binding VaultBinding
			if verifier != nil && looksLikeJWT(token) {
				claims, err := verifier.Verify(token)
				if err != nil {
					WriteErrorEnvelope(w, http.StatusUnauthorized, ErrCodeInvalidToken,
						"invalid phantom-auth token", nil)
					return
				}
				b, status, msg := resolveBindingForProfile(
					registry, claims.Profile, r.Header.Get("X-Brain-Vault"))
				if status != 0 {
					WriteErrorEnvelope(w, status, ErrCodeInvalidToken, msg, nil)
					return
				}
				binding = b
			} else {
				b, found := registry.LookupByToken(token)
				if !found {
					WriteErrorEnvelope(w, http.StatusUnauthorized, ErrCodeInvalidToken,
						"unknown bearer token", nil)
					return
				}
				binding = b
			}

			ctx := context.WithValue(r.Context(), authCtxKey{}, binding)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// looksLikeJWT reports whether a token has the compact-JWS three-part shape.
// A legacy opaque vault token is a single random string (no dots), so this
// cleanly separates the two paths without one masking the other.
func looksLikeJWT(token string) bool {
	return strings.Count(token, ".") == 2
}

// resolveBindingForProfile maps a JWT's profile (+ optional X-Brain-Vault) to
// exactly one vault binding. Returns (binding, 0, "") on success, else a
// non-zero HTTP status + message. A profile JWT can only reach its own
// profile's vaults — cross-profile access can't be expressed.
func resolveBindingForProfile(registry *Registry, profile, vault string) (VaultBinding, int, string) {
	if strings.TrimSpace(profile) == "" {
		return VaultBinding{}, http.StatusForbidden, "token carries no profile claim"
	}
	vaults := registry.LookupByProfile(profile)
	if len(vaults) == 0 {
		return VaultBinding{}, http.StatusForbidden, "no vault provisioned for profile " + profile
	}
	if vault != "" {
		for _, b := range vaults {
			if b.Key.Vault == vault {
				return b, 0, ""
			}
		}
		return VaultBinding{}, http.StatusForbidden,
			"token does not grant access to " + profile + "/" + vault
	}
	if len(vaults) == 1 {
		return vaults[0], 0, ""
	}
	return VaultBinding{}, http.StatusBadRequest,
		"profile " + profile + " has multiple vaults; specify one via the X-Brain-Vault header"
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
