package server

import (
	"context"
	"sync"
	"time"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// Level 2 per-binding storage overrides (v3.2): each VaultBinding
// resolves to its own ResolvedStorage{IndexPrefix, Bucket}. The
// daemon shares ONE *osearch.Client (one TCP pool) and ONE
// *MinIOBackend (one credential), but per-binding "views" thread the
// resolved prefix / bucket through to the underlying API.
//
// Two view types:
//   - osBindingView wraps the shared *osearch.Client via WithPrefix.
//     Satisfies osWriter, osExporter, and the backfillStubClient
//     interface — anything keyed by a single binding goes through here.
//   - minioBindingView wraps the shared *MinIOBackend with a per-binding
//     bucket. Satisfies AttachmentStore. The death-payload uploader is
//     intentionally NOT plumbed through here (uploads stay on the
//     shared bucket; they're per-vault by object key, not by bucket).
//
// Both views are constructed once per binding at startup and cached on
// the Daemon by VaultKey. The handler middleware fetches the view via
// d.viewForBinding(binding) and threads it down — no per-request
// allocation beyond the cache lookup.

// osBindingView is a *osearch.Client pre-bound to a binding's prefix.
type osBindingView struct {
	client *osearch.Client
}

// newOSBindingView returns a view of base with the supplied prefix.
// base must be non-nil; callers gate on cfg.OpenSearch.Enabled() first.
func newOSBindingView(base *osearch.Client, prefix string) *osBindingView {
	return &osBindingView{client: base.WithPrefix(prefix)}
}

// Client returns the underlying prefixed *osearch.Client. Used by
// callers that need to invoke methods not on the osWriter slice
// (notably Export + ScrollAttachments + Refresh).
func (v *osBindingView) Client() *osearch.Client { return v.client }

// --- osWriter ----------------------------------------------------------

func (v *osBindingView) UpsertSummary(ctx context.Context, doc osearch.SummaryDoc, waitForRefresh bool) error {
	return v.client.UpsertSummary(ctx, doc, waitForRefresh)
}
func (v *osBindingView) GetSummary(ctx context.Context, profile, vault, sha string) (*osearch.SummaryDoc, error) {
	return v.client.GetSummary(ctx, profile, vault, sha)
}
func (v *osBindingView) UpsertEntity(ctx context.Context, doc osearch.EntityDoc, waitForRefresh bool) error {
	return v.client.UpsertEntity(ctx, doc, waitForRefresh)
}
func (v *osBindingView) GetEntity(ctx context.Context, profile, vault, slug string) (*osearch.EntityDoc, error) {
	return v.client.GetEntity(ctx, profile, vault, slug)
}
func (v *osBindingView) UpsertAttachment(ctx context.Context, doc osearch.AttachmentDoc, waitForRefresh bool) error {
	return v.client.UpsertAttachment(ctx, doc, waitForRefresh)
}
func (v *osBindingView) GetAttachment(ctx context.Context, profile, vault, sha string) (*osearch.AttachmentDoc, error) {
	return v.client.GetAttachment(ctx, profile, vault, sha)
}

// v3.3 brain_reflect / brain_forget (issue #72 Phase 1). v.client is
// already prefix-bound (newOSBindingView wraps base.WithPrefix), so
// the plain (non-WithPrefix) method variants resolve to the binding's
// index set — matching how UpsertSummary etc. forward above.
func (v *osBindingView) DeleteSummary(ctx context.Context, profile, vault, sha string) error {
	return v.client.DeleteSummary(ctx, profile, vault, sha)
}
func (v *osBindingView) ScrollSummaries(ctx context.Context, profile, vault string, batchSize int, fn func(osearch.SummaryDoc) error) error {
	return v.client.ScrollSummaries(ctx, profile, vault, batchSize, fn)
}

// --- osExporter --------------------------------------------------------

func (v *osBindingView) Export(ctx context.Context, opts osearch.ExportOptions) (osearch.ExportManifest, error) {
	return v.client.Export(ctx, opts)
}

// --- AttachmentStore (per-binding bucket) ------------------------------

// minioBindingView wraps a shared *MinIOBackend with a per-binding
// bucket. Implements AttachmentStore; all reads/writes go to the
// binding's resolved bucket.
type minioBindingView struct {
	backend *MinIOBackend
	bucket  string
}

func newMinIOBindingView(backend *MinIOBackend, bucket string) *minioBindingView {
	return &minioBindingView{backend: backend, bucket: bucket}
}

func (v *minioBindingView) PutAttachment(ctx context.Context, profile, vault, sha, ext string, body []byte, contentType string) (string, error) {
	return v.backend.PutAttachmentInBucket(ctx, v.bucket, profile, vault, sha, ext, body, contentType)
}
func (v *minioBindingView) PutAttachmentWithTags(ctx context.Context, profile, vault, sha, ext string, body []byte, contentType string, indexTags []string) (string, error) {
	return v.backend.PutAttachmentWithTagsInBucket(ctx, v.bucket, profile, vault, sha, ext, body, contentType, indexTags)
}
func (v *minioBindingView) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return v.backend.PresignGetInBucket(ctx, v.bucket, key, ttl)
}
func (v *minioBindingView) GetAttachmentBytes(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	return v.backend.GetAttachmentBytesInBucket(ctx, v.bucket, key, maxBytes)
}

// --- binding view cache ------------------------------------------------

// bindingDeps holds the per-binding views the daemon resolved at
// startup (or on SIGHUP reload). Read-mostly: handlers Get() per
// request, reload() Set()s when bindings change.
//
// OS is the osWriter interface (not the concrete *osBindingView) so
// tests can inject in-memory fakes without bringing up an osearch
// client. Exporter holds the concrete view for snapshot rebuilds,
// which need the *osearch.Client surface (Export).
type bindingDeps struct {
	OS       osWriter        // nil when OS is not configured
	Exporter osExporter      // same underlying handle in production
	Attach   AttachmentStore // nil when MinIO backend isn't wired

	// PG is the Phase A per-binding Postgres view (System-of-Record +
	// projection Recaller/Projector). nil when Postgres is not configured
	// or its per-profile resources failed to build (non-fatal — legacy
	// path is unaffected). Resolved via Daemon.resolvePG; NO handler
	// consumes it yet (dormant, additive).
	PG *pgBindingView
}

// bindingDepCache is a tiny rwmutex-guarded map keyed by VaultKey.
// Stored on the Daemon; populated by buildBindingDeps and refreshed
// by reload(). The handlers fetch through Daemon.depsForBinding.
type bindingDepCache struct {
	mu   sync.RWMutex
	byKey map[VaultKey]*bindingDeps
}

func newBindingDepCache() *bindingDepCache {
	return &bindingDepCache{byKey: map[VaultKey]*bindingDeps{}}
}

func (c *bindingDepCache) Get(k VaultKey) (*bindingDeps, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	d, ok := c.byKey[k]
	return d, ok
}

func (c *bindingDepCache) Set(k VaultKey, d *bindingDeps) {
	c.mu.Lock()
	c.byKey[k] = d
	c.mu.Unlock()
}

func (c *bindingDepCache) Delete(k VaultKey) {
	c.mu.Lock()
	delete(c.byKey, k)
	c.mu.Unlock()
}
