package osearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// EnsureIndices is idempotent: creates pb_summaries, pb_entities,
// pb_attachments with the schemas defined below if they don't exist.
// Called once at daemon startup. Returns the first error encountered;
// no rollback — a partial create leaves the cluster in a usable but
// degraded state and the next startup completes it.
//
// Mappings:
//   - profile/vault: keyword, for term filters
//   - sha/slug: keyword, doc-ID hint
//   - title/body/raw_body/extracted_text: text with standard analyser
//     + fields.raw keyword sub-field for exact-match lookups
//   - tags/entities/attachments/mentioned_by/aliases: keyword arrays
//   - topic/reliability: keyword (low-cardinality, faceted)
//   - embedding: knn_vector with HNSW, cosine similarity, dim=768
//   - created_at/updated_at: date
//   - size_bytes: long
func (c *Client) EnsureIndices(ctx context.Context) error {
	return c.EnsureIndicesWithPrefix(ctx, c.prefix)
}

// EnsureIndicesWithPrefix runs the same idempotent create-if-missing
// pass as EnsureIndices, but resolves index names against the
// supplied prefix instead of the client's stored default. Daemon
// startup calls this once per distinct binding prefix so every
// (profile, vault) backed by an override gets its own physical
// indices created before HTTP serves.
func (c *Client) EnsureIndicesWithPrefix(ctx context.Context, prefix string) error {
	for _, idx := range []struct {
		logical string
		mapping map[string]any
	}{
		{IndexSummaries, summariesMapping()},
		{IndexEntities, entitiesMapping()},
		{IndexAttachments, attachmentsMapping()},
	} {
		if err := c.ensureIndex(ctx, prefix, idx.logical, idx.mapping); err != nil {
			return fmt.Errorf("ensure %s: %w", idx.logical, err)
		}
	}
	return nil
}

// EnsurePrefixes ensures the index set exists for every distinct
// prefix in the supplied slice. Duplicates are dropped; order is
// irrelevant. Used by daemon startup once the binding registry is
// loaded.
func (c *Client) EnsurePrefixes(ctx context.Context, prefixes []string) error {
	seen := make(map[string]struct{}, len(prefixes))
	for _, p := range prefixes {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		if err := c.EnsureIndicesWithPrefix(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) ensureIndex(ctx context.Context, prefix, logical string, mapping map[string]any) error {
	name := IndexNameWithPrefix(prefix, logical)

	existsReq := osapi.IndicesExistsReq{Indices: []string{name}}
	resp, err := c.api.Indices.Exists(ctx, existsReq)
	// opensearch-go v4 surfaces non-2xx as a typed error. The raw
	// response (or the parsed error message) carries the status: 200
	// → exists; 404 → fall through to create.
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if status == 0 {
		status = statusFromErr(err)
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		// fall through to create
	default:
		if err != nil {
			return fmt.Errorf("exists probe: %w", err)
		}
		return fmt.Errorf("exists probe: unexpected status %d", status)
	}

	body, err := json.Marshal(mapping)
	if err != nil {
		return fmt.Errorf("marshal mapping: %w", err)
	}

	createReq := osapi.IndicesCreateReq{
		Index: name,
		Body:  bytes.NewReader(body),
	}
	createResp, err := c.api.Indices.Create(ctx, createReq)
	if err != nil {
		// Tolerate the race where another daemon instance created the
		// index between our exists probe and our create call.
		if isAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create: %w", err)
	}
	if createResp != nil && createResp.Inspect().Response != nil &&
		createResp.Inspect().Response.StatusCode >= 400 {
		body, _ := io.ReadAll(createResp.Inspect().Response.Body)
		return fmt.Errorf("create returned %d: %s",
			createResp.Inspect().Response.StatusCode, string(body))
	}
	return nil
}

// DeleteIndex drops the physical index resolved from this client's
// prefix + the supplied logical name. Used by the operator CLI
// (`pbrainctl server binding delete --purge-data`) to tear down a
// binding's prefixed indices. Treats 404 as success so re-runs are
// idempotent.
func (c *Client) DeleteIndex(ctx context.Context, logical string) error {
	name := c.IndexName(logical)
	resp, err := c.api.Indices.Delete(ctx, osapi.IndicesDeleteReq{Indices: []string{name}})
	if err != nil {
		if statusFromErr(err) == http.StatusNotFound {
			return nil
		}
		if resp != nil && resp.Inspect().Response != nil &&
			resp.Inspect().Response.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("delete %s: %w", name, err)
	}
	return nil
}

func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "resource_already_exists_exception")
}

