package server

import (
	"bytes"
	"context"
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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
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

// OverrideBaseURL replaces the URL fragment used to build presigned
// upload URLs. Production callers never use this; integration tests
// need it so URLs returned by /merge/init route back to the
// httptest.Server they spun up (instead of the daemon's nominal :0
// listener which isn't actually serving).
func (b *LocalBackend) OverrideBaseURL(u string) {
	b.mu.Lock()
	b.baseURL = strings.TrimRight(u, "/")
	b.mu.Unlock()
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

// --- MinIOBackend ----------------------------------------------------

// MinIOBackend stores death-payload uploads in an S3-compatible
// bucket via minio-go. The brain PUTs the tarball directly to a
// presigned URL — daemon never sees the bytes — and CompleteUpload
// streams them back to local disk via GetObject before kicking the
// payload over to the reaper's brains/_pending/ tree. The hop
// through local disk keeps the reaper backend-agnostic (it always
// reads from the local FS) at the cost of one round-trip.
//
// Bucket layout:
//
//	<profile>/<vault>/_uploads/<upload_id>.tar   ← presigned PUT target
//
// After CompleteUpload moves the bytes to local _pending/, the
// upload key is deleted from MinIO so an aborted ship doesn't leave
// a permanent object behind. TTL expiry is managed via a bucket
// lifecycle rule (operator-configured; out of scope for the daemon).
//
// Single PUT only in Phase 5 — multipart is a future optimisation;
// at typical death-payload sizes (tens of MB) single PUT is plenty.
type MinIOBackend struct {
	client  *minio.Client
	bucket  string
	dataDir DataDir

	mu      sync.Mutex
	uploads map[string]minioUploadState
}

type minioUploadState struct {
	BrainID string
	Profile string
	Vault   string
	Bucket  string // v3.2: bucket resolved at presign; CompleteUpload reads from this
	Expires time.Time
	ObjKey  string
}

// MinIOOptions narrows what NewMinIOBackend needs. Mirrors the
// server.toml [storage] block; loaded by Start.
type MinIOOptions struct {
	Endpoint  string // "minio.example.com" or "minio.example.com:9000"
	Bucket    string
	AccessKey string
	SecretKey string
	UseSSL    bool

	// DataDir is needed by CompleteUpload — it writes the
	// downloaded payload to brains/_pending/ on local disk so the
	// reaper (which is FS-only) picks it up unchanged.
	DataDir DataDir
}

// NewMinIOBackend builds + sanity-checks the client. Errors at
// construction time (rather than first-request time) so the
// daemon fails loud on misconfiguration during startup.
func NewMinIOBackend(opts MinIOOptions) (*MinIOBackend, error) {
	if strings.TrimSpace(opts.Endpoint) == "" {
		return nil, errors.New("server: minio backend requires endpoint")
	}
	if strings.TrimSpace(opts.Bucket) == "" {
		return nil, errors.New("server: minio backend requires bucket")
	}
	if strings.TrimSpace(opts.AccessKey) == "" || strings.TrimSpace(opts.SecretKey) == "" {
		return nil, errors.New("server: minio backend requires access_key + secret_key")
	}
	if opts.DataDir == "" {
		return nil, errors.New("server: minio backend requires DataDir")
	}
	cli, err := minio.New(opts.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(opts.AccessKey, opts.SecretKey, ""),
		Secure: opts.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("server: minio client: %w", err)
	}
	return &MinIOBackend{
		client:  cli,
		bucket:  opts.Bucket,
		dataDir: opts.DataDir,
		uploads: map[string]minioUploadState{},
	}, nil
}

// resolveBucket picks the per-call bucket when set, otherwise falls
// back to the construction-time default. v3.2 Level 2: callers that
// already know the binding-resolved bucket pass it explicitly; legacy
// single-bucket callers pass "" and get the default. Empty string
// after fallback is a programmer error caught at construction time
// by NewMinIOBackend's bucket-required check.
func (m *MinIOBackend) resolveBucket(bucket string) string {
	if bucket != "" {
		return bucket
	}
	return m.bucket
}

// PutAttachment stores body at the canonical attachment key for
// (profile, vault, sha, ext) using the construction-time default
// bucket. Implements AttachmentStore (Phase 6). Idempotent — re-puts
// of the same content overwrite the same key since MinIO PutObject
// is last-write-wins on a content-addressed path. Returns the resolved
// object key the daemon writes into the OS attachment doc's MinIOKey
// field. v3.2 callers wanting a per-binding bucket use
// PutAttachmentInBucket (or the minioBindingView wrapper).
func (m *MinIOBackend) PutAttachment(ctx context.Context, profile, vault, sha, ext string, body []byte, contentType string) (string, error) {
	return m.PutAttachmentWithTagsInBucket(ctx, m.bucket, profile, vault, sha, ext, body, contentType, nil)
}

// PutAttachmentInBucket is PutAttachment targeted at an explicit
// bucket. v3.2 per-binding storage overrides: each binding's
// minioBindingView holds its resolved bucket and calls this directly
// rather than relying on the backend's struct-level bucket. The
// default-bucket convenience method above is preserved so callers
// without a per-binding view (legacy / tests) keep working.
func (m *MinIOBackend) PutAttachmentInBucket(ctx context.Context, bucket, profile, vault, sha, ext string, body []byte, contentType string) (string, error) {
	return m.PutAttachmentWithTagsInBucket(ctx, bucket, profile, vault, sha, ext, body, contentType, nil)
}

// PutAttachmentWithTags is PutAttachment with index-side tags mirrored
// onto the MinIO object as S3 object tags. v2.5.1: keeps MinIO and the
// pb_attachments index in sync at attach time so lifecycle policies and
// downstream consumers can filter on the same tag set the agent sees.
// One-way: drift can occur if the index tags change later — bulk re-sync
// is a future brain_reflect concern.
//
// indexTags is the flat []string the agent provides. encodeMinIOTags
// splits "key:value" pairs and packs bare tags under tag1..tagN.
// S3 caps at 10 object tags; extras are dropped with no error so a
// well-tagged ingest doesn't break.
func (m *MinIOBackend) PutAttachmentWithTags(ctx context.Context, profile, vault, sha, ext string, body []byte, contentType string, indexTags []string) (string, error) {
	return m.PutAttachmentWithTagsInBucket(ctx, m.bucket, profile, vault, sha, ext, body, contentType, indexTags)
}

// PutAttachmentWithTagsInBucket is PutAttachmentWithTags scoped to an
// explicit bucket (v3.2 per-binding storage). Default-bucket variant
// above delegates here. Empty bucket falls back to the backend default.
func (m *MinIOBackend) PutAttachmentWithTagsInBucket(ctx context.Context, bucket, profile, vault, sha, ext string, body []byte, contentType string, indexTags []string) (string, error) {
	b := m.resolveBucket(bucket)
	key := fmt.Sprintf("%s/%s/attachments/%s%s", profile, vault, sha, ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	opts := minio.PutObjectOptions{ContentType: contentType}
	if userTags := encodeMinIOTags(indexTags); len(userTags) > 0 {
		opts.UserTags = userTags
	}
	_, err := m.client.PutObject(ctx, b, key, bytes.NewReader(body), int64(len(body)), opts)
	if err != nil {
		return "", fmt.Errorf("server: minio put attachment %s in bucket %s: %w", key, b, err)
	}
	return key, nil
}

// encodeMinIOTags turns the agent's flat tag slice into the
// map[string]string that minio-go's UserTags option wants. Splits the
// first `:` of each tag — `vendor:UIA` -> {vendor: UIA}. Bare tags
// (`anatomy`) become {tag1: anatomy}, {tag2: ...} etc. S3 caps at 10
// object tags; anything past the cap is silently dropped to keep
// well-tagged ingests from failing. Tag chars that fall outside S3's
// validTagKeyValue regex are skipped (the regex allows alphanumerics,
// dashes, dots, underscores, colons, slashes, plus signs, at-signs,
// equals, and spaces — covers every tag shape phantom-brain uses).
func encodeMinIOTags(indexTags []string) map[string]string {
	if len(indexTags) == 0 {
		return nil
	}
	const maxObjectTags = 10
	out := make(map[string]string, len(indexTags))
	bare := 0
	for _, raw := range indexTags {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if len(out) >= maxObjectTags {
			break
		}
		var k, v string
		if i := strings.Index(raw, ":"); i > 0 && i < len(raw)-1 {
			k = strings.TrimSpace(raw[:i])
			v = strings.TrimSpace(raw[i+1:])
		} else {
			bare++
			k = fmt.Sprintf("tag%d", bare)
			v = raw
		}
		if !minioTagValid(k) || !minioTagValid(v) {
			continue
		}
		if len(k) > 128 || len(v) > 256 {
			continue
		}
		if _, exists := out[k]; exists {
			continue
		}
		out[k] = v
	}
	return out
}

var minioTagCharset = regexp.MustCompile(`^[a-zA-Z0-9\-+._:/@ =]+$`)

func minioTagValid(s string) bool {
	if s == "" {
		return false
	}
	return minioTagCharset.MatchString(s)
}

// GetAttachmentBytes streams the object at key into memory. Used by
// the SynthWorker to fetch attachments (PDFs) for daemon-side text
// extraction. maxBytes caps the read; 0 selects a 256 MiB ceiling.
// Returning the body in-memory matches the input shape of the PDF
// extractor (pdftotext reads stdin); the SynthWorker holds one of
// these at a time.
func (m *MinIOBackend) GetAttachmentBytes(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	return m.GetAttachmentBytesInBucket(ctx, m.bucket, key, maxBytes)
}

// GetAttachmentBytesInBucket is GetAttachmentBytes scoped to an
// explicit bucket (v3.2 per-binding storage). Empty bucket falls back
// to the backend default.
func (m *MinIOBackend) GetAttachmentBytesInBucket(ctx context.Context, bucket, key string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = 256 << 20
	}
	b := m.resolveBucket(bucket)
	obj, err := m.client.GetObject(ctx, b, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("server: minio get attachment %s from bucket %s: %w", key, b, err)
	}
	defer obj.Close()
	buf, err := io.ReadAll(io.LimitReader(obj, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("server: read attachment %s: %w", key, err)
	}
	if int64(len(buf)) > maxBytes {
		return nil, fmt.Errorf("server: attachment %s exceeds max %d bytes", key, maxBytes)
	}
	return buf, nil
}

// PresignGet returns a short-lived URL the agent can GET to retrieve
// the blob. Implements AttachmentStore (Phase 6). ttl bounds validity;
// the daemon's /api/brain/attach/{sha} handler typically passes
// 10 minutes — long enough for an agent to follow the redirect, short
// enough that a leaked URL expires fast.
func (m *MinIOBackend) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return m.PresignGetInBucket(ctx, m.bucket, key, ttl)
}

// PresignGetInBucket is PresignGet scoped to an explicit bucket
// (v3.2 per-binding storage). Empty bucket falls back to the
// backend default.
func (m *MinIOBackend) PresignGetInBucket(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	b := m.resolveBucket(bucket)
	u, err := m.client.PresignedGetObject(ctx, b, key, ttl, nil)
	if err != nil {
		return "", fmt.Errorf("server: minio presign get %s in bucket %s: %w", key, b, err)
	}
	return u.String(), nil
}

// BucketInfo is the operator-CLI-facing summary returned by ListBuckets.
// Mirrors minio.BucketInfo but exposed in our own type so cmd/ doesn't
// pull minio-go directly.
type BucketInfo struct {
	Name         string
	CreationDate time.Time
}

// CreateBucket is the idempotent bucket-create entry used by the
// operator CLI. Treats "already owned by you" as success so re-running
// `pbrainctl server bucket create foo` after a previous successful run
// exits 0.
func (m *MinIOBackend) CreateBucket(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("server: CreateBucket requires a name")
	}
	err := m.client.MakeBucket(ctx, name, minio.MakeBucketOptions{})
	if err == nil {
		return nil
	}
	switch minio.ToErrorResponse(err).Code {
	case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
		return nil
	}
	return fmt.Errorf("server: minio make bucket %q: %w", name, err)
}

// ListBuckets returns every bucket the daemon's MinIO credentials can
// see. Used by the operator CLI; daemon code paths use the binding-
// resolved bucket directly.
func (m *MinIOBackend) ListBuckets(ctx context.Context) ([]BucketInfo, error) {
	raw, err := m.client.ListBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("server: minio list buckets: %w", err)
	}
	out := make([]BucketInfo, 0, len(raw))
	for _, b := range raw {
		out = append(out, BucketInfo{Name: b.Name, CreationDate: b.CreationDate})
	}
	return out, nil
}

