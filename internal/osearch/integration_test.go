package osearch

import (
	"context"
	"os"
	"testing"
	"time"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// testClient connects to a live OpenSearch cluster pointed to by
// OPENSEARCH_URL and isolates state under a per-test index prefix.
// Tests using this helper skip when the env var is absent — keeps
// `make test` green on dev hosts without OS running, while the
// integration job in CI / `docker compose up opensearch` exercises
// the live path.
func testClient(t *testing.T) (*Client, context.Context, func()) {
	t.Helper()
	addr := os.Getenv("OPENSEARCH_URL")
	if addr == "" {
		t.Skip("OPENSEARCH_URL not set; skipping live OpenSearch test")
	}

	cfg := DefaultConfig()
	cfg.Addresses = []string{addr}
	cfg.RequestTimeout = 5 * time.Second
	// Per-test prefix prevents cross-test pollution and lets parallel
	// `go test` runs coexist.
	cfg.IndexPrefix = "pbtest_" + sanitizeName(t.Name()) + "_"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	c, err := Open(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatalf("Open: %v", err)
	}
	ensureLegacyIndices(t, ctx, c, c.prefix)

	cleanup := func() {
		// Drop the per-test indices so re-runs start clean.
		names := []string{
			c.IndexName(IndexSummaries),
			c.IndexName(IndexEntities),
			c.IndexName(IndexAttachments),
		}
		_, _ = c.api.Indices.Delete(context.Background(), osapi.IndicesDeleteReq{Indices: names})
		cancel()
	}
	return c, ctx, cleanup
}

func sanitizeName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			out = append(out, ch)
		case ch >= 'A' && ch <= 'Z':
			out = append(out, ch+32)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

func TestLive_UpsertAndGet_Summary(t *testing.T) {
	c, ctx, cleanup := testClient(t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)
	doc := SummaryDoc{
		Profile:     "p1",
		Vault:       "v1",
		SHA:         "deadbeef",
		Title:       "Hello world",
		Body:        "A short body for testing.",
		Tags:        []string{"test", "phase6"},
		Topic:       "infrastructure",
		Reliability: ReliabilityMedium,
		CreatedAt:   now,
		UpdatedAt:   now,
		Synthesised: true,
		Embedding:   nil,
	}
	if err := c.UpsertSummary(ctx, doc, true); err != nil {
		t.Fatalf("UpsertSummary: %v", err)
	}
	got, err := c.GetSummary(ctx, "p1", "v1", "deadbeef")
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if got == nil {
		t.Fatal("GetSummary returned nil; expected doc")
	}
	if got.Title != doc.Title || got.Body != doc.Body || got.Topic != doc.Topic {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, doc)
	}
}

func TestLive_UpsertIsIdempotent(t *testing.T) {
	c, ctx, cleanup := testClient(t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)
	mk := func(body string) SummaryDoc {
		return SummaryDoc{
			Profile: "p1", Vault: "v1", SHA: "abc",
			Title: "T", Body: body, CreatedAt: now, UpdatedAt: now,
		}
	}
	if err := c.UpsertSummary(ctx, mk("first"), true); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := c.UpsertSummary(ctx, mk("second"), true); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, err := c.GetSummary(ctx, "p1", "v1", "abc")
	if err != nil || got == nil {
		t.Fatalf("get: %v / nil=%v", err, got == nil)
	}
	if got.Body != "second" {
		t.Errorf("upsert did not replace: body=%q want %q", got.Body, "second")
	}
}

func TestLive_RecallBM25(t *testing.T) {
	c, ctx, cleanup := testClient(t)
	defer cleanup()

	seed := []SummaryDoc{
		{Profile: "p", Vault: "v", SHA: "a", Title: "Kubernetes pods", Body: "Pods are the smallest deployable unit.", Topic: "infrastructure"},
		{Profile: "p", Vault: "v", SHA: "b", Title: "Recipes for bread", Body: "Knead the dough.", Topic: "general"},
		{Profile: "p", Vault: "v", SHA: "c", Title: "Helm charts", Body: "Helm templates Kubernetes manifests.", Topic: "infrastructure"},
		// Different vault — must not appear in results.
		{Profile: "p", Vault: "other", SHA: "d", Title: "Kubernetes elsewhere", Body: "Should not match across vaults.", Topic: "infrastructure"},
	}
	for _, d := range seed {
		if err := c.UpsertSummary(ctx, d, false); err != nil {
			t.Fatalf("seed upsert %s: %v", d.SHA, err)
		}
	}
	if err := c.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	hits, err := c.Recall(ctx, RecallOptions{
		Profile: "p", Vault: "v",
		Index: IndexSummaries,
		Query: "kubernetes",
		TopK:  5,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected hits; got 0")
	}
	for _, h := range hits {
		if got := h.DocID; got == DocID("p", "other", "d") {
			t.Errorf("cross-vault leak: hit %s in p/v query", got)
		}
	}
	// Topic filter restricts to infrastructure (excludes bread).
	hitsT, err := c.Recall(ctx, RecallOptions{
		Profile: "p", Vault: "v", Index: IndexSummaries,
		Query: "kubernetes", Topic: "infrastructure", TopK: 5,
	})
	if err != nil {
		t.Fatalf("Recall topic: %v", err)
	}
	if len(hitsT) == 0 {
		t.Fatal("topic-filtered Recall returned 0")
	}
}

func TestLive_RecallVectorOnly(t *testing.T) {
	c, ctx, cleanup := testClient(t)
	defer cleanup()

	// Hand-built vectors so we have a predictable nearest neighbour.
	v1 := make([]float32, EmbeddingDim) // matches the target
	v1[0] = 1.0
	v2 := make([]float32, EmbeddingDim)
	v2[1] = 1.0
	v3 := make([]float32, EmbeddingDim)
	v3[0] = 0.99 // very close to v1

	for sha, vec := range map[string][]float32{"a": v1, "b": v2, "c": v3} {
		d := SummaryDoc{Profile: "p", Vault: "v", SHA: sha, Title: sha, Body: "x", Embedding: vec}
		if err := c.UpsertSummary(ctx, d, false); err != nil {
			t.Fatalf("seed %s: %v", sha, err)
		}
	}
	if err := c.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	q := make([]float32, EmbeddingDim)
	q[0] = 1.0 // most similar to v1 and v3
	hits, err := c.Recall(ctx, RecallOptions{
		Profile: "p", Vault: "v", Index: IndexSummaries,
		Embedding: q, TopK: 3,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected vector hits; got 0")
	}
	// The top hit should be "a" or "c" (both ~aligned with q),
	// definitely not "b" (orthogonal).
	top := hits[0].DocID
	if top != DocID("p", "v", "a") && top != DocID("p", "v", "c") {
		t.Errorf("unexpected top hit %q; want p/v/a or p/v/c", top)
	}
}
