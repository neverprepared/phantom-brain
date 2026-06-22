package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/neverprepared/mcp-phantom-brain/internal/brain"
	"github.com/neverprepared/mcp-phantom-brain/internal/canonicalize"
	"github.com/neverprepared/mcp-phantom-brain/internal/index"
	"github.com/neverprepared/mcp-phantom-brain/internal/vault"
)

// attachTool defines the brain_attach MCP tool schema.
//
// brain_attach ingests a binary file (PDF, image, .docx, …) into the
// brain. Storage scheme:
//
//	Raw/attachments/<sha>[ext]   the raw bytes, named by sha256
//	Raw/attachments/<sha>.md     a searchable stub with title, description,
//	                             attached_at, and any source URL
//
// The stub is what gets indexed for FTS5 + vector search; the binary
// itself is opaque to the index. PDF/text extraction is deferred to
// the synthesizer (Phase 2) so we don't shell out to pdftotext at
// ingest time from the agent process.
func attachTool() mcp.Tool {
	return mcp.NewTool("brain_attach",
		mcp.WithDescription(
			`Attach a binary file (PDF, image, .docx, etc.) to the brain. Stores by `+
				`SHA256 under Raw/attachments/ and writes a searchable markdown stub. `+
				`Text extraction (PDF -> text) happens later at synthesis time.`,
		),
		mcp.WithString("file_path",
			mcp.Required(),
			mcp.Description("Absolute path to the local file to attach. Caller must have read permission."),
		),
		mcp.WithString("title",
			mcp.Required(),
			mcp.Description("Human-readable title for the attachment."),
		),
		mcp.WithString("description",
			mcp.Description("Optional prose description. Indexed for FTS5 + vector recall. Pass at least a sentence — empty descriptions are searchable only by filename and title."),
		),
		mcp.WithString("source_url",
			mcp.Description("URL the file came from, when known."),
		),
	)
}

