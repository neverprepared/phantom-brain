package brain

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newJSONServer returns a test server whose single handler asserts the
// method/path/auth then writes the supplied JSON body. The asserted
// path lets each test confirm the client builds the right URL.
func newJSONServer(t *testing.T, wantMethod, wantPath string, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != wantMethod {
			t.Errorf("method = %q, want %q", r.Method, wantMethod)
		}
		if r.URL.Path != wantPath {
			t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth = %q, want Bearer tok", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, body)
	}))
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := NewClient(ClientOpts{BaseURL: baseURL, Token: "tok"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestClient_Health_OK(t *testing.T) {
	ts := newJSONServer(t, http.MethodGet, "/api/brain/health", 0, `{"status":"ok"}`)
	defer ts.Close()
	if err := newTestClient(t, ts.URL).Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestClient_Health_NonOKSurfacesAPIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"MAINTENANCE_MODE","message":"down"}}`)
	}))
	defer ts.Close()
	err := newTestClient(t, ts.URL).Health(context.Background())
	if !IsAPIErrorCode(err, "MAINTENANCE_MODE") {
		t.Fatalf("want MAINTENANCE_MODE APIError, got %v", err)
	}
}

func TestClient_Trace_PostsRequest(t *testing.T) {
	var gotBody TraceRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/brain/trace" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	req := TraceRequest{Kind: "audit", Message: "hello", Meta: map[string]any{"k": "v"}}
	if err := newTestClient(t, ts.URL).Trace(context.Background(), req); err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if gotBody.Kind != "audit" || gotBody.Message != "hello" {
		t.Errorf("daemon saw %+v", gotBody)
	}
	if gotBody.Meta["k"] != "v" {
		t.Errorf("meta not propagated: %+v", gotBody.Meta)
	}
}

func TestClient_Recall_DecodesHits(t *testing.T) {
	resp := RecallResponse{Hits: []RecallHitDTO{
		{SHA: "a1", Title: "First", Kind: "note", Score: 0.9, Snippet: "snip"},
		{SHA: "b2", Title: "Second", Kind: "attachment_stub", MimeType: "application/pdf", Score: 0.5},
	}}
	raw, _ := json.Marshal(resp)
	var gotReq RecallRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/brain/recall" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_, _ = w.Write(raw)
	}))
	defer ts.Close()
	out, err := newTestClient(t, ts.URL).Recall(context.Background(), RecallRequest{
		Query: "what is memory", Limit: 5, Topic: "memory", Reliability: []string{"high"},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if gotReq.Query != "what is memory" || gotReq.Limit != 5 || gotReq.Topic != "memory" {
		t.Errorf("daemon saw request %+v", gotReq)
	}
	if len(out.Hits) != 2 || out.Hits[0].SHA != "a1" || out.Hits[1].MimeType != "application/pdf" {
		t.Errorf("decoded hits wrong: %+v", out.Hits)
	}
}

func TestClient_Recall_503FallsBackViaAPIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"ONLINE_RECALL_DISABLED","message":"off"}}`)
	}))
	defer ts.Close()
	_, err := newTestClient(t, ts.URL).Recall(context.Background(), RecallRequest{Query: "x"})
	if !IsAPIErrorCode(err, "ONLINE_RECALL_DISABLED") {
		t.Fatalf("want ONLINE_RECALL_DISABLED, got %v", err)
	}
}

func TestClient_AttachGet_DecodesPresignedURL(t *testing.T) {
	body := `{"sha":"deadbeef","original":"f.pdf","mime_type":"application/pdf","size_bytes":1234,"url":"https://minio/x","expires_in":600}`
	ts := newJSONServer(t, http.MethodGet, "/api/brain/attach/deadbeef", 0, body)
	defer ts.Close()
	out, err := newTestClient(t, ts.URL).AttachGet(context.Background(), "deadbeef")
	if err != nil {
		t.Fatalf("AttachGet: %v", err)
	}
	if out.Original != "f.pdf" || out.SizeBytes != 1234 || out.URL != "https://minio/x" || out.ExpiresIn != 600 {
		t.Errorf("decoded %+v", out)
	}
}

