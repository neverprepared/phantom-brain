package osearch

import (
	"context"
	"testing"
)

// Phase D2b: the legacy pb_summaries / pb_entities / pb_attachments index
// mappings (and the EnsureIndices* wrappers) were removed from production —
// those indices are no longer written. The builders survive here as TEST
// fixtures so the live-OpenSearch round-trip tests can still bootstrap the
// legacy indices and exercise the retained osearch.Client CRUD + Recall
// methods, and so docs_test.go can assert the mapping JSON is well-formed.
// They go through the retained ensureIndex primitive via the exported
// EnsureIndexWithMapping wrapper.

// ensureLegacyIndices creates the three legacy indices under prefix using
// the test-only mappings below. Replaces the removed EnsureIndices*.
func ensureLegacyIndices(t *testing.T, ctx context.Context, c *Client, prefix string) {
	t.Helper()
	for _, idx := range []struct {
		logical string
		mapping map[string]any
	}{
		{IndexSummaries, summariesMapping()},
		{IndexEntities, entitiesMapping()},
		{IndexAttachments, attachmentsMapping()},
	} {
		if err := c.EnsureIndexWithMapping(ctx, prefix, idx.logical, idx.mapping); err != nil {
			t.Fatalf("ensure %s under %q: %v", idx.logical, prefix, err)
		}
	}
}

func commonSettings() map[string]any {
	return map[string]any{
		"index": map[string]any{
			"knn":                true,
			"number_of_shards":   1,
			"number_of_replicas": 0,
			"refresh_interval":   "1s",
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
				"profile":            map[string]any{"type": "keyword"},
				"vault":              map[string]any{"type": "keyword"},
				"sha":                map[string]any{"type": "keyword"},
				"kind":               map[string]any{"type": "keyword"},
				"memory_type":        map[string]any{"type": "keyword"},
				"source_path":        map[string]any{"type": "keyword"},
				"source_url":         map[string]any{"type": "keyword"},
				"source":             map[string]any{"type": "keyword"},
				"created_at":         map[string]any{"type": "date"},
				"updated_at":         map[string]any{"type": "date"},
				"captured_at":        map[string]any{"type": "date"},
				"title":              textWithRawKeyword(),
				"body":               map[string]any{"type": "text"},
				"raw_body":           map[string]any{"type": "text"},
				"tags":               map[string]any{"type": "keyword"},
				"topic":              map[string]any{"type": "keyword"},
				"reliability":        map[string]any{"type": "keyword"},
				"gate_reason":        map[string]any{"type": "text", "index": false},
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
