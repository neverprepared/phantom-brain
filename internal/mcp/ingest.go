package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/brain/wqueue"
	"github.com/neverprepared/phantom-brain/internal/canonicalize"
	"github.com/neverprepared/phantom-brain/internal/index"
	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/vault"
)

// ingestParams is the shared shape for brain_perceive (Raw/gathered/)
// and brain_learn (Raw/curated/). The only field that differs at the
// caller level is the destination subdirectory.
type ingestParams struct {
	// Subdir under Raw/. Must be one of "gathered" or "curated"
	// — the synthesizer's reliability gate keys on this.
	Subdir string

	// Frontmatter event-stamp key. brain_perceive uses gathered_at;
	// brain_learn uses learned_at; both encode write time as RFC3339.
	StampKey string

	Content   string
	Title     string
	Filename  string
	SourceURL string

	// v2.4: callers may override the default memory-classification
	// fields. brain_perceive/learn use the defaults set in
	// ingestMarkdown by switching on Subdir. task_complete passes
	// explicit values to mark its promoted note as a task_summary +
	// episodic memory + source=task_id. Empty values fall through to
	// the per-subdir defaults.
	KindOverride       string
	MemoryTypeOverride string
	SourceOverride     []string
	ReferencesOverride []string
}

// ingestResult mirrors what callers want to render back to the user.
type ingestResult struct {
	Status       string // "stored" or "duplicate"
	RelativePath string
	SHA          string
	// Notice is empty when the daemon accepted the write directly.
	// When the daemon was unreachable the write was persisted to
	// wqueue and Notice carries the user-facing "Queued ..." line.
	Notice string
}

