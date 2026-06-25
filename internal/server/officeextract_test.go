package server

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"
)

// buildZip assembles an in-memory zip from a name→content map. Used
// to synthesize minimal-but-valid OOXML files for the extractor tests
// without any external tooling — fully deterministic.
func buildZip(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestOfficeExtractDocx(t *testing.T) {
	docXML := `<?xml version="1.0"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p><w:r><w:t>Hello</w:t></w:r><w:r><w:t xml:space="preserve"> world</w:t></w:r></w:p>
    <w:p><w:r><w:t>Second paragraph</w:t></w:r></w:p>
  </w:body>
</w:document>`
	body := buildZip(t, map[string]string{"word/document.xml": docXML})

	got, err := OfficeExtract(mimeDocx, "memo.docx", body)
	if err != nil {
		t.Fatalf("OfficeExtract docx: %v", err)
	}
	if !strings.Contains(got, "Hello world") {
		t.Errorf("docx text missing run concatenation: %q", got)
	}
	if !strings.Contains(got, "Second paragraph") {
		t.Errorf("docx text missing second paragraph: %q", got)
	}
	// <w:p> boundary should produce a newline between paragraphs.
	if !strings.Contains(got, "\n") {
		t.Errorf("docx text missing paragraph newline: %q", got)
	}
}

func TestOfficeExtractXlsx(t *testing.T) {
	shared := `<?xml version="1.0"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="2" uniqueCount="2">
  <si><t>Invoice Total</t></si>
  <si><t>Acme Corp</t></si>
</sst>`
	body := buildZip(t, map[string]string{"xl/sharedStrings.xml": shared})

	got, err := OfficeExtract(mimeXlsx, "sheet.xlsx", body)
	if err != nil {
		t.Fatalf("OfficeExtract xlsx: %v", err)
	}
	if !strings.Contains(got, "Invoice Total") || !strings.Contains(got, "Acme Corp") {
		t.Errorf("xlsx sharedStrings text missing: %q", got)
	}
}

func TestOfficeExtractPptx(t *testing.T) {
	slide1 := `<?xml version="1.0"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
  <a:t>Slide One Title</a:t><a:t>Bullet point</a:t>
</p:sld>`
	slide2 := `<?xml version="1.0"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
  <a:t>Slide Two</a:t>
</p:sld>`
	body := buildZip(t, map[string]string{
		"ppt/slides/slide1.xml": slide1,
		"ppt/slides/slide2.xml": slide2,
		// A rels file that must NOT be treated as a slide.
		"ppt/slides/_rels/slide1.xml.rels": `<Relationships/>`,
	})

	got, err := OfficeExtract(mimePptx, "deck.pptx", body)
	if err != nil {
		t.Fatalf("OfficeExtract pptx: %v", err)
	}
	for _, want := range []string{"Slide One Title", "Bullet point", "Slide Two"} {
		if !strings.Contains(got, want) {
			t.Errorf("pptx text missing %q in %q", want, got)
		}
	}
}

func TestOfficeExtractExtensionFallback(t *testing.T) {
	// MIME is the generic octet-stream the loader assigns to xlsx; the
	// extension must drive dispatch.
	shared := `<?xml version="1.0"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <si><t>FallbackText</t></si>
</sst>`
	body := buildZip(t, map[string]string{"xl/sharedStrings.xml": shared})

	got, err := OfficeExtract("application/octet-stream", "data.xlsx", body)
	if err != nil {
		t.Fatalf("OfficeExtract fallback: %v", err)
	}
	if !strings.Contains(got, "FallbackText") {
		t.Errorf("extension-fallback dispatch failed: %q", got)
	}
}

func TestOfficeExtractLegacyBinary(t *testing.T) {
	// .doc / .xls / .ppt are out of scope — sentinel, not crash.
	for _, tc := range []struct {
		mime, name string
	}{
		{mimeDoc, "old.doc"},
		{mimeXls, "old.xls"},
		{mimePpt, "old.ppt"},
		{"application/octet-stream", "viaext.doc"},
	} {
		got, err := OfficeExtract(tc.mime, tc.name, []byte("\xD0\xCF\x11\xE0garbage"))
		if !errors.Is(err, ErrUnsupportedLegacyOffice) {
			t.Errorf("%s: want ErrUnsupportedLegacyOffice, got err=%v", tc.name, err)
		}
		if got != "" {
			t.Errorf("%s: want empty text, got %q", tc.name, got)
		}
	}
}

func TestOfficeExtractUnsupportedType(t *testing.T) {
	// A type we don't handle returns ("", nil) — caller treats as no-op.
	got, err := OfficeExtract("image/png", "pic.png", []byte("not a zip"))
	if err != nil {
		t.Errorf("unsupported type should not error, got %v", err)
	}
	if got != "" {
		t.Errorf("unsupported type should return empty, got %q", got)
	}
}

func TestOfficeExtractMalformedZip(t *testing.T) {
	// docx mime but body isn't a zip — error, soft-fail at caller.
	_, err := OfficeExtract(mimeDocx, "broken.docx", []byte("totally not a zip archive"))
	if err == nil {
		t.Error("malformed docx should return an error")
	}
}

func TestOfficeExtractMissingPart(t *testing.T) {
	// Valid zip, docx mime, but no word/document.xml part — empty, no error.
	body := buildZip(t, map[string]string{"docProps/core.xml": `<x/>`})
	got, err := OfficeExtract(mimeDocx, "empty.docx", body)
	if err != nil {
		t.Errorf("missing part should not error, got %v", err)
	}
	if got != "" {
		t.Errorf("missing part should yield empty text, got %q", got)
	}
}
