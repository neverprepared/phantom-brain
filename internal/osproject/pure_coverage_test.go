package osproject

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// --- buildFilters -----------------------------------------------------

// asMap coerces an `any` filter clause to map[string]any or fails.
func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T (%v)", v, v)
	}
	return m
}

// termValue extracts the single field/value of a {"term":{field:val}}
// or {"terms":{field:[...]}} clause.
func clauseKind(t *testing.T, clause any) (op, field string, val any) {
	t.Helper()
	m := asMap(t, clause)
	for _, k := range []string{"term", "terms"} {
		if inner, ok := m[k]; ok {
			im := asMap(t, inner)
			for f, v := range im {
				return k, f, v
			}
		}
	}
	t.Fatalf("clause is neither term nor terms: %v", m)
	return "", "", nil
}

func TestBuildFilters_MandatoryTenantScopeOnly(t *testing.T) {
	r := &Recaller{}
	filters := r.buildFilters(RecallQuery{Profile: "acme", Vault: "main"})

	if len(filters) != 2 {
		t.Fatalf("expected exactly 2 mandatory filters (profile, vault), got %d: %v", len(filters), filters)
	}
	// Order matters for pipeline weight alignment & readability: profile first.
	op, field, val := clauseKind(t, filters[0])
	if op != "term" || field != "profile" || val != "acme" {
		t.Fatalf("filter[0] = %s %s=%v, want term profile=acme", op, field, val)
	}
	op, field, val = clauseKind(t, filters[1])
	if op != "term" || field != "vault" || val != "main" {
		t.Fatalf("filter[1] = %s %s=%v, want term vault=main", op, field, val)
	}
}

func TestBuildFilters_AllOptionalFacets(t *testing.T) {
	r := &Recaller{}
	q := RecallQuery{
		Profile:     "acme",
		Vault:       "main",
		Kinds:       []string{"note", "web_scrape"},
		Topic:       "memory",
		MemoryType:  "semantic",
		Reliability: []string{"high", "medium"},
	}
	filters := r.buildFilters(q)

	// 2 mandatory + 4 optional facets.
	if len(filters) != 6 {
		t.Fatalf("expected 6 filters, got %d: %v", len(filters), filters)
	}

	// Index facets by field for assertion independent of append order.
	byField := map[string]struct {
		op  string
		val any
	}{}
	for _, f := range filters {
		op, field, val := clauseKind(t, f)
		byField[field] = struct {
			op  string
			val any
		}{op, val}
	}

	if got := byField["kind"]; got.op != "terms" {
		t.Errorf("kind should use `terms` (multi), got %q", got.op)
	} else if kinds, ok := got.val.([]string); !ok || len(kinds) != 2 || kinds[0] != "note" {
		t.Errorf("kind terms value = %v, want [note web_scrape]", got.val)
	}
	if got := byField["topic"]; got.op != "term" || got.val != "memory" {
		t.Errorf("topic = %s %v, want term memory", got.op, got.val)
	}
	if got := byField["memory_type"]; got.op != "term" || got.val != "semantic" {
		t.Errorf("memory_type = %s %v, want term semantic", got.op, got.val)
	}
	if got := byField["reliability"]; got.op != "terms" {
		t.Errorf("reliability should use `terms` (multi), got %q", got.op)
	}
}

func TestBuildFilters_EmptyFacetsAreOmitted(t *testing.T) {
	r := &Recaller{}
	// Empty slices and empty strings must NOT produce filter clauses.
	q := RecallQuery{
		Profile:     "acme",
		Vault:       "main",
		Kinds:       []string{}, // empty slice -> omitted
		Topic:       "",         // empty string -> omitted
		MemoryType:  "",
		Reliability: nil,
	}
	filters := r.buildFilters(q)
	if len(filters) != 2 {
		t.Fatalf("empty facets must add no clauses; got %d: %v", len(filters), filters)
	}
}

