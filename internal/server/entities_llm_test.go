package server

import (
	"context"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

func TestParseEntityResponse_PlainArray(t *testing.T) {
	got, err := parseEntityResponse(`["LangGraph", "ReAct", "Anthropic"]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"LangGraph", "ReAct", "Anthropic"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseEntityResponse_DropsDuplicates(t *testing.T) {
	got, _ := parseEntityResponse(`["LangGraph", "langgraph", "  LangGraph  "]`)
	if len(got) != 1 {
		t.Errorf("expected dedup to 1 entry; got %v", got)
	}
}

func TestParseEntityResponse_StripsCodeFences(t *testing.T) {
	in := "```json\n[\"Kubernetes\", \"Helm\"]\n```"
	got, err := parseEntityResponse(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 || got[0] != "Kubernetes" {
		t.Errorf("got %v", got)
	}
}

func TestParseEntityResponse_HandlesPrefacedJSON(t *testing.T) {
	// Some models like to chat before emitting JSON. Find the [ and
	// parse from there.
	in := "Sure, here are the entities:\n\n[\"Anthropic\", \"Claude\"]"
	got, err := parseEntityResponse(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"Anthropic", "Claude"}) {
		t.Errorf("got %v", got)
	}
}

func TestParseEntityResponse_EmptyArray(t *testing.T) {
	got, err := parseEntityResponse(`[]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty; got %v", got)
	}
}

func TestParseEntityResponse_DropsMarkdownNoise(t *testing.T) {
	got, _ := parseEntityResponse(`["RealEntity", "# section", "* bullet", ""]`)
	if !reflect.DeepEqual(got, []string{"RealEntity"}) {
		t.Errorf("got %v, want [RealEntity]", got)
	}
}

func TestBuildEntityPrompt_TruncatesLongBodies(t *testing.T) {
	// 9000 chars of body — should truncate to 8000 + marker.
	body := ""
	for i := 0; i < 9000; i++ {
		body += "a"
	}
	prompt := buildEntityPrompt("T", body)
	if !contains(prompt, "[...truncated]") {
		t.Error("long body should be truncated with marker")
	}
}

// TestExtractEntitiesBest_FallsBackOnCLIError verifies the LLM-first
// path degrades cleanly to the regex extractor when claude shells
// out with a non-zero exit. Uses the writeFakeClaude trick to inject
// a failing CLI on PATH.
func TestExtractEntitiesBest_FallsBackOnCLIError(t *testing.T) {
	writeFailingClaude(t)

	body := "## Real Section\n\nThis is **LangGraph** doing things."
	got := extractEntitiesBest(context.Background(), claudeBackend{}, "Title", body,
		slog.New(slog.DiscardHandler))
	// Regex extractor's behaviour: catches the H2 "Real Section" and
	// the bold "LangGraph". We just need at least one of the
	// expected fallback hits to confirm we fell back.
	found := false
	for _, e := range got {
		if e == "LangGraph" || e == "Real Section" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected regex-fallback to surface a known entity; got %v", got)
	}
}

// TestExtractEntitiesBest_UsesLLMOutput confirms the LLM path is
// preferred when the fake claude returns a clean JSON array.
func TestExtractEntitiesBest_UsesLLMOutput(t *testing.T) {
	writeFakeClaude(t, `["LangGraph", "ReAct"]`)

	body := "## Core Concept\n\nThe **ReAct** pattern uses **LangGraph** under the hood."
	got := extractEntitiesBest(context.Background(), claudeBackend{}, "Title", body,
		slog.New(slog.DiscardHandler))
	if !reflect.DeepEqual(got, []string{"LangGraph", "ReAct"}) {
		t.Errorf("got %v, want [LangGraph ReAct] from LLM path", got)
	}
}

// TestExtractEntitiesBest_NoBackend confirms a nil backend skips the LLM
// call entirely and goes straight to the regex extractor.
func TestExtractEntitiesBest_NoBackend(t *testing.T) {
	// Even with a working fake claude on PATH, a nil backend
	// short-circuits the LLM call.
	writeFakeClaude(t, `["should-be-ignored"]`)

	body := "## Real Heading\n\nbody **TermOne** more"
	got := extractEntitiesBest(context.Background(), nil, "Title", body,
		slog.New(slog.DiscardHandler))
	for _, e := range got {
		if e == "should-be-ignored" {
			t.Error("LLM path was reached despite a nil backend")
		}
	}
}

func TestExtractEntitiesLLM_TimeoutSurfaces(t *testing.T) {
	// Write a slow claude that sleeps longer than timeout.
	writeSlowClaude(t, 2*time.Second)
	_, err := ExtractEntitiesLLM(context.Background(), claudeBackend{}, "T", "body", "", 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
