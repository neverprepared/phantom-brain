package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is the agent's HTTP client against the Phase 2 daemon. Holds
// the API URL + bearer token + an underlying *http.Client. Methods
// honour ctx for cancellation and propagate daemon-side error
// envelopes as typed errors so the agent's snapcache + shipqueue
// paths can branch on them.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// ClientOpts narrows what NewClient needs. HTTPClient and Timeout
// are both optional. If HTTPClient is set it wins (caller controls
// timeouts/transport entirely); otherwise NewClient builds an
// http.Client with the supplied Timeout, falling back to 30s when
// Timeout is zero — fine for typical perceive/learn POSTs.
//
// Callers that POST large attachments (multi-MB base64 payloads to
// /api/brain/attach) should bump Timeout to several minutes; the
// 30s default frequently expires on constrained uplinks.
type ClientOpts struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	Timeout    time.Duration
}

// NewClient validates the inputs and returns a ready Client. Returns
// an error rather than panicking so callers can degrade to greenfield
// when the deploy contract is incomplete.
func NewClient(opts ClientOpts) (*Client, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("brain: NewClient requires BaseURL")
	}
	if strings.TrimSpace(opts.Token) == "" {
		return nil, errors.New("brain: NewClient requires Token")
	}
	hc := opts.HTTPClient
	if hc == nil {
		t := opts.Timeout
		if t <= 0 {
			t = 30 * time.Second
		}
		hc = &http.Client{Timeout: t}
	}
	return &Client{
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		token:   opts.Token,
		http:    hc,
	}, nil
}

// MergeInitResponse mirrors the daemon's mergeInitResponse JSON.
type MergeInitResponse struct {
	UploadID string `json:"upload_id"`
	URL      string `json:"url"`
	Token    string `json:"token"`
	Expires  int64  `json:"expires"`
}