func TestBuildFilters_PartialFacets(t *testing.T) {
	r := &Recaller{}
	q := RecallQuery{Profile: "p", Vault: "v", Topic: "tools"}
	filters := r.buildFilters(q)
	if len(filters) != 3 {
		t.Fatalf("expected 2 mandatory + 1 topic = 3, got %d", len(filters))
	}
	op, field, val := clauseKind(t, filters[2])
	if op != "term" || field != "topic" || val != "tools" {
		t.Fatalf("appended facet = %s %s=%v, want term topic=tools", op, field, val)
	}
}

// --- hybridSubQueries -------------------------------------------------

func TestHybridSubQueries_WithText_BM25ThenKNN(t *testing.T) {
	r := &Recaller{}
	q := RecallQuery{Profile: "p", Vault: "v", Text: "ledgers", Vector: []float32{0.1, 0.2}}
	filters := r.buildFilters(q)
	subs := r.hybridSubQueries(q, filters, 7, true)

	if len(subs) != 2 {
		t.Fatalf("hasText hybrid must have 2 sub-queries (BM25, kNN), got %d", len(subs))
	}

	// [0] must be the BM25 bool clause (order aligns with pipeline weights[0]).
	bm := asMap(t, subs[0])
	boolClause, ok := bm["bool"].(map[string]any)
	if !ok {
		t.Fatalf("sub-query[0] should be a bool (BM25) clause, got %v", bm)
	}
	must, ok := boolClause["must"].([]any)
	if !ok || len(must) != 1 {
		t.Fatalf("BM25 bool.must should hold the multi_match, got %v", boolClause["must"])
	}
	mm := asMap(t, must[0])
	if _, ok := mm["multi_match"]; !ok {
		t.Fatalf("BM25 must clause should be a multi_match, got %v", mm)
	}
	// The same filters slice must be threaded into the BM25 clause.
	if bf, ok := boolClause["filter"].([]any); !ok || len(bf) != len(filters) {
		t.Fatalf("BM25 filter should equal buildFilters output (len %d), got %v", len(filters), boolClause["filter"])
	}

	// [1] must be the kNN clause carrying vector, k and the filter.
	knn := asMap(t, subs[1])
	inner := asMap(t, knn["knn"])
	emb := asMap(t, inner["embedding"])
	if k, _ := emb["k"].(int); k != 7 {
		t.Fatalf("kNN k = %v, want 7", emb["k"])
	}
	vec, ok := emb["vector"].([]float32)
	if !ok || len(vec) != 2 {
		t.Fatalf("kNN vector should be the query vector, got %v", emb["vector"])
	}
	// kNN filter is wrapped in a bool.filter holding the same filters.
	knnFilter := asMap(t, emb["filter"])
	knnBool := asMap(t, knnFilter["bool"])
	if bf, ok := knnBool["filter"].([]any); !ok || len(bf) != len(filters) {
		t.Fatalf("kNN bool.filter should equal buildFilters output, got %v", knnBool["filter"])
	}
}

func TestHybridSubQueries_NoText_KNNOnly(t *testing.T) {
	r := &Recaller{}
	q := RecallQuery{Profile: "p", Vault: "v", Vector: []float32{1, 2, 3}}
	filters := r.buildFilters(q)
	subs := r.hybridSubQueries(q, filters, 10, false)

	if len(subs) != 1 {
		t.Fatalf("no-text hybrid must drop the BM25 clause -> 1 sub-query, got %d", len(subs))
	}
	knn := asMap(t, subs[0])
	if _, ok := knn["knn"]; !ok {
		t.Fatalf("the single sub-query must be a kNN clause, got %v", knn)
	}
}

// --- multiMatch -------------------------------------------------------

func TestMultiMatch_TitleBoostedFields(t *testing.T) {
	mm := multiMatch("quarterly invoice")
	inner, ok := mm["multi_match"].(map[string]any)
	if !ok {
		t.Fatalf("multiMatch should wrap a multi_match, got %v", mm)
	}
	if inner["query"] != "quarterly invoice" {
		t.Fatalf("query text not threaded through, got %v", inner["query"])
	}
	fields, ok := inner["fields"].([]string)
	if !ok {
		t.Fatalf("fields should be []string, got %T", inner["fields"])
	}
	// title must be boosted 2x; body + extracted_text at default weight.
	want := []string{"title^2", "body", "extracted_text"}
	if len(fields) != len(want) {
		t.Fatalf("fields = %v, want %v", fields, want)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Fatalf("fields[%d] = %q, want %q", i, fields[i], want[i])
		}
	}
}

