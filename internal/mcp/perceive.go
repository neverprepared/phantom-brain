package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/neverprepared/phantom-brain/internal/vault"
)

// perceiveTool defines the brain_perceive MCP tool schema.
//
// brain_perceive ingests web-gathered content into Raw/gathered/. It
// is the "you went looking, here's what you found" entry point —
// distinct from brain_learn, which is for content the operator has
// already curated. Both end up in Raw/ but in different
// subdirectories so the synthesizer can apply different reliability
// gates.
func perceiveTool() mcp.Tool {
	return mcp.NewTool("brain_perceive",
		mcp.WithDescription(
			`Ingest gathered web content into Raw/gathered/. Writes the markdown to disk, `+
				`computes a content SHA256, embeds for vector search, and indexes for FTS5. `+
				`Duplicates (same canonical content) are detected and skipped via SHA dedup. `+
				`Use after a WebSearch / WebFetch when the result is worth remembering.`,
		),
		mcp.WithString("content",
			mcp.Required(),
			mcp.Description("Raw markdown body. Frontmatter is added by this tool — do not pre-include it."),
		),
		mcp.WithString("title",
			mcp.Required(),
			mcp.Description("Human-readable page title. Used in frontmatter AND for slug generation if filename is empty."),
		),
		mcp.WithString("filename",
			mcp.Description("Optional filename hint (without directory). If omitted, derived from title via slug. Must end in .md."),
		),
		mcp.WithString("source_url",
			mcp.Description("URL the content came from, when known. Embedded in frontmatter."),
		),
	)
}

// handlePerceive ingests one item into Raw/gathered/.
func (s *Server) handlePerceive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	content, err := req.RequireString("content")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	title, err := req.RequireString("title")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	filename, _ := req.RequireString("filename")
	sourceURL, _ := req.RequireString("source_url")

	res, errMsg, ok := s.ingestMarkdown(ctx, ingestParams{
		Subdir:    "gathered",
		StampKey:  "gathered_at",
		Content:   content,
		Title:     title,
		Filename:  filename,
		SourceURL: sourceURL,
	})
	if !ok {
		return mcp.NewToolResultError(errMsg), nil
	}
	if res.Status == "duplicate" {
		return mcp.NewToolResultText(fmt.Sprintf("Duplicate (already in index). SHA: %s", res.SHA)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Stored to %s. SHA: %s. Queued for synthesis.%s", res.RelativePath, res.SHA, res.Notice)), nil
}

// resolvePerceiveFilename returns a final filename for a Raw/<subdir>/
// entry. Operator hint wins when present; otherwise we slug the title.
// Always ends in .md.
func resolvePerceiveFilename(hint, title string) string {
	hint = strings.TrimSpace(hint)
	if hint != "" {
		hint = filepath.Base(hint)
		if !strings.HasSuffix(hint, ".md") {
			hint += ".md"
		}
		return hint
	}
	slug := vault.Slug(title)
	if slug == "" {
		return ""
	}
	return slug + ".md"
}
