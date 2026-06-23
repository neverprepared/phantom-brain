package osearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// CountByVault counts docs in the prefixed logical index matching
// (profile, vault). Returns (0, nil) when the index does not exist
// (404) — the daemon's footgun detector treats a missing prefixed
// index as "zero docs at this binding's prefix" so newly-added
// overrides on empty bindings don't fire the check.
//
// The count endpoint is the cheapest read OS exposes: no source
// hydration, no scroll context, single integer result.
func (c *Client) CountByVault(ctx context.Context, logical, profile, vault string) (int64, error) {
	query := map[string]any{
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []map[string]any{
					{"term": map[string]any{"profile": profile}},
					{"term": map[string]any{"vault": vault}},
				},
			},
		},
	}
	body, err := json.Marshal(query)
	if err != nil {
		return 0, fmt.Errorf("marshal count query: %w", err)
	}
	resp, err := c.api.Indices.Count(ctx, &osapi.IndicesCountReq{
		Indices: []string{c.IndexName(logical)},
		Body:    bytes.NewReader(body),
	})
	if err != nil {
		if statusFromErr(err) == http.StatusNotFound {
			return 0, nil
		}
		if resp != nil && resp.Inspect().Response != nil &&
			resp.Inspect().Response.StatusCode == http.StatusNotFound {
			return 0, nil
		}
		return 0, fmt.Errorf("count %s: %w", logical, err)
	}
	if resp == nil || resp.Inspect().Response == nil {
		return 0, nil
	}
	raw, err := io.ReadAll(resp.Inspect().Response.Body)
	if err != nil {
		return 0, fmt.Errorf("read count response: %w", err)
	}
	var out struct {
		Count int64 `json:"count"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("decode count response: %w", err)
	}
	return out.Count, nil
}
