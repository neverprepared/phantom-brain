package pgstore

import "strings"

// SanitizeText strips NUL bytes (0x00) from a string. PostgreSQL TEXT
// columns reject NUL in UTF-8 input ("invalid byte sequence for encoding
// UTF8: 0x00", SQLSTATE 22021), but OpenSearch/JSON tolerate it — so
// legacy content (web scrapes, OCR'd PDFs, binary-contaminated bodies)
// can carry NULs that blow up an INSERT. Every text value written to the
// SoR from legacy OpenSearch docs MUST pass through here. NUL in text is
// always an artifact, so stripping (not replacing) is the right call.
//
// Fast path: the overwhelming majority of strings have no NUL, so we
// only allocate when one is present.
func SanitizeText(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// SanitizeTexts applies SanitizeText to every element of a slice,
// returning a NON-NIL slice (callers rely on non-nil for NOT NULL
// DEFAULT '{}' array columns). nil in → empty (non-nil) out.
func SanitizeTexts(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = SanitizeText(s)
	}
	return out
}