// RemoveBucketWithObjects empties the bucket then deletes it. Used by
// `pbrainctl server binding delete --purge-data`. Refuses to operate on
// the daemon's default bucket — purging that would wipe every binding's
// attachments.
func (m *MinIOBackend) RemoveBucketWithObjects(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("server: RemoveBucketWithObjects requires a name")
	}
	if name == m.bucket {
		return fmt.Errorf("server: refusing to remove daemon default bucket %q", name)
	}
	objCh := m.client.ListObjects(ctx, name, minio.ListObjectsOptions{Recursive: true})
	for rerr := range m.client.RemoveObjects(ctx, name, objCh, minio.RemoveObjectsOptions{}) {
		if rerr.Err != nil {
			return fmt.Errorf("server: minio remove object %s/%s: %w", name, rerr.ObjectName, rerr.Err)
		}
	}
	if err := m.client.RemoveBucket(ctx, name); err != nil {
		return fmt.Errorf("server: minio remove bucket %q: %w", name, err)
	}
	return nil
}

// EnsureBucketExists returns nil when the bucket exists, an error
// when it doesn't. Does NOT create — per the Level 2 contract the
// operator runs `mc mb` first. Idempotent — called once per distinct
// bucket at startup.
func (m *MinIOBackend) EnsureBucketExists(ctx context.Context, bucket string) error {
	if bucket == "" {
		return errors.New("server: EnsureBucketExists requires a bucket")
	}
	ok, err := m.client.BucketExists(ctx, bucket)
	if err != nil {
		return fmt.Errorf("server: minio bucket exists probe %q: %w", bucket, err)
	}
	if !ok {
		return fmt.Errorf("server: minio bucket %q does not exist — create it (mc mb) before starting the daemon", bucket)
	}
	return nil
}

