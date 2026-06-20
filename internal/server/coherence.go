package server

import (
	"fmt"
	"unicode"
)

// CoherenceResult mirrors the TS shape ({passed, reason?}). When
// Passed is false, Reason carries a one-line explanation suitable
// for surfacing to the operator in the summary frontmatter.
type CoherenceResult struct {
	Passed bool
	Reason string
}

// Coherence limits ported from src/validation/coherence.ts:1-9.
const (
	coherenceMinChars = 10
	coherenceMaxChars = 50_000
	coherenceMinLetterRun = 3 // 3+ consecutive letters required
)

// CheckCoherence is the cheap structural guard the synthesizer runs
// before paying for an LLM call. Rejects: too-short content, too-long
// content, content that's pure whitespace/punctuation. Failure is a
// signal — the synthesizer flips the verdict to low + "coherence-fail"
// rather than calling the gate at all.
func CheckCoherence(content string) CoherenceResult {
	n := len(content)
	if n < coherenceMinChars {
		return CoherenceResult{Reason: fmt.Sprintf("content too short (%d < %d chars)", n, coherenceMinChars)}
	}
	if n > coherenceMaxChars {
		return CoherenceResult{Reason: fmt.Sprintf("content too long (%d > %d chars)", n, coherenceMaxChars)}
	}
	if !hasConsecutiveLetters(content, coherenceMinLetterRun) {
		return CoherenceResult{Reason: fmt.Sprintf("no run of %d+ consecutive letters", coherenceMinLetterRun)}
	}
	return CoherenceResult{Passed: true}
}

// hasConsecutiveLetters scans content for a run of at least n
// consecutive Unicode letters. We use unicode.IsLetter rather than
// ASCII-only [a-z] so non-English content (which we do ingest)
// doesn't trip the check.
func hasConsecutiveLetters(s string, n int) bool {
	run := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			run++
			if run >= n {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}