// InitMerge calls /api/brain/merge/init and returns the upload
// handle the agent should PUT the death tarball to.
func (c *Client) InitMerge(ctx context.Context, brainID string, payloadSize int64, ttlSecs int) (*MergeInitResponse, error) {
	body := map[string]any{
		"brain_id":     brainID,
		"payload_size": payloadSize,
		"ttl_secs":     ttlSecs,
	}
	var out MergeInitResponse
	if err := c.do(ctx, http.MethodPost, "/api/brain/merge/init", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UploadTarball PUTs the tarball bytes to the URL returned by
// InitMerge. The URL already carries the auth token so we don't
// add a bearer header. Returns the number of bytes received per the
// daemon's response.
func (c *Client) UploadTarball(ctx context.Context, uploadURL string, body io.Reader, size int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, body)
	if err != nil {
		return 0, err
	}
	if size > 0 {
		req.ContentLength = size
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("brain: upload tarball: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, decodeErrorEnvelope(resp)
	}
	var ack struct {
		ReceivedBytes int64 `json:"received_bytes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ack)
	return ack.ReceivedBytes, nil
}

// CompleteMerge finalises an upload — daemon moves it into
// brains/_pending/<brain_id>.tar for the reaper to pick up.
func (c *Client) CompleteMerge(ctx context.Context, uploadID, brainID string) error {
	body := map[string]any{"brain_id": brainID}
	return c.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/brain/merge/complete/%s", uploadID), body, nil)
}

// MergeStatus is the JSON returned by GET /api/brain/merge/{brain_id}.
type MergeStatus struct {
	BrainID         string `json:"brain_id"`
	State           string `json:"state"` // "pending" | "merged"
	MergedAt        string `json:"merged_at,omitempty"`
	RawCount        int    `json:"raw_count,omitempty"`
	AttachmentCount int    `json:"attachment_count,omitempty"`
}

// GetMergeStatus polls /api/brain/merge/{brain_id}. Used by tests +
// future ops tooling; the brain itself doesn't poll (it dies after
// upload and a new brain births fresh on its host).
func (c *Client) GetMergeStatus(ctx context.Context, brainID string) (*MergeStatus, error) {
	var out MergeStatus
	if err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/api/brain/merge/%s", brainID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Health hits /api/brain/health. Unauthenticated on the daemon side
// but we still pass the bearer for consistency. Used by smoke tests
// + ops tooling.
// --- Phase 6 write endpoints --------------------------------------

// MemoryFields are the v2.4 classification fields shared across the
// three write request shapes. Each caller fills what it knows; the
// daemon validates Kind against the closed enum and rejects unknowns.
// MemoryType is optional (empty = "undecided / not applicable").
type MemoryFields struct {
	Kind       string    `json:"kind,omitempty"`        // closed enum — see osearch.Kind
	MemoryType string    `json:"memory_type,omitempty"` // semantic | episodic | procedural | ""
	Source     []string  `json:"source,omitempty"`      // provenance: URLs, task IDs, agent IDs, file paths
	References []string  `json:"references,omitempty"`  // SHAs of related summaries (graph hook)
	CapturedAt *time.Time `json:"captured_at,omitempty"` // when the content was authored, not when OS got it; nil = unset
}

// PerceiveRequest mirrors internal/server.PerceiveRequest. Defined
// independently so internal/brain doesn't pull a daemon-side import.
type PerceiveRequest struct {
	SHA        string    `json:"sha"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	URL        string    `json:"url,omitempty"`
	SourcePath string    `json:"source_path,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	Embedding  []float32 `json:"embedding,omitempty"`
	MemoryFields
}

// LearnRequest mirrors internal/server.LearnRequest.
type LearnRequest struct {
	SHA        string    `json:"sha"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	SourcePath string    `json:"source_path,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	Embedding  []float32 `json:"embedding,omitempty"`
	MemoryFields
}

// AttachRequest mirrors internal/server.AttachRequest. The attachment
// blob's metadata carries its own memory fields so the OS doc the
// daemon stores can be filtered alongside summaries.
type AttachRequest struct {
	SHA              string    `json:"sha"`
	OriginalFilename string    `json:"original_filename"`
	Title            string    `json:"title,omitempty"`
	MIMEType         string    `json:"mime_type,omitempty"`
	BytesB64         string    `json:"bytes_b64"`
	Description      string    `json:"description,omitempty"`
	ExtractedText    string    `json:"extracted_text,omitempty"`
	Tags             []string  `json:"tags,omitempty"`
	Embedding        []float32 `json:"embedding,omitempty"`
	MemoryFields
}

// TraceRequest mirrors internal/server.TraceRequest.
type TraceRequest struct {
	Kind    string         `json:"kind"`
	Message string         `json:"message"`
	Meta    map[string]any `json:"meta,omitempty"`
}

// WriteResponse mirrors internal/server.WriteResponse.
type WriteResponse struct {
	SHA           string `json:"sha"`
	IndexedAt     int64  `json:"indexed_at"`
	SynthEnqueued bool   `json:"synth_enqueued"`
}

// Perceive POSTs a gathered-content doc to the daemon. Returns the
// daemon's accept envelope on 202; surfaces typed *APIError on 4xx/5xx.
func (c *Client) Perceive(ctx context.Context, req PerceiveRequest) (*WriteResponse, error) {
	var out WriteResponse
	if err := c.do(ctx, http.MethodPost, "/api/brain/perceive", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Learn POSTs a curated-content doc to the daemon.
func (c *Client) Learn(ctx context.Context, req LearnRequest) (*WriteResponse, error) {
	var out WriteResponse
	if err := c.do(ctx, http.MethodPost, "/api/brain/learn", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Attach POSTs a binary attachment + its extracted text + metadata.
// The bytes_b64 field carries the raw blob; daemon decodes, validates
// SHA, stores to MinIO, indexes metadata in OS.
func (c *Client) Attach(ctx context.Context, req AttachRequest) (*WriteResponse, error) {
	var out WriteResponse
	if err := c.do(ctx, http.MethodPost, "/api/brain/attach", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Trace appends an audit-log line to the daemon's vault log. No
// content indexing; no synth.
func (c *Client) Trace(ctx context.Context, req TraceRequest) error {
	return c.do(ctx, http.MethodPost, "/api/brain/trace", req, nil)
}

// RecallRequest is the body of POST /api/brain/recall (Phase C online
// recall). Only Query is required; Embedding + facet filters are
// optional. The (profile, vault) scope is derived daemon-side from the
// bearer token, so it is NOT sent. Mirrors server.RecallRequest.
type RecallRequest struct {
	Query       string    `json:"query"`
	Embedding   []float32 `json:"embedding,omitempty"`
	Limit       int       `json:"limit,omitempty"`
	Kinds       []string  `json:"kinds,omitempty"`
	Topic       string    `json:"topic,omitempty"`
	MemoryType  string    `json:"memory_type,omitempty"`
	Reliability []string  `json:"reliability,omitempty"`
}

// RecallHitDTO is one ranked recall result on the wire. Mirrors
// server.RecallHitDTO field-for-field (identical JSON tags).
type RecallHitDTO struct {
	SHA              string  `json:"sha"`
	Title            string  `json:"title"`
	Kind             string  `json:"kind"`
	MemoryType       string  `json:"memory_type,omitempty"`
	Topic            string  `json:"topic,omitempty"`
	Reliability      string  `json:"reliability,omitempty"`
	SourceURL        string  `json:"source_url,omitempty"`
	MimeType         string  `json:"mime_type,omitempty"`
	OriginalFilename string  `json:"original_filename,omitempty"`
	Snippet          string  `json:"snippet,omitempty"`
	Score            float64 `json:"score"`
}

// RecallResponse is the 200 body of POST /api/brain/recall.
type RecallResponse struct {
	Hits []RecallHitDTO `json:"hits"`
}

// Recall POSTs an online-recall query to the daemon. Transport failures
// wrap ErrDaemonUnreachable; non-2xx (e.g. 503 when online recall isn't
// enabled for the binding) returns the decoded *APIError. Callers detect
// either and fall back to the local snapshot.
func (c *Client) Recall(ctx context.Context, req RecallRequest) (*RecallResponse, error) {
	var out RecallResponse
	if err := c.do(ctx, http.MethodPost, "/api/brain/recall", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FetchResponse is the 200 body of GET /api/brain/fetch/{sha}. Mirrors
// server.FetchDTO field-for-field (identical JSON tags). Body is the full
// (untruncated) stored document — the read companion to Recall's snippet.
type FetchResponse struct {
	SHA        string   `json:"sha"`
	Title      string   `json:"title"`
	Kind       string   `json:"kind"`
	SourcePath string   `json:"source_path,omitempty"`
	SourceURL  string   `json:"source_url,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Body       string   `json:"body"`
}

// Fetch GETs the full body of one record by SHA from the daemon. Transport
// failures wrap ErrDaemonUnreachable; non-2xx (e.g. 404 unknown SHA, 503
// when online fetch isn't enabled for the binding) returns the decoded
// *APIError. brain_fetch is online-only and surfaces either as a tool error.
func (c *Client) Fetch(ctx context.Context, sha string) (*FetchResponse, error) {
	var out FetchResponse
	if err := c.do(ctx, http.MethodGet, "/api/brain/fetch/"+sha, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AttachGet retrieves a presigned URL the agent can fetch to pull
// the binary. Returns *APIError 404 when the SHA isn't in this
// vault's attachments index.
type AttachGetResponse struct {
	SHA       string `json:"sha"`
	Original  string `json:"original"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	URL       string `json:"url"`
	ExpiresIn int    `json:"expires_in"`
}

func (c *Client) AttachGet(ctx context.Context, sha string) (*AttachGetResponse, error) {
	var out AttachGetResponse
	if err := c.do(ctx, http.MethodGet, "/api/brain/attach/"+sha, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CaptureGetResponse mirrors internal/server's handleCaptureGet
// envelope: presigned URL to the raw bytes captured at synth time.
// 404 surfaces as *APIError with Code=NOT_FOUND when no capture
// exists for the summary.
type CaptureGetResponse struct {
	SHA       string `json:"sha"`
	SourceURL string `json:"source_url"`
	SizeBytes int64  `json:"size_bytes"`
	URL       string `json:"url"`
	ExpiresIn int    `json:"expires_in"`
}

func (c *Client) CaptureGet(ctx context.Context, sha string) (*CaptureGetResponse, error) {
	var out CaptureGetResponse
	if err := c.do(ctx, http.MethodGet, "/api/brain/capture/"+sha, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/api/brain/health", nil, nil)
}

// --- v3.3 brain_reflect maintenance cycle (issue #72 Phase 1) ------

// ReflectCandidate mirrors internal/server.ReflectCandidate — one
// forget-candidate surfaced by the daemon's read-only reflect scan.
type ReflectCandidate struct {
	SHA    string `json:"sha"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// ReflectResponse mirrors internal/server.ReflectResponse.
type ReflectResponse struct {
	Candidates []ReflectCandidate `json:"candidates"`
}

// ForgetResponse mirrors internal/server.ForgetResponse.
type ForgetResponse struct {
	SHA       string `json:"sha"`
	Forgotten bool   `json:"forgotten"`
}

// Reflect GETs the daemon's read-only forget-candidate report. The
// agent surfaces the result and the operator decides what to Forget;
// nothing is deleted here (propose-then-apply).
func (c *Client) Reflect(ctx context.Context) (*ReflectResponse, error) {
	var out ReflectResponse
	if err := c.do(ctx, http.MethodGet, "/api/brain/reflect", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Forget POSTs a single-SHA forget. The daemon deletes the summary doc
// and triggers a snapshot rebuild so the removal propagates. This is a
// delete, NOT an ingest — it does not go through the write-ahead queue.
func (c *Client) Forget(ctx context.Context, sha string) (*ForgetResponse, error) {
	var out ForgetResponse
	if err := c.do(ctx, http.MethodPost, "/api/brain/forget", map[string]string{"sha": sha}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- v3.4 re-synthesis backfill (issue #82) ----------------------------

// ResynthSampleItem mirrors internal/server.ResynthSampleItem — one
// preview row in a backlog report.
type ResynthSampleItem struct {
	SHA   string `json:"sha"`
	Title string `json:"title"`
}

// ResynthResponse mirrors internal/server.ResynthResult — the report from
// a resynth scan (and, on apply, the background work that was kicked off).
type ResynthResponse struct {
	BacklogCount int                 `json:"backlog_count"`
	Sample       []ResynthSampleItem `json:"sample"`
	Started      bool                `json:"started"`
	Pending      int                 `json:"pending"`
}

// Resynth POSTs a re-synthesis request. dryRun reports the backlog of
// Synthesised=false docs without mutating; apply (dryRun=false) starts a
// background backfill that re-processes them. limit<=0 means all. The
// fix-it apply-companion to Reflect (issue #82).
func (c *Client) Resynth(ctx context.Context, dryRun bool, limit int) (*ResynthResponse, error) {
	var out ResynthResponse
	body := map[string]any{"dry_run": dryRun, "limit": limit}
	if err := c.do(ctx, http.MethodPost, "/api/brain/resynth", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// do is the shared request helper. Encodes body as JSON when
// non-nil, decodes the response into out when non-nil, and turns
// non-2xx responses into typed APIErrors carrying the daemon's
// error envelope when present.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("brain: marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("brain: %s %s: %w: %w", method, path, ErrDaemonUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeErrorEnvelope(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("brain: decode response: %w", err)
	}
	return nil
}

// APIError carries a daemon error-envelope payload as a Go error.
// Callers errors.As-extract it to branch on Code (e.g. STALE_SNAPSHOT
// → re-fetch /snapshot/current, MAINTENANCE_MODE → retry later).
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Details    map[string]any
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("brain: daemon API error %d %s: %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("brain: daemon API error %d: %s", e.StatusCode, e.Message)
}

// decodeErrorEnvelope reads the response body and returns an
// *APIError. Falls back to a generic message when the body is
// missing/malformed (a 502 from a reverse proxy, say).
func decodeErrorEnvelope(resp *http.Response) error {
	raw, _ := io.ReadAll(resp.Body)
	apiErr := &APIError{StatusCode: resp.StatusCode}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Code != "" {
		apiErr.Code = env.Error.Code
		apiErr.Message = env.Error.Message
		apiErr.Details = env.Error.Details
		return apiErr
	}
	apiErr.Message = strings.TrimSpace(string(raw))
	if apiErr.Message == "" {
		apiErr.Message = resp.Status
	}
	return apiErr
}

// IsAPIErrorCode reports whether err is an *APIError whose Code
// matches. Convenience for the agent's birth/upload retry logic.
func IsAPIErrorCode(err error, code string) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Code == code
}
