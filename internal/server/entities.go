package server

import (
	"regexp"
	"strings"
	"unicode"
)

// genericHeadingsSet is the deny-list of generic section titles that
// don't constitute named entities. Stored as a set for O(1) lookup;
// lowercased on read since the regex captures may vary in case.
var genericHeadingsSet = map[string]bool{
	// Ported from src/vault/entities.ts (≈50 entries).
	"overview": true, "summary": true, "introduction": true, "background": true,
	"contents": true, "table of contents": true, "abstract": true,
	"conclusion": true, "conclusions": true, "discussion": true,
	"references": true, "see also": true, "further reading": true,
	"resources": true, "related": true, "appendix": true, "notes": true,
	"data": true, "results": true, "methods": true, "methodology": true,
	"agents": true, "tools": true, "memory": true, "training": true,
	"governance": true, "infrastructure": true, "knowledge": true,
	"examples": true, "example": true, "implementation": true,
	"architecture": true, "design": true, "usage": true, "installation": true,
	"setup": true, "configuration": true, "options": true, "parameters": true,
	"arguments": true, "returns": true, "throws": true, "errors": true,
	"warnings": true, "caveats": true, "limitations": true, "future work": true,
	"acknowledgments": true, "license": true, "authors": true,
	"history": true, "changelog": true, "faq": true, "faqs": true,
	"glossary": true, "definitions": true,

	// Phase 6: added after a corpus survey surfaced these as the top
	// noise sources. Email-scrape pipelines + curated-doc templates
	// repeat them in every doc — they're scaffolding, not entities.
	"extracted data": true, "findings": true, "key concepts": true,
	"key takeaways": true, "key points": true, "mentions": true,
	"source reliability": true, "metadata": true, "tags": true,
	"category": true, "vendor": true, "date": true, "type": true,
	"subject": true, "from": true, "to": true, "cc": true, "bcc": true,
	"key attachments": true, "attachments": true,
}

// numericPrefixRe rejects headings that look like outline items —
// "1. Premise", "3. Anatomy", "12. Open questions". These are
// section pointers, not named entities.
var numericPrefixRe = regexp.MustCompile(`^\d+\.\s`)

// questionStarters reject headings that read as questions ("What is X?",
// "How to Y?"). Same as TS heuristic.
var questionStarters = map[string]bool{
	"what": true, "why": true, "how": true, "when": true, "where": true,
	"who": true, "which": true, "should": true, "can": true, "is": true,
	"are": true, "do": true, "does": true, "will": true, "would": true,
	"could": true,
}

// entityLengthMin/Max bound a candidate's character count. Inclusive
// at both ends. Ported from TS.
const (
	entityLengthMin = 3
	entityLengthMax = 50
)

var (
	headingRe = regexp.MustCompile(`(?m)^##\s+(.+?)\s*$`)
	boldRe    = regexp.MustCompile(`\*\*([^\*]+?)\*\*`)
)

// ExtractEntities pulls candidate named entities out of markdown body
// text using two heuristics:
//
//  1. Level-2 headings (## Foo) — unless the heading is a generic
//     section title, starts with a question word, or has 5+ words.
//  2. Bold terms (**Foo**) — must start with a capital letter, must
//     not contain sentence-ending punctuation.
//
// Returns a de-duplicated list preserving first-seen order. Same
// algorithm as src/vault/entities.ts so a vault re-synthesised under
// the Go server doesn't get a different entity set than under TS.
func ExtractEntities(body string) []string {
	seen := map[string]bool{}
	var out []string

	for _, m := range headingRe.FindAllStringSubmatch(body, -1) {
		c := candidateFromHeading(m[1])
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}

	for _, m := range boldRe.FindAllStringSubmatch(body, -1) {
		c := candidateFromBold(m[1])
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}

	return out
}

// candidateFromHeading returns the cleaned heading text if it passes
// all heuristic filters, otherwise "".
func candidateFromHeading(raw string) string {
	t := strings.TrimSpace(raw)
	if strings.HasSuffix(t, ":") {
		return ""
	}
	if !entityLengthOK(t) {
		return ""
	}
	if isGeneric(t) {
		return ""
	}
	if startsWithQuestion(t) {
		return ""
	}
	if wordCount(t) >= 5 {
		return ""
	}
	if numericPrefixRe.MatchString(t) {
		return ""
	}
	return t
}

// candidateFromBold returns the cleaned bold text if it passes the
// filters specific to bold matches, otherwise "".
func candidateFromBold(raw string) string {
	t := strings.TrimSpace(raw)
	if !entityLengthOK(t) {
		return ""
	}
	if isGeneric(t) {
		return ""
	}
	// First rune must be a capital letter — filters out **note:**,
	// **important:**, etc.
	first := []rune(t)
	if len(first) == 0 || !unicode.IsUpper(first[0]) {
		return ""
	}
	// Trailing colon = label ("Note:", "Important:") — not a name.
	if strings.HasSuffix(t, ":") {
		return ""
	}
	// Sentence punctuation suggests this is a sentence fragment, not
	// a name.
	if strings.ContainsAny(t, ".!?") {
		return ""
	}
	return t
}

// entityLengthOK returns true when the candidate's char count is in
// the configured window.
func entityLengthOK(t string) bool {
	n := len(t)
	return n >= entityLengthMin && n <= entityLengthMax
}

// isGeneric checks the deny-list (case-insensitive).
func isGeneric(t string) bool {
	return genericHeadingsSet[strings.ToLower(t)]
}

// startsWithQuestion reports whether the candidate's first word is a
// question starter (case-insensitive). Single-word candidates can't
// be questions, so they pass through here.
func startsWithQuestion(t string) bool {
	idx := strings.IndexFunc(t, unicode.IsSpace)
	if idx < 0 {
		return false
	}
	first := strings.ToLower(t[:idx])
	return questionStarters[first]
}

// wordCount returns the whitespace-separated word count of the
// candidate. Used to reject sentence-shaped headings.
func wordCount(t string) int {
	if strings.TrimSpace(t) == "" {
		return 0
	}
	return len(strings.Fields(t))
}

// EntitySnippet returns a ~1500-char window centred on the first
// mention of the entity in body, trimmed to sentence boundaries.
// Falls back to the first window of body when the entity isn't
// directly mentioned (matches the TS behaviour for cases where the
// heading text differs from the body mention).
func EntitySnippet(body, entity string) string {
	const window = 1500
	idx := strings.Index(strings.ToLower(body), strings.ToLower(entity))
	if idx < 0 {
		if len(body) <= window {
			return body
		}
		return trimToSentenceBoundary(body[:window])
	}
	start := idx - window/2
	if start < 0 {
		start = 0
	}
	end := start + window
	if end > len(body) {
		end = len(body)
	}
	return trimToSentenceBoundary(body[start:end])
}

// trimToSentenceBoundary chops off any trailing fragment after the
// last full-sentence terminator (., !, ?) so the snippet doesn't end
// mid-word. Falls through unchanged if no terminator is found.
func trimToSentenceBoundary(s string) string {
	last := -1
	for i, r := range s {
		if r == '.' || r == '!' || r == '?' {
			last = i
		}
	}
	if last < 0 {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(s[:last+1])
}