// ingestMarkdown is the shared write+index path. Returns ingestResult
// for the happy path; the (errMsg string, ok bool) signature is
// reserved for argument-validation failures the caller would surface
// as MCP tool errors (not Go-side errors, which are exceptional).
func (s *Server) ingestMarkdown(ctx context.Context, p ingestParams) (*ingestResult, string, bool) {
	if strings.TrimSpace(p.Content) == "" {
		return nil, "content must be non-empty", false
	}
	if strings.TrimSpace(p.Title) == "" {
		return nil, "title must be non-empty", false
	}
	if p.Subdir != "gathered" && p.Subdir != "curated" {
		return nil, "internal: subdir must be 'gathered' or 'curated'", false
	}
	if p.StampKey == "" {
		return nil, "internal: stamp key required", false
	}

	fm := map[string]any{
		"title":      p.Title,
		p.StampKey:   time.Now().UTC().Format(time.RFC3339),
	}
	if p.SourceURL != "" {
		fm["source_url"] = p.SourceURL
	}
	doc := &vault.Document{Frontmatter: fm, Body: p.Content}
	rendered, err := doc.Render()
	if err != nil {
		return nil, fmt.Sprintf("render document: %v", err), false
	}

	// SumBody hashes only the canonical body — frontmatter (with its
	// time.Now() ingestion stamps) is excluded so re-perceiving
	// identical content across a wall-clock second still dedups.
	sha, err := canonicalize.SumBody(rendered)
	if err != nil {
		return nil, fmt.Sprintf("canonicalize: %v", err), false
	}

	// Local dedup: even in Phase 6 the index check is the cheapest
	// way to spot a re-paste, and it preserves the same UX the
	// operator already trusts ("Duplicate" instead of a redundant
	// POST). The daemon's OS-side upsert is idempotent by SHA anyway,
	// so a false-negative here is fine.
	has, err := s.deps.Index.Has(sha)
	if err != nil {
		return nil, fmt.Sprintf("index has: %v", err), false
	}
	if has {
		return &ingestResult{Status: "duplicate", SHA: sha}, "", true
	}

	if s.deps.Embedder.Dims() != s.deps.Index.Dims() {
		return nil, fmt.Sprintf("embedder/index dim mismatch: embedder=%d index=%d",
			s.deps.Embedder.Dims(), s.deps.Index.Dims()), false
	}
	embInput := strings.TrimSpace(p.Title + "\n\n" + p.Content)
	embs, err := s.deps.Embedder.Embed(ctx, []string{embInput})
	if err != nil {
		return nil, fmt.Sprintf("embed: %v", err), false
	}

	tags := doc.FrontmatterStrings("tags")
	tagBlob := strings.Join(tags, " ")

	// Phase 6: if a daemon client is wired, POST the canonical write
	// and skip local file + index writes. The agent's local cache
	// catches up on the next snapshot pull (snapshot-at-birth UX is
	// retained per plan).
	if client := lifecycleClient(s); client != nil {
		dest := relativeRawPath(p.Subdir, p.Filename, p.Title)
		// v2.4: default memory-classification fields per subdir.
		// brain_perceive (gathered) is a semantic web scrape;
		// brain_learn (curated) is a semantic note. CapturedAt is
		// now (we just received it).
		now := time.Now().UTC()
		// Per-subdir defaults; caller can override via *Override fields.
		applyOverrides := func(mf brain.MemoryFields) brain.MemoryFields {
			if p.KindOverride != "" {
				mf.Kind = p.KindOverride
			}
			if p.MemoryTypeOverride != "" {
				mf.MemoryType = p.MemoryTypeOverride
			}
			if len(p.SourceOverride) > 0 {
				mf.Source = p.SourceOverride
			}
			if len(p.ReferencesOverride) > 0 {
				mf.References = p.ReferencesOverride
			}
			return mf
		}
		var notice string
		switch p.Subdir {
		case "gathered":
			mf := brain.MemoryFields{
				Kind:       string(osearch.KindWebScrape),
				MemoryType: string(osearch.MemorySemantic),
				CapturedAt: &now,
			}
			if p.SourceURL != "" {
				mf.Source = []string{p.SourceURL}
			}
			mf = applyOverrides(mf)
			req := brain.PerceiveRequest{
				SHA: sha, Title: p.Title, Body: p.Content,
				URL: p.SourceURL, SourcePath: dest, Tags: tags,
				Embedding:    embs[0],
				MemoryFields: mf,
			}
			res, err := s.enqueueAndAttempt(ctx, wqueue.KindPerceive, sha, req, nil, "",
				func(ctx context.Context) error {
					_, e := client.Perceive(ctx, req)
					return e
				})
			if err != nil {
				return nil, fmt.Sprintf("daemon perceive: %v", err), false
			}
			notice = res.Notice
		case "curated":
			mf := brain.MemoryFields{
				Kind:       string(osearch.KindNote),
				MemoryType: string(osearch.MemorySemantic),
				CapturedAt: &now,
			}
			mf = applyOverrides(mf)
			kind := wqueue.KindLearn
			if p.KindOverride == string(osearch.KindTaskSummary) {
				kind = wqueue.KindTaskPromote
			}
			req := brain.LearnRequest{
				SHA: sha, Title: p.Title, Body: p.Content,
				SourcePath: dest, Tags: tags,
				Embedding:    embs[0],
				MemoryFields: mf,
			}
			res, err := s.enqueueAndAttempt(ctx, kind, sha, req, nil, "",
				func(ctx context.Context) error {
					_, e := client.Learn(ctx, req)
					return e
				})
			if err != nil {
				return nil, fmt.Sprintf("daemon learn: %v", err), false
			}
			notice = res.Notice
		}
		if s.deps.Lifecycle != nil {
			s.deps.Lifecycle.RecordWrite()
		}
		return &ingestResult{Status: "stored", RelativePath: dest, SHA: sha, Notice: notice}, "", true
	}

	// Legacy fallback: no daemon client — write locally so the v1.x
	// BRAIN_VAULT_PATH-only mode keeps working for operators who
	// haven't migrated to the agent contract.
	resolvedName := resolvePerceiveFilename(p.Filename, p.Title)
	if resolvedName == "" {
		return nil, "could not derive a filename from title (slug is empty)", false
	}
	destRel := filepath.Join("Raw", p.Subdir, resolvedName)
	destAbs := filepath.Join(s.deps.VaultDir, destRel)
	if err := vault.WriteAtomicFile(destAbs, rendered, 0o644); err != nil {
		return nil, fmt.Sprintf("write: %v", err), false
	}
	if err := s.deps.Index.Upsert(ctx, index.Record{
		SHA:        sha,
		SourcePath: destRel,
		Title:      p.Title,
		Tags:       tagBlob,
		Body:       p.Content,
		Embedding:  embs[0],
	}); err != nil {
		return nil, fmt.Sprintf("index upsert: %v", err), false
	}
	if s.deps.Lifecycle != nil {
		s.deps.Lifecycle.RecordWrite()
	}
	return &ingestResult{Status: "stored", RelativePath: destRel, SHA: sha}, "", true
}

// lifecycleClient returns the daemon client if one is wired (either
// injected directly via deps.Client or derived from deps.Lifecycle).
// Single helper so the daemon-vs-local branch in every handler stays
// readable.
func lifecycleClient(s *Server) *brain.Client {
	if s.deps.Client != nil {
		return s.deps.Client
	}
	if s.deps.Lifecycle != nil {
		return s.deps.Lifecycle.Client()
	}
	return nil
}

// relativeRawPath composes the Raw/<subdir>/<filename>.md path the
// daemon stores as source_path. Mirrors the legacy local layout so
// future Wiki readers can still map back to the canonical filename.
func relativeRawPath(subdir, filename, title string) string {
	name := resolvePerceiveFilename(filename, title)
	if name == "" {
		name = "untitled.md"
	}
	return filepath.Join("Raw", subdir, name)
}
