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
	for _, idx := range []struct {
		logical string
		mapping map[string]any
	}{
		{IndexSummaries, summariesMapping()},
		{IndexEntities, entitiesMapping()},
		{IndexAttachments, attachmentsMapping()},
	} {
		if err := c.ensureIndex(ctx, idx.logical, idx.mapping); err != nil {
			return fmt.Errorf("ensure %s: %w", idx.logical, err)
		}
	}
	return nil
}

func (c *Client) ensureIndex(ctx context.Context, logical string, mapping map[string]any) error {
	name := c.IndexName(logical)

	existsReq := osapi.IndicesExistsReq{Indices: []string{name}}
	resp, err := c.api.Indices.Exists(ctx, existsReq)
	if err != nil {
		return fmt.Errorf("exists probe: %w", err)
	}
	// opensearch-go returns 200 for exists, 404 for not. Errors with
	// a non-2xx are returned as nil err + non-nil resp on this client,
	// so we check the status explicitly.
	switch {
	case resp != nil && resp.StatusCode == http.StatusOK:
		return nil
	case resp != nil && resp.StatusCode == http.StatusNotFound:
		// fallthrough to create
	case resp != nil:
		return fmt.Errorf("exists probe: unexpected status %d", resp.StatusCode)
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

func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "resource_already_exists_exception")
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
				"profile":       map[string]any{"type": "keyword"},
				"vault":         map[string]any{"type": "keyword"},
				"sha":           map[string]any{"type": "keyword"},
				"source_path":   map[string]any{"type": "keyword"},
				"source_url":    map[string]any{"type": "keyword"},
				"created_at":    map[string]any{"type": "date"},
				"updated_at":    map[string]any{"type": "date"},
				"title":         textWithRawKeyword(),
				"body":          map[string]any{"type": "text"},
				"raw_body":      map[string]any{"type": "text"},
				"tags":          map[string]any{"type": "keyword"},
				"topic":         map[string]any{"type": "keyword"},
				"reliability":   map[string]any{"type": "keyword"},
				"gate_reason":   map[string]any{"type": "text", "index": false},
				"entities":      map[string]any{"type": "keyword"},
				"attachments":   map[string]any{"type": "keyword"},
				"synthesised":   map[string]any{"type": "boolean"},
				"embedding":     knnVectorField(),
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
				"original_filename": textWithRawKeyword(),
				"title":             textWithRawKeyword(),
				"mime_type":         map[string]any{"type": "keyword"},
				"size_bytes":        map[string]any{"type": "long"},
				"created_at":        map[string]any{"type": "date"},
				"minio_key":         map[string]any{"type": "keyword"},
				"extracted_text":    map[string]any{"type": "text"},
				"summary_sha":       map[string]any{"type": "keyword"},
				"embedding":         knnVectorField(),
			},
		},
	}
}
