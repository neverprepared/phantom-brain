package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// CaptureResult describes a successful raw-source capture: where the
// bytes landed in MinIO, what content-type the upstream advertised,
// and how many bytes were stored. The OS doc's capture_minio_key is
// populated from Key.
type CaptureResult struct {
	Key         string // <profile>/<vault>/captures/<sha>.<ext>
	ContentType string
	SizeBytes   int64
}

// CaptureURL fetches url, streams the response (bounded by maxBytes)
// into MinIO via the supplied AttachmentStore (PutAttachment is the
// same primitive used for /attach). Returns a CaptureResult on
// success.
//
// Failure modes (all return error; caller logs + leaves capture_minio_key empty):
//   - URL malformed / unreachable / non-2xx response
//   - Response exceeds maxBytes
//   - MinIO put fails
//
// All errors are non-fatal at the call site — capture is a best-effort
// archival pass, not a precondition for the gate / distill / index
// writes that follow.
func CaptureURL(ctx context.Context, store AttachmentStore, profile, vault, docSHA, url string, maxBytes int64, userAgent string, timeout time.Duration) (*CaptureResult, error) {
	if store == nil {
		return nil, errors.New("capture: no attachment store (local backend doesn't support capture; need MinIO)")
	}
	if url == "" || docSHA == "" {
		return nil, errors.New("capture: url and doc sha required")
	}
	if maxBytes <= 0 {
		maxBytes = 10 * 1024 * 1024 // 10 MB default
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if userAgent == "" {
		userAgent = "phantom-brain/2 (+https://github.com/neverprepared/mcp-phantom-brain)"
	}

	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("capture: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html, application/xhtml+xml, application/json, text/*;q=0.9, */*;q=0.5")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("capture: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("capture: %s returned status %d", url, resp.StatusCode)
	}

	// Bound the read at maxBytes+1 so we can detect overflow. If the
	// upstream sends more, return an error rather than silently
	// truncating — operator visibility matters.
	limited := io.LimitReader(resp.Body, maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("capture: read body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("capture: %s exceeded %d-byte cap", url, maxBytes)
	}

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	ext := extFromContentType(contentType)

	key, err := store.PutAttachment(ctx, profile, vault, captureKeyPrefix+docSHA, ext, body, contentType)
	if err != nil {
		return nil, fmt.Errorf("capture: minio put: %w", err)
	}
	return &CaptureResult{
		Key:         key,
		ContentType: contentType,
		SizeBytes:   int64(len(body)),
	}, nil
}

// captureKeyPrefix routes capture blobs into a sibling namespace from
// attachments. The MinIO key shape is:
//
//	<profile>/<vault>/attachments/captures-<docSHA><ext>
//
// AttachmentStore's PutAttachment composes the key from
// (profile, vault, sha, ext) so we encode "captures" into the sha
// segment. Operationally indistinguishable from a real attachment
// blob; tooling can filter by the "captures-" prefix.
const captureKeyPrefix = "captures-"

// extFromContentType picks a file extension from an HTTP Content-Type.
// Conservative — only common types get a specific extension; everything
// else falls through to ".bin" so the key is always extensioned.
func extFromContentType(ct string) string {
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	switch ct {
	case "text/html", "application/xhtml+xml":
		return ".html"
	case "application/json":
		return ".json"
	case "application/xml", "text/xml":
		return ".xml"
	case "text/plain":
		return ".txt"
	case "text/markdown":
		return ".md"
	case "application/pdf":
		return ".pdf"
	}
	return ".bin"
}
