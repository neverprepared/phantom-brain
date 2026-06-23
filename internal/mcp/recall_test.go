package mcp

import (
	"strings"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/index"
)

func TestFtsPhrase(t *testing.T) {
	cases := map[string]string{
		// Motivating bug: multi-word query needs OR fan-out so BM25
		// matches docs that have all the terms but not consecutively.
		"loop engineering AI coding agents": `"loop" OR "engineering" OR "AI" OR "coding" OR "agents"`,
		// Single token passes through quoted.
		"kubernetes": `"kubernetes"`,
		// Adjacent tokens still produce OR (not phrase) — relying on
		// BM25's term-frequency ranking to surface docs where they
		// also appear adjacently.
		"ReAct pattern": `"ReAct" OR "pattern"`,
		// Embedded quote in token gets escaped.
		`he said "hi"`: `"he" OR "said" OR """hi"""`,
		// FTS5 operator tokens dropped — bare AND/OR/NOT would error.
		"AI and ML":  `"AI" OR "ML"`,
		"foo or bar": `"foo" OR "bar"`,
		// Whitespace collapses; empty string returns empty.
		"":    "",
		"   ": "",
		// Punctuation in tokens is preserved inside quotes — FTS5
		// won't reinterpret them as operators because they're literal.
		"foo:bar baz": `"foo:bar" OR "baz"`,
	}
	for in, want := range cases {
		if got := ftsPhrase(in); got != want {
			t.Errorf("ftsPhrase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKindIndicator(t *testing.T) {
	cases := []struct {
		kind, tags, want string
	}{
		{"note", "", "[note]"},
		{"web_scrape", "", "[web]"},
		{"task_summary", "", "[task]"},
		{"email_import", "", "[email]"},
		{"attachment_stub", "mime:application/pdf attachment", "[attachment pdf]"},
		{"attachment_stub", "attachment mime:image/png", "[attachment png]"},
		{"attachment_stub", "attachment", "[attachment]"},
		{"attachment_stub", "mime:text", "[attachment text]"},
		{"", "", "[unknown]"},
		{"future_kind", "", "[future_kind]"},
	}
	for _, c := range cases {
		if got := kindIndicator(c.kind, c.tags); got != c.want {
			t.Errorf("kindIndicator(%q,%q) = %q, want %q", c.kind, c.tags, got, c.want)
		}
	}
}

func TestRenderRecallHitsIncludesTitleKindSnippet(t *testing.T) {
	hits := []index.Hit{
		{
			SHA: "deadbeef", SourcePath: "Raw/curated/2026-tax.md",
			Title: "Tax forms 2026", Kind: "note",
			Snippet: "Short body excerpt.",
			Score:   0.123, VectorRank: 1, TextRank: 2,
		},
		{
			SHA: "cafebabe", SourcePath: "attachment://cafebabe",
			Title: "1099-misc-2025.pdf", Kind: "attachment_stub",
			Tags:    "attachment mime:application/pdf",
			Snippet: "(binary attachment — see fetch hint)",
			Score:   0.05, VectorRank: 0, TextRank: 3,
		},
	}
	out := renderRecallHits("tax", hits, 0, 0)

	wantSubs := []string{
		"## 1. Tax forms 2026 [note]",
		"- SHA: `deadbeef`",
		"- Path: `Raw/curated/2026-tax.md`",
		"- Snippet: Short body excerpt.",
		"## 2. 1099-misc-2025.pdf [attachment pdf]",
		"- Fetch via `GET /api/brain/attach/cafebabe`",
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q\n--full--\n%s", s, out)
		}
	}

	// Non-attachment hits must not get the fetch hint.
	if strings.Contains(strings.SplitN(out, "## 2.", 2)[0], "/api/brain/attach/") {
		t.Errorf("note hit got attachment fetch hint")
	}
}

func TestRenderRecallHitsFallsBackToPathWhenTitleEmpty(t *testing.T) {
	hits := []index.Hit{{
		SHA: "x", SourcePath: "Wiki/x.md", Kind: "web_scrape", Score: 1,
	}}
	out := renderRecallHits("q", hits, 0, 0)
	if !strings.Contains(out, "## 1. Wiki/x.md [web]") {
		t.Errorf("fallback heading missing: %s", out)
	}
}
