package canonicalize

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Filename normalises a human-supplied filename into a canonical
// shape suitable for storing as an attachment's original_filename
// or any other "this is what we'd call it on disk" field.
//
// Rules:
//   1. Lowercase the entire string (Unicode-aware via strings.ToLower).
//   2. Strip whitespace immediately adjacent to a hyphen, so " - "
//      collapses to "-" — this matches how operators name documents
//      (e.g. "AppOmni Separation - Downing" → "...separation-downing"
//      not "...separation_-_downing").
//   3. Strip "noise" punctuation entirely:
//      .,!?'"`():;[]{}@#$%^&*+=
//      The basename's "." is dropped; the extension's leading "."
//      is preserved because the split happens before stripping.
//   4. Replace remaining whitespace and filesystem-unsafe characters
//      (/ \ : * ? " < > |) with a single underscore.
//   5. Collapse runs of underscores into a single underscore.
//   6. Trim leading/trailing underscores and hyphens from the
//      basename (but preserve the leading "." of the extension).
//
// Unicode (accented chars, non-Latin scripts) is preserved — only
// lowercased. MinIO + OpenSearch both handle UTF-8 in keys and
// fields, so there's no portability win in stripping diacritics.
//
// Examples:
//   "AppOmni Separation Agreement - Downing.PDF"
//     → "appomni_separation_agreement-downing.pdf"
//
//   "Q2 (Final) Report — René's Notes.docx"
//     → "q2_final_report_—_renés_notes.docx"   (em-dash preserved
//        because rules only strip whitespace around the ASCII hyphen;
//        Unicode dashes pass through as ordinary characters)
//
//   "../etc/passwd"
//     → "etc_passwd"            (slashes → "_", trim leading "_")
//
//   ""
//     → ""                       (empty in → empty out; caller guards)
func Filename(human string) string {
	human = strings.TrimSpace(human)
	if human == "" {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(human))
	base := strings.ToLower(human[:len(human)-len(filepath.Ext(human))])

	base = hyphenSpacesRe.ReplaceAllString(base, "-")
	base = stripPunctRe.ReplaceAllString(base, "")
	base = fsUnsafeOrWSRe.ReplaceAllString(base, "_")
	base = collapseUnderscoreRe.ReplaceAllString(base, "_")
	base = strings.Trim(base, "_-")

	return base + ext
}

var (
	// step 2: collapse "<spaces>-<spaces>" → "-"
	hyphenSpacesRe = regexp.MustCompile(`\s*-\s*`)

	// step 3: noise punctuation we drop entirely (no replacement).
	// Chars that are ALSO filesystem-unsafe (? * " < > | / \ :) are
	// deliberately omitted here so step 4 turns them into a single
	// underscore — preserving the word boundary instead of silently
	// collapsing tokens. Tilde is also omitted (shows up in some
	// home-dir-derived filenames where stripping would be confusing).
	stripPunctRe = regexp.MustCompile("[.,!'`();:\\[\\]{}@#$%^&+=]")

	// step 4: whitespace OR filesystem-hostile characters → "_".
	// (Slash / backslash / colon / asterisk / question / quote /
	// angle brackets / pipe / control whitespace including tabs.)
	fsUnsafeOrWSRe = regexp.MustCompile(`[/\\:*?"<>|\s]`)

	// step 5: collapse "__..." → "_"
	collapseUnderscoreRe = regexp.MustCompile(`_+`)
)
