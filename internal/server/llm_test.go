package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/ollama"
)

func TestNewLLMBackend_Selection(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		b := NewLLMBackend(SynthConfig{Backend: "claude"})
		if _, ok := b.(claudeBackend); !ok {
			t.Errorf("backend = %T, want claudeBackend", b)
		}
		if b.Name() != "claude-cli" {
			t.Errorf("name = %q", b.Name())
		}
	})

	cases := []struct{ name, backend string }{
		{"explicit ollama", "ollama"},
		{"empty defaults to ollama", ""},
		{"unknown falls through to ollama", "gpt5-cli"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewLLMBackend(SynthConfig{Backend: tc.backend})
			ob, ok := b.(*ollamaBackend)
			if !ok {
				t.Fatalf("backend = %T, want *ollamaBackend", b)
			}
			// Empty model resolves to the package default.
			if ob.model != ollama.DefaultGenModel {
				t.Errorf("model = %q, want %q", ob.model, ollama.DefaultGenModel)
			}
			if !strings.HasPrefix(b.Name(), "ollama:") {
				t.Errorf("name = %q", b.Name())
			}
		})
	}

	t.Run("model override is honored", func(t *testing.T) {
		b := NewLLMBackend(SynthConfig{Backend: "ollama", OllamaModel: "llama3.1:8b"})
		ob := b.(*ollamaBackend)
		if ob.model != "llama3.1:8b" {
			t.Errorf("model = %q", ob.model)
		}
	})
}

// TestOllamaBackend_CompleteAndAvailable drives the backend against a
// fake Ollama serving both /api/version (health) and /api/generate.
func TestOllamaBackend_CompleteAndAvailable(t *testing.T) {
	var versionHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			versionHits++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":"0.x"}`))
		case "/api/generate":
			_, _ = w.Write([]byte(`{"response":"  distilled body  ","done":true}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	b := NewLLMBackend(SynthConfig{Backend: "ollama", OllamaBaseURL: srv.URL, OllamaModel: "m"})

	if !b.Available() {
		t.Fatal("Available() = false, want true against healthy server")
	}
	// Second call must use the cached success — no extra /api/version hit.
	if !b.Available() {
		t.Fatal("Available() second call = false")
	}
	if versionHits != 1 {
		t.Errorf("version probed %d times, want 1 (success cached)", versionHits)
	}

	got, err := b.Complete(context.Background(), LLMRequest{Prompt: "summarise", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	// Complete trims the model's whitespace.
	if got != "distilled body" {
		t.Errorf("got %q, want trimmed %q", got, "distilled body")
	}
}

func TestOllamaBackend_UnavailableWhenDown(t *testing.T) {
	// Port 1 is unbindable — Health fails fast.
	b := NewLLMBackend(SynthConfig{Backend: "ollama", OllamaBaseURL: "http://127.0.0.1:1"})
	if b.Available() {
		t.Error("Available() = true against down server, want false")
	}
}
