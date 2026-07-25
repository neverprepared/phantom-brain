package server

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testAuth stands in for phantom-auth: an Ed25519 key, a JWKS endpoint, and
// a mint helper — so the brain's verifier can be exercised end-to-end.
type testAuth struct {
	priv ed25519.PrivateKey
	kid  string
	srv  *httptest.Server
}

func newTestAuth(t *testing.T) *testAuth {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pub)
	kid := hex.EncodeToString(sum[:])[:16]
	body, _ := json.Marshal(map[string]any{
		"keys": []map[string]any{{
			"kty": "OKP", "crv": "Ed25519",
			"x":   base64.RawURLEncoding.EncodeToString(pub),
			"use": "sig", "alg": "EdDSA", "kid": kid,
		}},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return &testAuth{priv: priv, kid: kid, srv: srv}
}

func (a *testAuth) verifier() *Verifier {
	return NewVerifier(a.srv.URL, "phantom-auth", "phantom")
}

func (a *testAuth) mint(t *testing.T, profile string, edit ...func(jwt.MapClaims)) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": "phantom-auth", "aud": "phantom", "sub": "profile:" + profile,
		"profile": profile, "scope": []string{}, "ceiling": "",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	for _, e := range edit {
		e(claims)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = a.kid
	s, err := tok.SignedString(a.priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerifier_ValidToken(t *testing.T) {
	a := newTestAuth(t)
	v := a.verifier()
	tok := a.mint(t, "personal", func(c jwt.MapClaims) {
		c["scope"] = []string{"brain:recall"}
		c["ceiling"] = "infra"
	})
	claims, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Profile != "personal" {
		t.Errorf("profile = %q, want personal", claims.Profile)
	}
	if len(claims.Scope) != 1 || claims.Scope[0] != "brain:recall" {
		t.Errorf("scope = %v", claims.Scope)
	}
	if claims.Ceiling != "infra" {
		t.Errorf("ceiling = %q", claims.Ceiling)
	}
}

func TestVerifier_Rejections(t *testing.T) {
	a := newTestAuth(t)
	v := a.verifier()

	cases := map[string]string{
		"expired":   a.mint(t, "p", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }),
		"wrong_aud": a.mint(t, "p", func(c jwt.MapClaims) { c["aud"] = "someone-else" }),
		"wrong_iss": a.mint(t, "p", func(c jwt.MapClaims) { c["iss"] = "evil" }),
		"garbage":   "not.a.jwt",
	}
	for name, tok := range cases {
		if _, err := v.Verify(tok); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestVerifier_UnknownKeyRejected(t *testing.T) {
	a := newTestAuth(t)     // serves key A's JWKS
	other := newTestAuth(t) // signs with key B
	v := a.verifier()
	if _, err := v.Verify(other.mint(t, "personal")); err == nil {
		t.Error("token from an unknown key should be rejected")
	}
}

func TestAuthMiddleware_JWT_ResolvesSingleVault(t *testing.T) {
	dir := t.TempDir()
	seedVault(t, dir, "personal", "memory", "")
	r := NewRegistry()
	if _, err := r.Load(LoadOpts{ConfigDir: dir}); err != nil {
		t.Fatal(err)
	}
	a := newTestAuth(t)
	mw := AuthMiddleware(r, a.verifier())

	var got VaultBinding
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got, _ = BindingFromContext(req.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+a.mint(t, "personal"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got.Key.Profile != "personal" || got.Key.Vault != "memory" {
		t.Errorf("binding = %s, want personal/memory", got.Key)
	}
}

func TestAuthMiddleware_JWT_MultiVaultNeedsHeader(t *testing.T) {
	dir := t.TempDir()
	seedVault(t, dir, "personal", "memory", "")
	seedVault(t, dir, "personal", "work", "")
	r := NewRegistry()
	if _, err := r.Load(LoadOpts{ConfigDir: dir}); err != nil {
		t.Fatal(err)
	}
	a := newTestAuth(t)
	mw := AuthMiddleware(r, a.verifier())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	// no header → ambiguous → 400
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+a.mint(t, "personal"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no header: code = %d, want 400", rec.Code)
	}

	// with header → resolves the named vault
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+a.mint(t, "personal"))
	req.Header.Set("X-Brain-Vault", "work")
	rec = httptest.NewRecorder()
	var got VaultBinding
	h = mw(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got, _ = BindingFromContext(req.Context())
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || got.Key.Vault != "work" {
		t.Fatalf("with header: code = %d vault = %q", rec.Code, got.Key.Vault)
	}
}

func TestAuthMiddleware_JWT_UnknownProfileForbidden(t *testing.T) {
	dir := t.TempDir()
	seedVault(t, dir, "personal", "memory", "")
	r := NewRegistry()
	if _, err := r.Load(LoadOpts{ConfigDir: dir}); err != nil {
		t.Fatal(err)
	}
	a := newTestAuth(t)
	mw := AuthMiddleware(r, a.verifier())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler must not run")
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+a.mint(t, "ghost")) // no vault for 'ghost'
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", rec.Code)
	}
}

func TestAuthMiddleware_JWT_InvalidRejected(t *testing.T) {
	dir := t.TempDir()
	seedVault(t, dir, "personal", "memory", "")
	r := NewRegistry()
	if _, err := r.Load(LoadOpts{ConfigDir: dir}); err != nil {
		t.Fatal(err)
	}
	other := newTestAuth(t) // signs with a key the verifier doesn't know
	mw := AuthMiddleware(r, newTestAuth(t).verifier())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler must not run")
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+other.mint(t, "personal"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

func TestAuthMiddleware_LegacyTokenStillWorksWithVerifier(t *testing.T) {
	dir := t.TempDir()
	tok := seedVault(t, dir, "personal", "memory", "") // opaque token, no dots
	r := NewRegistry()
	if _, err := r.Load(LoadOpts{ConfigDir: dir}); err != nil {
		t.Fatal(err)
	}
	a := newTestAuth(t)
	mw := AuthMiddleware(r, a.verifier()) // verifier present, but token isn't a JWT
	var got VaultBinding
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got, _ = BindingFromContext(req.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || got.Key.Vault != "memory" {
		t.Fatalf("legacy token: code = %d vault = %q", rec.Code, got.Key.Vault)
	}
}
