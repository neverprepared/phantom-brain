package index

import (
	"context"
	"fmt"
	"sort"
)

// Hit is one search result row.
type Hit struct {
	SHA        string
	SourcePath string

	// Score is the result's fused score (higher is better). For
	// SearchVector this is cosine similarity converted from distance;
	// for SearchText it's BM25 rank-position (transformed to higher-
	// better); for SearchHybrid it's the RRF sum across both.
	Score float64

	// VectorRank and TextRank are 1-based positions in their
	// respective rankings, or 0 if not present in that ranking.
	// Surfaced for debugging hybrid results.
	VectorRank int
	TextRank   int
}

// SearchVector returns the limit nearest neighbours to query.
//
// Score is 1 - distance so higher is better — distance is sqlite-vec's
// L2 by default for vec0, scaled to [0,1]-ish for our typical
// normalized embeddings. Don't read across embedding models.
func (i *Index) SearchVector(ctx context.Context, query []float32, limit int) ([]Hit, error) {
	if len(query) != i.dims {
		return nil, fmt.Errorf("%w: query has %d, index has %d", ErrDimMismatch, len(query), i.dims)
	}
	if limit <= 0 {
		return nil, nil
	}
	blob, err := serializeF32(query)
	if err != nil {
		return nil, fmt.Errorf("index: SearchVector: serialize: %w", err)
	}

	rows, err := i.sql.QueryContext(ctx, `
		SELECT m.sha, m.source_path, v.distance
		FROM vec_embeddings v
		JOIN vec_map m ON m.rowid = v.rowid
		WHERE v.embedding MATCH ? AND k = ?
		ORDER BY v.distance
	`, blob, limit)
	if err != nil {
		return nil, fmt.Errorf("index: SearchVector: %w", err)
	}
	defer rows.Close()

	var out []Hit
	rank := 0
	for rows.Next() {
		var (
			sha, path string
			distance  float64
		)
		if err := rows.Scan(&sha, &path, &distance); err != nil {
			return nil, fmt.Errorf("index: SearchVector: scan: %w", err)
		}
		rank++
		out = append(out, Hit{
			SHA:        sha,
			SourcePath: path,
			Score:      1.0 - distance,
			VectorRank: rank,
		})
	}
	return out, rows.Err()
}

// SearchText runs an FTS5 BM25 query and returns up to limit results,
// best-first.
//
// query is interpreted by FTS5's MATCH syntax. Callers wanting plain
// keyword search should pre-quote with FTS5 phrase semantics; callers
// who want column-scoped queries can write {title body} : foo
// natively.
//
// Score is a higher-is-better transform of rank position so it RRF-
// fuses cleanly with vector scores. The raw BM25 magnitude is
// intentionally opaque outside this package.
func (i *Index) SearchText(ctx context.Context, query string, limit int) ([]Hit, error) {
	if query == "" || limit <= 0 {
		return nil, nil
	}
	rows, err := i.sql.QueryContext(ctx, `
		SELECT sha
		FROM fts_memories
		WHERE fts_memories MATCH ?
		ORDER BY rank
		LIMIT ?
	`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("index: SearchText: %w", err)
	}
	defer rows.Close()

	var shas []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, fmt.Errorf("index: SearchText: scan: %w", err)
		}
		shas = append(shas, sha)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Hydrate paths in a second query so the FTS5 path stays narrow.
	paths, err := i.pathsForSHAs(ctx, shas)
	if err != nil {
		return nil, err
	}

	out := make([]Hit, 0, len(shas))
	for rank, sha := range shas {
		out = append(out, Hit{
			SHA:        sha,
			SourcePath: paths[sha],
			Score:      1.0 / float64(rank+1),
			TextRank:   rank + 1,
		})
	}
	return out, nil
}

// rrfK is the standard reciprocal-rank-fusion constant. Higher values
// reduce the dominance of top-ranked items; 60 is the canonical value
// from Cormack-Buettcher-Clarke (2009).
const rrfK = 60.0

// SearchHybrid runs both halves of the query, fuses with Reciprocal
// Rank Fusion, and returns up to limit results.
//
// Documents that appear in only one ranking still score; documents in
// both get added scores and naturally float to the top.
//
// We over-fetch (limit*3) from each half so the fused ranking has
// material to work with — a document weak in vector but strong in
// FTS5 might miss the top-`limit` of vector but still belong in the
// hybrid top-`limit`.
func (i *Index) SearchHybrid(ctx context.Context, textQuery string, vectorQuery []float32, limit int) ([]Hit, error) {
	if limit <= 0 {
		return nil, nil
	}
	overfetch := limit * 3
	if overfetch < 10 {
		overfetch = 10
	}

	textHits, err := i.SearchText(ctx, textQuery, overfetch)
	if err != nil {
		return nil, err
	}
	vecHits, err := i.SearchVector(ctx, vectorQuery, overfetch)
	if err != nil {
		return nil, err
	}

	fused := make(map[string]*Hit)
	for _, h := range textHits {
		c := h
		fused[h.SHA] = &c
		fused[h.SHA].Score = 1.0 / (rrfK + float64(h.TextRank))
	}
	for _, h := range vecHits {
		got, ok := fused[h.SHA]
		if !ok {
			c := h
			fused[h.SHA] = &c
			fused[h.SHA].Score = 1.0 / (rrfK + float64(h.VectorRank))
			continue
		}
		got.VectorRank = h.VectorRank
		got.SourcePath = h.SourcePath
		got.Score += 1.0 / (rrfK + float64(h.VectorRank))
	}

	all := make([]Hit, 0, len(fused))
	for _, h := range fused {
		all = append(all, *h)
	}
	sort.SliceStable(all, func(a, b int) bool {
		return all[a].Score > all[b].Score
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// pathsForSHAs returns sha -> source_path for the given SHAs.
// Single-query batch lookup; preserves caller's slice order in the
// returned map sense (map iteration order is randomized but lookup
// is O(1)).
func (i *Index) pathsForSHAs(ctx context.Context, shas []string) (map[string]string, error) {
	if len(shas) == 0 {
		return map[string]string{}, nil
	}
	// Build a parameterized IN clause.
	q := `SELECT sha, source_path FROM vec_map WHERE sha IN (?`
	args := make([]any, 0, len(shas))
	args = append(args, shas[0])
	for _, s := range shas[1:] {
		q += `,?`
		args = append(args, s)
	}
	q += `)`

	rows, err := i.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("index: pathsForSHAs: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string, len(shas))
	for rows.Next() {
		var sha, path string
		if err := rows.Scan(&sha, &path); err != nil {
			return nil, fmt.Errorf("index: pathsForSHAs: scan: %w", err)
		}
		out[sha] = path
	}
	return out, rows.Err()
}
