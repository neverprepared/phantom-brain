package osearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// Hit mirrors internal/index.Hit so callers can swap recall backends.
// DocID is the OS document ID (profile/vault/key); Index identifies
// which logical index produced the hit (pb_summaries, pb_entities,
// pb_attachments).
type Hit struct {
	DocID      string
	Index      string
	Source     json.RawMessage
	Score      float64
	VectorRank int
	TextRank   int
}

// rrfK matches internal/index/search.go — Cormack-Buettcher-Clarke (2009).
const rrfK = 60.0

// RecallOptions configures a hybrid recall query.
type RecallOptions struct {
	// Prefix is the per-binding index prefix resolved by the caller
	// (binding.Storage.IndexPrefix in Stream B/D terms). Empty falls
	// back to the client's default prefix.
	Prefix string

	// Profile and Vault are required — every recall is scoped to a
	// single vault binding to prevent cross-vault data bleed.
	Profile string
	Vault   string

	// Logical index name (e.g. IndexSummaries). Recall queries one
	// index at a time; callers wanting cross-index results run
	// multiple queries and fuse client-side.
	Index string

	// Query is the human-typed text. Empty = vector-only recall.
	Query string

	// Embedding is the 768-dim query vector. nil = text-only recall.
	Embedding []float32

	// TopK caps the returned hits. The implementation over-fetches
	// internally to give RRF material to fuse.
	TopK int

	// Topic, when set, adds a `term` filter on the doc's `topic` field.
	// Empty = no topic restriction.
	Topic string
}

// Recall runs a hybrid BM25 + kNN query against one OS index, scoped
// to (profile, vault), and returns the top-K fused hits. When only
// Query is set it falls back to BM25-only; when only Embedding is
// set it falls back to kNN-only.
func (c *Client) Recall(ctx context.Context, opts RecallOptions) ([]Hit, error) {
	if opts.Profile == "" || opts.Vault == "" {
		return nil, fmt.Errorf("osearch.Recall: profile and vault required")
	}
	if opts.Index == "" {
		opts.Index = IndexSummaries
	}
	if opts.TopK <= 0 {
		return nil, nil
	}
	overfetch := opts.TopK * 3
	if overfetch < 10 {
		overfetch = 10
	}

	hasText := opts.Query != ""
	hasVec := len(opts.Embedding) > 0
	if !hasText && !hasVec {
		return nil, fmt.Errorf("osearch.Recall: at least one of Query or Embedding required")
	}

	var (
		textHits, vecHits []Hit
		err               error
	)

	if hasText {
		textHits, err = c.searchBM25(ctx, opts, overfetch)
		if err != nil {
			return nil, fmt.Errorf("bm25: %w", err)
		}
	}
	if hasVec {
		vecHits, err = c.searchKNN(ctx, opts, overfetch)
		if err != nil {
			return nil, fmt.Errorf("knn: %w", err)
		}
	}

	// Single-mode short-circuit avoids RRF when only one ranking exists.
	if !hasVec {
		if len(textHits) > opts.TopK {
			textHits = textHits[:opts.TopK]
		}
		return textHits, nil
	}
	if !hasText {
		if len(vecHits) > opts.TopK {
			vecHits = vecHits[:opts.TopK]
		}
		return vecHits, nil
	}

	fused := map[string]*Hit{}
	for i := range textHits {
		h := textHits[i]
		c := h
		c.Score = 1.0 / (rrfK + float64(h.TextRank))
		fused[h.DocID] = &c
	}
	for i := range vecHits {
		h := vecHits[i]
		got, ok := fused[h.DocID]
		if !ok {
			c := h
			c.Score = 1.0 / (rrfK + float64(h.VectorRank))
			fused[h.DocID] = &c
			continue
		}
		got.VectorRank = h.VectorRank
		if got.Source == nil {
			got.Source = h.Source
		}
		got.Score += 1.0 / (rrfK + float64(h.VectorRank))
	}

	all := make([]Hit, 0, len(fused))
	for _, h := range fused {
		all = append(all, *h)
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].Score > all[b].Score })
	if len(all) > opts.TopK {
		all = all[:opts.TopK]
	}
	return all, nil
}

// filters is the vault-scoping bool/must block shared by both halves.
func vaultFilter(opts RecallOptions) []map[string]any {
	f := []map[string]any{
		{"term": map[string]any{"profile": opts.Profile}},
		{"term": map[string]any{"vault": opts.Vault}},
	}
	if opts.Topic != "" {
		f = append(f, map[string]any{"term": map[string]any{"topic": opts.Topic}})
	}
	return f
}

func (c *Client) searchBM25(ctx context.Context, opts RecallOptions, size int) ([]Hit, error) {
	// Field set differs per index — summaries/attachments have body
	// content; entities lean on name + accumulated body. The
	// multi_match here is the union; missing fields are silently
	// skipped by OS.
	body := map[string]any{
		"size": size,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": vaultFilter(opts),
				"must": []map[string]any{
					{
						"multi_match": map[string]any{
							"query": opts.Query,
							"fields": []string{
								"title^2", "name^2", "body", "raw_body",
								"extracted_text", "tags", "aliases",
							},
							"type": "best_fields",
						},
					},
				},
			},
		},
	}
	return c.runSearch(ctx, opts.Prefix, opts.Index, body, false)
}

func (c *Client) searchKNN(ctx context.Context, opts RecallOptions, size int) ([]Hit, error) {
	// OpenSearch's k-NN query with a pre-filter on profile/vault.
	// `num_candidates` defaults to a sensible value; explicit override
	// only when topK is large.
	body := map[string]any{
		"size": size,
		"query": map[string]any{
			"knn": map[string]any{
				"embedding": map[string]any{
					"vector": opts.Embedding,
					"k":      size,
					"filter": map[string]any{
						"bool": map[string]any{
							"must": vaultFilter(opts),
						},
					},
				},
			},
		},
	}
	return c.runSearch(ctx, opts.Prefix, opts.Index, body, true)
}

func (c *Client) runSearch(ctx context.Context, prefix, logical string, query map[string]any, vector bool) ([]Hit, error) {
	if prefix == "" {
		prefix = c.prefix
	}
	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("marshal query: %w", err)
	}
	resp, err := c.api.Search(ctx, &osapi.SearchReq{
		Indices: []string{IndexNameWithPrefix(prefix, logical)},
		Body:    bytes.NewReader(body),
	})
	if err != nil {
		if resp != nil && resp.Inspect().Response != nil {
			b, _ := io.ReadAll(resp.Inspect().Response.Body)
			return nil, fmt.Errorf("search %s: %w (body: %s)", logical, err, string(b))
		}
		return nil, fmt.Errorf("search %s: %w", logical, err)
	}

	hits := make([]Hit, 0, len(resp.Hits.Hits))
	for i, h := range resp.Hits.Hits {
		hit := Hit{
			DocID:  h.ID,
			Index:  h.Index,
			Source: h.Source,
		}
		if vector {
			hit.VectorRank = i + 1
		} else {
			hit.TextRank = i + 1
		}
		hits = append(hits, hit)
	}
	return hits, nil
}
