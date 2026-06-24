package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FetchResult is the complete stored record for one SHA — the
// untruncated counterpart to a recall Hit. Recall returns a 150-char
// snippet for *discovery*; FetchBySHA returns the whole body for
// *retrieval*, read from the same local snapshot so the two never
// disagree.
type FetchResult struct {
	SHA        string
	Title      string
	Kind       string
	SourcePath string
	Tags       string
	Body       string
}

// FetchBySHA returns the full stored record for sha from the local
// snapshot, or (nil, nil) when no row matches. Body is the complete
// markdown body, never truncated. It joins vec_map (metadata) to
// fts_memories (body) — the exact tables recall reads — so any SHA
// recall surfaced is always fetchable here.
func (i *Index) FetchBySHA(ctx context.Context, sha string) (*FetchResult, error) {
	if sha == "" {
		return nil, fmt.Errorf("index: FetchBySHA: sha is required")
	}
	row := i.sql.QueryRowContext(ctx, `
		SELECT m.sha, m.title, m.kind, m.source_path, m.tags, f.body
		FROM vec_map m
		JOIN fts_memories f ON f.sha = m.sha
		WHERE m.sha = ?`, sha)

	var r FetchResult
	switch err := row.Scan(&r.SHA, &r.Title, &r.Kind, &r.SourcePath, &r.Tags, &r.Body); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("index: FetchBySHA: %w", err)
	}
	return &r, nil
}