// statusFromErr parses the HTTP status code out of an opensearch-go
// typed error message, which formats as "status: [NNN ...]". Returns
// 0 when the message doesn't contain a status.
func statusFromErr(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	idx := strings.Index(msg, "status: [")
	if idx == -1 {
		return 0
	}
	tail := msg[idx+len("status: ["):]
	var n int
	for _, ch := range tail {
		if ch < '0' || ch > '9' {
			break
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

// commonSettings enables k-NN at the index level (required for the
// knn_vector field type) and uses sane defaults for a single-node
// dev cluster.
func commonSettings() map[string]any {
	return map[string]any{
		"index": map[string]any{
			"knn":                  true,
			"number_of_shards":     1,
			"number_of_replicas":   0,
			"refresh_interval":     "1s",
		},
	}
}

func textWithRawKeyword() map[string]any {
	return map[string]any{
		"type": "text",
		"fields": map[string]any{
			"raw": map[string]any{"type": "keyword", "ignore_above": 256},
		},
	}
}

func knnVectorField() map[string]any {
	return map[string]any{
		"type":      "knn_vector",
		"dimension": EmbeddingDim,
		"method": map[string]any{
			"name":       "hnsw",
			"space_type": "cosinesimil",
			"engine":     "lucene",
			"parameters": map[string]any{"ef_construction": 128, "m": 16},
		},
	}
}

func summariesMapping() map[string]any {
	return map[string]any{
		"settings": commonSettings(),
		"mappings": map[string]any{
			"properties": map[string]any{
				"profile":     map[string]any{"type": "keyword"},
				"vault":       map[string]any{"type": "keyword"},
				"sha":         map[string]any{"type": "keyword"},
				"kind":        map[string]any{"type": "keyword"},
				"memory_type": map[string]any{"type": "keyword"},
				"source_path": map[string]any{"type": "keyword"},
				"source_url":  map[string]any{"type": "keyword"},
				"source":      map[string]any{"type": "keyword"},
				"created_at":  map[string]any{"type": "date"},
				"updated_at":  map[string]any{"type": "date"},
				"captured_at": map[string]any{"type": "date"},
				"title":       textWithRawKeyword(),
				"body":        map[string]any{"type": "text"},
				"raw_body":    map[string]any{"type": "text"},
				"tags":        map[string]any{"type": "keyword"},
				"topic":       map[string]any{"type": "keyword"},
				"reliability": map[string]any{"type": "keyword"},
				"gate_reason": map[string]any{"type": "text", "index": false},
				"entities":           map[string]any{"type": "keyword"},
				"attachments":        map[string]any{"type": "keyword"},
				"references":         map[string]any{"type": "keyword"},
				"capture_minio_key":  map[string]any{"type": "keyword"},
				"capture_size_bytes": map[string]any{"type": "long"},
				"synthesised":        map[string]any{"type": "boolean"},
				"embedding":          knnVectorField(),
			},
		},
	}
}

func entitiesMapping() map[string]any {
	return map[string]any{
		"settings": commonSettings(),
		"mappings": map[string]any{
			"properties": map[string]any{
				"profile":      map[string]any{"type": "keyword"},
				"vault":        map[string]any{"type": "keyword"},
				"slug":         map[string]any{"type": "keyword"},
				"name":         textWithRawKeyword(),
				"aliases":      map[string]any{"type": "keyword"},
				"body":         map[string]any{"type": "text"},
				"tags":         map[string]any{"type": "keyword"},
				"topic":        map[string]any{"type": "keyword"},
				"mentioned_by": map[string]any{"type": "keyword"},
				"created_at":   map[string]any{"type": "date"},
				"updated_at":   map[string]any{"type": "date"},
				"embedding":    knnVectorField(),
			},
		},
	}
}

func attachmentsMapping() map[string]any {
	return map[string]any{
		"settings": commonSettings(),
		"mappings": map[string]any{
			"properties": map[string]any{
				"profile":           map[string]any{"type": "keyword"},
				"vault":             map[string]any{"type": "keyword"},
				"sha":               map[string]any{"type": "keyword"},
				"kind":              map[string]any{"type": "keyword"},
				"memory_type":       map[string]any{"type": "keyword"},
				"original_filename": textWithRawKeyword(),
				"title":             textWithRawKeyword(),
				"mime_type":         map[string]any{"type": "keyword"},
				"size_bytes":        map[string]any{"type": "long"},
				"created_at":        map[string]any{"type": "date"},
				"captured_at":       map[string]any{"type": "date"},
				"minio_key":         map[string]any{"type": "keyword"},
				"extracted_text":    map[string]any{"type": "text"},
				"summary_sha":       map[string]any{"type": "keyword"},
				"source":            map[string]any{"type": "keyword"},
				"references":        map[string]any{"type": "keyword"},
				"tags":              map[string]any{"type": "keyword"},
				"embedding":         knnVectorField(),
			},
		},
	}
}
