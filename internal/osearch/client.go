// Package osearch is the daemon's OpenSearch client. Phase 6 makes
// OpenSearch the canonical store of synthesized content: summaries,
// entities, and attachment metadata + extracted text. Agents keep
// local sqlite-vec + FTS5 caches built from snapshot tarballs that
// this package exports from the OS view.
//
// Layout:
//   - client.go (this file) — config + opensearch-go client wrapper
//   - schema.go             — index mappings + Ensure-on-startup
//   - docs.go               — typed doc structs (Summary, Entity, Attachment)
//   - upsert.go             — idempotent upsert by doc ID
//   - recall.go             — hybrid BM25 + kNN, RRF fusion
//   - export.go             — bulk scroll → sqlite-vec+FTS5 tarball
//
// Doc IDs are <profile>/<vault>/<sha256> within each index. The
// (profile, vault) keyword pair is denormalised onto every doc so
// query-time term filters scope results without index-per-vault
// proliferation.
package osearch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	opensearch "github.com/opensearch-project/opensearch-go/v4"
	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// Config holds the daemon's OpenSearch connection settings. Loaded
// from the [opensearch] block in server.toml (added later in Phase 6).
type Config struct {
	// Addresses is the list of OpenSearch endpoint URLs. Typically a
	// single entry: "http://opensearch:9200" inside the compose
	// network or "https://os.internal:9200" against a real cluster.
	Addresses []string

	// Username/Password for basic auth. Empty means no auth — fine
	// for the single-node dev-mode container; required in prod.
	Username string
	Password string

	// InsecureSkipVerify disables TLS cert verification. Only set
	// true for self-signed dev clusters; never in production.
	InsecureSkipVerify bool

	// RequestTimeout caps each HTTP round-trip to OS. The daemon's
	// write endpoints should fail fast rather than block the agent.
	RequestTimeout time.Duration

	// IndexPrefix lets tests/dev environments isolate state from
	// production. Empty means use the canonical names; "test_" yields
	// "test_pb_summaries", etc. Set via the OPENSEARCH_INDEX_PREFIX
	// env var so integration tests can sandbox themselves.
	IndexPrefix string
}

// DefaultConfig returns a Config with safe single-node-dev defaults.
// Callers override fields from server.toml before calling Open.
func DefaultConfig() Config {
	return Config{
		Addresses:      []string{"http://localhost:9200"},
		RequestTimeout: 10 * time.Second,
	}
}

// Client wraps the opensearch-go v4 client with the daemon-specific
// resolution of index names (so tests can prefix without touching
// every call site).
type Client struct {
	api    *osapi.Client
	prefix string
	cfg    Config
}

// Open dials OpenSearch, runs a one-shot info() probe to fail fast on
// misconfiguration, and returns a ready Client. It does NOT call
// EnsureIndices — that's the caller's responsibility at daemon startup
// so the error path is explicit in logs.
func Open(ctx context.Context, cfg Config) (*Client, error) {
	if len(cfg.Addresses) == 0 {
		return nil, errors.New("osearch: at least one address required")
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify},
	}

	osCfg := opensearch.Config{
		Addresses: cfg.Addresses,
		Username:  cfg.Username,
		Password:  cfg.Password,
		Transport: transport,
	}

	api, err := osapi.NewClient(osapi.Config{Client: osCfg})
	if err != nil {
		return nil, fmt.Errorf("osearch: build client: %w", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()

	if _, err := api.Info(probeCtx, nil); err != nil {
		return nil, fmt.Errorf("osearch: cluster info probe failed: %w", err)
	}

	return &Client{api: api, prefix: cfg.IndexPrefix, cfg: cfg}, nil
}

// API exposes the underlying typed client for callers that need a
// codepath not yet wrapped here (rare; prefer adding a method on Client).
func (c *Client) API() *osapi.Client { return c.api }

// IndexName resolves a logical index name (e.g. "pb_summaries") to
// its prefixed physical form. Use this everywhere instead of string
// constants so the IndexPrefix override works.
func (c *Client) IndexName(logical string) string {
	if c.prefix == "" {
		return logical
	}
	return c.prefix + strings.TrimPrefix(logical, "")
}

// WithPrefix returns a shallow-copy of the Client that routes every
// index operation through the supplied prefix instead of the one
// baked in at Open time. The underlying HTTP connection + auth are
// shared. Used by the daemon to derive per-binding views without
// opening a second TCP pool.
//
// Phase 7 (Level 2 per-binding storage): every (profile, vault)
// binding resolves to its own ResolvedStorage{IndexPrefix, Bucket}.
// Handlers look up the binding's view, call WithPrefix(binding.
// Storage.IndexPrefix), and hand the result to the OS write/read
// methods. Bindings with no override keep using the daemon-global
// prefix; bindings with [storage_overrides] get their prefixed
// physical indices.
func (c *Client) WithPrefix(prefix string) *Client {
	if c == nil {
		return nil
	}
	cp := *c
	cp.prefix = prefix
	return &cp
}

// Prefix returns the index prefix this Client is configured with.
// Used by callers that need to log "which physical index am I about
// to hit" without re-resolving via IndexName.
func (c *Client) Prefix() string { return c.prefix }
