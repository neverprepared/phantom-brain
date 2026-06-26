package pgstore

import "testing"

func TestSanitizeText(t *testing.T) {
	cases := map[string]string{
		"clean":             "clean",
		"":                  "",
		"a\x00b":            "ab",
		"\x00lead":          "lead",
		"trail\x00":         "trail",
		"\x00\x00multi\x00": "multi",
		"keep\ttab\nnl":     "keep\ttab\nnl", // only NUL is stripped
	}
	for in, want := range cases {
		if got := SanitizeText(in); got != want {
			t.Errorf("SanitizeText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeTexts(t *testing.T) {
	got := SanitizeTexts([]string{"a\x00", "b", "\x00c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// nil in → non-nil empty out (NOT NULL DEFAULT '{}' columns).
	if out := SanitizeTexts(nil); out == nil {
		t.Error("SanitizeTexts(nil) returned nil; want non-nil empty slice")
	}
}
