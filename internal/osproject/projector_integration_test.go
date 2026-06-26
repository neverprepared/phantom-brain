//go:build integration

// Integration coverage for the OpenSearch write projection (pb_records
// index + Projector) against a real OpenSearch via testcontainers.
// Build-tagged OFF by default so `make test` neither compiles this file
// nor needs Docker. Run with:
//
//	GOFLAGS="-tags=sqlite_fts5,integration" go test ./internal/osproject/ -run Integration -count=1 -v
package osproject

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	tcopensearch "github.com/testcontainers/testcontainers-go/modules/opensearch"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

const testImage = "opensearchproject/opensearch:2.18.0"

// startOS spins up a single-node OpenSearch (security disabled, http)
// and returns a Client + the test prefix. The module serves http with
// security disabled for single-node, so no TLS / auth is needed.
func startOS(t *testing.T) (*osearch.Client, string) {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcopensearch.Run(ctx, testImage)
	if err != nil {
		t.Fatalf("start opensearch container: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	addr, err := ctr.Address(ctx)
	if err != nil {
		t.Fatalf("container address: %v", err)
	}

	cfg := osearch.DefaultConfig()
	cfg.Addresses = []string{addr}
	cfg.RequestTimeout = 15 * time.Second
	// The module sets default admin/admin even with security disabled;
	// supplying them is harmless on an http endpoint.
	cfg.Username = ctr.User
	cfg.Password = ctr.Password

	openCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c, err := osearch.Open(openCtx, cfg)
	if err != nil {
		t.Fatalf("osearch.Open: %v", err)
	}
	return c, "pbproj_test_"
}

// sampleEmbedding returns a deterministic 768-dim unit-ish vector. base
// shifts the values so two calls with different base produce distinct
// (but still close) vectors for the kNN proof.
func sampleEmbedding(base float32) []float32 {
	v := make([]float32, osearch.EmbeddingDim)
	for i := range v {
		v[i] = base + float32(i%7)*0.01
	}
	return v
}

func baseRecord(sha, title, body string) pgdb.Record {
	now := time.Now().UTC().Truncate(time.Second)
	return pgdb.Record{
		ID:         1,
		Profile:    "tctest",
		Vault:      "main",
		Sha:        sha,
		Kind:       "note",
		Title:      title,
		Body:       pgtype.Text{String: body, Valid: true},
		RawBody:    pgtype.Text{String: "RAW: should not be projected", Valid: true},
		Source:     []string{"task:42"},
		Tags:       []string{"finance"},
		CreatedAt:  pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt:  pgtype.Timestamptz{Time: now, Valid: true},
		CapturedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
}

// rawSearch runs a search body against the index and returns the total
// hit count + the _id of the first hit (if any). Uses the typed
// SearchResp, which parses hits.total.value and hits[]._id directly.
func rawSearch(t *testing.T, c *osearch.Client, prefix string, body map[string]any) (total int, firstID string) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal search: %v", err)
	}
	resp, err := c.API().Search(context.Background(), &osapi.SearchReq{
		Indices: []string{osearch.IndexNameWithPrefix(prefix, LogicalRecords)},
		Body:    bytes.NewReader(b),
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	total = resp.Hits.Total.Value
	if len(resp.Hits.Hits) > 0 {
		firstID = resp.Hits.Hits[0].ID
	}
	return total, firstID
}

// getDocSource fetches one doc's _source by _id, or returns found=false.
func getDocSource(t *testing.T, c *osearch.Client, prefix, id string) (src map[string]any, found bool) {
	t.Helper()
	resp, err := c.API().Document.Get(context.Background(), osapi.DocumentGetReq{
		Index:      osearch.IndexNameWithPrefix(prefix, LogicalRecords),
		DocumentID: id,
	})
	if err != nil {
		raw := 0
		if resp != nil && resp.Inspect().Response != nil {
			raw = resp.Inspect().Response.StatusCode
		}
		if raw == 404 {
			return nil, false
		}
		// statusFromErr-style 404 in the error string.
		if bytes.Contains([]byte(err.Error()), []byte("404")) {
			return nil, false
		}
		t.Fatalf("get %s: %v", id, err)
	}
	if resp == nil || !resp.Found {
		return nil, false
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Source, &out); err != nil {
		t.Fatalf("decode source: %v", err)
	}
	return out, true
}

func TestOSProjectionIntegration(t *testing.T) {
	c, prefix := startOS(t)
	ctx := context.Background()
	proj := NewWithRefresh(c, prefix)

	t.Run("EnsureIndexIdempotent", func(t *testing.T) {
		if err := EnsureIndex(ctx, c, prefix); err != nil {
			t.Fatalf("EnsureIndex (1st): %v", err)
		}
		if err := EnsureIndex(ctx, c, prefix); err != nil {
			t.Fatalf("EnsureIndex (2nd, idempotent): %v", err)
		}
		// Confirm index.knn=true in the settings.
		resp, err := c.API().Indices.Settings.Get(ctx, &osapi.SettingsGetReq{
			Indices: []string{osearch.IndexNameWithPrefix(prefix, LogicalRecords)},
		})
		if err != nil {
			t.Fatalf("get settings: %v", err)
		}
		raw := resp.Inspect().Response
		defer raw.Body.Close()
		data, _ := io.ReadAll(raw.Body)
		if !bytes.Contains(data, []byte(`"knn":"true"`)) && !bytes.Contains(data, []byte(`"knn": "true"`)) {
			t.Fatalf("expected index.knn=true in settings, got: %s", string(data))
		}
	})

	t.Run("ProjectWithEmbedding", func(t *testing.T) {
		emb := pgvector.NewVector(sampleEmbedding(0.10))
		rec := baseRecord("sha-emb", "Quarterly invoices", "We paid every quarterly invoice on time.")
		rec.ID = 7
		rec.Embedding = &emb
		if err := proj.Project(ctx, rec); err != nil {
			t.Fatalf("Project: %v", err)
		}

		id := osearch.DocID(rec.Profile, rec.Vault, rec.Sha)
		src, found := getDocSource(t, c, prefix, id)
		if !found {
			t.Fatalf("doc %s not found after project", id)
		}
		if got := int(src["id"].(float64)); got != 7 {
			t.Fatalf("id round-trip: got %d want 7", got)
		}
		if src["kind"] != "note" {
			t.Fatalf("kind round-trip: got %v want note", src["kind"])
		}
		if src["sha"] != "sha-emb" {
			t.Fatalf("sha round-trip: got %v want sha-emb", src["sha"])
		}
		if _, ok := src["embedding"]; !ok {
			t.Fatal("expected embedding field present on the doc")
		}
		// raw_body must NOT be projected.
		if _, ok := src["raw_body"]; ok {
			t.Fatal("raw_body must not be projected")
		}
	})

	t.Run("EnglishAnalyzerStemming", func(t *testing.T) {
		// "invoice" (singular) must match the doc titled "invoices"
		// (plural) via the english analyzer's stemming.
		totalBody, _ := rawSearch(t, c, prefix, map[string]any{
			"query": map[string]any{
				"match": map[string]any{"body": "invoice"},
			},
		})
		if totalBody < 1 {
			t.Fatalf("expected body match on 'invoice' (stemmed), got %d hits", totalBody)
		}
		totalTitle, _ := rawSearch(t, c, prefix, map[string]any{
			"query": map[string]any{
				"match": map[string]any{"title": "invoice"},
			},
		})
		if totalTitle < 1 {
			t.Fatalf("expected title match on 'invoice' (stemmed 'invoices'), got %d hits", totalTitle)
		}
	})

	t.Run("KNNFindsNearVector", func(t *testing.T) {
		// A vector near the projected one (base 0.10) should retrieve it.
		near := sampleEmbedding(0.1001)
		total, firstID := rawSearch(t, c, prefix, map[string]any{
			"size": 1,
			"query": map[string]any{
				"knn": map[string]any{
					"embedding": map[string]any{
						"vector": near,
						"k":      3,
					},
				},
			},
		})
		if total < 1 {
			t.Fatalf("expected kNN to return the projected doc, got %d hits", total)
		}
		want := osearch.DocID("tctest", "main", "sha-emb")
		if firstID != want {
			t.Fatalf("kNN top hit: got %q want %q", firstID, want)
		}
	})

	t.Run("ProjectWithoutEmbedding", func(t *testing.T) {
		rec := baseRecord("sha-noemb", "Receipts", "A pile of receipts for the audit.")
		rec.ID = 11
		// Embedding nil.
		if err := proj.Project(ctx, rec); err != nil {
			t.Fatalf("Project (no embedding): %v", err)
		}
		id := osearch.DocID(rec.Profile, rec.Vault, rec.Sha)
		src, found := getDocSource(t, c, prefix, id)
		if !found {
			t.Fatalf("doc %s not found", id)
		}
		if _, ok := src["embedding"]; ok {
			t.Fatal("expected no embedding field on a record projected without a vector")
		}
		// Still text-searchable.
		total, _ := rawSearch(t, c, prefix, map[string]any{
			"query": map[string]any{"match": map[string]any{"body": "receipt"}},
		})
		if total < 1 {
			t.Fatalf("expected text match for the embedding-less doc, got %d", total)
		}
	})

	t.Run("IdempotentReProject", func(t *testing.T) {
		rec := baseRecord("sha-reproj", "First title", "first body about ledgers.")
		rec.ID = 20
		if err := proj.Project(ctx, rec); err != nil {
			t.Fatalf("Project (1st): %v", err)
		}
		rec.Title = "Second title wins"
		if err := proj.Project(ctx, rec); err != nil {
			t.Fatalf("Project (2nd): %v", err)
		}
		id := osearch.DocID(rec.Profile, rec.Vault, rec.Sha)
		src, found := getDocSource(t, c, prefix, id)
		if !found {
			t.Fatalf("doc %s not found", id)
		}
		if src["title"] != "Second title wins" {
			t.Fatalf("latest title should win, got %v", src["title"])
		}
		// Exactly one doc at that _id — a term query on sha returns 1.
		total, _ := rawSearch(t, c, prefix, map[string]any{
			"query": map[string]any{"term": map[string]any{"sha": "sha-reproj"}},
		})
		if total != 1 {
			t.Fatalf("expected exactly 1 doc for sha-reproj, got %d", total)
		}
	})

	t.Run("DeleteProjectionIdempotent", func(t *testing.T) {
		rec := baseRecord("sha-del", "Doomed", "this doc will be deleted.")
		rec.ID = 30
		if err := proj.Project(ctx, rec); err != nil {
			t.Fatalf("Project: %v", err)
		}
		id := osearch.DocID(rec.Profile, rec.Vault, rec.Sha)
		if _, found := getDocSource(t, c, prefix, id); !found {
			t.Fatalf("doc %s should exist before delete", id)
		}
		if err := proj.DeleteProjection(ctx, rec.Profile, rec.Vault, rec.Sha); err != nil {
			t.Fatalf("DeleteProjection: %v", err)
		}
		if _, found := getDocSource(t, c, prefix, id); found {
			t.Fatalf("doc %s should be gone after delete", id)
		}
		// Second delete is a no-op (404-tolerant).
		if err := proj.DeleteProjection(ctx, rec.Profile, rec.Vault, rec.Sha); err != nil {
			t.Fatalf("DeleteProjection (2nd, idempotent): %v", err)
		}
	})
}
