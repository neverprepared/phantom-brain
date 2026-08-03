package vaultxfer

import (
	"strings"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/canonicalize"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := map[string]string{
		"plain note":         "# Note\n\nsome body text\n",
		"no trailing nl":     "# Note\nno trailing newline",
		"skill w/ own fm":    "---\nname: aws-patterns\ndescription: AWS stuff\n---\n# body\ninstructions\n",
		"body starts dashes": "---\n# not really frontmatter\n",
	}
	m := Meta{SHA: "deadbeef", Kind: "skill", Title: "aws-patterns", Tags: []string{"a", "b"}, CapturedAt: "2026-08-03T00:00:00Z"}

	for name, body := range cases {
		raw, err := Encode(m, body)
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		gotMeta, gotBody, err := Decode(raw)
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if gotBody != body {
			t.Errorf("%s: body not verbatim\n want %q\n got  %q", name, body, gotBody)
		}
		if gotMeta.Kind != m.Kind || gotMeta.Title != m.Title {
			t.Errorf("%s: meta lost: %+v", name, gotMeta)
		}
	}
}

// The whole point: a record's canonical identity must survive the round-trip,
// even when the body is a skill whose FIRST bytes are its own `---` frontmatter
// (this is where the mart's injected blank line broke dedup).
func TestIdentityPreservedThroughRoundTrip(t *testing.T) {
	body := "---\nname: aws-patterns\ndescription: FIRST\n---\n# AWS\ninstructions here\n"
	want, _ := canonicalize.Sum([]byte(body)) // verbatim kind → Sum over the body

	raw, err := Encode(Meta{Kind: "skill", Title: "aws-patterns"}, body)
	if err != nil {
		t.Fatal(err)
	}
	// A metadata block sits above the skill's own frontmatter (double fence).
	if strings.Count(string(raw), "---\n") < 3 {
		t.Fatalf("expected metadata fence + skill's own fence:\n%s", raw)
	}
	_, gotBody, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := canonicalize.Sum([]byte(gotBody))
	if got != want {
		t.Errorf("identity changed across round-trip: want %s got %s", want[:12], got[:12])
	}
}

func TestFilename(t *testing.T) {
	if got := Filename("AWS Patterns!", "deadbeefcafef00d"); got != "aws-patterns-deadbeefcafe.md" {
		t.Errorf("got %q", got)
	}
	if got := Filename("", ""); got != "record.md" {
		t.Errorf("empty: got %q", got)
	}
}
