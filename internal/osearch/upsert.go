package osearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// UpsertSummary writes or replaces a summary doc by content SHA. The
// doc ID is profile/vault/sha, so re-perceiving identical bytes is a
// no-op replacement. WaitForRefresh forces the doc to be searchable
// immediately on return — set in tests; in production, the 1s
// refresh_interval is fine.
func (c *Client) UpsertSummary(ctx context.Context, doc SummaryDoc, waitForRefresh bool) error {
	return c.UpsertSummaryWithPrefix(ctx, c.prefix, doc, waitForRefresh)
}

// UpsertSummaryWithPrefix writes the doc against the index resolved
// from the supplied per-call prefix. Used by daemon handlers once
// they hold the binding's resolved storage handle.
func (c *Client) UpsertSummaryWithPrefix(ctx context.Context, prefix string, doc SummaryDoc, waitForRefresh bool) error {
	if doc.Profile == "" || doc.Vault == "" || doc.SHA == "" {
		return errors.New("osearch: summary doc requires profile, vault, sha")
	}
	return c.putDoc(ctx, prefix, IndexSummaries, DocID(doc.Profile, doc.Vault, doc.SHA), doc, waitForRefresh)
}

// UpsertEntity writes or replaces an entity doc by canonical slug.
// Entities accumulate mentions across sources; the caller (synth
// queue) is responsible for merging MentionedBy[] before calling.
// For atomic append-without-read-modify-write, use UpdateEntityMentions.
func (c *Client) UpsertEntity(ctx context.Context, doc EntityDoc, waitForRefresh bool) error {
	return c.UpsertEntityWithPrefix(ctx, c.prefix, doc, waitForRefresh)
}

// UpsertEntityWithPrefix writes the entity doc against the index
// resolved from the supplied per-call prefix.
func (c *Client) UpsertEntityWithPrefix(ctx context.Context, prefix string, doc EntityDoc, waitForRefresh bool) error {
	if doc.Profile == "" || doc.Vault == "" || doc.Slug == "" {
		return errors.New("osearch: entity doc requires profile, vault, slug")
	}
	return c.putDoc(ctx, prefix, IndexEntities, DocID(doc.Profile, doc.Vault, doc.Slug), doc, waitForRefresh)
}

// UpsertAttachment writes or replaces an attachment metadata doc.
// The binary itself lives in MinIO at doc.MinIOKey; this index holds
// only the searchable metadata + extracted text.
func (c *Client) UpsertAttachment(ctx context.Context, doc AttachmentDoc, waitForRefresh bool) error {
	return c.UpsertAttachmentWithPrefix(ctx, c.prefix, doc, waitForRefresh)
}

// UpsertAttachmentWithPrefix writes the attachment metadata doc
// against the index resolved from the supplied per-call prefix.
func (c *Client) UpsertAttachmentWithPrefix(ctx context.Context, prefix string, doc AttachmentDoc, waitForRefresh bool) error {
	if doc.Profile == "" || doc.Vault == "" || doc.SHA == "" {
		return errors.New("osearch: attachment doc requires profile, vault, sha")
	}
	return c.putDoc(ctx, prefix, IndexAttachments, DocID(doc.Profile, doc.Vault, doc.SHA), doc, waitForRefresh)
}

func (c *Client) putDoc(ctx context.Context, prefix, logical, id string, payload any, waitForRefresh bool) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req := osapi.IndexReq{
		Index:      IndexNameWithPrefix(prefix, logical),
		DocumentID: id,
		Body:       bytes.NewReader(body),
	}
	if waitForRefresh {
		req.Params.Refresh = "true"
	}

	resp, err := c.api.Index(ctx, req)
	if err != nil {
		return fmt.Errorf("index %s/%s: %w", logical, id, err)
	}
	if resp != nil {
		ir := resp.Inspect().Response
		if ir != nil && ir.StatusCode >= 400 {
			b, _ := io.ReadAll(ir.Body)
			return fmt.Errorf("index %s/%s: status %d: %s", logical, id, ir.StatusCode, string(b))
		}
	}
	return nil
}

// GetSummary fetches a summary doc by its full ID (profile/vault/sha).
// Returns (nil, nil) when the doc does not exist.
func (c *Client) GetSummary(ctx context.Context, profile, vault, sha string) (*SummaryDoc, error) {
	return c.GetSummaryWithPrefix(ctx, c.prefix, profile, vault, sha)
}

// GetSummaryWithPrefix fetches against the index resolved from the
// supplied per-call prefix. Returns (nil, nil) when the doc is absent.
func (c *Client) GetSummaryWithPrefix(ctx context.Context, prefix, profile, vault, sha string) (*SummaryDoc, error) {
	var doc SummaryDoc
	found, err := c.getDoc(ctx, prefix, IndexSummaries, DocID(profile, vault, sha), &doc)
	if err != nil || !found {
		return nil, err
	}
	return &doc, nil
}

// GetEntity fetches an entity doc by (profile, vault, slug).
func (c *Client) GetEntity(ctx context.Context, profile, vault, slug string) (*EntityDoc, error) {
	return c.GetEntityWithPrefix(ctx, c.prefix, profile, vault, slug)
}

