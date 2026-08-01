package osearch

import (
	"context"
	"net/http"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// EnsureIndexWithMapping is a thin exported wrapper over the package's
// proven ensureIndex create-if-missing logic. It lets callers outside
// this package (e.g. internal/osproject) stand up their own index with
// a custom mapping while reusing the same exists-probe + race-tolerant
// create path the legacy index set uses.
//
// prefix is the per-binding storage prefix (may be empty); logical is
// the bare index name (e.g. "pb_records"); mapping is the full
// {settings, mappings} body. Idempotent: an already-present index
// returns nil.
func (c *Client) EnsureIndexWithMapping(ctx context.Context, prefix, logical string, mapping map[string]any) error {
	return c.ensureIndex(ctx, prefix, logical, mapping)
}

// PutDoc is a thin exported wrapper over putDoc — idempotent
// upsert-by-_id against the index resolved from prefix+logical. payload
// is JSON-marshalled as the document body; waitForRefresh forces the
// doc searchable on return (test-only; production relies on the 1s
// refresh_interval).
func (c *Client) PutDoc(ctx context.Context, prefix, logical, id string, payload any, waitForRefresh bool) error {
	return c.putDoc(ctx, prefix, logical, id, payload, 0, waitForRefresh)
}

// PutDocVersioned is PutDoc with OpenSearch external_gte version control.
// version must be a positive, per-_id monotonically-increasing value (the
// record projection uses updated_at epoch-nanos): OpenSearch keeps the
// highest version and rejects a strictly-older write with ErrVersionConflict,
// so an out-of-order upsert cannot clobber newer content. An equal version is
// accepted (idempotent re-delivery). version <= 0 falls back to an unversioned
// upsert. Callers should treat ErrVersionConflict as success.
func (c *Client) PutDocVersioned(ctx context.Context, prefix, logical, id string, payload any, version int64, waitForRefresh bool) error {
	return c.putDoc(ctx, prefix, logical, id, payload, version, waitForRefresh)
}

// DeleteDoc removes a single document by _id from the index resolved
// from prefix+logical. A 404 (doc absent) is treated as success so the
// delete is idempotent — matching the 404-tolerance pattern in
// DeleteIndex / deleteDoc.
func (c *Client) DeleteDoc(ctx context.Context, prefix, logical, id string) error {
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
		return err
	}
	return nil
}
