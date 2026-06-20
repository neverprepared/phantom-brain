package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mindmorass/mcp-phantom-brain/internal/canonicalize"
	"github.com/mindmorass/mcp-phantom-brain/internal/index"
	"github.com/mindmorass/mcp-phantom-brain/internal/vault"
)

// perceiveTool defines the brain_perceive MCP tool schema.
//
// brain_perceive ingests web-gathered content into the brain. It is
// the "you went looking, here's what you found" entry point —
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
//
// Steps (in order):
//
//  1. Build the on-disk markdown bytes: frontmatter (title,
//     source_url, gathered_at) + body. Render via vault.Document
//     so keys are alpha-sorted.
//  2. Canonicalize-SHA256 the rendered bytes. This is the dedup key.
//  3. Check index.Has(sha). If yes, return "duplicate" and skip
//     everything else.
//  4. Resolve filename (operator hint, slug fallback).
//  5. Atomic-write to <VaultDir>/Raw/gathered/<filename>.
//  6. Embed title + body via the injected Embedder.
//  7. Upsert into the index.
//  8. Return a status line with sha + relative path.
func (s *Server) handlePerceive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	content, err := req.RequireString("content")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if strings.TrimSpace(content) == "" {
		return mcp.NewToolResultError("content must be non-empty"), nil
	}
	title, err := req.RequireString("title")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if strings.TrimSpace(title) == "" {
		return mcp.NewToolResultError("title must be non-empty"), nil
	}
	filename, _ := req.RequireString("filename")
	sourceURL, _ := req.RequireString("source_url")

	// Build frontmatter. gathered_at is now-ish; we don't accept a
	// caller-supplied timestamp because the perceive event semantically
	// happens at write-time.
	fm := map[string]any{
		"title":        title,
		"gathered_at":  time.Now().UTC().Format(time.RFC3339),
	}
	if sourceURL != "" {
		fm["source_url"] = sourceURL
	}
	doc := &vault.Document{Frontmatter: fm, Body: content}
	rendered, err := doc.Render()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("render document: %v", err)), nil
	}

	sha, err := canonicalize.Sum(rendered)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("canonicalize: %v", err)), nil
	}

	has, err := s.deps.Index.Has(sha)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("index has: %v", err)), nil
	}
	if has {
		return mcp.NewToolResultText(fmt.Sprintf("Duplicate (already in index). SHA: %s", sha)), nil
	}

	resolvedName := resolvePerceiveFilename(filename, title)
	if resolvedName == "" {
		return mcp.NewToolResultError("could not derive a filename from title (slug is empty)"), nil
	}

	destRel := filepath.Join("Raw", "gathered", resolvedName)
	destAbs := filepath.Join(s.deps.VaultDir, destRel)
	if err := vault.WriteAtomicFile(destAbs, rendered, 0o644); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("write: %v", err)), nil
	}

	if s.deps.Embedder.Dims() != s.deps.Index.Dims() {
		return mcp.NewToolResultError(fmt.Sprintf(
			"embedder/index dim mismatch: embedder=%d index=%d",
			s.deps.Embedder.Dims(), s.deps.Index.Dims(),
		)), nil
	}
	embInput := strings.TrimSpace(title + "\n\n" + content)
	embs, err := s.deps.Embedder.Embed(ctx, []string{embInput})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("embed: %v", err)), nil
	}

	tags := strings.Join(doc.FrontmatterStrings("tags"), " ")
	if err := s.deps.Index.Upsert(ctx, index.Record{
		SHA:        sha,
		SourcePath: destRel,
		Title:      title,
		Tags:       tags,
		Body:       content,
		Embedding:  embs[0],
	}); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("index upsert: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Stored to %s. SHA: %s. Queued for synthesis.", destRel, sha)), nil
}

// resolvePerceiveFilename returns a final filename for a Raw/gathered/
// entry. Operator hint wins when present; otherwise we slug the title.
// Always ends in .md.
func resolvePerceiveFilename(hint, title string) string {
	hint = strings.TrimSpace(hint)
	if hint != "" {
		// Strip any leading dirs the operator passed by accident.
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
