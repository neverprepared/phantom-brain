package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StorageBackend is the abstraction over death-payload upload sinks.
// LocalBackend (default) writes through the daemon's own HTTP
// handlers; MinIOBackend (Phase 2 day 5 wires the surface but the
// real S3 plumbing is Phase 5) presigns to a real S3-compatible
// bucket.
//
// Phase 2 single-PUT upload: the brain POSTs the entire tarball in
// one request. Multipart is Phase 5 — at typical death-payload sizes
// (tens of MB) the single PUT is fine and the simpler protocol is
// easier to reason about during cutover.
type StorageBackend interface {
	// NewUpload allocates an upload_id and returns the URL the brain
	// should PUT to plus an HMAC-signed token the daemon will verify
	// at upload time. ttl bounds the URL's validity.
	NewUpload(brainID string, ttl time.Duration) (UploadHandle, error)

	// AcceptUpload is called by the daemon's upload handler after
	// authenticating the URL token. It streams the body into the
	// backend's pending area, returning the size received.
	AcceptUpload(uploadID string, body io.Reader, maxBytes int64) (int64, error)

	// CompleteUpload finalises an upload by moving it to the location
	// the reaper polls (brains/_pending/<brain_id>.tar). Returns the
	// final path on disk so handlers can echo it back for diagnostics.
	CompleteUpload(profile, vault, brainID, uploadID string) (string, error)

	// VerifyToken checks a token previously issued by NewUpload. Used
	// by the upload handler to authenticate without re-loading the
	// vault binding. Returns the brain_id encoded in the token + an
	// error on expiry/tamper.
	VerifyToken(uploadID, token string) (brainID string, err error)
}

// UploadHandle is what NewUpload returns: the URL the brain should
// PUT to and an opaque token the daemon will verify.
type UploadHandle struct {
	UploadID string
	URL      string
	Token    string
	Expires  time.Time
}

// --- LocalBackend ----------------------------------------------------

// LocalBackend stores uploads under the daemon's own data dir and
// serves them via daemon HTTP routes. The "presigned URL" is just a
// daemon URL with an HMAC token in the query string; tokens are
// signed with a process-startup random key, so they don't survive a
// daemon restart (which is fine — brains retry).
type LocalBackend struct {
	dataDir  DataDir
	baseURL  string // e.g. http://localhost:9998 — used to build upload URLs
	hmacKey  []byte

	mu      sync.Mutex
	uploads map[string]localUploadState // upload_id -> state
}

type localUploadState struct {
	BrainID string
	Profile string
	Vault   string
	Expires time.Time
}

// NewLocalBackend wires the local sink. baseURL is what the daemon
// advertises; if behind a reverse proxy, override via server.toml.
func NewLocalBackend(dataDir DataDir, baseURL string) (*LocalBackend, error) {
	if baseURL == "" {
		return nil, errors.New("server: NewLocalBackend requires a baseURL")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("server: hmac key: %w", err)
	}
	return &LocalBackend{
		dataDir: dataDir,
		baseURL: strings.TrimRight(baseURL, "/"),
		hmacKey: key,
		uploads: map[string]localUploadState{},
	}, nil
}

// RegisterUpload pre-records the binding for an upload_id so
// AcceptUpload + CompleteUpload can confirm the upload was started
// by an authenticated caller. Called from the /merge/init handler.
func (b *LocalBackend) RegisterUpload(uploadID, brainID, profile, vault string, expires time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.uploads[uploadID] = localUploadState{
		BrainID: brainID, Profile: profile, Vault: vault, Expires: expires,
	}
}

// LookupUpload returns the state for an upload_id. Used by handlers
// to enforce the (profile, vault) of the token matches the binding
// holding the upload — defense in depth against a token that
// authenticates against the wrong vault.
func (b *LocalBackend) LookupUpload(uploadID string) (localUploadState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.uploads[uploadID]
	return s, ok
}

func (b *LocalBackend) NewUpload(brainID string, ttl time.Duration) (UploadHandle, error) {
	uploadID, err := newUploadID()
	if err != nil {
		return UploadHandle{}, err
	}
	expires := time.Now().Add(ttl)
	tok := b.sign(uploadID, brainID, expires)
	u := fmt.Sprintf("%s/api/brain/merge/upload/%s?token=%s&expires=%d",
		b.baseURL, uploadID, url.QueryEscape(tok), expires.Unix())
	return UploadHandle{
		UploadID: uploadID,
		URL:      u,
		Token:    tok,
		Expires:  expires,
	}, nil
}

