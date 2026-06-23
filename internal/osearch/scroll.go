package osearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// ScrollAttachments paginates pb_attachments scoped to (profile, vault),
// decoding each hit into an AttachmentDoc and invoking fn. Returning
// an error from fn aborts the scroll. batchSize <= 0 falls back to 500.
//
// Used by the backfill subcommand to walk every existing attachment
// metadata doc; kept exported so future operator tooling that needs
// to iterate attachments doesn't have to re-implement the scroll
// boilerplate already burned into scrollSummariesIntoIndex.
func (c *Client) ScrollAttachments(ctx context.Context, profile, vault string, batchSize int, fn func(AttachmentDoc) error) error {
	return c.ScrollAttachmentsWithPrefix(ctx, c.prefix, profile, vault, batchSize, fn)
}

// ScrollAttachmentsWithPrefix paginates pb_attachments at the index
// resolved from the supplied per-call prefix.
func (c *Client) ScrollAttachmentsWithPrefix(ctx context.Context, prefix, profile, vault string, batchSize int, fn func(AttachmentDoc) error) error {
	if profile == "" || vault == "" {
		return fmt.Errorf("osearch.ScrollAttachments: profile and vault required")
	}
	if batchSize <= 0 {
		batchSize = 500
	}
	keepAlive := time.Minute

	query := map[string]any{
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []map[string]any{
					{"term": map[string]any{"profile": profile}},
					{"term": map[string]any{"vault": vault}},
				},
			},
		},
		"size": batchSize,
		"sort": []map[string]any{{"_doc": map[string]any{"order": "asc"}}},
	}
	body, err := json.Marshal(query)
	if err != nil {
		return fmt.Errorf("marshal query: %w", err)
	}

	resp, err := c.api.Search(ctx, &osapi.SearchReq{
		Indices: []string{IndexNameWithPrefix(prefix, IndexAttachments)},
		Body:    bytes.NewReader(body),
		Params:  osapi.SearchParams{Scroll: keepAlive},
	})
	if err != nil {
		return fmt.Errorf("initial search: %w", err)
	}

	scrollID := ""
	if resp.ScrollID != nil {
		scrollID = *resp.ScrollID
	}
	defer func() {
		if scrollID != "" {
			_, _ = c.api.Scroll.Delete(context.Background(), osapi.ScrollDeleteReq{ScrollIDs: []string{scrollID}})
		}
	}()

	hits := resp.Hits.Hits
	for len(hits) > 0 {
		for _, h := range hits {
			var doc AttachmentDoc
			if err := json.Unmarshal(h.Source, &doc); err != nil {
				return fmt.Errorf("decode hit %s: %w", h.ID, err)
			}
			if err := fn(doc); err != nil {
				return err
			}
		}
		if scrollID == "" {
			break
		}
		nextResp, err := c.api.Scroll.Get(ctx, osapi.ScrollGetReq{
			ScrollID: scrollID,
			Params:   osapi.ScrollGetParams{Scroll: keepAlive},
		})
		if err != nil {
			return fmt.Errorf("scroll get: %w", err)
		}
		hits = nextResp.Hits.Hits
		if nextResp.ScrollID != nil {
			scrollID = *nextResp.ScrollID
		}
	}
	return nil
}
