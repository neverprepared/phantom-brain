package osearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// scrollIndex paginates the index resolved from (prefix, logical), scoped to
// (profile, vault), decoding each hit into a fresh T and invoking fn. Returning
// an error from fn aborts the scroll. batchSize <= 0 falls back to 500. label
// names the public entry point for error wrapping (e.g. "osearch.ScrollSummaries").
//
// This holds the entire query/marshal/search/scroll-loop body shared by every
// Scroll*WithPrefix method — the single place to fix scroll behavior.
func scrollIndex[T any](ctx context.Context, c *Client, prefix, logical, profile, vault, label string, batchSize int, fn func(T) error) error {
	if profile == "" || vault == "" {
		return fmt.Errorf("%s: profile and vault required", label)
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
		Indices: []string{IndexNameWithPrefix(prefix, logical)},
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
			var doc T
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

// ScrollAttachments paginates pb_attachments scoped to (profile, vault),
// decoding each hit into an AttachmentDoc and invoking fn. Returning
// an error from fn aborts the scroll. batchSize <= 0 falls back to 500.
//
// Used by the backfill subcommand to walk every existing attachment
// metadata doc; kept exported so future operator tooling that needs
// to iterate attachments doesn't have to re-implement the scroll
// boilerplate already burned into scrollIndex.
func (c *Client) ScrollAttachments(ctx context.Context, profile, vault string, batchSize int, fn func(AttachmentDoc) error) error {
	return c.ScrollAttachmentsWithPrefix(ctx, c.prefix, profile, vault, batchSize, fn)
}

// ScrollAttachmentsWithPrefix paginates pb_attachments at the index
// resolved from the supplied per-call prefix.
func (c *Client) ScrollAttachmentsWithPrefix(ctx context.Context, prefix, profile, vault string, batchSize int, fn func(AttachmentDoc) error) error {
	return scrollIndex(ctx, c, prefix, IndexAttachments, profile, vault, "osearch.ScrollAttachments", batchSize, fn)
}

// ScrollSummaries paginates pb_summaries scoped to (profile, vault),
// decoding each hit into a SummaryDoc and invoking fn. Returning an
// error from fn aborts the scroll. batchSize <= 0 falls back to 500.
//
// Added for v3.3 brain_reflect (issue #72 Phase 1): the maintenance
// detector walks every summary doc to find forget-candidates.
func (c *Client) ScrollSummaries(ctx context.Context, profile, vault string, batchSize int, fn func(SummaryDoc) error) error {
	return c.ScrollSummariesWithPrefix(ctx, c.prefix, profile, vault, batchSize, fn)
}

// ScrollSummariesWithPrefix paginates pb_summaries at the index
// resolved from the supplied per-call prefix.
func (c *Client) ScrollSummariesWithPrefix(ctx context.Context, prefix, profile, vault string, batchSize int, fn func(SummaryDoc) error) error {
	return scrollIndex(ctx, c, prefix, IndexSummaries, profile, vault, "osearch.ScrollSummaries", batchSize, fn)
}

// ScrollEntities paginates pb_entities scoped to (profile, vault),
// decoding each hit into an EntityDoc and invoking fn. Returning an
// error from fn aborts the scroll. batchSize <= 0 falls back to 500.
//
// Added for the Phase B2 backfill (daemon-cutover): the entity-graph
// reconstruction walks every legacy entity doc to invert MentionedBy[]
// into record_entities links.
func (c *Client) ScrollEntities(ctx context.Context, profile, vault string, batchSize int, fn func(EntityDoc) error) error {
	return c.ScrollEntitiesWithPrefix(ctx, c.prefix, profile, vault, batchSize, fn)
}

// ScrollEntitiesWithPrefix paginates pb_entities at the index resolved
// from the supplied per-call prefix.
func (c *Client) ScrollEntitiesWithPrefix(ctx context.Context, prefix, profile, vault string, batchSize int, fn func(EntityDoc) error) error {
	return scrollIndex(ctx, c, prefix, IndexEntities, profile, vault, "osearch.ScrollEntities", batchSize, fn)
}
