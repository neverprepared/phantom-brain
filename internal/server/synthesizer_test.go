package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// --- ScoreDomain -----------------------------------------------------

func TestScoreDomain(t *testing.T) {
	cases := []struct {
		url  string
		want DomainTier
	}{
		{"", TierUnknown},
		{"not a url", TierUnknown},
		{"https://arxiv.org/abs/2401.0000", TierAuthoritative},
		{"https://www.arxiv.org/abs/2401.0000", TierAuthoritative},
		{"https://blog.arxiv.org/post", TierAuthoritative},
		{"https://en.wikipedia.org/wiki/Go", TierCredible},
		{"https://example.com/post", TierUnknown},
		{"https://content-farm.com/x", TierLowQuality},
	}
	for _, c := range cases {
		got := ScoreDomain(c.url)
		if got != c.want {
			t.Errorf("ScoreDomain(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// --- CheckCoherence --------------------------------------------------

func TestCheckCoherence(t *testing.T) {
	cases := []struct {
		name    string
		content string
		ok      bool
	}{
		{"too-short", "abc", false},
		{"whitespace-only", strings.Repeat(" ", 50), false},
		{"punct-only", strings.Repeat(".", 50), false},
		{"normal", "Hello world, this is fine.", true},
		{"too-long", strings.Repeat("a", 60000), false},
	}
	for _, c := range cases {
		got := CheckCoherence(c.content)
		if got.Passed != c.ok {
			t.Errorf("%s: passed=%v, want %v (reason: %s)", c.name, got.Passed, c.ok, got.Reason)
		}
	}
}

// --- ExtractEntities -------------------------------------------------

func TestExtractEntities_HeadingsAndBolds(t *testing.T) {
	body := `# Top

## Foo Bar
some text

## Overview
generic — should be skipped

## What is X
question — should be skipped

## A really long heading that has too many words to count
skipped — wordCount >= 5

Here is **Alice Smith** doing things, and **bob** is lowercase.
**Note:** this is **Quite Important.** — has period.
`
	got := ExtractEntities(body)
	want := []string{"Foo Bar", "Alice Smith"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractEntities_DedupAndOrder(t *testing.T) {
	body := `## Alpha

## Beta

## Alpha

**Beta** **Gamma**`
	got := ExtractEntities(body)
	want := []string{"Alpha", "Beta", "Gamma"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestEntitySnippet_CentersOnMention(t *testing.T) {
	body := strings.Repeat("preamble. ", 200) + "Important Thing happens here. " + strings.Repeat("trailing. ", 200)
	snip := EntitySnippet(body, "Important Thing")
	if !strings.Contains(snip, "Important Thing") {
		t.Errorf("snippet missing entity: %q", snip[:120])
	}
	if len(snip) > 1600 {
		t.Errorf("snippet too long: %d", len(snip))
	}
}

// --- parseGateVerdict ------------------------------------------------

func TestParseGateVerdict(t *testing.T) {
	cases := []struct {
		raw   string
		ok    bool
		rel   Reliability
		topic Topic
	}{
		{`{"reliability":"high","topic":"tools","reason":"primary source"}`, true, ReliabilityHigh, TopicTools},
		{"```json\n{\"reliability\":\"low\",\"category\":\"informal\",\"topic\":\"general\",\"reason\":\"opinion\"}\n```",
			true, ReliabilityLow, TopicGeneral},
		{"some preamble {\"reliability\":\"medium\",\"topic\":\"agents\",\"reason\":\"x\"} done",
			true, ReliabilityMedium, TopicAgents},
		{"no json here", false, "", ""},
		{"", false, "", ""},
	}
	for i, c := range cases {
		v, err := parseGateVerdict(c.raw)
		if c.ok && err != nil {
			t.Errorf("[%d] unexpected err: %v", i, err)
			continue
		}
		if !c.ok && err == nil {
			t.Errorf("[%d] expected err, got %+v", i, v)
			continue
		}
		if c.ok && (v.Reliability != c.rel || v.Topic != c.topic) {
			t.Errorf("[%d] got %+v", i, v)
		}
	}
}

// --- Synthesizer end-to-end (with fake claude CLI) -------------------

// writeFakeClaude drops a script at dir/claude that prints fixedOutput
// on stdout and exits 0. Prepends dir to PATH for the test so the
// gate / summarize calls reach this fake.
func writeFakeClaude(t *testing.T, fixedOutput string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\nprintf %s '" + strings.ReplaceAll(fixedOutput, "'", "'\\''") + "'\n"
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return path
}

func TestSynthesizer_HappyPathWritesSummaryAndEntities(t *testing.T) {
	_ = writeFakeClaude(t, `{"reliability":"high","topic":"tools","reason":"primary docs"}`)
	d := DataDir(t.TempDir())
	binding := VaultBinding{
		Key:      VaultKey{Profile: "personal", Vault: "memory"},
		Defaults: VaultDefaults{ReaperPollIntervalSecs: 5},
	}
	if err := EnsureCollectiveSkeleton(d, binding.Key.Profile, binding.Key.Vault); err != nil {
		t.Fatal(err)
	}

	// Seed a raw file + queue item.
	rawRel := "Raw/curated/notes-on-go.md"
	rawAbs := filepath.Join(d.VaultDir(binding.Key.Profile, binding.Key.Vault), rawRel)
	body := "# Notes on Go\n\n## Goroutines\n\nThe **GMP scheduler** manages M:N threading. **Channels** are reified queues.\n\nMore prose to satisfy coherence rules with letters."
	if err := os.WriteFile(rawAbs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := EnqueueItem(d, binding.Key.Profile, binding.Key.Vault, QueueItem{
		RawPath: rawRel,
		Source:  "curated",
		Title:   "Notes on Go",
		Format:  "markdown",
		ContentHash: "deadbeef",
	}, "deadbeef0001")
	if err != nil {
		t.Fatal(err)
	}

	res, err := SynthesizeOne(context.Background(), d, binding, slog.New(slog.DiscardHandler), &sync.Mutex{})
	if err != nil {
		t.Fatalf("SynthesizeOne: %v", err)
	}
	if res.Reliability != ReliabilityMedium && res.Reliability != ReliabilityHigh {
		// curated sources short-circuit to medium regardless of LLM output.
		t.Errorf("reliability = %q, want medium (curated short-circuit)", res.Reliability)
	}
	// Summary page on disk.
	summaryAbs := filepath.Join(d.VaultDir(binding.Key.Profile, binding.Key.Vault), res.SummaryPath)
	sb, err := os.ReadFile(summaryAbs)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if !strings.Contains(string(sb), "title: Notes on Go") {
		t.Errorf("frontmatter missing title; got: %s", sb[:300])
	}
	// Entity pages — at least Goroutines (heading) + the bolded GMP scheduler + Channels.
	if len(res.EntityPaths) < 2 {
		t.Errorf("expected entities, got %v", res.EntityPaths)
	}
	for _, p := range res.EntityPaths {
		if _, err := os.Stat(filepath.Join(d.VaultDir(binding.Key.Profile, binding.Key.Vault), p)); err != nil {
			t.Errorf("entity page missing: %s", p)
		}
	}
	// _log.md got an entry.
	logBody, _ := os.ReadFile(filepath.Join(d.VaultDir(binding.Key.Profile, binding.Key.Vault), "Wiki", "_log.md"))
	if !strings.Contains(string(logBody), "Notes on Go") {
		t.Errorf("log missing entry: %s", logBody)
	}
	// Provenance updated.
	provRaw, _ := os.ReadFile(filepath.Join(d.IndexDir(binding.Key.Profile, binding.Key.Vault), "provenance.json"))
	var prov provenanceMap
	if err := json.Unmarshal(provRaw, &prov); err != nil {
		t.Fatalf("parse provenance: %v", err)
	}
	entry, ok := prov[rawRel]
	if !ok {
		t.Fatalf("provenance missing entry for %s; got %+v", rawRel, prov)
	}
	if len(entry.WikiPages) < 1 || entry.WikiPages[0] != res.SummaryPath {
		t.Errorf("provenance wiki_pages off: %v", entry.WikiPages)
	}
	// Queue item moved to done.
	doneDir := filepath.Join(d.VaultDir(binding.Key.Profile, binding.Key.Vault), "queue", "done")
	entries, _ := os.ReadDir(doneDir)
	if len(entries) != 1 {
		t.Errorf("expected 1 done entry, got %d", len(entries))
	}
}

func TestSynthesizer_QueueEmpty(t *testing.T) {
	d := DataDir(t.TempDir())
	binding := VaultBinding{Key: VaultKey{Profile: "personal", Vault: "memory"}}
	_ = EnsureCollectiveSkeleton(d, binding.Key.Profile, binding.Key.Vault)
	_, err := SynthesizeOne(context.Background(), d, binding, slog.New(slog.DiscardHandler), &sync.Mutex{})
	if !errors.Is(err, errQueueEmpty) {
		t.Errorf("expected errQueueEmpty, got %v", err)
	}
}

func TestSynthesizer_CoherenceFailMarksLow(t *testing.T) {
	_ = writeFakeClaude(t, `{"reliability":"high","topic":"tools","reason":"x"}`)
	d := DataDir(t.TempDir())
	binding := VaultBinding{Key: VaultKey{Profile: "personal", Vault: "memory"}}
	_ = EnsureCollectiveSkeleton(d, binding.Key.Profile, binding.Key.Vault)

	rawRel := "Raw/gathered/short.md"
	rawAbs := filepath.Join(d.VaultDir(binding.Key.Profile, binding.Key.Vault), rawRel)
	// Too-short content trips coherence.
	if err := os.WriteFile(rawAbs, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = EnqueueItem(d, binding.Key.Profile, binding.Key.Vault, QueueItem{
		RawPath: rawRel, Source: "gathered", Title: "short", Format: "markdown",
	}, "abcd0001")

	res, err := SynthesizeOne(context.Background(), d, binding, slog.New(slog.DiscardHandler), &sync.Mutex{})
	if err != nil {
		t.Fatalf("SynthesizeOne: %v", err)
	}
	if res.Reliability != ReliabilityLow {
		t.Errorf("reliability = %q, want low (coherence-fail)", res.Reliability)
	}
}

// --- maintenance flag ------------------------------------------------

func TestMaintenance_SetClearReport(t *testing.T) {
	d := DataDir(t.TempDir())
	k := VaultKey{Profile: "personal", Vault: "memory"}
	if InMaintenance(d, k) {
		t.Fatal("fresh vault should not be in maintenance")
	}
	if err := SetMaintenance(d, k); err != nil {
		t.Fatal(err)
	}
	if !InMaintenance(d, k) {
		t.Fatal("expected maintenance flag set")
	}
	if err := ClearMaintenance(d, k); err != nil {
		t.Fatal(err)
	}
	if InMaintenance(d, k) {
		t.Fatal("expected maintenance flag cleared")
	}
}

// --- helpers ---------------------------------------------------------

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Hello World":              "hello-world",
		"  Trimmed!!  ":            "trimmed",
		"":                          "",
		"Multi --- Dash":            "multi-dash",
		"UPPER_CASE_id_42":          "upper-case-id-42",
		strings.Repeat("a", 200):    strings.Repeat("a", 80),
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
