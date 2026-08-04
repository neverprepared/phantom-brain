package server

import (
	"testing"

	"github.com/neverprepared/phantom-brain/internal/canonicalize"
	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// canonicalSHAForKind is the daemon's authoritative content-address: verbatim
// kinds (skill/todo/session) key on the full canonical doc (frontmatter is
// semantic), everything else on the canonical body only.
func TestCanonicalSHAForKind(t *testing.T) {
	descA := "---\nname: x\ndescription: FIRST\n---\n# body\nline\n"
	descB := "---\nname: x\ndescription: SECOND\n---\n# body\nline\n" // only the description differs

	sum := func(s string) string { h, _ := canonicalize.Sum([]byte(s)); return h }
	sumBody := func(s string) string { h, _ := canonicalize.SumBody([]byte(s)); return h }

	// Verbatim kinds use Sum → a description edit is a NEW identity (not lost).
	for _, k := range []osearch.Kind{osearch.KindSkill, osearch.KindTodo, osearch.KindSession} {
		a, err := canonicalSHAForKind(k, descA)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		b, _ := canonicalSHAForKind(k, descB)
		if a != sum(descA) {
			t.Errorf("%s: expected Sum-based sha", k)
		}
		if a == b {
			t.Errorf("%s: description edit must change identity (frontmatter is semantic for verbatim kinds)", k)
		}
	}

	// Non-verbatim kinds use SumBody → frontmatter (e.g. ingestion timestamps)
	// does NOT fragment dedup.
	a, _ := canonicalSHAForKind(osearch.KindNote, descA)
	b, _ := canonicalSHAForKind(osearch.KindNote, descB)
	if a != sumBody(descA) {
		t.Error("note: expected SumBody-based sha")
	}
	if a != b {
		t.Error("note: frontmatter-only edit must NOT change identity (body-keyed)")
	}

	// Framing robustness for both: the mart's leading blank line + trailing
	// whitespace/newlines canonicalize away → same identity → round-trip dedups.
	framed := "---\nname: x\ndescription: FIRST\n---\n\n# body\nline   \n\n\n"
	if s1, _ := canonicalSHAForKind(osearch.KindSkill, descA); s1 != sum(framed) {
		t.Error("skill: cosmetic framing must not change identity")
	}
	if s1, _ := canonicalSHAForKind(osearch.KindNote, descA); s1 != sumBody(framed) {
		t.Error("note: cosmetic framing must not change identity")
	}
}

// A faithful vault import preserves topic + reliability verbatim; empty means
// "don't override" the caller's default (learn's reliability=medium, etc.).
func TestApplyMemoryFieldsPreservesTopicReliability(t *testing.T) {
	doc := osearch.SummaryDoc{Vault: "memory", Reliability: osearch.ReliabilityMedium}
	applyMemoryFields(&doc, MemoryFields{Kind: "note", Topic: "governance", Reliability: "high"})
	if doc.Topic != "governance" {
		t.Errorf("topic not preserved: %q", doc.Topic)
	}
	if doc.Reliability != osearch.ReliabilityHigh {
		t.Errorf("reliability not preserved: %q", doc.Reliability)
	}

	kept := osearch.SummaryDoc{Vault: "memory", Topic: "keep", Reliability: osearch.ReliabilityMedium}
	applyMemoryFields(&kept, MemoryFields{Kind: "note"}) // empty topic/reliability
	if kept.Topic != "keep" || kept.Reliability != osearch.ReliabilityMedium {
		t.Errorf("empty fields must not override: topic=%q reliability=%q", kept.Topic, kept.Reliability)
	}

	if msg := validateMemoryFields(MemoryFields{Reliability: "bogus"}); msg == "" {
		t.Error("expected invalid reliability to be rejected")
	}
}