// GetEntityWithPrefix fetches against the index resolved from the
// supplied per-call prefix.
func (c *Client) GetEntityWithPrefix(ctx context.Context, prefix, profile, vault, slug string) (*EntityDoc, error) {
	var doc EntityDoc
	found, err := c.getDoc(ctx, prefix, IndexEntities, DocID(profile, vault, slug), &doc)
	if err != nil || !found {
		return nil, err
	}
	return &doc, nil
}

// GetAttachment fetches an attachment metadata doc by (profile, vault, sha).
func (c *Client) GetAttachment(ctx context.Context, profile, vault, sha string) (*AttachmentDoc, error) {
	return c.GetAttachmentWithPrefix(ctx, c.prefix, profile, vault, sha)
}

// GetAttachmentWithPrefix fetches against the index resolved from
// the supplied per-call prefix.
func (c *Client) GetAttachmentWithPrefix(ctx context.Context, prefix, profile, vault, sha string) (*AttachmentDoc, error) {
	var doc AttachmentDoc
	found, err := c.getDoc(ctx, prefix, IndexAttachments, DocID(profile, vault, sha), &doc)
	if err != nil || !found {
		return nil, err
	}
	return &doc, nil
}

func (c *Client) getDoc(ctx context.Context, prefix, logical, id string, out any) (bool, error) {
	resp, err := c.api.Document.Get(ctx, osapi.DocumentGetReq{
		Index:      IndexNameWithPrefix(prefix, logical),
		DocumentID: id,
	})
	if err != nil {
		if statusFromErr(err) == http.StatusNotFound {
			return false, nil
		}
		if resp != nil && resp.Inspect().Response != nil &&
			resp.Inspect().Response.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("get %s/%s: %w", logical, id, err)
	}
	if resp == nil || !resp.Found {
		return false, nil
	}
	if err := json.Unmarshal(resp.Source, out); err != nil {
		return false, fmt.Errorf("decode source: %w", err)
	}
	return true, nil
}

// DeleteSummary removes a summary doc by (profile, vault, sha). Used
// by tests and `brain_reflect` cleanup. Missing-doc returns nil.
func (c *Client) DeleteSummary(ctx context.Context, profile, vault, sha string) error {
	return c.DeleteSummaryWithPrefix(ctx, c.prefix, profile, vault, sha)
}

// DeleteSummaryWithPrefix deletes against the index resolved from
// the supplied per-call prefix.
func (c *Client) DeleteSummaryWithPrefix(ctx context.Context, prefix, profile, vault, sha string) error {
	return c.deleteDoc(ctx, prefix, IndexSummaries, DocID(profile, vault, sha))
}

// DeleteEntity removes an entity doc.
func (c *Client) DeleteEntity(ctx context.Context, profile, vault, slug string) error {
	return c.DeleteEntityWithPrefix(ctx, c.prefix, profile, vault, slug)
}

// DeleteEntityWithPrefix deletes against the index resolved from
// the supplied per-call prefix.
func (c *Client) DeleteEntityWithPrefix(ctx context.Context, prefix, profile, vault, slug string) error {
	return c.deleteDoc(ctx, prefix, IndexEntities, DocID(profile, vault, slug))
}

// DeleteAttachment removes an attachment metadata doc. The MinIO
// blob is NOT deleted (attachments are immutable by design).
func (c *Client) DeleteAttachment(ctx context.Context, profile, vault, sha string) error {
	return c.DeleteAttachmentWithPrefix(ctx, c.prefix, profile, vault, sha)
}

// DeleteAttachmentWithPrefix deletes against the index resolved from
// the supplied per-call prefix.
func (c *Client) DeleteAttachmentWithPrefix(ctx context.Context, prefix, profile, vault, sha string) error {
	return c.deleteDoc(ctx, prefix, IndexAttachments, DocID(profile, vault, sha))
}

func (c *Client) deleteDoc(ctx context.Context, prefix, logical, id string) error {
	resp, err := c.api.Document.Delete(ctx, osapi.DocumentDeleteReq{
		Index:      IndexNameWithPrefix(prefix, logical),
		DocumentID: id,
	})
	if err != nil {
		if statusFromErr(err) == http.StatusNotFound {
			return nil
		}
		if resp != nil && resp.Inspect().Response != nil &&
			resp.Inspect().Response.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("delete %s/%s: %w", logical, id, err)
	}
	return nil
}

// Refresh forces an immediate refresh of all phantom-brain indices,
// making recent writes searchable. Test-only — production relies on
// the 1s refresh_interval.
func (c *Client) Refresh(ctx context.Context) error {
	return c.RefreshWithPrefix(ctx, c.prefix)
}

// RefreshWithPrefix forces an immediate refresh of the index set
// resolved from the supplied per-call prefix.
func (c *Client) RefreshWithPrefix(ctx context.Context, prefix string) error {
	for _, logical := range []string{IndexSummaries, IndexEntities, IndexAttachments} {
		_, err := c.api.Indices.Refresh(ctx, &osapi.IndicesRefreshReq{
			Indices: []string{IndexNameWithPrefix(prefix, logical)},
		})
		if err != nil {
			return fmt.Errorf("refresh %s: %w", logical, err)
		}
	}
	return nil
}