func TestClient_AttachGet_NotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"NOT_FOUND","message":"no attachment"}}`)
	}))
	defer ts.Close()
	_, err := newTestClient(t, ts.URL).AttachGet(context.Background(), "missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 APIError, got %v", err)
	}
}

func TestClient_CaptureGet_DecodesEnvelope(t *testing.T) {
	body := `{"sha":"c0ffee","source_url":"https://example.com","size_bytes":42,"url":"https://minio/cap","expires_in":300}`
	ts := newJSONServer(t, http.MethodGet, "/api/brain/capture/c0ffee", 0, body)
	defer ts.Close()
	out, err := newTestClient(t, ts.URL).CaptureGet(context.Background(), "c0ffee")
	if err != nil {
		t.Fatalf("CaptureGet: %v", err)
	}
	if out.SourceURL != "https://example.com" || out.SizeBytes != 42 || out.ExpiresIn != 300 {
		t.Errorf("decoded %+v", out)
	}
}

func TestClient_Reflect_DecodesCandidates(t *testing.T) {
	body := `{"candidates":[{"sha":"s1","title":"Stale One","reason":"stale-gate"},{"sha":"s2","title":"Stale Two","reason":"stale-gate"}]}`
	ts := newJSONServer(t, http.MethodGet, "/api/brain/reflect", 0, body)
	defer ts.Close()
	out, err := newTestClient(t, ts.URL).Reflect(context.Background())
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(out.Candidates) != 2 || out.Candidates[0].SHA != "s1" || out.Candidates[1].Reason != "stale-gate" {
		t.Errorf("decoded %+v", out.Candidates)
	}
}

func TestClient_Resynth_DryRunSendsFlags(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/brain/resynth" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"backlog_count":3,"sample":[{"sha":"x","title":"T"}],"started":false,"pending":0}`)
	}))
	defer ts.Close()
	out, err := newTestClient(t, ts.URL).Resynth(context.Background(), true, 10)
	if err != nil {
		t.Fatalf("Resynth: %v", err)
	}
	if gotBody["dry_run"] != true {
		t.Errorf("dry_run not sent true: %+v", gotBody)
	}
	if gotBody["limit"].(float64) != 10 {
		t.Errorf("limit not sent: %+v", gotBody)
	}
	if out.BacklogCount != 3 || out.Started || len(out.Sample) != 1 || out.Sample[0].SHA != "x" {
		t.Errorf("decoded %+v", out)
	}
}

func TestClient_GetMergeStatus_Decodes(t *testing.T) {
	ts := newJSONServer(t, http.MethodGet, "/api/brain/merge/brain-9", 0,
		`{"brain_id":"brain-9","state":"merged","merged_at":"2026-01-01T00:00:00Z","raw_count":4,"attachment_count":2}`)
	defer ts.Close()
	out, err := newTestClient(t, ts.URL).GetMergeStatus(context.Background(), "brain-9")
	if err != nil {
		t.Fatalf("GetMergeStatus: %v", err)
	}
	if out.State != "merged" || out.RawCount != 4 || out.AttachmentCount != 2 {
		t.Errorf("decoded %+v", out)
	}
}

// TestClient_TransportFailureWrapsUnreachable confirms do() tags every
// transport-level failure with ErrDaemonUnreachable so callers can
// branch (queue-and-retry) instead of treating it as a hard error.
func TestClient_TransportFailureWrapsUnreachable(t *testing.T) {
	// Stand up then immediately close a server so the port refuses.
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()
	c := newTestClient(t, url)

	if err := c.Health(context.Background()); !errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("Health: want ErrDaemonUnreachable, got %v", err)
	}
	if _, err := c.Recall(context.Background(), RecallRequest{Query: "q"}); !errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("Recall: want ErrDaemonUnreachable, got %v", err)
	}
	if _, err := c.Fetch(context.Background(), "sha"); !errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("Fetch: want ErrDaemonUnreachable, got %v", err)
	}
}

// TestClient_MalformedSuccessBodyErrors covers the decode-error branch
// in do() — a 2xx with a body that isn't the expected shape.
func TestClient_MalformedSuccessBodyErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `not json at all`)
	}))
	defer ts.Close()
	_, err := newTestClient(t, ts.URL).Reflect(context.Background())
	if err == nil {
		t.Fatal("expected decode error")
	}
	if errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("decode failure should not be tagged unreachable: %v", err)
	}
}

// TestClient_NonJSONErrorBodyFallsBack covers decodeErrorEnvelope's
// fallback when the error body is not the daemon envelope shape.
func TestClient_NonJSONErrorBodyFallsBack(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `502 Bad Gateway from proxy`)
	}))
	defer ts.Close()
	err := newTestClient(t, ts.URL).Health(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.StatusCode != http.StatusBadGateway || apiErr.Code != "" {
		t.Errorf("want bare 502 with no code, got %+v", apiErr)
	}
	if apiErr.Message != "502 Bad Gateway from proxy" {
		t.Errorf("message = %q", apiErr.Message)
	}
}
