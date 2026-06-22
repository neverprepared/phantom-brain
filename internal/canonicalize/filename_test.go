package canonicalize

import "testing"

func TestFilename(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// The motivating example from the operator's spec.
		{"appomni separation", "AppOmni Separation Agreement - Downing.PDF",
			"appomni_separation_agreement-downing.pdf"},

		// Multi-hyphen + multiple spaces, with mixed case.
		{"multi hyphen", "CA - CIIAA - Curtis Downing.pdf",
			"ca-ciiaa-curtis_downing.pdf"},

		// Hyphen without surrounding spaces stays put.
		{"tight hyphen", "Phase-6-Notes.md", "phase-6-notes.md"},

		// Punctuation: parens, apostrophe, comma, period inside basename
		// — all stripped.
		{"punct stripped", "Q2 (Final) Report - O'Brien, Curtis.docx",
			"q2_final_report-obrien_curtis.docx"},

		// Multiple internal spaces collapse to a single underscore.
		{"multi space", "Hello   World.txt", "hello_world.txt"},

		// Filesystem-unsafe chars become underscores; path traversal
		// attempt becomes inert.
		{"path traversal", "../etc/passwd", "etc_passwd"},
		{"backslash", `weird\name?file.txt`, "weird_name_file.txt"},
		{"pipes + stars", "*foo|bar*.tmp", "foo_bar.tmp"},

		// Leading/trailing underscores trimmed; extension kept.
		{"leading dashes", "  ---weird name---  .pdf", "weird_name.pdf"},

		// Unicode preserved (only lowercased).
		{"unicode", "René's Été.txt", "renés_été.txt"},

		// No extension.
		{"no ext", "Just a Name", "just_a_name"},

		// Empty / whitespace-only input.
		{"empty", "", ""},
		{"whitespace only", "   ", ""},

		// Pre-normalised input is a no-op (idempotent).
		{"idempotent",
			"appomni_separation_agreement-downing.pdf",
			"appomni_separation_agreement-downing.pdf"},

		// Uppercase extension lowered.
		{"uppercase ext", "report.PDF", "report.pdf"},

		// Numeric-only basename survives.
		{"numeric", "2025-07-15.pdf", "2025-07-15.pdf"},

		// Em-dash and en-dash are Unicode chars, not ASCII hyphen —
		// they pass through (we don't normalize them to "-").
		{"em dash preserved", "Report — Final.pdf", "report_—_final.pdf"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Filename(c.in)
			if got != c.want {
				t.Errorf("Filename(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestFilenameIsIdempotent(t *testing.T) {
	// Property: applying Filename twice gives the same result as once.
	// Spotchecks above the cases — any "almost canonical" string should
	// converge after a single pass.
	for _, in := range []string{
		"AppOmni - Downing.PDF",
		"Some name with spaces.txt",
		"weird/name?.bin",
		"q2 (final) report.docx",
	} {
		once := Filename(in)
		twice := Filename(once)
		if once != twice {
			t.Errorf("not idempotent: Filename(%q)=%q then Filename(%q)=%q",
				in, once, once, twice)
		}
	}
}
