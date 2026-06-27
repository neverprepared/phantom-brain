// Package ollama wraps the local Ollama HTTP API.
//
// The only call pbrainctl makes against Ollama is /api/embed: turn text
// into a fixed-dim []float32 for the vector index. Everything else
// (LLM completions, model management) is the daemon's concern via the
// synthesizer, and even the daemon delegates to a brainbox session per
// the "No API keys for agents" rule. So this package is intentionally
// small.
//
// Defaults match the v4.x TypeScript implementation so a brain born
// before the Go cut-over and a brain born after produce
// byte-comparable embeddings against the same vault.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultBaseURL = "http://localhost:11434"
	DefaultModel   = "nomic-embed-text"
	DefaultDims    = 768
	DefaultTimeout = 60 * time.Second

	// DefaultGenModel is the fallback text-generation model for the
	// daemon-side synth backend (gate / distill / entity extraction)
	// when the operator doesn't configure one. Distinct from DefaultModel
	// (an embedding model that cannot generate). Must be pulled locally
	// on the Ollama host: `ollama pull qwen2.5:7b`.
	DefaultGenModel = "qwen2.5:7b"
)

// Options configures a Client. Use OptionsFromEnv() to populate from
// the standard env vars, or build directly for tests.
type Options struct {
	BaseURL string
	Model   string

	// Dims is the expected embedding dimensionality. Embed() rejects
	// responses whose vector length doesn't match — drift here would
	// silently corrupt the vector index schema.
	Dims int

	// HTTP overrides the default http.Client. Useful for tests and for
	// installing tracing transports. If nil, an http.Client with
	// Timeout=DefaultTimeout is used.
	HTTP *http.Client
}

// OptionsFromEnv reads OLLAMA_BASE_URL, EMBEDDING_MODEL, EMBEDDING_DIMS.
// Missing or invalid values fall back to defaults.
func OptionsFromEnv() Options {
	o := Options{
		BaseURL: strings.TrimSpace(os.Getenv("OLLAMA_BASE_URL")),
		Model:   strings.TrimSpace(os.Getenv("EMBEDDING_MODEL")),
	}
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	if o.Model == "" {
		o.Model = DefaultModel
	}
	if raw := strings.TrimSpace(os.Getenv("EMBEDDING_DIMS")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			o.Dims = v
		}
	}
	if o.Dims == 0 {
		o.Dims = DefaultDims
	}
	return o
}

// Client is a thin HTTP client over Ollama. Safe for concurrent use.
type Client struct {
	opts Options
	http *http.Client
}

// New returns a Client with the supplied options. Empty fields fall
// back to defaults.
func New(opts Options) *Client {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.Model == "" {
		opts.Model = DefaultModel
	}
	if opts.Dims == 0 {
		opts.Dims = DefaultDims
	}
	hc := opts.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{opts: opts, http: hc}
}

// Dims returns the configured embedding dimensionality so callers
// (vector index setup, schema migrations) can read it without
// snapshotting Options.
func (c *Client) Dims() int { return c.opts.Dims }

// Model returns the configured embedding model name.
func (c *Client) Model() string { return c.opts.Model }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed turns input strings into vectors. The returned slice is
// 1:1-aligned with inputs.
//
// No automatic batching: pass too many at once and Ollama may reject
// the request or OOM. Caller is responsible for chunking. We'll add an
// auto-batching wrapper if a real consumer needs it.
//
// An empty input returns (nil, nil) without an API call.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(embedRequest{Model: c.opts.Model, Input: inputs})
	if err != nil {
		return nil, fmt.Errorf("ollama embed: marshal: %w", err)
	}

	url := strings.TrimRight(c.opts.BaseURL, "/") + "/api/embed"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama embed: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Cap bytes read on error responses — some servers stream HTML
		// error pages of unbounded size.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("ollama embed: %s: %s",
			resp.Status, strings.TrimSpace(string(errBody)))
	}

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ollama embed: decode: %w", err)
	}

	if len(out.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("ollama embed: input/output count mismatch: sent %d, got %d",
			len(inputs), len(out.Embeddings))
	}
	for i, v := range out.Embeddings {
		if len(v) != c.opts.Dims {
			return nil, fmt.Errorf("ollama embed: vector %d has dim %d, want %d",
				i, len(v), c.opts.Dims)
		}
	}

	return out.Embeddings, nil
}

type generateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
	// Format is Ollama's structured-output constraint. "json" forces the
	// model to emit a single valid JSON value — used for the gate verdict
	// so small local models don't wrap their output in prose. Empty for
	// free-form prose (the distill pass).
	Format string `json:"format,omitempty"`
}

type generateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// Generate runs a single non-streaming completion against /api/generate
// and returns the model's response text. An empty model falls back to
// the client's configured Model. When jsonFormat is true it pins
// format:"json" so the response is guaranteed-parseable JSON.
//
// The supplied ctx governs the deadline. Build the generation client
// without an http.Client timeout (Options.HTTP = &http.Client{}) so a
// slow local distillation isn't truncated by the transport — the caller's
// ctx is the single source of timeout truth.
func (c *Client) Generate(ctx context.Context, model, prompt string, jsonFormat bool) (string, error) {
	if strings.TrimSpace(model) == "" {
		model = c.opts.Model
	}
	reqBody := generateRequest{Model: model, Prompt: prompt, Stream: false}
	if jsonFormat {
		reqBody.Format = "json"
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("ollama generate: marshal: %w", err)
	}

	url := strings.TrimRight(c.opts.BaseURL, "/") + "/api/generate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ollama generate: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama generate: do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("ollama generate: %s: %s",
			resp.Status, strings.TrimSpace(string(errBody)))
	}

	var out generateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("ollama generate: decode: %w", err)
	}
	return out.Response, nil
}

// Health probes the Ollama server's /api/version endpoint. Returns
// nil on success or an error suitable for surfacing to the operator
// at startup. Cheap; safe to call from a fast health check.
func (c *Client) Health(ctx context.Context) error {
	url := strings.TrimRight(c.opts.BaseURL, "/") + "/api/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("ollama health: new request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ollama health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama health: %s", resp.Status)
	}
	return nil
}