// --- snippetFor / trimPrefix -----------------------------------------

func TestSnippetFor_HighlightPriority(t *testing.T) {
	hl := map[string][]string{
		"title":          {"<em>title</em> frag"},
		"body":           {"body frag"},
		"extracted_text": {"extracted frag"},
	}
	if got := snippetFor(hl, hitSource{}); got != "<em>title</em> frag" {
		t.Fatalf("title highlight should win, got %q", got)
	}
	// title absent -> body wins over extracted_text.
	delete(hl, "title")
	if got := snippetFor(hl, hitSource{}); got != "body frag" {
		t.Fatalf("body highlight should win over extracted_text, got %q", got)
	}
	// only extracted_text present.
	delete(hl, "body")
	if got := snippetFor(hl, hitSource{}); got != "extracted frag" {
		t.Fatalf("extracted_text highlight should be used, got %q", got)
	}
}

func TestSnippetFor_EmptyFragmentSkipped(t *testing.T) {
	// A present-but-empty fragment list / empty string must not win;
	// fall through to the body prefix fallback.
	hl := map[string][]string{
		"title": {""},
		"body":  {},
	}
	src := hitSource{Body: "  fallback body text  "}
	if got := snippetFor(hl, src); got != "fallback body text" {
		t.Fatalf("empty highlights should fall back to trimmed body, got %q", got)
	}
}

func TestSnippetFor_FallbackBodyThenExtracted(t *testing.T) {
	// No highlights at all: body preferred, then extracted_text.
	if got := snippetFor(nil, hitSource{Body: "the body", ExtractedText: "the extract"}); got != "the body" {
		t.Fatalf("body should be the first fallback, got %q", got)
	}
	if got := snippetFor(nil, hitSource{ExtractedText: "the extract"}); got != "the extract" {
		t.Fatalf("extracted_text should be the second fallback, got %q", got)
	}
	if got := snippetFor(nil, hitSource{}); got != "" {
		t.Fatalf("no content -> empty snippet, got %q", got)
	}
}

func TestTrimPrefix_Boundaries(t *testing.T) {
	if got := trimPrefix("   "); got != "" {
		t.Fatalf("whitespace-only -> empty, got %q", got)
	}
	short := "short string"
	if got := trimPrefix("  " + short + "  "); got != short {
		t.Fatalf("short string should be trimmed and returned whole, got %q", got)
	}
	// Exactly snippetLen runes -> returned whole.
	exact := strings.Repeat("a", snippetLen)
	if got := trimPrefix(exact); got != exact {
		t.Fatalf("exactly snippetLen runes should be returned whole (len %d)", len([]rune(got)))
	}
	// snippetLen+1 -> truncated to snippetLen runes.
	long := strings.Repeat("b", snippetLen+50)
	got := trimPrefix(long)
	if n := len([]rune(got)); n != snippetLen {
		t.Fatalf("over-long string should truncate to %d runes, got %d", snippetLen, n)
	}
}

func TestTrimPrefix_MultibyteRuneSafe(t *testing.T) {
	// Truncation must be on rune boundaries, not byte boundaries — an
	// emoji is 4 bytes but counts as 1 rune. snippetLen+10 runes of a
	// multibyte char must truncate to exactly snippetLen runes and stay
	// valid UTF-8.
	long := strings.Repeat("é", snippetLen+10)
	got := trimPrefix(long)
	if n := len([]rune(got)); n != snippetLen {
		t.Fatalf("multibyte truncation should yield %d runes, got %d", snippetLen, n)
	}
}

// --- textOrEmpty / timeOrNil -----------------------------------------

func TestTextOrEmpty(t *testing.T) {
	if got := textOrEmpty(pgtype.Text{String: "hello", Valid: true}); got != "hello" {
		t.Fatalf("valid text = %q, want hello", got)
	}
	// Invalid (NULL) must yield "" even if String is populated.
	if got := textOrEmpty(pgtype.Text{String: "ignored", Valid: false}); got != "" {
		t.Fatalf("NULL text must yield empty, got %q", got)
	}
}

