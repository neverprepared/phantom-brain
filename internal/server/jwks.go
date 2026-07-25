package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims are the phantom-auth JWT claims the brain acts on: which profile
// the caller is (the service account), its tool scope, and its residency
// ceiling. The subject is "profile:<name>"; Profile carries the bare name.
type Claims struct {
	Profile string
	Scope   []string
	Ceiling string
}

// errUnknownKID signals the token's kid isn't in the cached JWKS, so the
// Verifier should refetch once (key rotation) before rejecting.
var errUnknownKID = errors.New("phantom-auth: unknown JWKS kid")

type jwkEntry struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
}

type jwksDoc struct {
	Keys []jwkEntry `json:"keys"`
}

// Verifier verifies phantom-auth EdDSA JWTs against a cached JWKS. It holds
// only public keys, so it can verify but never mint. Safe for concurrent use.
type Verifier struct {
	url      string
	issuer   string
	audience string
	client   *http.Client
	ttl      time.Duration

	mu      sync.Mutex
	byKID   map[string]ed25519.PublicKey
	fetched time.Time
}

// NewVerifier builds a Verifier for the given JWKS URL. issuer/audience are
// the expected `iss`/`aud`; both are enforced.
func NewVerifier(jwksURL, issuer, audience string) *Verifier {
	return &Verifier{
		url:      jwksURL,
		issuer:   issuer,
		audience: audience,
		client:   &http.Client{Timeout: 10 * time.Second},
		ttl:      5 * time.Minute,
	}
}

// keys returns the cached kid→public-key map, refetching when forced or the
// TTL has lapsed.
func (v *Verifier) keys(force bool) (map[string]ed25519.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !force && v.byKID != nil && time.Since(v.fetched) < v.ttl {
		return v.byKID, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("phantom-auth: JWKS fetch %s: status %d", v.url, resp.StatusCode)
	}
	var doc jwksDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("phantom-auth: decode JWKS: %w", err)
	}
	m := make(map[string]ed25519.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.Kid == "" {
			continue // only Ed25519 OKP keys are usable here
		}
		raw, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		m[k.Kid] = ed25519.PublicKey(raw)
	}
	v.byKID = m
	v.fetched = time.Now()
	return m, nil
}

// Verify checks a token's signature, issuer, audience, and expiry, returning
// its Claims. Any failure returns an error (the caller maps that to 401).
func (v *Verifier) Verify(token string) (*Claims, error) {
	var lastErr error
	for _, force := range []bool{false, true} {
		keys, err := v.keys(force)
		if err != nil {
			lastErr = err
			continue
		}
		kidMissed := false
		parser := jwt.NewParser(
			jwt.WithValidMethods([]string{"EdDSA"}),
			jwt.WithIssuer(v.issuer),
			jwt.WithAudience(v.audience),
			jwt.WithExpirationRequired(),
		)
		parsed, perr := parser.Parse(token, func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			key, ok := keys[kid]
			if !ok {
				kidMissed = true
				return nil, errUnknownKID
			}
			return key, nil
		})
		if perr != nil {
			// A kid miss on the cached set → refetch once, then retry.
			if kidMissed && !force {
				lastErr = perr
				continue
			}
			return nil, perr
		}
		mc, ok := parsed.Claims.(jwt.MapClaims)
		if !ok {
			return nil, errors.New("phantom-auth: unexpected claims type")
		}
		return claimsFromMap(mc), nil
	}
	if lastErr == nil {
		lastErr = errors.New("phantom-auth: verification failed")
	}
	return nil, lastErr
}

func claimsFromMap(mc jwt.MapClaims) *Claims {
	profile, _ := mc["profile"].(string)
	ceiling, _ := mc["ceiling"].(string)
	var scope []string
	if raw, ok := mc["scope"].([]any); ok {
		for _, s := range raw {
			if str, ok := s.(string); ok {
				scope = append(scope, str)
			}
		}
	}
	return &Claims{Profile: profile, Scope: scope, Ceiling: ceiling}
}
