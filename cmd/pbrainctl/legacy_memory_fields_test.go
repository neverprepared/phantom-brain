package main

import (
	"testing"
	"time"

	"github.com/neverprepared/mcp-phantom-brain/internal/vault"
)

func TestBuildLegacyMemoryFields_EmailShape(t *testing.T) {
	doc, err := vault.Parse([]byte(`---
type: invoice
vendor: Ultimate Internet Access (UIA)
date: 2025-07-08
subject: 'Invoice'
from_email: 'billing@uia.net'
category: utilities
attachment_file: 'Invoice-468478.pdf'
obsidian_note: '2025/07/08/note.md'
---

body text
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, looksEmail := buildLegacyMemoryFields(doc, "Raw/curated/foo.md")
	if !looksEmail {
		t.Error("expected looksLikeEmailImport=true (has from_email + vendor + obsidian_note)")
	}
	if got.fields.CapturedAt == nil {
		t.Fatal("CapturedAt should have parsed from frontmatter date:2025-07-08")
	}
	if got.fields.CapturedAt.Year() != 2025 || got.fields.CapturedAt.Month() != time.July || got.fields.CapturedAt.Day() != 8 {
		t.Errorf("CapturedAt = %v, want 2025-07-08", got.fields.CapturedAt)
	}
	wantSourceContains := []string{"from_email:billing@uia.net", "obsidian_note:2025/07/08/note.md", "subject:Invoice"}
	for _, w := range wantSourceContains {
		found := false
		for _, s := range got.fields.Source {
			if s == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Source missing %q; got %v", w, got.fields.Source)
		}
	}
	wantTags := []string{"type:invoice", "vendor:Ultimate Internet Access (UIA)", "category:utilities"}
	for _, w := range wantTags {
		found := false
		for _, tag := range got.extraTags {
			if tag == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("extraTags missing %q; got %v", w, got.extraTags)
		}
	}
}

func TestBuildLegacyMemoryFields_NonEmailNote(t *testing.T) {
	// A technical note with no email frontmatter — should NOT be
	// flagged as email-import; caller will then apply the default
	// kind=note based on the kindLearn switch.
	doc, err := vault.Parse([]byte(`---
title: 'Agent Security Threat Model'
tags: [agents, security]
---

body
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, looksEmail := buildLegacyMemoryFields(doc, "Raw/curated/agent-security.md")
	if looksEmail {
		t.Error("expected looksLikeEmailImport=false for a note without email-shape frontmatter")
	}
}

func TestBuildLegacyMemoryFields_DateAsString(t *testing.T) {
	// Operator might quote the date — make sure string form still
	// parses (yaml's auto-typing as time.Time is the common case,
	// quoted-string is the fallback we want to keep supporting).
	doc, err := vault.Parse([]byte(`---
date: '2024-01-15'
vendor: x
---
body`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, _ := buildLegacyMemoryFields(doc, "Raw/curated/x.md")
	if got.fields.CapturedAt == nil {
		t.Fatalf("CapturedAt nil on quoted string date")
	}
	if got.fields.CapturedAt.Year() != 2024 {
		t.Errorf("CapturedAt year = %d, want 2024", got.fields.CapturedAt.Year())
	}
}

func TestBuildLegacyMemoryFields_MissingDate(t *testing.T) {
	doc, err := vault.Parse([]byte(`---
vendor: UIA
---
body`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, _ := buildLegacyMemoryFields(doc, "Raw/curated/x.md")
	if got.fields.CapturedAt != nil {
		t.Errorf("CapturedAt should remain nil when date frontmatter absent; got %v", got.fields.CapturedAt)
	}
}
