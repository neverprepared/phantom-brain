package osproject

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// HybridPipeline is the cluster-level OpenSearch search pipeline that
// fuses the two sub-queries of a `hybrid` query (BM25 first, kNN second)
// engine-side. min_max normalization + arithmetic_mean combination with
// equal weights. It is a CLUSTER resource (not per-index/prefix), so one
// EnsureSearchPipeline call per cluster suffices.
const HybridPipeline = "pb-hybrid"

// hybridPipelineBody is the PUT body for the normalization-processor
// search pipeline. weights[0] applies to the first sub-query (BM25),
// weights[1] to the second (kNN) — the order MUST match the order the
// sub-queries appear in the `hybrid.queries` array built by Recall.
var hybridPipelineBody = map[string]any{
	"description": "phantom-brain hybrid BM25+kNN",
	"phase_results_processors": []any{
		map[string]any{
			"normalization-processor": map[string]any{
				"normalization": map[string]any{"technique": "min_max"},
				"combination": map[string]any{
					"technique":  "arithmetic_mean",
					"parameters": map[string]any{"weights": []float64{0.5, 0.5}},
				},
			},
		},
	},
}

// EnsureSearchPipeline idempotently PUTs the pb-hybrid search pipeline.
// The search-pipeline endpoint is NOT typed in opensearch-go v4's
// opensearchapi surface, so we issue it through the low-level transport
// (c.API().Client.Perform). PUT overwrites in place, so calling this on
// every startup is safe. 2xx => success; non-2xx => error with the body.
func EnsureSearchPipeline(ctx context.Context, c *osearch.Client) error {
	body, err := json.Marshal(hybridPipelineBody)
	if err != nil {
		return fmt.Errorf("osproject: recall: marshal pipeline body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"/_search/pipeline/"+HybridPipeline, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("osproject: recall: build pipeline request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.API().Client.Perform(req)
	if err != nil {
		return fmt.Errorf("osproject: recall: put search pipeline: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("osproject: recall: put search pipeline: status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// Recaller queries the pb_records index with hybrid (BM25 + kNN) search,
// tenant-scoped and faceted. It is the read counterpart to Projector and
// holds no embedding logic — the caller pre-computes the query vector.
type Recaller struct {
	client *osearch.Client
	prefix string
}

// NewRecaller builds a Recaller over the pb_records index resolved from
// the supplied per-binding prefix.
func NewRecaller(client *osearch.Client, prefix string) *Recaller {
	return &Recaller{client: client, prefix: prefix}
}

// RecallQuery is one recall request. Profile/Vault are required tenant
// scope; at least one of Text/Vector must be present. Vector empty =>
// BM25-only degraded mode (no search pipeline). The optional filters are
// applied to BOTH sub-queries.
type RecallQuery struct {
	Profile, Vault string // REQUIRED — tenant scope

	Text   string    // lexical query (BM25)
	Vector []float32 // optional query embedding; empty => BM25-only

	Kinds       []string // optional filter (terms on `kind`)
	Topic       string   // optional filter (term on `topic`)
	MemoryType  string   // optional filter (term on `memory_type`)
	Reliability []string // optional filter (terms on `reliability`)

	Size int // page size; default 10 if <=0
	K    int // kNN k; default = max(Size, 10) if <=0
}

// RecallHit is one ranked, rendered result. Score is the fused _score
// (engine-normalized in hybrid mode, raw BM25 in degraded mode).
type RecallHit struct {
	ID          int64
	SHA         string
	Title       string
	Kind        string
	MemoryType  string
	Topic       string
	Reliability string
	SourceURL   string

	MimeType         string
	OriginalFilename string

	Snippet string  // highlight fragment, else body/extracted_text prefix
	Score   float64 // fused _score
}

// hitSource is the subset of recordDoc we read back from _source for
// rendering. `embedding` is excluded at query time, so it is omitted.
type hitSource struct {
	ID               int64  `json:"id"`
	SHA              string `json:"sha"`
	Title            string `json:"title"`
	Kind             string `json:"kind"`
	MemoryType       string `json:"memory_type"`
	Topic            string `json:"topic"`
	Reliability      string `json:"reliability"`
	SourceURL        string `json:"source_url"`
	MimeType         string `json:"mime_type"`
	OriginalFilename string `json:"original_filename"`
	Body             string `json:"body"`
	ExtractedText    string `json:"extracted_text"`
}

const snippetLen = 160

// Recall runs the hybrid (or degraded BM25-only) query and returns the
// fused-ranked hits in response order.
func (r *Recaller) Recall(ctx context.Context, q RecallQuery) ([]RecallHit, error) {
	if q.Profile == "" || q.Vault == "" {
		return nil, fmt.Errorf("osproject: recall: profile and vault required")
	}
	hasText := q.Text != ""
	hasVec := len(q.Vector) > 0
	if !hasText && !hasVec {
		return nil, fmt.Errorf("osproject: recall: at least one of Text or Vector required")
	}

	size := q.Size
	if size <= 0 {
		size = 10
	}
	k := q.K
	if k <= 0 {
		k = size
		if k < 10 {
			k = 10
		}
	}

	filters := r.buildFilters(q)

	body := map[string]any{
		"size": size,
		"_source": map[string]any{
			"excludes": []string{"embedding"},
		},
		"highlight": map[string]any{
			"fields": map[string]any{
				"title":          map[string]any{},
				"body":           map[string]any{},
				"extracted_text": map[string]any{},
			},
			"fragment_size":       160,
			"number_of_fragments": 1,
			// Query-level cap so a single oversized doc cannot fail the whole
			// recall query. Without this, OpenSearch enforces the index-level
			// index.highlight.max_analyzed_offset (default 1,000,000) by
			// throwing illegal_argument_exception → all shards failed → the
			// entire recall 400s. Set at query level, OS instead stops
			// analyzing past the offset (partial/no snippet for the giant doc)
			// and the query still succeeds. Must be <= the index-level setting.
			"max_analyzed_offset": 1000000,
		},
	}

	usePipeline := hasVec
	if usePipeline {
		body["query"] = map[string]any{
			"hybrid": map[string]any{
				"queries": r.hybridSubQueries(q, filters, k, hasText),
			},
		}
	} else {
		// Degraded BM25-only: plain bool, no hybrid, no pipeline.
		body["query"] = map[string]any{
			"bool": map[string]any{
				"must":   []any{multiMatch(q.Text)},
				"filter": filters,
			},
		}
	}

	hits, err := r.search(ctx, body, usePipeline)
	if err != nil {
		return nil, err
	}
	return hits, nil
}

// hybridSubQueries returns the ordered sub-query list: BM25 first, kNN
// second — matching the pipeline weight order. When Text is empty the
// BM25 clause is dropped (a single-clause hybrid of just the knn query).
func (r *Recaller) hybridSubQueries(q RecallQuery, filters []any, k int, hasText bool) []any {
	knn := map[string]any{
		"knn": map[string]any{
			"embedding": map[string]any{
				"vector": q.Vector,
				"k":      k,
				"filter": map[string]any{
					"bool": map[string]any{"filter": filters},
				},
			},
		},
	}
	if !hasText {
		return []any{knn}
	}
	bm25 := map[string]any{
		"bool": map[string]any{
			"must":   []any{multiMatch(q.Text)},
			"filter": filters,
		},
	}
	return []any{bm25, knn}
}

// multiMatch is the shared lexical clause: title boosted 2x over body and
// extracted_text.
func multiMatch(text string) map[string]any {
	return map[string]any{
		"multi_match": map[string]any{
			"query":  text,
			"fields": []string{"title^2", "body", "extracted_text"},
		},
	}
}

// buildFilters assembles the filter clauses applied to BOTH sub-queries:
// mandatory profile+vault terms, then any optional facet filters.
func (r *Recaller) buildFilters(q RecallQuery) []any {
	filters := []any{
		map[string]any{"term": map[string]any{"profile": q.Profile}},
		map[string]any{"term": map[string]any{"vault": q.Vault}},
	}
	if len(q.Kinds) > 0 {
		filters = append(filters, map[string]any{"terms": map[string]any{"kind": q.Kinds}})
	}
	if q.Topic != "" {
		filters = append(filters, map[string]any{"term": map[string]any{"topic": q.Topic}})
	}
	if q.MemoryType != "" {
		filters = append(filters, map[string]any{"term": map[string]any{"memory_type": q.MemoryType}})
	}
	if len(q.Reliability) > 0 {
		filters = append(filters, map[string]any{"terms": map[string]any{"reliability": q.Reliability}})
	}
	return filters
}

// search executes the query body against pb_records (under the binding
// prefix), optionally with the pb-hybrid search pipeline, and parses the
// typed response into RecallHits in fused order. The opensearchapi
// SearchReq carries the pipeline via Params.SearchPipeline (typed in v4),
// so no low-level transport is needed for the search itself.
func (r *Recaller) search(ctx context.Context, body map[string]any, usePipeline bool) ([]RecallHit, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("osproject: recall: marshal query: %w", err)
	}
	req := &osapi.SearchReq{
		Indices: []string{osearch.IndexNameWithPrefix(r.prefix, LogicalRecords)},
		Body:    bytes.NewReader(raw),
	}
	if usePipeline {
		req.Params.SearchPipeline = HybridPipeline
	}

	resp, err := r.client.API().Search(ctx, req)
	if err != nil {
		if resp != nil && resp.Inspect().Response != nil {
			b, _ := io.ReadAll(resp.Inspect().Response.Body)
			return nil, fmt.Errorf("osproject: recall: search: %w (body: %s)", err, strings.TrimSpace(string(b)))
		}
		return nil, fmt.Errorf("osproject: recall: search: %w", err)
	}

	hits := make([]RecallHit, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		var src hitSource
		if len(h.Source) > 0 {
			if err := json.Unmarshal(h.Source, &src); err != nil {
				return nil, fmt.Errorf("osproject: recall: decode _source: %w", err)
			}
		}
		hits = append(hits, RecallHit{
			ID:               src.ID,
			SHA:              src.SHA,
			Title:            src.Title,
			Kind:             src.Kind,
			MemoryType:       src.MemoryType,
			Topic:            src.Topic,
			Reliability:      src.Reliability,
			SourceURL:        src.SourceURL,
			MimeType:         src.MimeType,
			OriginalFilename: src.OriginalFilename,
			Snippet:          snippetFor(h.Highlight, src),
			Score:            float64(h.Score),
		})
	}
	return hits, nil
}

// snippetFor picks the best highlight fragment (title > body >
// extracted_text priority), falling back to a trimmed prefix of body or
// extracted_text when no highlight was produced.
func snippetFor(highlight map[string][]string, src hitSource) string {
	for _, field := range []string{"title", "body", "extracted_text"} {
		if frags, ok := highlight[field]; ok && len(frags) > 0 && frags[0] != "" {
			return frags[0]
		}
	}
	if s := trimPrefix(src.Body); s != "" {
		return s
	}
	return trimPrefix(src.ExtractedText)
}

// trimPrefix returns up to snippetLen runes of s, trimmed.
func trimPrefix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= snippetLen {
		return s
	}
	return strings.TrimSpace(string(r[:snippetLen]))
}
