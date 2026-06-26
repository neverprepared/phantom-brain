// Package osproject is the OpenSearch write projection for phantom-brain
// (design §13.1). Postgres is the System of Record; OpenSearch is a
// DERIVED search projection. The River projection worker (internal/
// projection) calls a Projector per written record; this package
// provides the real OpenSearch Projector plus the single index it
// writes into.
//
// One index — pb_records — holds every record kind (note, web_scrape,
// task_summary, attachment_stub); `kind` is a filter field, not an
// index boundary. Entities and facts are NOT projected — entity
// resolution stays in Postgres. The hybrid recall QUERY (BM25+kNN
// fusion, search pipeline) is a separate later layer; this is the
// write/index-design side only.
package osproject

import (
	"context"
	"fmt"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// LogicalRecords is the bare (unprefixed) name of the projection index.
// Per-binding storage prefixes are applied by osearch.IndexNameWithPrefix.
const LogicalRecords = "pb_records"

// RecordsMapping returns the full {settings, mappings} body for the
// pb_records index.
//
//   - title / body / extracted_text use the built-in english analyzer
//     (stemming) so "invoice" matches a doc titled "invoices".
//   - title additionally carries a .raw keyword sub-field (ignore_above
//     256) for exact-match, sort, and aggregation.
//   - embedding is a knn_vector (dim 768, hnsw / cosinesimil / lucene,
//     ef_construction 128 / m 16) — the same params as the legacy
//     knnVectorField() so vectors are interchangeable across indices.
//     It is OPTIONAL on a doc: a record projects on write, often before
//     synth has computed its embedding; the field is simply absent
//     until a later re-project fills it.
//   - index.knn is enabled at the settings level (required for the
//     knn_vector field type), with 1 shard / 0 replicas / 1s refresh —
//     matching the legacy commonSettings() single-node profile.
//
// raw_body, minio_key, and size_bytes are deliberately NOT projected:
// raw_body is pre-synth noise (Postgres-only), and minio_key/size_bytes
// are fetch-time metadata the renderer doesn't need from search.
func RecordsMapping() map[string]any {
	textEnglish := func() map[string]any {
		return map[string]any{"type": "text", "analyzer": "english"}
	}
	titleField := map[string]any{
		"type":     "text",
		"analyzer": "english",
		"fields": map[string]any{
			"raw": map[string]any{"type": "keyword", "ignore_above": 256},
		},
	}
	knnVector := map[string]any{
		"type":      "knn_vector",
		"dimension": osearch.EmbeddingDim,
		"method": map[string]any{
			"name":       "hnsw",
			"space_type": "cosinesimil",
			"engine":     "lucene",
			"parameters": map[string]any{"ef_construction": 128, "m": 16},
		},
	}

	return map[string]any{
		"settings": map[string]any{
			"index": map[string]any{
				"knn":                true,
				"number_of_shards":   1,
				"number_of_replicas": 0,
				"refresh_interval":   "1s",
			},
		},
		"mappings": map[string]any{
			"properties": map[string]any{
				// identity / scoping
				"id":      map[string]any{"type": "long"},
				"profile": map[string]any{"type": "keyword"},
				"vault":   map[string]any{"type": "keyword"},
				"sha":     map[string]any{"type": "keyword"},
				// classification (low-cardinality, faceted)
				"kind":        map[string]any{"type": "keyword"},
				"memory_type": map[string]any{"type": "keyword"},
				"topic":       map[string]any{"type": "keyword"},
				"reliability": map[string]any{"type": "keyword"},
				// provenance / render metadata
				"source":            map[string]any{"type": "keyword"},
				"source_url":        map[string]any{"type": "keyword"},
				"tags":              map[string]any{"type": "keyword"},
				"mime_type":         map[string]any{"type": "keyword"},
				"original_filename": map[string]any{"type": "keyword"},
				"embedding_model":   map[string]any{"type": "keyword"},
				// analyzed text
				"title":          titleField,
				"body":           textEnglish(),
				"extracted_text": textEnglish(),
				// dates
				"captured_at": map[string]any{"type": "date"},
				"created_at":  map[string]any{"type": "date"},
				"updated_at":  map[string]any{"type": "date"},
				// vector
				"embedding": knnVector,
			},
		},
	}
}

// EnsureIndex creates pb_records (under the supplied per-binding prefix)
// if it does not already exist. Idempotent — a present index is a no-op.
func EnsureIndex(ctx context.Context, c *osearch.Client, prefix string) error {
	if err := c.EnsureIndexWithMapping(ctx, prefix, LogicalRecords, RecordsMapping()); err != nil {
		return fmt.Errorf("osproject: ensure %s: %w", LogicalRecords, err)
	}
	return nil
}