// RegisterUpload pre-records the (profile, vault) binding for an
// upload_id so CompleteUpload can route the downloaded bytes to the
// right vault's _pending/ dir. Called by the /merge/init handler
// right after NewUpload returns. Symmetric with LocalBackend.
func (m *MinIOBackend) RegisterUpload(uploadID, brainID, profile, vault, bucket, objKey string, expires time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uploads[uploadID] = minioUploadState{
		BrainID: brainID, Profile: profile, Vault: vault,
		Bucket: m.resolveBucket(bucket), Expires: expires, ObjKey: objKey,
	}
}

// NewUpload presigns a PUT URL the brain can PUT the tarball to
// without a bearer token (the presigned URL carries S3 SigV4 auth).
// brainID is encoded in the object key so an operator inspecting the
// bucket can attribute orphaned uploads.
//
// NOTE: NewUpload returns a placeholder URL; the handler calls
// PresignedPutForUpload after RegisterUpload to do the real
// presign once we know which (profile, vault) the object key
// belongs under. Two-step so the upload state and the presign
// stay in sync. (LocalBackend doesn't need this because its sign()
// is purely derived from the upload_id.)
func (m *MinIOBackend) NewUpload(brainID string, ttl time.Duration) (UploadHandle, error) {
	uploadID, err := newUploadID()
	if err != nil {
		return UploadHandle{}, err
	}
	return UploadHandle{
		UploadID: uploadID,
		Expires:  time.Now().Add(ttl),
		// URL and Token are populated by PresignedPutForUpload once
		// the handler has bound the upload to a (profile, vault).
	}, nil
}

