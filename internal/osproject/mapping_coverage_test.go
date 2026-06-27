package osproject

import (
	"testing"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// props digs settings/mappings out of RecordsMapping for assertions.
func mappingProps(t *testing.T) map[string]any {
	t.Helper()
	body := RecordsMapping()
	mappings, ok := body["mappings"].(map[string]any)
	if !ok {
		t.Fatalf("mappings missing/typed wrong: %v", body["mappings"])
	}
	props, ok := mappings["properties"].(map[string]any)
	if !ok {
		t.Fatalf("mappings.properties missing: %v", mappings)
	}
	return props
}

func TestRecordsMapping_KNNSettings(t *testing.T) {
	body := RecordsMapping()
	settings := body["settings"].(map[string]any)
	idx := settings["index"].(map[string]any)

	// index.knn MUST be true — the knn_vector field type requires it.
	if knn, _ := idx["knn"].(bool); !knn {
		t.Fatalf("settings.index.knn must be true, got %v", idx["knn"])
	}
	// Single-node profile (matches legacy commonSettings).
	if idx["number_of_shards"] != 1 {
		t.Errorf("number_of_shards = %v, want 1", idx["number_of_shards"])
	}
	if idx["number_of_replicas"] != 0 {
		t.Errorf("number_of_replicas = %v, want 0", idx["number_of_replicas"])
	}
	if idx["refresh_interval"] != "1s" {
		t.Errorf("refresh_interval = %v, want 1s", idx["refresh_interval"])
	}
}

func TestRecordsMapping_EmbeddingKNNVector(t *testing.T) {
	props := mappingProps(t)
	emb, ok := props["embedding"].(map[string]any)
	if !ok {
		t.Fatalf("embedding field missing: %v", props["embedding"])
	}
	if emb["type"] != "knn_vector" {
		t.Fatalf("embedding type = %v, want knn_vector", emb["type"])
	}
	// Dimension MUST equal the shared EmbeddingDim so vectors are
	// interchangeable across indices.
	if emb["dimension"] != osearch.EmbeddingDim {
		t.Fatalf("embedding dimension = %v, want %d (osearch.EmbeddingDim)", emb["dimension"], osearch.EmbeddingDim)
	}
	method := emb["method"].(map[string]any)
	if method["name"] != "hnsw" {
		t.Errorf("method.name = %v, want hnsw", method["name"])
	}
	if method["space_type"] != "cosinesimil" {
		t.Errorf("method.space_type = %v, want cosinesimil", method["space_type"])
	}
	if method["engine"] != "lucene" {
		t.Errorf("method.engine = %v, want lucene", method["engine"])
	}
	params := method["parameters"].(map[string]any)
	if params["ef_construction"] != 128 {
		t.Errorf("ef_construction = %v, want 128", params["ef_construction"])
	}
	if params["m"] != 16 {
		t.Errorf("m = %v, want 16", params["m"])
	}
}

func TestRecordsMapping_TitleHasRawKeywordSubfield(t *testing.T) {
	props := mappingProps(t)
	title := props["title"].(map[string]any)
	if title["type"] != "text" || title["analyzer"] != "english" {
		t.Fatalf("title should be english-analyzed text, got %v", title)
	}
	fields, ok := title["fields"].(map[string]any)
	if !ok {
		t.Fatalf("title.fields missing (need .raw subfield): %v", title)
	}
	raw, ok := fields["raw"].(map[string]any)
	if !ok {
		t.Fatalf("title.fields.raw missing: %v", fields)
	}
	if raw["type"] != "keyword" {
		t.Errorf("title.raw type = %v, want keyword", raw["type"])
	}
	if raw["ignore_above"] != 256 {
		t.Errorf("title.raw ignore_above = %v, want 256", raw["ignore_above"])
	}
}

func TestRecordsMapping_AnalyzedTextFields(t *testing.T) {
	props := mappingProps(t)
	// body + extracted_text use the english analyzer (stemming) for
	// "invoice" -> "invoices" recall.
	for _, f := range []string{"body", "extracted_text"} {
		fm, ok := props[f].(map[string]any)
		if !ok {
			t.Fatalf("%s field missing: %v", f, props[f])
		}
		if fm["type"] != "text" || fm["analyzer"] != "english" {
			t.Errorf("%s should be english-analyzed text, got %v", f, fm)
		}
	}
}

func TestRecordsMapping_FacetFieldsAreKeyword(t *testing.T) {
	props := mappingProps(t)
	// Low-cardinality faceted fields must be `keyword` (exact, aggregatable).
	for _, f := range []string{
		"profile", "vault", "sha", "kind", "memory_type", "topic",
		"reliability", "source", "source_url", "tags", "mime_type",
		"original_filename", "embedding_model",
	} {
		fm, ok := props[f].(map[string]any)
		if !ok {
			t.Fatalf("facet field %q missing from mapping", f)
		}
		if fm["type"] != "keyword" {
			t.Errorf("facet %q type = %v, want keyword", f, fm["type"])
		}
	}
	// id is a long; dates are date-typed.
	if id := props["id"].(map[string]any); id["type"] != "long" {
		t.Errorf("id type = %v, want long", id["type"])
	}
	for _, d := range []string{"captured_at", "created_at", "updated_at"} {
		if dm := props[d].(map[string]any); dm["type"] != "date" {
			t.Errorf("%s type = %v, want date", d, dm["type"])
		}
	}
}

func TestRecordsMapping_OmitsNonProjectedFields(t *testing.T) {
	props := mappingProps(t)
	// raw_body, minio_key, size_bytes are NOT projected — they must be
	// absent from the index mapping too.
	for _, f := range []string{"raw_body", "minio_key", "size_bytes", "gate_reason", "synthesised"} {
		if _, ok := props[f]; ok {
			t.Errorf("field %q must NOT be in the projection mapping", f)
		}
	}
}
