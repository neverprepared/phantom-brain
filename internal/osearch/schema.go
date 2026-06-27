package osearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// Phase D2b: the legacy create-if-missing entry points (EnsureIndices /
// EnsureIndicesWithPrefix / EnsurePrefixes) and the pb_summaries /
// pb_entities / pb_attachments mappings were removed — those indices are
// no longer written. The lower-level ensureIndex create-if-missing
// helper remains: it is the engine behind generic.go's
// EnsureIndexWithMapping, which the pb_records projection
// (internal/osproject) uses to ensure its index at startup.

// ensureIndex is the idempotent create-if-missing primitive. It resolves
// the physical index name from prefix + logical, probes for existence,
// and creates it with the supplied mapping when absent. Tolerates the
// concurrent-create race (two daemons probing then creating).
func (c *Client) ensureIndex(ctx context.Context, prefix, logical string, mapping map[string]any) error {
	name := IndexNameWithPrefix(prefix, logical)

	existsReq := osapi.IndicesExistsReq{Indices: []string{name}}
	resp, err := c.api.Indices.Exists(ctx, existsReq)
	// opensearch-go v4 surfaces non-2xx as a typed error. The raw
	// response (or the parsed error message) carries the status: 200
	// → exists; 404 → fall through to create.
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if status == 0 {
		status = statusFromErr(err)
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		// fall through to create
	default:
		if err != nil {
			return fmt.Errorf("exists probe: %w", err)
		}
		return fmt.Errorf("exists probe: unexpected status %d", status)
	}

	body, err := json.Marshal(mapping)
	if err != nil {
		return fmt.Errorf("marshal mapping: %w", err)
	}

	createReq := osapi.IndicesCreateReq{
		Index: name,
		Body:  bytes.NewReader(body),
	}
	createResp, err := c.api.Indices.Create(ctx, createReq)
	if err != nil {
		// Tolerate the race where another daemon instance created the
		// index between our exists probe and our create call.
		if isAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create: %w", err)
	}
	if createResp != nil && createResp.Inspect().Response != nil &&
		createResp.Inspect().Response.StatusCode >= 400 {
		body, _ := io.ReadAll(createResp.Inspect().Response.Body)
		return fmt.Errorf("create returned %d: %s",
			createResp.Inspect().Response.StatusCode, string(body))
	}
	return nil
}

// DeleteIndex drops the physical index resolved from this client's
// prefix + the supplied logical name. Used by the operator CLI
// (`pbrainctl server binding delete --purge-data`) to tear down a
// binding's prefixed indices. Treats 404 as success so re-runs are
// idempotent.
func (c *Client) DeleteIndex(ctx context.Context, logical string) error {
	name := c.IndexName(logical)
	resp, err := c.api.Indices.Delete(ctx, osapi.IndicesDeleteReq{Indices: []string{name}})
	if err != nil {
		if statusFromErr(err) == http.StatusNotFound {
			return nil
		}
		if resp != nil && resp.Inspect().Response != nil &&
			resp.Inspect().Response.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("delete %s: %w", name, err)
	}
	return nil
}

func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "resource_already_exists_exception")
}

// statusFromErr parses the HTTP status code out of an opensearch-go
// typed error message, which formats as "status: [NNN ...]". Returns
// 0 when the message doesn't contain a status.
func statusFromErr(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	idx := strings.Index(msg, "status: [")
	if idx == -1 {
		return 0
	}
	tail := msg[idx+len("status: ["):]
	var n int
	for _, ch := range tail {
		if ch < '0' || ch > '9' {
			break
		}
		n = n*10 + int(ch-'0')
	}
	return n
}
