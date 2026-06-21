package vault

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// MaxSlugLength is the cap on Slug() output. Matches the v4.x
// TypeScript MAX_SLUG_LENGTH so filenames generated before and after
// the cut-over stay comparable.
const MaxSlugLength = 60

// Slug derives a filesystem-safe identifier from an arbitrary string.
//
// Pipeline (in order):
//
//  1. NFKD normalize so accented characters decompose to base + mark
//     (é -> e + combining acute).
//  2. Drop combining marks (categories Mn) — this strips the accents,
//     leaving just the base ASCII letter.
//  3. Lowercase.
//  4. Replace any byte that isn't [a-z0-9] with a hyphen.
//  5. Collapse runs of hyphens to a single hyphen.
//  6. Trim leading and trailing hyphens.
//  7. Truncate to MaxSlugLength, then re-trim trailing hyphens (the
//     truncation may have landed mid-hyphen-run).
//
// Empty/whitespace input or input that contains only unsupported
// runes yields "" — caller decides whether to fall back to a uuid.
func Slug(s string) string {
	// NFKD + strip combining marks via golang.org/x/text/transform.
	t := transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)))
	folded, _, err := transform.String(t, s)
	if err != nil {
		folded = s
	}

	var b strings.Builder
	b.Grow(len(folded))
	lastHyphen := false
	for _, r := range folded {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			lastHyphen = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}

	out := strings.Trim(b.String(), "-")
	if len(out) > MaxSlugLength {
		out = strings.TrimRight(out[:MaxSlugLength], "-")
	}
	return out
}