// PresignedPutForUpload finishes the NewUpload handshake by presigning
// the actual PUT URL. Split from NewUpload so the handler can call
// RegisterUpload (which carries the bound (profile, vault)) between
// the two steps without juggling extra parameters in the storage
// interface.
func (m *MinIOBackend) PresignedPutForUpload(ctx context.Context, bucket, profile, vault, uploadID string, ttl time.Duration) (string, string, error) {
	b := m.resolveBucket(bucket)
	objKey := fmt.Sprintf("%s/%s/_uploads/%s.tar", profile, vault, uploadID)
	u, err := m.client.PresignedPutObject(ctx, b, objKey, ttl)
	if err != nil {
		return "", "", fmt.Errorf("server: presign put in bucket %s: %w", b, err)
	}
	// Token isn't strictly used by MinIO (SigV4 query params are the
	// auth); we return the object key so the handler can stash it on
	// the upload state for CompleteUpload to find.
	return u.String(), objKey, nil
}

// AcceptUpload is a hard error for MinIO — uploads bypass the daemon
// entirely. If a client somehow PUTs to the local upload route while
// the MinIO backend is configured, surfacing the misconfiguration
// loudly is better than silently dropping the bytes.
func (m *MinIOBackend) AcceptUpload(string, io.Reader, int64) (int64, error) {
	return 0, errors.New("server: AcceptUpload is local-backend only — MinIO uploads go directly to S3 via the presigned URL")
}

