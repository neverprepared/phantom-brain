package server

import (
	"context"
	"strings"
	"testing"
)

func TestOCRAvailableNoPanic(t *testing.T) {
	// Just exercise the probe; result is environment-dependent (CI may
	// not have tesseract). The contract is "returns a bool, never panics".
	_ = OCRAvailable()
}

func TestOCRExtractImageEmptyInput(t *testing.T) {
	// Empty input short-circuits before ever invoking tesseract, so this
	// is deterministic regardless of whether the binary is installed.
	got, err := OCRExtractImage(context.Background(), nil)
	if err != nil {
		t.Fatalf("empty image OCR: unexpected err %v", err)
	}
	if got != "" {
		t.Errorf("empty image OCR: want empty, got %q", got)
	}
}

func TestOCRExtractScannedPDFEmptyInput(t *testing.T) {
	got, err := OCRExtractScannedPDF(context.Background(), nil)
	if err != nil {
		t.Fatalf("empty scanned-pdf OCR: unexpected err %v", err)
	}
	if got != "" {
		t.Errorf("empty scanned-pdf OCR: want empty, got %q", got)
	}
}

func TestOCRTruncateReuse(t *testing.T) {
	// OCR paths share truncateText with the PDF extractor; verify the
	// shared helper behaves (over-cap input gets the marker).
	long := strings.Repeat("a", maxExtractedTextBytes+100)
	out := truncateText(long, maxExtractedTextBytes)
	if !strings.HasSuffix(out, "...[truncated]") {
		t.Errorf("truncateText should append marker on over-cap input")
	}
	if len(out) > maxExtractedTextBytes+len("...[truncated]") {
		t.Errorf("truncateText output too long: %d", len(out))
	}
}

func TestOCRExtractImageRealEngine(t *testing.T) {
	if !OCRAvailable() {
		t.Skip("tesseract not installed; skipping real-engine OCR test")
	}
	// We don't ship a fixture image; this guard just documents that when
	// tesseract IS present, garbage bytes produce an error (soft-fail at
	// the caller), not a panic.
	_, err := OCRExtractImage(context.Background(), []byte("not a real image"))
	if err == nil {
		t.Log("tesseract accepted garbage input without error (engine-dependent)")
	}
}