func (b *LocalBackend) AcceptUpload(uploadID string, body io.Reader, maxBytes int64) (int64, error) {
	st, ok := b.LookupUpload(uploadID)
	if !ok {
		return 0, errors.New("server: unknown upload_id")
	}
	if time.Now().After(st.Expires) {
		return 0, errors.New("server: upload expired")
	}
	dir := filepath.Join(b.dataDir.BrainsDir(st.Profile, st.Vault), "_uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	path := filepath.Join(dir, uploadID+".tar")
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if maxBytes <= 0 {
		maxBytes = 5 << 30 // 5 GiB sane default
	}
	n, err := io.Copy(f, io.LimitReader(body, maxBytes+1))
	if err != nil {
		_ = os.Remove(path)
		return n, err
	}
	if n > maxBytes {
		_ = os.Remove(path)
		return n, fmt.Errorf("server: upload exceeds max %d bytes", maxBytes)
	}
	if err := f.Sync(); err != nil {
		return n, err
	}
	return n, nil
}

func (b *LocalBackend) CompleteUpload(profile, vault, brainID, uploadID string) (string, error) {
	st, ok := b.LookupUpload(uploadID)
	if !ok {
		return "", errors.New("server: unknown upload_id")
	}
	if st.Profile != profile || st.Vault != vault || st.BrainID != brainID {
		return "", errors.New("server: upload binding mismatch")
	}
	srcPath := filepath.Join(b.dataDir.BrainsDir(profile, vault), "_uploads", uploadID+".tar")
	if _, err := os.Stat(srcPath); err != nil {
		return "", fmt.Errorf("server: upload not on disk: %w", err)
	}
	dstDir := filepath.Join(b.dataDir.BrainsDir(profile, vault), "_pending")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", err
	}
	dstPath := filepath.Join(dstDir, brainID+".tar")
	if err := os.Rename(srcPath, dstPath); err != nil {
		return "", err
	}
	b.mu.Lock()
	delete(b.uploads, uploadID)
	b.mu.Unlock()
	return dstPath, nil
}

func (b *LocalBackend) VerifyToken(uploadID, token string) (string, error) {
	st, ok := b.LookupUpload(uploadID)
	if !ok {
		return "", errors.New("server: unknown upload_id")
	}
	expected := b.sign(uploadID, st.BrainID, st.Expires)
	if !hmac.Equal([]byte(expected), []byte(token)) {
		return "", errors.New("server: invalid token")
	}
	if time.Now().After(st.Expires) {
		return "", errors.New("server: token expired")
	}
	return st.BrainID, nil
}

// sign returns the HMAC-SHA256 of (upload_id || brain_id ||
// expires-unix) keyed with the daemon's secret. Tokens are
// constant-time-comparable via hmac.Equal.
func (b *LocalBackend) sign(uploadID, brainID string, expires time.Time) string {
	mac := hmac.New(sha256.New, b.hmacKey)
	fmt.Fprintf(mac, "%s|%s|%d", uploadID, brainID, expires.Unix())
	return hex.EncodeToString(mac.Sum(nil))
}

// newUploadID returns a hex-encoded 16-byte random ID.
func newUploadID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// PruneExpired drops upload state for entries past their TTL. Called
// opportunistically by the daemon (no dedicated goroutine — uploads
// are small and infrequent).
func (b *LocalBackend) PruneExpired() {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, st := range b.uploads {
		if now.After(st.Expires) {
			delete(b.uploads, id)
		}
	}
}

// --- MinIOBackend (Phase 5 wires the real client) --------------------

// MinIOBackend is a placeholder so server.toml's [storage] backend =
// "minio" can be selected without compile-time gating. Phase 5 wires
// minio-go for real presigned multipart. Phase 2 returns a clear
// error so operators don't accidentally configure it before the
// implementation is ready.
type MinIOBackend struct{}

// NewMinIOBackend reports the Phase-5 status to the operator
// immediately rather than failing at request time.
func NewMinIOBackend() (*MinIOBackend, error) {
	return nil, errors.New("server: minio backend not implemented yet (Phase 5)")
}

func (m *MinIOBackend) NewUpload(string, time.Duration) (UploadHandle, error) {
	return UploadHandle{}, errors.New("minio not implemented")
}
func (m *MinIOBackend) AcceptUpload(string, io.Reader, int64) (int64, error) {
	return 0, errors.New("minio not implemented")
}
func (m *MinIOBackend) CompleteUpload(string, string, string, string) (string, error) {
	return "", errors.New("minio not implemented")
}
func (m *MinIOBackend) VerifyToken(string, string) (string, error) {
	return "", errors.New("minio not implemented")
}

// parseDurationSecs is a tiny helper for handlers that read TTL in
// seconds from a JSON body. Returns the default when n <= 0.
func parseDurationSecs(s string, def time.Duration) time.Duration {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return time.Duration(n) * time.Second
}
