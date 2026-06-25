package server

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// ErrUnsupportedLegacyOffice flags the pre-OOXML binary office
// formats (.doc / .xls / .ppt). They are compound-document (OLE2)
// blobs, not zip+XML, so the pure-Go path here can't read them and
// we deliberately do NOT pull in libreoffice/antiword. The caller
// logs this once and leaves the attachment with an empty body.
var ErrUnsupportedLegacyOffice = errors.New("unsupported legacy binary office format")

// OOXML mime types we extract. Modern Office files are zip archives
// of XML parts; we crack the zip and pull the user-visible text.
const (
	mimeDocx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeXlsx = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	mimePptx = "application/vnd.openxmlformats-officedocument.presentationml.presentation"

	mimeDoc = "application/msword"
	mimeXls = "application/vnd.ms-excel"
	mimePpt = "application/vnd.ms-powerpoint"
)

// OfficeExtract pulls plain text out of a modern OOXML office file
// using only the stdlib (archive/zip + encoding/xml) — no system
// deps, so it's always available regardless of the host environment.
//
// Dispatch is by mime first, falling back to the filename extension
// when mime is empty or the generic octet-stream the bulk loader
// assigns to types it doesn't recognise.
//
// Returns:
//   - extracted text (possibly truncated) for docx/xlsx/pptx
//   - ("", ErrUnsupportedLegacyOffice) for .doc/.xls/.ppt
//   - ("", nil) for anything else (caller treats as "nothing to do")
//
// A malformed-but-OOXML-shaped file returns an error (logged Warn,
// non-fatal); a recognised type that simply has no text returns "".
func OfficeExtract(mime, filename string, body []byte) (string, error) {
	switch officeKind(mime, filename) {
	case "docx":
		return extractDocx(body)
	case "xlsx":
		return extractXlsx(body)
	case "pptx":
		return extractPptx(body)
	case "legacy":
		return "", ErrUnsupportedLegacyOffice
	default:
		return "", nil
	}
}

// officeKind resolves the format to a coarse tag. Mime wins; on a
// missing/generic mime we fall back to the filename extension, which
// is how octet-stream-tagged xlsx/pptx attachments still get routed.
func officeKind(mime, filename string) string {
	switch mime {
	case mimeDocx:
		return "docx"
	case mimeXlsx:
		return "xlsx"
	case mimePptx:
		return "pptx"
	case mimeDoc, mimeXls, mimePpt:
		return "legacy"
	}
	switch strings.ToLower(path.Ext(filename)) {
	case ".docx":
		return "docx"
	case ".xlsx":
		return "xlsx"
	case ".pptx":
		return "pptx"
	case ".doc", ".xls", ".ppt":
		return "legacy"
	}
	return ""
}

// openZip wraps the body in a zip reader. OOXML files are zip
// archives; a parse failure here means the bytes aren't a valid
// OOXML container (truncated upload, wrong type) — surfaced as an
// error the caller logs and tolerates.
func openZip(body []byte) (*zip.Reader, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("office: open zip: %w", err)
	}
	return zr, nil
}

// zipPart reads the named entry from the archive in full. Returns
// (nil, nil) when the part is absent — some valid files legitimately
// omit a part (e.g. an xlsx with no shared strings), which is not an
// error, just "no text here".
func zipPart(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("office: open part %s: %w", name, err)
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				return nil, fmt.Errorf("office: read part %s: %w", name, err)
			}
			return b, nil
		}
	}
	return nil, nil
}

// extractDocx reads word/document.xml and concatenates the text in
// <w:t> runs, inserting a newline at each <w:p> paragraph boundary so
// the stored text keeps rough line structure.
func extractDocx(body []byte) (string, error) {
	zr, err := openZip(body)
	if err != nil {
		return "", err
	}
	part, err := zipPart(zr, "word/document.xml")
	if err != nil {
		return "", err
	}
	if part == nil {
		return "", nil
	}
	text, err := collectRuns(part, "t", "p")
	if err != nil {
		return "", fmt.Errorf("office: docx: %w", err)
	}
	return truncateText(text, maxExtractedTextBytes), nil
}

// extractXlsx reads xl/sharedStrings.xml and concatenates the <t>
// elements. Shared strings hold the bulk of a sheet's textual cell
// content; numeric cells are stored inline in the worksheets and are
// intentionally out of scope (low search value, high parse cost).
func extractXlsx(body []byte) (string, error) {
	zr, err := openZip(body)
	if err != nil {
		return "", err
	}
	part, err := zipPart(zr, "xl/sharedStrings.xml")
	if err != nil {
		return "", err
	}
	if part == nil {
		// A sheet with only inline/numeric data has no sharedStrings part.
		return "", nil
	}
	// No paragraph boundary in sharedStrings; each <si>/<t> is one string.
	// Newline-join on <t> keeps distinct cell strings on separate lines.
	text, err := collectRuns(part, "t", "")
	if err != nil {
		return "", fmt.Errorf("office: xlsx: %w", err)
	}
	return truncateText(text, maxExtractedTextBytes), nil
}

// extractPptx reads every ppt/slides/slideN.xml part and concatenates
// the <a:t> text runs across all slides.
func extractPptx(body []byte) (string, error) {
	zr, err := openZip(body)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, f := range zr.File {
		// Match ppt/slides/slide<N>.xml but not the slideLayouts/
		// slideMasters/ subtrees or rels files.
		if !strings.HasPrefix(f.Name, "ppt/slides/slide") ||
			!strings.HasSuffix(f.Name, ".xml") ||
			strings.Contains(f.Name, "/_rels/") {
			continue
		}
		part, err := zipPart(zr, f.Name)
		if err != nil {
			return "", err
		}
		if part == nil {
			continue
		}
		slideText, err := collectRuns(part, "t", "")
		if err != nil {
			return "", fmt.Errorf("office: pptx %s: %w", f.Name, err)
		}
		if strings.TrimSpace(slideText) == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(slideText)
	}
	return truncateText(sb.String(), maxExtractedTextBytes), nil
}

// collectRuns streams the XML and gathers CharData inside every
// element whose local name == textLocal (namespace-agnostic, so
// <w:t>, <a:t>, and <t> all match on Local "t"). When breakLocal is
// non-empty, the close of each such element inserts a newline — used
// to mark <w:p> paragraph boundaries in docx. Otherwise runs are
// joined with a newline per text element.
func collectRuns(xmlBytes []byte, textLocal, breakLocal string) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	var sb strings.Builder
	inText := 0 // nesting depth of the target text element (usually 0/1)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("xml decode: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == textLocal {
				inText++
			}
		case xml.EndElement:
			if t.Name.Local == textLocal && inText > 0 {
				inText--
				if breakLocal == "" {
					// One text element per line when there's no paragraph
					// element to key newlines off of.
					sb.WriteString("\n")
				}
			}
			if breakLocal != "" && t.Name.Local == breakLocal {
				sb.WriteString("\n")
			}
		case xml.CharData:
			if inText > 0 {
				sb.Write(t)
			}
		}
	}
	// Collapse the inevitable run of blank lines into single newlines and
	// trim — keeps the stored text tidy without dropping word boundaries.
	return strings.TrimSpace(squeezeBlankLines(sb.String())), nil
}

// squeezeBlankLines collapses runs of 2+ newlines down to a single
// newline. The streaming collector emits a newline per text element
// and per paragraph close, which can stack; this keeps the output
// readable.
func squeezeBlankLines(s string) string {
	var sb strings.Builder
	var prevNL bool
	for _, r := range s {
		if r == '\n' {
			if prevNL {
				continue
			}
			prevNL = true
		} else {
			prevNL = false
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
