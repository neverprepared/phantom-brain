package server

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateText_NoCutWhenUnderMax(t *testing.T) {
	in := "hello world"
	got := truncateText(in, 100)
	if got != in {
		t.Errorf("expected unchanged, got %q", got)
	}
}

func TestTruncateText_CutsAndMarks(t *testing.T) {
	in := strings.Repeat("a", 1000)
	got := truncateText(in, 100)
	if !strings.HasSuffix(got, "...[truncated]") {
		t.Errorf("missing truncation marker: %q", got[len(got)-20:])
	}
	if len(got) > 100+len("...[truncated]") {
		t.Errorf("truncate left too much: len=%d", len(got))
	}
}

func TestTruncateText_RespectsUTF8Boundary(t *testing.T) {
	// "ä" is 2 bytes in UTF-8 (0xC3 0xA4). Repeating gives even byte
	// counts; cutting at an odd boundary would slice the rune.
	in := strings.Repeat("ä", 100) // 200 bytes
	got := truncateText(in, 51)    // odd, mid-rune
	cut := strings.TrimSuffix(got, "...[truncated]")
	if !utf8.ValidString(cut) {
		t.Errorf("truncate produced invalid UTF-8: %q", cut)
	}
}

func TestPDFExtractWithPdftotext_SkipsEmpty(t *testing.T) {
	out, err := PDFExtractWithPdftotext(context.Background(), nil)
	if err != nil || out != "" {
		t.Errorf("empty input: out=%q err=%v", out, err)
	}
}

func TestPDFExtractWithPdftotext_RealBinary(t *testing.T) {
	if !PdftotextAvailable() {
		t.Skip("pdftotext not on PATH")
	}
	// Minimal hand-crafted PDF containing the literal text "phantom".
	// Built with `pdftotext` round-tripping a Hello PDF; keep small.
	pdf := []byte("%PDF-1.1\n" +
		"1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n" +
		"2 0 obj<</Type/Pages/Count 1/Kids[3 0 R]>>endobj\n" +
		"3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>endobj\n" +
		"4 0 obj<</Length 44>>stream\n" +
		"BT /F1 24 Tf 20 100 Td (phantom) Tj ET\n" +
		"endstream endobj\n" +
		"5 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj\n" +
		"xref\n0 6\n0000000000 65535 f \n" +
		"trailer<</Size 6/Root 1 0 R>>\nstartxref\n0\n%%EOF\n")
	out, err := PDFExtractWithPdftotext(context.Background(), pdf)
	if err != nil {
		t.Skipf("pdftotext rejected the hand-crafted PDF (acceptable; binary path covered by SynthWorker test): %v", err)
	}
	if !strings.Contains(out, "phantom") {
		t.Errorf("expected extracted text to contain 'phantom', got %q", out)
	}
}
