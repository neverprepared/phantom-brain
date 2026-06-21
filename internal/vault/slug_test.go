package vault

import (
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Simple
		{"hello", "hello"},
		{"Hello World", "hello-world"},
		{"  spaces  ", "spaces"},

		// Punctuation
		{"hello, world!", "hello-world"},
		{"foo/bar/baz", "foo-bar-baz"},
		{"---title---", "title"},
		{"a___b", "a-b"},

		// Accents folded to ASCII
		{"café", "cafe"},
		{"naïve résumé", "naive-resume"},
		{"Crème Brûlée", "creme-brulee"},

		// Numbers kept
		{"version 1.2.3", "version-1-2-3"},
		{"2026-06-20 notes", "2026-06-20-notes"},

		// Multiple hyphens collapse
		{"a--b---c", "a-b-c"},
		{"!!a!!b!!", "a-b"},

		// Unsupported runes become hyphens
		{"日本語 text", "text"},
		{"🎉 party", "party"},

		// Empty after stripping
		{"", ""},
		{"   ", ""},
		{"!@#$%^&*()", ""},
		{"日本語", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := Slug(c.in); got != c.want {
				t.Errorf("Slug(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSlugTruncatedAtMax(t *testing.T) {
	long := strings.Repeat("hello-world-", 20)
	got := Slug(long)
	if len(got) > MaxSlugLength {
		t.Errorf("len = %d, want <= %d", len(got), MaxSlugLength)
	}
	// And the truncated end should not be a trailing hyphen.
	if strings.HasSuffix(got, "-") {
		t.Errorf("truncated slug ends with hyphen: %q", got)
	}
}

func TestSlugIsIdempotent(t *testing.T) {
	// Slug(Slug(x)) == Slug(x). Important when callers re-slug a value
	// they already slugged earlier.
	for _, in := range []string{
		"Hello World",
		"naïve résumé",
		"2026-06-20 something",
		strings.Repeat("x", 100),
	} {
		once := Slug(in)
		twice := Slug(once)
		if once != twice {
			t.Errorf("not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}
}
