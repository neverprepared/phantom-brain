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
