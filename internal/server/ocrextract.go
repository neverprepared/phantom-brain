package server

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ocrTimeout bounds a single tesseract invocation on one image.
// OCR is materially slower than pdftotext's parse — a dense
// full-page scan can take tens of seconds — so the cap is wider
// than pdftotextTimeout while still short enough to keep the synth
// worker draining when a pathological image hangs the engine.
const ocrTimeout = 120 * time.Second

// scannedPDFTimeout bounds the whole rasterize-then-OCR pipeline
// for an image-only PDF. It covers pdftoppm's rasterization of up
// to scannedPDFMaxPages pages plus a tesseract pass over each, so
// it's wider than ocrTimeout.
const scannedPDFTimeout = 300 * time.Second

// scannedPDFMaxPages caps how many pages of a scanned PDF we OCR.
// Each page is a full tesseract pass (slow), so an unbounded scan
// could stall the worker for minutes on a large document. The first
// 20 pages capture the bulk of any practical document's searchable
// content; deeper pages can be re-enriched later via brain_reflect.
const scannedPDFMaxPages = 20

// OCRAvailable reports whether the tesseract OCR binary is on PATH.
// Mirrors PdftotextAvailable's snapshot-at-startup pattern — the
// worker probes once and gates per-job OCR calls on the result, so
// dev/CI environments without tesseract degrade to empty bodies
// instead of failing the pipeline.
func OCRAvailable() bool {
	_, err := exec.LookPath("tesseract")
	return err == nil
}

// OCRExtractImage shells out to `tesseract - - -l eng` (stdin image
// to stdout text). Used for raster image attachments (png/jpeg/etc.)
// that carry no embedded text. The output is truncated to
// maxExtractedTextBytes with a marker appended when cut.
//
// Tolerant of garbage input: a returned error is logged at Warn by
// the caller and the attachment keeps an empty ExtractedText —
// never blocks the synth pipeline.
func OCRExtractImage(ctx context.Context, body []byte) (string, error) {
	if len(body) == 0 {
		return "", nil
	}
	cctx, cancel := context.WithTimeout(ctx, ocrTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "tesseract", "-", "-", "-l", "eng")
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Include the first chunk of stderr — tesseract's diagnostics
		// (e.g. "Image too small to scale") help triage but can be noisy.
		serr := stderr.String()
		if len(serr) > 256 {
			serr = serr[:256] + "..."
		}
		return "", fmt.Errorf("tesseract: %w (stderr: %s)", err, serr)
	}
	return truncateText(stdout.String(), maxExtractedTextBytes), nil
}

// OCRExtractScannedPDF handles image-only (scanned) PDFs that
// pdftotext can't pull text from: rasterize each page to PNG with
// poppler's pdftoppm, then OCR each page with tesseract and
// concatenate. Bounded to scannedPDFMaxPages pages and
// scannedPDFTimeout overall to keep the cost finite.
//
// Both pdftoppm and tesseract must be on PATH; callers gate on
// PdftotextAvailable + OCRAvailable. Soft-fail: a returned error is
// logged and the attachment keeps an empty body.
func OCRExtractScannedPDF(ctx context.Context, body []byte) (string, error) {
	if len(body) == 0 {
		return "", nil
	}
	cctx, cancel := context.WithTimeout(ctx, scannedPDFTimeout)
	defer cancel()

	// Scratch dir for the source PDF + the rasterized PNG pages. Cleaned
	// up unconditionally — we never leave temp files behind.
	dir, err := os.MkdirTemp("", "pb-ocr-pdf-*")
	if err != nil {
		return "", fmt.Errorf("ocr scanned pdf: mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	pdfPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(pdfPath, body, 0o600); err != nil {
		return "", fmt.Errorf("ocr scanned pdf: write pdf: %w", err)
	}

	// pdftoppm -png -r 150 in.pdf <dir>/page  →  page-1.png, page-2.png ...
	// 150 DPI is a reasonable OCR/cost tradeoff; -l caps the page count so
	// pdftoppm itself never rasterizes past our budget.
	prefix := filepath.Join(dir, "page")
	ppm := exec.CommandContext(cctx, "pdftoppm", "-png", "-r", "150",
		"-l", fmt.Sprintf("%d", scannedPDFMaxPages), pdfPath, prefix)
	var ppmErr bytes.Buffer
	ppm.Stderr = &ppmErr
	if err := ppm.Run(); err != nil {
		serr := ppmErr.String()
		if len(serr) > 256 {
			serr = serr[:256] + "..."
		}
		return "", fmt.Errorf("pdftoppm: %w (stderr: %s)", err, serr)
	}

	// Collect the generated PNGs in page order. pdftoppm zero-pads the
	// suffix when there are many pages, so a lexical sort matches page
	// order for any consistent padding width.
	pngs, err := filepath.Glob(prefix + "*.png")
	if err != nil {
		return "", fmt.Errorf("ocr scanned pdf: glob pages: %w", err)
	}
	if len(pngs) == 0 {
		// pdftoppm produced nothing — empty or non-rasterizable PDF.
		return "", nil
	}
	sort.Strings(pngs)
	if len(pngs) > scannedPDFMaxPages {
		pngs = pngs[:scannedPDFMaxPages]
	}

	var sb strings.Builder
	for _, png := range pngs {
		pageBytes, rerr := os.ReadFile(png)
		if rerr != nil {
			// One unreadable page shouldn't sink the whole document.
			continue
		}
		// Reuse the single-image OCR path so timeout/truncation behaviour
		// is consistent. The per-page truncation cap is fine — the
		// concatenated result is truncated again below.
		pageText, oerr := OCRExtractImage(cctx, pageBytes)
		if oerr != nil {
			// Soft per-page: skip a page tesseract chokes on, keep going.
			continue
		}
		if strings.TrimSpace(pageText) == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(pageText)
		if sb.Len() >= maxExtractedTextBytes {
			break // already past the storage cap; stop paying OCR cost
		}
	}
	return truncateText(sb.String(), maxExtractedTextBytes), nil
}