func TestTimeOrNil(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	got := timeOrNil(pgtype.Timestamptz{Time: now, Valid: true})
	if got == nil || !got.Equal(now) {
		t.Fatalf("valid timestamp should round-trip, got %v", got)
	}
	// NULL -> nil pointer so omitempty drops it (no zero epoch).
	if got := timeOrNil(pgtype.Timestamptz{Time: now, Valid: false}); got != nil {
		t.Fatalf("NULL timestamp must yield nil pointer, got %v", got)
	}
}

// --- recordDoc JSON projection shape ---------------------------------

// marshalDoc marshals a recordDoc and decodes it back into a generic map
// so tests can assert presence/absence of keys (the omitempty contract).
func marshalDoc(t *testing.T, d recordDoc) map[string]any {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal recordDoc: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal recordDoc: %v", err)
	}
	return m
}

func TestRecordDoc_RequiredFieldsAlwaysPresent(t *testing.T) {
	// id/profile/vault/sha/kind have no omitempty: they must serialise
	// even at zero value (id=0 is a legitimate value, not "absent").
	m := marshalDoc(t, recordDoc{})
	for _, k := range []string{"id", "profile", "vault", "sha", "kind"} {
		if _, ok := m[k]; !ok {
			t.Errorf("required field %q must always be present (no omitempty), got keys %v", k, keys(m))
		}
	}
}

func TestRecordDoc_NeverProjectsDroppedFields(t *testing.T) {
	// raw_body, minio_key, size_bytes are deliberately not part of the
	// projection. They have no struct field, so they can never appear —
	// assert the contract explicitly so a future struct edit trips here.
	full := recordDoc{
		ID: 9, Profile: "p", Vault: "v", Sha: "s", Kind: "note",
		Title: "t", Body: "b", ExtractedText: "x",
		MemoryType: "semantic", Topic: "memory", Reliability: "high",
		Source: []string{"task:1"}, SourceURL: "http://x", Tags: []string{"finance"},
		MimeType: "text/plain", OriginalFilename: "f.txt", EmbeddingModel: "nomic",
	}
	m := marshalDoc(t, full)
	for _, k := range []string{"raw_body", "minio_key", "size_bytes", "gate_reason", "synthesised"} {
		if _, ok := m[k]; ok {
			t.Errorf("field %q must NOT be projected, but appeared in %v", k, keys(m))
		}
	}
}

func TestRecordDoc_OmitemptyDropsAbsentOptionals(t *testing.T) {
	// Only required fields set; every optional must be absent.
	m := marshalDoc(t, recordDoc{ID: 1, Profile: "p", Vault: "v", Sha: "s", Kind: "note"})
	for _, k := range []string{
		"memory_type", "topic", "reliability", "source", "source_url",
		"tags", "mime_type", "original_filename", "embedding_model",
		"title", "body", "extracted_text",
		"captured_at", "created_at", "updated_at", "embedding",
	} {
		if _, ok := m[k]; ok {
			t.Errorf("optional field %q should be omitted when empty, but appeared", k)
		}
	}
}

func TestRecordDoc_EmbeddingSerialisesWhenPresent(t *testing.T) {
	d := recordDoc{ID: 1, Profile: "p", Vault: "v", Sha: "s", Kind: "note",
		Embedding: []float32{0.1, 0.2, 0.3}}
	m := marshalDoc(t, d)
	emb, ok := m["embedding"].([]any)
	if !ok {
		t.Fatalf("non-nil embedding should serialise as an array, got %T", m["embedding"])
	}
	if len(emb) != 3 {
		t.Fatalf("embedding length = %d, want 3", len(emb))
	}
}