// CompleteUpload pulls the object from MinIO down to brains/_pending/
// on local disk so the reaper finds it next pass. Deletes the MinIO
// object on success.
func (m *MinIOBackend) CompleteUpload(profile, vault, brainID, uploadID string) (string, error) {
	st, ok := m.lookupUpload(uploadID)
	if !ok {
		return "", errors.New("server: unknown upload_id")
	}
	if st.Profile != profile || st.Vault != vault || st.BrainID != brainID {
		return "", errors.New("server: upload binding mismatch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Stream the object down to local disk via a temp file + rename
	// so a torn download can't leave a half-written tar in
	// _pending/ for the reaper to choke on.
	dstDir := filepath.Join(m.dataDir.BrainsDir(profile, vault), "_pending")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", err
	}
	dstPath := filepath.Join(dstDir, brainID+".tar")
	tmpPath := dstPath + ".tmp"

	tmp, err := os.Create(tmpPath)
	if err != nil {
		return "", err
	}
	bucket := st.Bucket
	if bucket == "" {
		bucket = m.bucket
	}
	obj, err := m.client.GetObject(ctx, bucket, st.ObjKey, minio.GetObjectOptions{})
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("server: get object %s from bucket %s: %w", st.ObjKey, bucket, err)
	}
	defer obj.Close()
	if _, err := io.Copy(tmp, obj); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("server: stream object %s: %w", st.ObjKey, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}

	// Best-effort cleanup of the MinIO upload. Failure here is
	// logged-but-not-fatal — bucket lifecycle policy is the backstop
	// for orphaned uploads.
	_ = m.client.RemoveObject(ctx, bucket, st.ObjKey, minio.RemoveObjectOptions{})

	m.mu.Lock()
	delete(m.uploads, uploadID)
	m.mu.Unlock()

	return dstPath, nil
}

// VerifyToken is unused for MinIO — the presigned URL carries S3
// SigV4 query parameters which the S3 server validates directly.
// Returns nil so callers that check "is this token valid?" treat
// MinIO as always-valid. The daemon's upload route is gated to
// local backend only, so this method is effectively unreachable.
func (m *MinIOBackend) VerifyToken(string, string) (string, error) {
	return "", errors.New("server: VerifyToken is local-backend only — MinIO auth is in the presigned URL")
}

// lookupUpload mirrors LocalBackend.LookupUpload. Returns the
// recorded binding for an upload_id.
func (m *MinIOBackend) lookupUpload(uploadID string) (minioUploadState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.uploads[uploadID]
	return s, ok
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