func (s *Server) handleAttach(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	filePath, err := req.RequireString("file_path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if strings.TrimSpace(filePath) == "" {
		return mcp.NewToolResultError("file_path must be non-empty"), nil
	}
	title, err := req.RequireString("title")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if strings.TrimSpace(title) == "" {
		return mcp.NewToolResultError("title must be non-empty"), nil
	}
	description, _ := req.RequireString("description")
	sourceURL, _ := req.RequireString("source_url")

	// Read the binary. Size cap at 100 MB so a misuse can't OOM the
	// MCP server — well above any realistic single attachment.
	st, err := os.Stat(filePath)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("stat %q: %v", filePath, err)), nil
	}
	if st.Size() > 100*1024*1024 {
		return mcp.NewToolResultError(fmt.Sprintf("file too large (%d bytes; max 100 MB)", st.Size())), nil
	}
	if st.IsDir() {
		return mcp.NewToolResultError(fmt.Sprintf("path is a directory: %q", filePath)), nil
	}

	raw, err := os.ReadFile(filePath)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read: %v", err)), nil
	}

	// Bytewise SHA on the BLOB (not canonicalize): binary content has
	// no canonical form. Stable identity is the raw bytes.
	h := sha256.Sum256(raw)
	blobSHA := hex.EncodeToString(h[:])

	ext := strings.ToLower(filepath.Ext(filePath))
	blobName := blobSHA + ext
	blobRel := filepath.Join("Raw", "attachments", blobName)
	blobAbs := filepath.Join(s.deps.VaultDir, blobRel)
	stubRel := filepath.Join("Raw", "attachments", blobSHA+".md")
	stubAbs := filepath.Join(s.deps.VaultDir, stubRel)

	// Build the stub markdown first so its canonical SHA is the
	// index key (consistent with how brain_perceive / brain_learn key
	// off the rendered markdown SHA). The blob SHA goes into
	// frontmatter as metadata.
	fm := map[string]any{
		"title":       title,
		"attached_at": time.Now().UTC().Format(time.RFC3339),
		"blob_sha":    blobSHA,
		"blob_path":   blobRel,
		"bytes":       st.Size(),
	}
	if sourceURL != "" {
		fm["source_url"] = sourceURL
	}
	if ext != "" {
		fm["ext"] = ext
	}

	doc := &vault.Document{Frontmatter: fm, Body: description}
	rendered, err := doc.Render()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("render stub: %v", err)), nil
	}

	stubSHA, err := canonicalize.Sum(rendered)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("canonicalize stub: %v", err)), nil
	}

	// Dedup by blob — bytes are content-addressed; the stub SHA
	// depends on attached_at (RFC3339, second precision), so two
	// back-to-back attaches that straddle a second boundary would
	// generate different stub SHAs and bypass the index-based check.
	// A stat on the blob path is cheap and exact.
	if _, err := os.Stat(blobAbs); err == nil {
		return mcp.NewToolResultText(fmt.Sprintf("Duplicate (blob already stored). Blob SHA: %s. Stub: %s.", blobSHA, stubSHA)), nil
	}
	if has, err := s.deps.Index.Has(stubSHA); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("index has: %v", err)), nil
	} else if has {
		return mcp.NewToolResultText(fmt.Sprintf("Duplicate (stub already indexed). SHA: %s. Blob: %s.", stubSHA, blobSHA)), nil
	}

	// Embed (title + description). Empty description doesn't trip the
	// embedder — title-only is a valid (if weak) input.
	if s.deps.Embedder.Dims() != s.deps.Index.Dims() {
		return mcp.NewToolResultError(fmt.Sprintf("embedder/index dim mismatch: embedder=%d index=%d",
			s.deps.Embedder.Dims(), s.deps.Index.Dims())), nil
	}
	embInput := strings.TrimSpace(title + "\n\n" + description)
	embs, err := s.deps.Embedder.Embed(ctx, []string{embInput})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("embed: %v", err)), nil
	}

	// Phase 6: hand the blob + metadata to the daemon. Daemon stores
	// the bytes in MinIO and indexes metadata + extracted text (if
	// any) in OS. Day 7 keeps text extraction agent-side as empty —
	// the daemon's gate pass can later run pdftotext/textutil on the
	// stored blob. For now the description carries the only
	// searchable text.
	if client := lifecycleClient(s); client != nil {
		mimeType := guessMIMEType(ext)
		if _, err := client.Attach(ctx, brain.AttachRequest{
			SHA:              blobSHA,
			OriginalFilename: filepath.Base(filePath),
			Title:            title,
			MIMEType:         mimeType,
			BytesB64:         base64.StdEncoding.EncodeToString(raw),
			ExtractedText:    description,
			Embedding:        embs[0],
		}); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("daemon attach: %v", err)), nil
		}
		if s.deps.Lifecycle != nil {
			s.deps.Lifecycle.RecordWrite()
		}
		return mcp.NewToolResultText(fmt.Sprintf(
			"Attached %d bytes via daemon. Blob SHA: %s.",
			st.Size(), blobSHA,
		)), nil
	}

	// Legacy fallback: no daemon — old local file + index pipeline.
	if err := vault.WriteAtomicFile(blobAbs, raw, 0o644); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("write blob: %v", err)), nil
	}
	if err := vault.WriteAtomicFile(stubAbs, rendered, 0o644); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("write stub: %v", err)), nil
	}
	if err := s.deps.Index.Upsert(ctx, index.Record{
		SHA:        stubSHA,
		SourcePath: stubRel,
		Title:      title,
		Tags:       "attachment " + strings.TrimPrefix(ext, "."),
		Body:       description,
		Embedding:  embs[0],
	}); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("index upsert: %v", err)), nil
	}
	if s.deps.Lifecycle != nil {
		s.deps.Lifecycle.RecordWrite()
	}
	return mcp.NewToolResultText(fmt.Sprintf(
		"Attached %d bytes to %s. Stub: %s. Blob SHA: %s.",
		st.Size(), stubRel, stubSHA, blobSHA,
	)), nil
}

// guessMIMEType maps a file extension to a coarse MIME type. Used by
// the agent-side attach handler to populate metadata for the daemon's
// OS doc; not authoritative — daemons that want server-side
// detection should run libmagic.
func guessMIMEType(ext string) string {
	switch strings.ToLower(ext) {
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".txt":
		return "text/plain"
	case ".md":
		return "text/markdown"
	case ".html", ".htm":
		return "text/html"
	case ".json":
		return "application/json"
	case ".csv":
		return "text/csv"
	case ".doc":
		return "application/msword"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	}
	return "application/octet-stream"
}