func TestRecordDoc_DatesSerialiseWhenPresent(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	d := recordDoc{ID: 1, Profile: "p", Vault: "v", Sha: "s", Kind: "note",
		CapturedAt: &ts, CreatedAt: &ts, UpdatedAt: &ts}
	m := marshalDoc(t, d)
	for _, k := range []string{"captured_at", "created_at", "updated_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("date %q should serialise when the pointer is set", k)
		}
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- pgvector.Slice contract (used by Project's embedding guard) ------

func TestPgvectorSliceRoundTrip(t *testing.T) {
	// Project relies on (*pgvector.Vector).Slice() returning the same
	// values it was built from; guard the assumption used in the
	// nil/empty-vector embedding logic.
	in := []float32{0.5, -0.25, 1.0}
	v := pgvector.NewVector(in)
	out := v.Slice()
	if len(out) != len(in) {
		t.Fatalf("slice length = %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("slice[%d] = %v, want %v", i, out[i], in[i])
		}
	}
	// A zero-length vector slices to a zero-length slice (the guard in
	// Project drops it rather than sending an empty knn vector).
	if got := pgvector.NewVector([]float32{}).Slice(); len(got) != 0 {
		t.Fatalf("empty vector should slice to empty, got %v", got)
	}
}

// --- index naming + constants ----------------------------------------

func TestIndexNameWithPrefix_Usage(t *testing.T) {
	// This is the exact resolution the Projector + Recaller perform to
	// target their physical index.
	if got := osearch.IndexNameWithPrefix("", LogicalRecords); got != "pb_records" {
		t.Fatalf("empty prefix should yield bare logical name, got %q", got)
	}
	if got := osearch.IndexNameWithPrefix("client_x_", LogicalRecords); got != "client_x_pb_records" {
		t.Fatalf("prefixed index = %q, want client_x_pb_records", got)
	}
	if LogicalRecords != "pb_records" {
		t.Fatalf("LogicalRecords drifted from pb_records: %q", LogicalRecords)
	}
}

// --- constructors (wiring + waitForRefresh contract) -----------------

func TestConstructors_WiringAndRefreshContract(t *testing.T) {
	// nil client is fine — the constructors only store the pointer; they
	// never dereference it. We assert the prefix is threaded and the
	// waitForRefresh contract: New defaults false, NewWithRefresh forces
	// true (the doc-immediately-searchable, test-only mode).
	p := New(nil, "client_x_")
	if p.prefix != "client_x_" {
		t.Fatalf("New prefix = %q, want client_x_", p.prefix)
	}
	if p.waitForRefresh {
		t.Fatal("New must default waitForRefresh=false (production relies on refresh_interval)")
	}

	pr := NewWithRefresh(nil, "client_x_")
	if !pr.waitForRefresh {
		t.Fatal("NewWithRefresh must force waitForRefresh=true")
	}

	rc := NewRecaller(nil, "pbproj_")
	if rc.prefix != "pbproj_" {
		t.Fatalf("NewRecaller prefix = %q, want pbproj_", rc.prefix)
	}
}

// --- hybrid pipeline body contract -----------------------------------

func TestHybridPipelineBody_NormalizationContract(t *testing.T) {
	procs, ok := hybridPipelineBody["phase_results_processors"].([]any)
	if !ok || len(procs) != 1 {
		t.Fatalf("expected one phase_results_processor, got %v", hybridPipelineBody["phase_results_processors"])
	}
	proc := procs[0].(map[string]any)
	norm := proc["normalization-processor"].(map[string]any)

	technique := norm["normalization"].(map[string]any)["technique"]
	if technique != "min_max" {
		t.Fatalf("normalization technique = %v, want min_max", technique)
	}
	comb := norm["combination"].(map[string]any)
	if comb["technique"] != "arithmetic_mean" {
		t.Fatalf("combination technique = %v, want arithmetic_mean", comb["technique"])
	}
	weights := comb["parameters"].(map[string]any)["weights"].([]float64)
	if len(weights) != 2 || weights[0] != 0.5 || weights[1] != 0.5 {
		t.Fatalf("weights = %v, want [0.5 0.5] (equal BM25/kNN)", weights)
	}
	if HybridPipeline != "pb-hybrid" {
		t.Fatalf("HybridPipeline name drifted: %q", HybridPipeline)
	}
}
