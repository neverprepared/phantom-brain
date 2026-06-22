package server

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// PDFExtractor converts a raw PDF byte stream into plain text. The
// SynthWorker holds one of these and calls it for attachment_stub
// docs whose MIMEType is application/pdf and whose ExtractedText is
// still empty (agent-side extraction missed the file, or the agent
// couldn't run poppler-utils in its environment).
//
// Implementations should be tolerant of malformed PDFs: a returned
// error gets logged at Warn and the doc keeps an empty
// ExtractedText — never blocks the synth pipeline.
type PDFExtractor func(ctx context.Context, body []byte) (string, error)

// maxExtractedTextBytes caps the stored text per attachment. 500 KB
// of UTF-8 is ~80k words — enough to make any practical PDF
// searchable while keeping the OS doc bounded.
const maxExtractedTextBytes = 500 * 1024

// pdftotextTimeout bounds a single subprocess invocation. PDFs with
// pathological cross-reference tables can hang pdftotext for
// minutes; 60s is well over a reasonable PDF's parse time and short
// enough to keep the worker draining.
const pdftotextTimeout = 60 * time.Second

// PdftotextAvailable reports whether the poppler-utils `pdftotext`
// binary is on PATH. Mirrors ClaudeCLIAvailable's snapshot-at-startup
// pattern — the worker probes once and gates per-job calls on the
// result, so dev environments without poppler don't fail the
// pipeline.
func PdftotextAvailable() bool {
	_, err := exec.LookPath("pdftotext")
	return err == nil
}

// PDFExtractWithPdftotext shells out to `pdftotext -layout -nopgbrk
// - -` (stdin to stdout). -layout preserves rough column structure
// (better for tables); -nopgbrk drops form-feed page separators
// which serve no purpose in a search index. The output is truncated
// to maxExtractedTextBytes with a marker appended when cut.
func PDFExtractWithPdftotext(ctx context.Context, body []byte) (string, error) {
	if len(body) == 0 {
		return "", nil
	}
	cctx, cancel := context.WithTimeout(ctx, pdftotextTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "pdftotext", "-layout", "-nopgbrk", "-", "-")
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Include the first chunk of stderr — poppler's diagnostics are
		// useful but can be noisy on weird PDFs.
		serr := stderr.String()
		if len(serr) > 256 {
			serr = serr[:256] + "..."
		}
		return "", fmt.Errorf("pdftotext: %w (stderr: %s)", err, serr)
	}
	return truncateText(stdout.String(), maxExtractedTextBytes), nil
}

// truncateText cuts s at max bytes on a UTF-8 boundary (scan back
// at most 3 bytes — UTF-8 sequences are 1-4 bytes) and appends a
// "...[truncated]" marker so a recall hit on the marker signals
// "the original was longer than we stored".
func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	// While s[cut] is a continuation byte (10xxxxxx), back up so the
	// slice ends on a complete rune. Bound the scan to 3 bytes — past
	// that we're at the start of a multi-byte sequence and cutting is
	// safe regardless.
	for i := 0; i < 3 && cut > 0 && s[cut]&0xC0 == 0x80; i++ {
		cut--
	}
	return s[:cut] + "...[truncated]"
}
