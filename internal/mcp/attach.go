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

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/brain/wqueue"
	"github.com/neverprepared/phantom-brain/internal/canonicalize"
	"github.com/neverprepared/phantom-brain/internal/osearch"
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
		mcp.WithString("content_type",
			mcp.Description("MIME type override (e.g. \"application/pdf\"). When empty, the agent guesses from the file extension."),
		),
		mcp.WithArray("tags",
			mcp.Description("Free-form labels for faceting/recall, e.g. \"vendor:UIA\", \"invoice\"."),
			mcp.Items(map[string]any{"type": "string"}),
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
	contentType, _ := req.RequireString("content_type")
	tags, _ := req.RequireStringSlice("tags")

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

	// Phase D2b: no local read cache, so no local stub markdown and no
	// local dedup. The daemon builds the searchable record from the
	// metadata + bytes below and SHA-dedups idempotently, so a re-attach
	// lands as a benign no-op upsert daemon-side.

	// Embed (title + description). Empty description doesn't trip the
	// embedder — title-only is a valid (if weak) input.
	embInput := strings.TrimSpace(title + "\n\n" + description)
	embs, err := s.deps.Embedder.Embed(ctx, []string{embInput})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("embed: %v", err)), nil
	}

	// Phase D2b: writes are daemon-only. The daemon stores the bytes in
	// MinIO and indexes metadata + extracted text (if any) in the
	// Postgres SoR. A nil client means the agent contract isn't
	// configured — return a clear error rather than dropping the write.
	client := lifecycleClient(s)
	if client == nil {
		return mcp.NewToolResultError("daemon client not configured (set CL_BRAIN_API / CL_BRAIN_API_TOKEN); writes are daemon-only"), nil
	}
	{
		mimeType := strings.TrimSpace(contentType)
		if mimeType == "" {
			mimeType = guessMIMEType(ext)
		}
		// v2.4: attachment is stamped as attachment_stub, semantic.
		// Source carries the original local path so the operator can
		// trace back where the file came from.
		mf := brain.MemoryFields{
			Kind:       string(osearch.KindAttachmentStub),
			MemoryType: string(osearch.MemorySemantic),
			CapturedAt: timePtr(time.Now().UTC()),
			Source:     []string{filePath},
		}
		if sourceURL != "" {
			mf.Source = append(mf.Source, sourceURL)
		}
		// Queue payload omits BytesB64 — bytes are staged to disk
		// by wqueue under <vault>/wqueue-attach/<sha><ext> so the
		// sqlite row stays KB-scale. The live attempt below uses
		// liveReq with BytesB64 populated.
		queueReq := brain.AttachRequest{
			SHA:              blobSHA,
			OriginalFilename: canonicalize.Filename(filepath.Base(filePath)),
			Title:            title,
			MIMEType:         mimeType,
			Description:      description,
			Tags:             tags,
			Embedding:        embs[0],
			MemoryFields:     mf,
		}
		liveReq := queueReq
		liveReq.BytesB64 = base64.StdEncoding.EncodeToString(raw)
		res, err := s.enqueueAndAttempt(ctx, wqueue.KindAttach, blobSHA, queueReq, raw, ext,
			func(ctx context.Context) error {
				_, e := client.Attach(ctx, liveReq)
				return e
			})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("daemon attach: %v", err)), nil
		}
		if s.deps.Lifecycle != nil {
			s.deps.Lifecycle.RecordWrite()
		}
		return mcp.NewToolResultText(fmt.Sprintf(
			"Attached %d bytes via daemon. Blob SHA: %s.%s",
			st.Size(), blobSHA, res.Notice,
		)), nil
	}
}

// timePtr returns a pointer to t. Used so callers can pass a non-nil
// *time.Time into the MemoryFields wire structs (nil = "captured_at
// unknown" and serializes as an omitted field).
func timePtr(t time.Time) *time.Time { return &t }

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
