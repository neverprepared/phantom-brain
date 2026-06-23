package index

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// fakeEmbedder produces a deterministic vector derived from a single
// scalar per input string. Tests pass distinct scalars so vectors are
// distinguishable, and nearest-neighbour queries have predictable
// expected results.
type fakeEmbedder struct {
	dims int
	plan map[string][]float32 // input -> embedding
}

func (f *fakeEmbedder) Dims() int { return f.dims }
func (f *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, s := range inputs {
		v, ok := f.plan[s]
		if !ok {
			return nil, errors.New("fakeEmbedder: no plan for " + s)
		}
		out[i] = v
	}
	return out, nil
}

func openTest(t *testing.T, dims int) *Index {
	t.Helper()
	idx, err := Open(t.TempDir(), dims)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func TestOpenAppliesSchemaIdempotent(t *testing.T) {
	dir := t.TempDir()
	idx, err := Open(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen — schema-if-not-exists must be a no-op.
	idx2, err := Open(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	_ = idx2.Close()
}

func TestOpenRejectsBadArgs(t *testing.T) {
	if _, err := Open("", 4); err == nil {
		t.Error("empty dir should fail")
	}
	if _, err := Open(t.TempDir(), 0); err == nil {
		t.Error("zero dims should fail")
	}
	if _, err := Open(t.TempDir(), -1); err == nil {
		t.Error("negative dims should fail")
	}
}

func TestUpsertAndHas(t *testing.T) {
	idx := openTest(t, 3)
	ctx := context.Background()

	if has, _ := idx.Has("sha-a"); has {
		t.Error("fresh index should not Have anything")
	}

	rec := Record{
		SHA:        "sha-a",
		SourcePath: "Wiki/a.md",
		Title:      "title a",
		Tags:       "alpha bravo",
		Body:       "body of a",
		Embedding:  []float32{1, 0, 0},
	}
	if err := idx.Upsert(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if has, _ := idx.Has("sha-a"); !has {
		t.Error("Has should be true after Upsert")
	}
}

func TestUpsertReplacesByCSHA(t *testing.T) {
	idx := openTest(t, 3)
	ctx := context.Background()

	first := Record{SHA: "k", SourcePath: "Wiki/k.md", Title: "v1", Body: "old body", Embedding: []float32{1, 0, 0}}
	if err := idx.Upsert(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := Record{SHA: "k", SourcePath: "Wiki/k-renamed.md", Title: "v2", Body: "new body", Embedding: []float32{0, 1, 0}}
	if err := idx.Upsert(ctx, second); err != nil {
		t.Fatal(err)
	}

	// Vector search for the OLD vector should return the new record
	// only if the rowid was preserved; either way only one row should
	// exist.
	hits, err := idx.SearchVector(ctx, []float32{0, 1, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if hits[0].SourcePath != "Wiki/k-renamed.md" {
		t.Errorf("source_path = %q, want renamed", hits[0].SourcePath)
	}

	// FTS should reflect the new title.
	thits, err := idx.SearchText(ctx, "v2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(thits) != 1 {
		t.Errorf("text hits for new title = %d, want 1", len(thits))
	}

	// And the old title should be gone.
	oldHits, err := idx.SearchText(ctx, "v1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldHits) != 0 {
		t.Errorf("old title still searchable: %v", oldHits)
	}
}

func TestUpsertRejectsBadArgs(t *testing.T) {
	idx := openTest(t, 3)
	ctx := context.Background()

	if err := idx.Upsert(ctx, Record{SourcePath: "p", Embedding: []float32{1, 0, 0}}); err == nil {
		t.Error("empty SHA should fail")
	}
	if err := idx.Upsert(ctx, Record{SHA: "s", Embedding: []float32{1, 0, 0}}); err == nil {
		t.Error("empty SourcePath should fail")
	}
	if err := idx.Upsert(ctx, Record{SHA: "s", SourcePath: "p", Embedding: []float32{1, 0}}); !errors.Is(err, ErrDimMismatch) {
		t.Errorf("dim mismatch should return ErrDimMismatch; got %v", err)
	}
}

func TestSearchVectorReturnsNearestFirst(t *testing.T) {
	idx := openTest(t, 3)
	ctx := context.Background()

	records := []Record{
		{SHA: "x", SourcePath: "Wiki/x.md", Embedding: []float32{1, 0, 0}},
		{SHA: "y", SourcePath: "Wiki/y.md", Embedding: []float32{0, 1, 0}},
		{SHA: "z", SourcePath: "Wiki/z.md", Embedding: []float32{0, 0, 1}},
	}
	for _, r := range records {
		if err := idx.Upsert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	hits, err := idx.SearchVector(ctx, []float32{1, 0, 0}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	if hits[0].SHA != "x" {
		t.Errorf("nearest = %s, want x", hits[0].SHA)
	}
	if hits[0].VectorRank != 1 || hits[1].VectorRank != 2 || hits[2].VectorRank != 3 {
		t.Errorf("ranks = [%d %d %d], want [1 2 3]", hits[0].VectorRank, hits[1].VectorRank, hits[2].VectorRank)
	}
	if hits[0].Score < hits[1].Score || hits[1].Score < hits[2].Score {
		t.Errorf("scores not descending: %v", []float64{hits[0].Score, hits[1].Score, hits[2].Score})
	}
}

func TestSearchTextBM25Ordering(t *testing.T) {
	idx := openTest(t, 3)
	ctx := context.Background()

	for _, r := range []Record{
		{SHA: "a", SourcePath: "Wiki/a.md", Title: "apple banana", Body: "fruit basket", Embedding: []float32{1, 0, 0}},
		{SHA: "b", SourcePath: "Wiki/b.md", Title: "banana", Body: "yellow", Embedding: []float32{0, 1, 0}},
		{SHA: "c", SourcePath: "Wiki/c.md", Title: "carrot", Body: "orange", Embedding: []float32{0, 0, 1}},
	} {
		if err := idx.Upsert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	hits, err := idx.SearchText(ctx, "banana", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	// Both have 'banana'; FTS5 should rank them. Just confirm carrot
	// is NOT present and ranks are 1, 2.
	for _, h := range hits {
		if h.SHA == "c" {
			t.Errorf("carrot leaked into banana search")
		}
	}
	if hits[0].TextRank != 1 || hits[1].TextRank != 2 {
		t.Errorf("ranks = [%d %d], want [1 2]", hits[0].TextRank, hits[1].TextRank)
	}
}

func TestSearchTextEmptyQueryReturnsNil(t *testing.T) {
	idx := openTest(t, 3)
	hits, err := idx.SearchText(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if hits != nil {
		t.Errorf("hits = %v, want nil", hits)
	}
}

func TestSearchHybridFusesRankings(t *testing.T) {
	idx := openTest(t, 3)
	ctx := context.Background()

	// Three docs. Document "fish" wins on vector (exact match). Document
	// "fishing" wins on text (title match). RRF should put both at the
	// top above the unrelated "swimming" doc.
	for _, r := range []Record{
		{SHA: "fish", SourcePath: "Wiki/fish.md", Title: "salmon trout", Body: "anatomy notes",
			Embedding: []float32{1, 0, 0}},
		{SHA: "fishing", SourcePath: "Wiki/fishing.md", Title: "fishing guide", Body: "rods reels lines",
			Embedding: []float32{0, 1, 0}},
		{SHA: "swimming", SourcePath: "Wiki/swimming.md", Title: "swimming techniques", Body: "freestyle butterfly",
			Embedding: []float32{0, 0, 1}},
	} {
		if err := idx.Upsert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	hits, err := idx.SearchHybrid(ctx, "fishing", []float32{1, 0, 0}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %d hits, want >= 2", len(hits))
	}

	// "fish" gets a high vector rank; "fishing" gets a high text rank.
	// The first two hits must be one of those two (order may vary by
	// RRF tiebreak but both should beat "swimming").
	top := map[string]bool{hits[0].SHA: true, hits[1].SHA: true}
	if !top["fish"] || !top["fishing"] {
		t.Errorf("top 2 = %v, want fish + fishing", top)
	}
}

func TestSearchHybridSingleRankingStillReturns(t *testing.T) {
	idx := openTest(t, 3)
	ctx := context.Background()

	if err := idx.Upsert(ctx, Record{
		SHA: "only", SourcePath: "Wiki/only.md", Title: "unique word", Body: "lonely",
		Embedding: []float32{1, 0, 0},
	}); err != nil {
		t.Fatal(err)
	}

	// Query has text match only (no useful vector).
	hits, err := idx.SearchHybrid(ctx, "unique", []float32{0, 0, 1}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 1 {
		t.Fatal("text-only ranking should still surface hits")
	}
}

func TestDeleteRemovesEverything(t *testing.T) {
	idx := openTest(t, 3)
	ctx := context.Background()

	if err := idx.Upsert(ctx, Record{
		SHA: "k", SourcePath: "Wiki/k.md", Title: "title", Body: "body", Embedding: []float32{1, 0, 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if has, _ := idx.Has("k"); has {
		t.Error("Has true after Delete")
	}

	vec, _ := idx.SearchVector(ctx, []float32{1, 0, 0}, 5)
	if len(vec) != 0 {
		t.Errorf("vector hits after delete: %v", vec)
	}
	txt, _ := idx.SearchText(ctx, "title", 5)
	if len(txt) != 0 {
		t.Errorf("text hits after delete: %v", txt)
	}
}

func TestDeleteIdempotent(t *testing.T) {
	idx := openTest(t, 3)
	if err := idx.Delete(context.Background(), "never-existed"); err != nil {
		t.Errorf("delete of missing sha should be no-op; got %v", err)
	}
}

func TestAllSHAs(t *testing.T) {
	idx := openTest(t, 3)
	ctx := context.Background()

	if shas, _ := idx.AllSHAs(); len(shas) != 0 {
		t.Errorf("fresh index AllSHAs = %v", shas)
	}

	for _, sha := range []string{"a", "b", "c"} {
		if err := idx.Upsert(ctx, Record{
			SHA: sha, SourcePath: filepath.Join("Wiki", sha+".md"),
			Embedding: []float32{1, 0, 0},
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := idx.AllSHAs()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("len = %d, want 3", len(got))
	}
	for _, sha := range []string{"a", "b", "c"} {
		if got[sha] != filepath.Join("Wiki", sha+".md") {
			t.Errorf("AllSHAs[%s] = %q", sha, got[sha])
		}
	}
}

// TestEmbedderInterfaceShape just compiles — confirming that a struct
// satisfying internal/index.Embedder is buildable from outside the
// package and from internal/ollama specifically. Real ollama hookup
// happens in the tool ports.
func TestEmbedderInterfaceShape(t *testing.T) {
	var _ Embedder = &fakeEmbedder{dims: 3}
}

func TestSearchHydratesTitleKindTagsSnippet(t *testing.T) {
	idx := openTest(t, 3)
	ctx := context.Background()

	rec := Record{
		SHA:        "h",
		SourcePath: "Wiki/h.md",
		Title:      "Tax forms 2026",
		Tags:       "vendor:UIA mime:application/pdf attachment",
		Body:       "---\ntitle: ignore me\n---\nLine one of body.\n\nLine two — should collapse.",
		Kind:       "attachment_stub",
		Embedding:  []float32{1, 0, 0},
	}
	if err := idx.Upsert(ctx, rec); err != nil {
		t.Fatal(err)
	}

	vec, err := idx.SearchVector(ctx, []float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 1 {
		t.Fatalf("vec hits = %d, want 1", len(vec))
	}
	if vec[0].Title != "Tax forms 2026" || vec[0].Kind != "attachment_stub" {
		t.Errorf("vec hit metadata = %+v", vec[0])
	}
	if vec[0].Tags == "" {
		t.Errorf("vec hit Tags empty")
	}

	txt, err := idx.SearchText(ctx, "tax", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(txt) != 1 || txt[0].Title != "Tax forms 2026" || txt[0].Kind != "attachment_stub" {
		t.Errorf("text hit metadata = %+v", txt)
	}

	hyb, err := idx.SearchHybrid(ctx, "tax", []float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hyb) != 1 {
		t.Fatalf("hybrid hits = %d, want 1", len(hyb))
	}
	if hyb[0].Title != "Tax forms 2026" || hyb[0].Kind != "attachment_stub" {
		t.Errorf("hybrid hit metadata = %+v", hyb[0])
	}
	if hyb[0].Snippet == "" {
		t.Errorf("hybrid hit Snippet empty")
	}
	if !contains(hyb[0].Snippet, "Line one of body.") {
		t.Errorf("snippet missing body text: %q", hyb[0].Snippet)
	}
	if contains(hyb[0].Snippet, "ignore me") {
		t.Errorf("snippet leaked frontmatter: %q", hyb[0].Snippet)
	}
}

func TestSnippetStripsFrontmatterAndTruncates(t *testing.T) {
	body := "---\nfoo: bar\nbaz: qux\n---\nhello   world\nsecond\tline"
	got := Snippet(body, 150)
	want := "hello world second line"
	if got != want {
		t.Errorf("Snippet = %q, want %q", got, want)
	}

	long := "abcdefghij" // 10 chars
	for i := 0; i < 20; i++ {
		long += "abcdefghij"
	}
	got = Snippet(long, 50)
	if len([]rune(got)) != 51 { // 50 + ellipsis
		t.Errorf("truncated len = %d runes, want 51", len([]rune(got)))
	}
	if got[len(got)-len("…"):] != "…" {
		t.Errorf("truncated suffix = %q, want ellipsis", got)
	}
}

func TestSchemaApplyIdempotentAcrossVersions(t *testing.T) {
	// Simulate a pre-fix/49 snapshot: open, close, reopen. The second
	// open re-runs ALTER TABLE ADD COLUMN which would otherwise fail.
	dir := t.TempDir()
	idx, err := Open(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	_ = idx.Close()
	idx2, err := Open(dir, 3)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	_ = idx2.Close()
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
