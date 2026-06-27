package server

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/neverprepared/phantom-brain/internal/ollama"
)

// defaultLLMTimeout guards Complete callers that pass a zero timeout —
// the Claude CLI path in particular treats a zero context deadline as
// "already expired", so a backend must never forward a zero through.
const defaultLLMTimeout = 30 * time.Second

// LLMRequest is one synth LLM call. JSON signals that the expected
// response is a JSON value (the gate verdict object), letting an Ollama
// backend pin format:"json" so small local models emit parseable output.
// The Claude CLI backend ignores JSON — its prompts already demand bare
// JSON and the parsers tolerate stray fences/preambles either way.
type LLMRequest struct {
	Prompt  string
	Model   string
	Timeout time.Duration
	JSON    bool
}

// LLMBackend is the synth text-generation seam. RunGate, SummarizeContent,
// and ExtractEntitiesLLM call Complete instead of shelling out directly,
// so the operator can run synth on a local Ollama model (the default,
// zero Claude tokens) or the bundled `claude` CLI without the pipeline
// caring which. Implementations MUST degrade by returning an error —
// callers fall back to raw content / the regex extractor, never panic.
type LLMBackend interface {
	// Complete runs one completion. Honors req.Timeout (falling back to
	// defaultLLMTimeout when zero) and the parent ctx.
	Complete(ctx context.Context, req LLMRequest) (string, error)
	// Available reports whether the backend can serve a request right now.
	// Cheap and safe to call per job.
	Available() bool
	// Name is a short label for startup logs (e.g. "ollama:qwen2.5:7b").
	Name() string
}

// claudeBackend routes synth through the bundled `claude` CLI — the
// pre-Ollama behaviour, still selectable via [synth] backend = "claude".
type claudeBackend struct{}

func (claudeBackend) Complete(ctx context.Context, req LLMRequest) (string, error) {
	t := req.Timeout
	if t <= 0 {
		t = defaultLLMTimeout
	}
	return CallClaudeCLI(ctx, req.Prompt, req.Model, t)
}

func (claudeBackend) Available() bool { return ClaudeCLIAvailable() }
func (claudeBackend) Name() string    { return "claude-cli" }

// ollamaBackend routes synth through a local Ollama model via
// /api/generate — the default backend. Health is probed lazily and the
// first success is cached, so a daemon that comes up before the Ollama
// service self-heals on the next job rather than staying degraded until
// restart (the Claude CLI snapshot-at-startup model couldn't do this).
type ollamaBackend struct {
	client  *ollama.Client
	model   string
	healthy atomic.Bool
}

func (b *ollamaBackend) Complete(ctx context.Context, req LLMRequest) (string, error) {
	t := req.Timeout
	if t <= 0 {
		t = defaultLLMTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	m := req.Model
	if strings.TrimSpace(m) == "" {
		m = b.model
	}
	out, err := b.client.Generate(ctx, m, req.Prompt, req.JSON)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (b *ollamaBackend) Available() bool {
	if b.healthy.Load() {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := b.client.Health(ctx); err != nil {
		return false
	}
	b.healthy.Store(true)
	return true
}

func (b *ollamaBackend) Name() string { return "ollama:" + b.model }

// NewLLMBackend builds the synth backend from config. Unknown backend
// names fall through to Ollama — the daemon should prefer the local,
// token-free path over refusing to synth on a config typo.
func NewLLMBackend(cfg SynthConfig) LLMBackend {
	switch strings.ToLower(strings.TrimSpace(cfg.Backend)) {
	case "claude", "claude-cli":
		return claudeBackend{}
	default: // "ollama", empty, or unrecognized
		base := strings.TrimSpace(cfg.OllamaBaseURL)
		if base == "" {
			base = ollama.DefaultBaseURL
		}
		model := strings.TrimSpace(cfg.OllamaModel)
		if model == "" {
			model = ollama.DefaultGenModel
		}
		cl := ollama.New(ollama.Options{
			BaseURL: base,
			Model:   model,
			// No transport-level timeout — local distillation can run long;
			// the per-call ctx deadline in Complete is the single timeout.
			HTTP: &http.Client{},
		})
		return &ollamaBackend{client: cl, model: model}
	}
}
