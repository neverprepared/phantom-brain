package osearch

import (
	"encoding/json"
	"testing"
)

func TestDocID(t *testing.T) {
	got := DocID("alpha", "memory", "abc123")
	want := "alpha:memory:abc123"
	if got != want {
		t.Fatalf("DocID = %q, want %q", got, want)
	}
}

func TestEntitySlug(t *testing.T) {
	cases := map[string]string{
		"Foo Bar":           "foo-bar",
		"  Whitespace  ":    "whitespace",
		"Slash/In/Name":     "slash-in-name",
		"Already-Slugged":   "already-slugged",
		"weird!!chars??":    "weird-chars",
		"UNICODE_ßéé_2025!": "unicode-2025",
		"_---_":             "",
	}
	for input, want := range cases {
		if got := EntitySlug(input); got != want {
			t.Errorf("EntitySlug(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSoRKind(t *testing.T) {
	// Only attachment_stub collapses to "attachment"; everything else
	// passes through. The returned strings MUST all satisfy the SoR
	// records_kind_chk constraint (migrations/0001).
	cases := map[Kind]string{
		KindAttachmentStub: "attachment",
		KindNote:           "note",
		KindWebScrape:      "web_scrape",
		KindTaskSummary:    "task_summary",
		KindEmailImport:    "email_import",
		KindManualCurate:   "manual_curate",
	}
	for in, want := range cases {
		if got := SoRKind(in); got != want {
			t.Errorf("SoRKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSHA256Hex(t *testing.T) {
	got := SHA256Hex([]byte("hello"))
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Fatalf("SHA256Hex(\"hello\") = %q, want %q", got, want)
	}
}

// TestMappingsAreValidJSON guards against typos in the mapping
// builders that would crash daemon startup when EnsureIndices runs
// json.Marshal on them. Each mapping must round-trip cleanly.
func TestMappingsAreValidJSON(t *testing.T) {
	for name, m := range map[string]map[string]any{
		"summaries":   summariesMapping(),
		"entities":    entitiesMapping(),
		"attachments": attachmentsMapping(),
	} {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var back map[string]any
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("%s: round-trip unmarshal: %v", name, err)
		}
		// Sanity: every index must declare both settings (for knn=true)
		// and mappings.properties (for the field types).
		if _, ok := back["settings"]; !ok {
			t.Errorf("%s mapping missing settings", name)
		}
		mappings, ok := back["mappings"].(map[string]any)
		if !ok {
			t.Fatalf("%s mapping missing mappings block", name)
		}
		props, ok := mappings["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s mapping missing properties", name)
		}
		for _, required := range []string{"profile", "vault"} {
			if _, ok := props[required]; !ok {
				t.Errorf("%s missing required field %q", name, required)
			}
		}
	}
}

// TestKnnVectorDimMatches catches accidental edits that drift the
// schema vector dim from the agreed-upon EmbeddingDim constant.
func TestKnnVectorDimMatches(t *testing.T) {
	v := knnVectorField()
	dim, ok := v["dimension"].(int)
	if !ok {
		t.Fatalf("dimension field is %T, want int", v["dimension"])
	}
	if dim != EmbeddingDim {
		t.Fatalf("knn_vector dimension = %d, EmbeddingDim = %d — these must match", dim, EmbeddingDim)
	}
}

func TestIndexNameWithPrefix(t *testing.T) {
	c := &Client{prefix: "test_"}
	if got := c.IndexName(IndexSummaries); got != "test_pb_summaries" {
		t.Errorf("prefixed IndexName = %q, want test_pb_summaries", got)
	}
	c2 := &Client{prefix: ""}
	if got := c2.IndexName(IndexSummaries); got != "pb_summaries" {
		t.Errorf("unprefixed IndexName = %q, want pb_summaries", got)
	}
}

// TestSummaryDocRoundTrip ensures the struct's JSON tags produce
// field names that match the index mapping. A drift here means
// writes silently dynamic-map under the wrong field name.
func TestSummaryDocRoundTrip(t *testing.T) {
	in := SummaryDoc{
		Profile:     "p",
		Vault:       "v",
		SHA:         "abc",
		Title:       "hello",
		Body:        "world",
		Tags:        []string{"a", "b"},
		Topic:       "agents",
		Reliability: ReliabilityMedium,
		Entities:    []string{"foo"},
		Synthesised: true,
		Embedding:   []float32{0.1, 0.2},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"profile", "vault", "sha", "title", "body", "tags", "topic", "reliability", "entities", "synthesised", "embedding"} {
		if _, ok := out[k]; !ok {
			t.Errorf("SummaryDoc JSON missing field %q", k)
		}
	}
}
