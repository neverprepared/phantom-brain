package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEmbedSingleInput(t *testing.T) {
	want := []float32{0.1, 0.2, 0.3}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("path = %q, want /api/embed", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "test-model" {
			t.Errorf("model = %q", req.Model)
		}
		if len(req.Input) != 1 || req.Input[0] != "hello" {
			t.Errorf("input = %v", req.Input)
		}
		_ = json.NewEncoder(w).Encode(embedResponse{
			Model:      "test-model",
			Embeddings: [][]float32{want},
		})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Model: "test-model", Dims: 3})
	got, err := c.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !floatsEqual(got[0], want) {
		t.Errorf("got %v, want [[0.1 0.2 0.3]]", got)
	}
}

func TestEmbedBatchAlignment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Echo length-aligned vectors derived from input index so the
		// test can assert 1:1 ordering preservation.
		out := make([][]float32, len(req.Input))
		for i := range req.Input {
			out[i] = []float32{float32(i), float32(i + 1)}
		}
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: out})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Dims: 2})
	got, err := c.Embed(context.Background(), []string{"a", "b", "c", "d"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d vectors, want 4", len(got))
	}
	for i, v := range got {
		if v[0] != float32(i) || v[1] != float32(i+1) {
			t.Errorf("vec[%d] = %v, want [%d %d]", i, v, i, i+1)
		}
	}
}

func TestEmbedEmptyInputSkipsHTTP(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Dims: 3})
	got, err := c.Embed(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for empty input", got)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("empty input should not make an HTTP call; got %d calls", calls)
	}
}

func TestEmbedDimensionMismatchIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{
			Embeddings: [][]float32{{0.1, 0.2}}, // 2 dims, client expects 3
		})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Dims: 3})
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "dim") {
		t.Errorf("expected dim mismatch error, got: %v", err)
	}
}

func TestEmbedCountMismatchIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{
			Embeddings: [][]float32{{0.1, 0.2, 0.3}}, // 1 vec, sent 2 inputs
		})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Dims: 3})
	_, err := c.Embed(context.Background(), []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "count mismatch") {
		t.Errorf("expected count mismatch error, got: %v", err)
	}
}

func TestEmbedHTTPErrorIsWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found: nomic-embed-text", http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Dims: 3})
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("error should include server message; got: %v", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should include status code; got: %v", err)
	}
}

func TestEmbedNetworkErrorSurfaces(t *testing.T) {
	c := New(Options{BaseURL: "http://127.0.0.1:1", Dims: 3, HTTP: &http.Client{Timeout: 100 * time.Millisecond}})
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected network error")
	}
}

func TestEmbedContextCancellation(t *testing.T) {
	// Server delays response well beyond the client's 50ms deadline,
	// but selects on the request context too so server cleanup at
	// test end is fast — otherwise httptest.Server.Close blocks for
	// the full timer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Dims: 3})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.Embed(ctx, []string{"x"})
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context") {
		t.Errorf("error should reference context cancellation; got: %v", err)
	}
}

func TestHealth(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/version" {
				t.Errorf("path = %q", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":"0.x.y"}`))
		}))
		defer srv.Close()

		c := New(Options{BaseURL: srv.URL})
		if err := c.Health(context.Background()); err != nil {
			t.Errorf("Health = %v, want nil", err)
		}
	})
	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()
		c := New(Options{BaseURL: srv.URL})
		if err := c.Health(context.Background()); err == nil {
			t.Error("expected error on 502")
		}
	})
}

func TestDefaultsApply(t *testing.T) {
	c := New(Options{})
	if c.opts.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want default", c.opts.BaseURL)
	}
	if c.opts.Model != DefaultModel {
		t.Errorf("Model = %q, want default", c.opts.Model)
	}
	if c.Dims() != DefaultDims {
		t.Errorf("Dims = %d, want %d", c.Dims(), DefaultDims)
	}
	if c.http == nil {
		t.Error("default http.Client should be installed")
	}
}

func TestOptionsFromEnv(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "http://ollama.test:9999")
	t.Setenv("EMBEDDING_MODEL", "custom-embed")
	t.Setenv("EMBEDDING_DIMS", "1024")

	o := OptionsFromEnv()
	if o.BaseURL != "http://ollama.test:9999" {
		t.Errorf("BaseURL = %q", o.BaseURL)
	}
	if o.Model != "custom-embed" {
		t.Errorf("Model = %q", o.Model)
	}
	if o.Dims != 1024 {
		t.Errorf("Dims = %d", o.Dims)
	}
}

func TestOptionsFromEnvDefaults(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv("EMBEDDING_MODEL", "")
	t.Setenv("EMBEDDING_DIMS", "")

	o := OptionsFromEnv()
	if o.BaseURL != DefaultBaseURL || o.Model != DefaultModel || o.Dims != DefaultDims {
		t.Errorf("defaults not applied: %+v", o)
	}
}

func TestOptionsFromEnvIgnoresBogusDims(t *testing.T) {
	t.Setenv("EMBEDDING_DIMS", "not-a-number")
	o := OptionsFromEnv()
	if o.Dims != DefaultDims {
		t.Errorf("bogus EMBEDDING_DIMS should fall back to default; got %d", o.Dims)
	}
}

// floatsEqual is exact equality (no tolerance) because the test echoes
// values verbatim through the JSON round-trip.
func floatsEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
