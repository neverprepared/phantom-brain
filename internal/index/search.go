package index

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Hit is one search result row.
type Hit struct {
	SHA        string
	SourcePath string

	// Title is the doc's title (from SummaryDoc.Title at export time).
	// Empty when the snapshot was produced by a pre-fix/49 daemon.
	Title string

	// Kind is the SummaryDoc kind ("note", "web_scrape",
	// "attachment_stub", "task_summary", ...). Empty for legacy rows.
	Kind string

	// Tags is the space-joined tag blob (same wire shape as the
	// fts_memories.tags column). Render consumers split on whitespace
	// to extract prefixed facets like "mime:application/pdf".
	Tags string

	// Snippet is the first ~150 chars of the doc body, frontmatter
	// stripped and newlines collapsed. Populated by the hybrid search
	// path so renderRecallHits can show context without a second hop.
	Snippet string

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
		SELECT m.sha, m.source_path, m.title, m.kind, m.tags, v.distance
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
			sha, path, title, kind, tags string
			distance                     float64
		)
		if err := rows.Scan(&sha, &path, &title, &kind, &tags, &distance); err != nil {
			return nil, fmt.Errorf("index: SearchVector: scan: %w", err)
		}
		rank++
		out = append(out, Hit{
			SHA:        sha,
			SourcePath: path,
			Title:      title,
			Kind:       kind,
			Tags:       tags,
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

	// Hydrate paths + render metadata in a second query so the FTS5
	// path stays narrow.
	meta, err := i.metaForSHAs(ctx, shas)
	if err != nil {
		return nil, err
	}

	out := make([]Hit, 0, len(shas))
	for rank, sha := range shas {
		m := meta[sha]
		out = append(out, Hit{
			SHA:        sha,
			SourcePath: m.sourcePath,
			Title:      m.title,
			Kind:       m.kind,
			Tags:       m.tags,
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
		if h.Title != "" {
			got.Title = h.Title
		}
		if h.Kind != "" {
			got.Kind = h.Kind
		}
		if h.Tags != "" {
			got.Tags = h.Tags
		}
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

	// Hydrate snippets for the final top-K only — keeps the FTS body
	// scan small even when overfetch pulled wide.
	shas := make([]string, 0, len(all))
	for _, h := range all {
		shas = append(shas, h.SHA)
	}
	snips, err := i.snippetsForSHAs(ctx, shas)
	if err != nil {
		return nil, err
	}
	for i := range all {
		all[i].Snippet = snips[all[i].SHA]
	}
	return all, nil
}

// hitMeta is the per-SHA bundle metaForSHAs hydrates from vec_map.
type hitMeta struct {
	sourcePath string
	title      string
	kind       string
	tags       string
}

// metaForSHAs returns sha -> render metadata for the given SHAs in a
// single batched lookup.
func (i *Index) metaForSHAs(ctx context.Context, shas []string) (map[string]hitMeta, error) {
	if len(shas) == 0 {
		return map[string]hitMeta{}, nil
	}
	q := `SELECT sha, source_path, title, kind, tags FROM vec_map WHERE sha IN (?`
	args := make([]any, 0, len(shas))
	args = append(args, shas[0])
	for _, s := range shas[1:] {
		q += `,?`
		args = append(args, s)
	}
	q += `)`

	rows, err := i.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("index: metaForSHAs: %w", err)
	}
	defer rows.Close()
	out := make(map[string]hitMeta, len(shas))
	for rows.Next() {
		var sha string
		var m hitMeta
		if err := rows.Scan(&sha, &m.sourcePath, &m.title, &m.kind, &m.tags); err != nil {
			return nil, fmt.Errorf("index: metaForSHAs: scan: %w", err)
		}
		out[sha] = m
	}
	return out, rows.Err()
}

// snippetsForSHAs returns sha -> 150-char snippet pulled from
// fts_memories.body. Snippets are normalised: frontmatter stripped,
// newlines collapsed, truncated with an ellipsis. Empty bodies yield
// an empty string.
func (i *Index) snippetsForSHAs(ctx context.Context, shas []string) (map[string]string, error) {
	if len(shas) == 0 {
		return map[string]string{}, nil
	}
	q := `SELECT sha, body FROM fts_memories WHERE sha IN (?`
	args := make([]any, 0, len(shas))
	args = append(args, shas[0])
	for _, s := range shas[1:] {
		q += `,?`
		args = append(args, s)
	}
	q += `)`

	rows, err := i.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("index: snippetsForSHAs: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string, len(shas))
	for rows.Next() {
		var sha, body string
		if err := rows.Scan(&sha, &body); err != nil {
			return nil, fmt.Errorf("index: snippetsForSHAs: scan: %w", err)
		}
		out[sha] = Snippet(body, snippetMaxRunes)
	}
	return out, rows.Err()
}

// snippetMaxRunes is the target snippet length surfaced in recall hits.
// Sized to be informative without flooding the agent's context window.
const snippetMaxRunes = 150

// Snippet strips a leading YAML frontmatter block, collapses all
// whitespace to single spaces, and truncates to maxRunes (appending
// an ellipsis when the body was longer). Exported so callers outside
// the package can render hits the same way.
func Snippet(body string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	b := strings.TrimLeft(body, " \t\r\n")
	if strings.HasPrefix(b, "---\n") || strings.HasPrefix(b, "---\r\n") {
		// Skip past the opening fence.
		rest := b[4:]
		if strings.HasPrefix(b, "---\r\n") {
			rest = b[5:]
		}
		if end := strings.Index(rest, "\n---"); end >= 0 {
			tail := rest[end+4:]
			// Trim the closing fence's own newline.
			tail = strings.TrimLeft(tail, "\r\n")
			b = tail
		}
	}
	// Collapse whitespace runs to single spaces.
	var sb strings.Builder
	sb.Grow(len(b))
	prevSpace := false
	for _, r := range b {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			if !prevSpace && sb.Len() > 0 {
				sb.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		sb.WriteRune(r)
		prevSpace = false
	}
	out := strings.TrimRight(sb.String(), " ")
	runes := []rune(out)
	if len(runes) <= maxRunes {
		return out
	}
	return string(runes[:maxRunes]) + "…"
}
