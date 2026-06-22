package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/neverprepared/mcp-phantom-brain/internal/brain"
	"github.com/neverprepared/mcp-phantom-brain/internal/canonicalize"
	"github.com/neverprepared/mcp-phantom-brain/internal/ollama"
	"github.com/neverprepared/mcp-phantom-brain/internal/osearch"
	"github.com/neverprepared/mcp-phantom-brain/internal/vault"
)

// ingestBulkCmd is the Phase 6 bootstrap loader: walk a vault dir
// (Raw/curated, Raw/gathered, Raw/attachments) and POST every entry
// to the daemon via Client.{Perceive,Learn,Attach}. Replaces the
// migrate-legacy story for v2.0 deployments.
//
// Routing heuristic:
//   - *.md under any path containing "/curated/"   → Client.Learn
//   - *.md anywhere else (gathered, top-level)     → Client.Perceive
//   - binary files under "/attachments/"           → Client.Attach
//   - everything else                              → skipped (with a log)
//
// Embedding runs locally via Ollama (same model the daemon's snapshot
// export expects, 768-dim nomic-embed-text). The daemon is stateless
// w.r.t. embeddings — the agent ships them per write. Skipping the
// embed-on-ingest step would leave docs that the snapshot exporter
// can't include, so we require Ollama to be reachable.
//
// Concurrency: bounded worker pool. Default 4 keeps a personal MinIO
// + claude CLI deployment from stampeding while still finishing a
// 1.3 GB Obsidian vault overnight rather than over a week.
func ingestBulkCmd() *cobra.Command {
	var (
		api          string
		token        string
		concurrency  int
		dryRun       bool
		maxFileBytes int64
		timeoutSecs  int
	)
	c := &cobra.Command{
		Use:   "ingest-bulk <vault-dir>",
		Short: "Walk a vault dir and POST every file to the daemon",
		Long: `ingest-bulk loads an existing on-disk vault into the Phase 6 daemon.
Files are routed by path:
  Raw/curated/*.md         → brain_learn  (POST /api/brain/learn)
  Raw/gathered/*.md (etc)  → brain_perceive (POST /api/brain/perceive)
  Raw/attachments/*        → brain_attach (POST /api/brain/attach)

Embeddings are computed locally via Ollama (nomic-embed-text); the
daemon does not embed on the agent's behalf. Idempotent: re-running
against an unchanged tree is a no-op (daemon dedups by SHA).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vaultDir := args[0]
			if concurrency <= 0 {
				concurrency = 4
			}
			if maxFileBytes <= 0 {
				maxFileBytes = 100 * 1024 * 1024 // 100 MB; daemon attach max
			}

			// Plan is a pure filesystem walk — works without creds or
			// network. Run it first so --dry-run can short-circuit
			// before we touch Ollama or build the daemon client.
			plan, err := scanVaultForIngest(vaultDir, maxFileBytes)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(),
				"ingest-bulk: %d files queued (perceive=%d learn=%d attach=%d skipped=%d) from %s\n",
				plan.total(), plan.perceive, plan.learn, plan.attach, plan.skipped, vaultDir)

			if dryRun {
				return nil
			}

			if api == "" {
				api = strings.TrimSpace(os.Getenv("CL_BRAIN_API"))
			}
			if token == "" {
				token = strings.TrimSpace(os.Getenv("CL_BRAIN_API_TOKEN"))
			}
			if api == "" || token == "" {
				return errors.New("ingest-bulk: --api/--token (or CL_BRAIN_API/CL_BRAIN_API_TOKEN env) required")
			}
			if timeoutSecs <= 0 {
				timeoutSecs = 600 // 10 min — attach POSTs of multi-MB
				// base64 payloads over WAN routinely cross 30s.
			}
			client, err := brain.NewClient(brain.ClientOpts{
				BaseURL: api, Token: token,
				Timeout: time.Duration(timeoutSecs) * time.Second,
			})
			if err != nil {
				return fmt.Errorf("build daemon client: %w", err)
			}
			emb := ollama.New(ollama.OptionsFromEnv())
			if emb.Dims() != osearch.EmbeddingDim {
				return fmt.Errorf("embedder dim %d != osearch.EmbeddingDim %d — daemon will reject vectors",
					emb.Dims(), osearch.EmbeddingDim)
			}

			ctx, cancel := signalCancel(cmd.Context())
			defer cancel()

			runner := newIngestRunner(client, emb, concurrency, cmd.ErrOrStderr())
			start := time.Now()
			if err := runner.run(ctx, vaultDir, plan); err != nil {
				return err
			}
			runner.report(cmd.ErrOrStderr(), time.Since(start))
			return nil
		},
	}
	c.Flags().StringVar(&api, "api", "", "daemon URL (defaults to $CL_BRAIN_API)")
	c.Flags().StringVar(&token, "token", "", "daemon bearer token (defaults to $CL_BRAIN_API_TOKEN)")
	c.Flags().IntVar(&concurrency, "concurrency", 4, "concurrent POSTs in flight")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "scan + report without POSTing")
	c.Flags().Int64Var(&maxFileBytes, "max-file-bytes", 100*1024*1024, "skip files larger than this (defaults to 100 MB)")
	c.Flags().IntVar(&timeoutSecs, "timeout-secs", 600, "per-request HTTP timeout (default 10 min; bump on slow uplinks)")
	return c
}

// signalCancel wraps the inherited ctx with SIGINT/SIGTERM-driven
// cancellation so Ctrl-C drains in-flight requests cleanly.
func signalCancel(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// --- planning -----------------------------------------------------

type ingestPlan struct {
	perceive int
	learn    int
	attach   int
	skipped  int
	files    []ingestItem
}

func (p ingestPlan) total() int { return p.perceive + p.learn + p.attach }

type ingestKind int

const (
	kindSkip ingestKind = iota
	kindPerceive
	kindLearn
	kindAttach
)

type ingestItem struct {
	relPath string
	kind    ingestKind
	size    int64
}

// scanVaultForIngest walks vaultDir and classifies every file. Pure
// filesystem read — no POSTs, no embeddings — so dry-run is cheap.
func scanVaultForIngest(vaultDir string, maxFileBytes int64) (*ingestPlan, error) {
	plan := &ingestPlan{}
	root := filepath.Clean(vaultDir)
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat vault dir: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", root)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			// Skip Wiki/ (synthesised output — daemon regenerates),
			// _index/ (vault-private SQLite + caches), .git, hidden dirs.
			name := d.Name()
			if name == "Wiki" || name == "_index" || strings.HasPrefix(name, ".") {
				if path != root {
					return fs.SkipDir
				}
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		info, ierr := d.Info()
		if ierr != nil {
			return nil // best effort
		}
		size := info.Size()
		kind := classifyIngestPath(rel)
		if kind == kindSkip {
			plan.skipped++
			return nil
		}
		if size > maxFileBytes {
			plan.skipped++
			return nil
		}
		switch kind {
		case kindPerceive:
			plan.perceive++
		case kindLearn:
			plan.learn++
		case kindAttach:
			plan.attach++
		}
		plan.files = append(plan.files, ingestItem{relPath: rel, kind: kind, size: size})
		return nil
	})
	return plan, err
}

// classifyIngestPath routes a vault-relative path to the right
// daemon endpoint. Mirrors the existing Raw/{curated,gathered,attachments}
// layout the v1.x agents wrote.
func classifyIngestPath(rel string) ingestKind {
	rel = filepath.ToSlash(rel)
	switch {
	case strings.HasPrefix(rel, "Raw/curated/") && strings.HasSuffix(rel, ".md"):
		return kindLearn
	case strings.HasPrefix(rel, "Raw/gathered/") && strings.HasSuffix(rel, ".md"):
		return kindPerceive
	case strings.HasPrefix(rel, "Raw/attachments/"):
		// Skip the .md stub files alongside binaries — daemon re-derives
		// metadata from the blob itself.
		if strings.HasSuffix(rel, ".md") {
			return kindSkip
		}
		return kindAttach
	}
	return kindSkip
}

// --- runner -------------------------------------------------------

type ingestRunner struct {
	client      *brain.Client
	embedder    *ollama.Client
	concurrency int
	stderr      writer

	ok       atomic.Int64
	dup      atomic.Int64
	errCount atomic.Int64
}

// writer is the slice of io.Writer the runner needs. Defined locally
// so the cmd helpers don't need a separate import block.
type writer interface{ Write([]byte) (int, error) }

func newIngestRunner(client *brain.Client, embedder *ollama.Client, concurrency int, stderr writer) *ingestRunner {
	return &ingestRunner{client: client, embedder: embedder, concurrency: concurrency, stderr: stderr}
}

func (r *ingestRunner) run(ctx context.Context, vaultDir string, plan *ingestPlan) error {
	jobs := make(chan ingestItem, r.concurrency*2)
	var wg sync.WaitGroup
	for i := 0; i < r.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := r.process(ctx, vaultDir, job); err != nil {
					r.errCount.Add(1)
					fmt.Fprintf(r.stderr, "ERROR %s: %v\n", job.relPath, err)
					continue
				}
			}
		}()
	}
	for _, it := range plan.files {
		select {
		case <-ctx.Done():
			break
		case jobs <- it:
		}
	}
	close(jobs)
	wg.Wait()
	return nil
}

func (r *ingestRunner) process(ctx context.Context, vaultDir string, it ingestItem) error {
	abs := filepath.Join(vaultDir, it.relPath)
	raw, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	switch it.kind {
	case kindPerceive, kindLearn:
		return r.processMarkdown(ctx, it, raw)
	case kindAttach:
		return r.processAttach(ctx, it, raw)
	}
	return nil
}

func (r *ingestRunner) processMarkdown(ctx context.Context, it ingestItem, raw []byte) error {
	doc, err := vault.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	title := strings.TrimSpace(doc.FrontmatterString("title"))
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(it.relPath), filepath.Ext(it.relPath))
	}
	body := doc.Body
	if strings.TrimSpace(body) == "" {
		return errors.New("empty body")
	}
	sourceURL := strings.TrimSpace(doc.FrontmatterString("source_url"))
	tags := doc.FrontmatterStrings("tags")

	// v2.4: build memory-classification fields from the legacy
	// frontmatter shape (Obsidian email-scrape format). Maps:
	//   type/vendor/category → tags[]
	//   from_email/obsidian_note → source[]
	//   date → captured_at (parsed as YYYY-MM-DD)
	// If the doc looks like an email import (has from_email or
	// vendor or obsidian_note), kind = email_import; otherwise
	// the per-subdir default (web_scrape / note) applies.
	mf, looksLikeEmailImport := buildLegacyMemoryFields(doc, it.relPath)
	// Set default Kind + MemoryType per source kind. Email-imports get
	// the more-specific classification; everything else falls through
	// to note (curated) or web_scrape (gathered). Without this, the
	// 57+ technical/curated notes without email frontmatter end up
	// with empty kind in OS and aren't faceted-queryable.
	switch {
	case looksLikeEmailImport && it.kind == kindLearn:
		mf.fields.Kind = string(osearch.KindEmailImport)
		mf.fields.MemoryType = string(osearch.MemoryEpisodic)
	case it.kind == kindLearn:
		mf.fields.Kind = string(osearch.KindNote)
		mf.fields.MemoryType = string(osearch.MemorySemantic)
	case it.kind == kindPerceive:
		mf.fields.Kind = string(osearch.KindWebScrape)
		mf.fields.MemoryType = string(osearch.MemorySemantic)
	}
	tags = append(tags, mf.extraTags...)
	// dedup tags
	tags = uniqueStrings(tags)

	// Canonicalise the rendered markdown for the SHA — matches the
	// runtime ingest path so re-perceiving an unchanged file dedups
	// against an already-indexed doc.
	rendered, err := doc.Render()
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	// SumBody (not Sum) so re-running bulk-ingest against an unchanged
	// vault dedups even though the agent re-renders the frontmatter
	// with a fresh time.Now() stamp on each pass.
	sha, err := canonicalize.SumBody(rendered)
	if err != nil {
		return fmt.Errorf("canonicalize: %w", err)
	}

	embInput := strings.TrimSpace(title + "\n\n" + body)
	embs, err := r.embedder.Embed(ctx, []string{embInput})
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}

	if it.kind == kindLearn {
		_, err = r.client.Learn(ctx, brain.LearnRequest{
			SHA: sha, Title: title, Body: body,
			SourcePath: it.relPath, Tags: tags, Embedding: embs[0],
			MemoryFields: mf.fields,
		})
	} else {
		_, err = r.client.Perceive(ctx, brain.PerceiveRequest{
			SHA: sha, Title: title, Body: body, URL: sourceURL,
			SourcePath: it.relPath, Tags: tags, Embedding: embs[0],
			MemoryFields: mf.fields,
		})
	}
	if err != nil {
		return err
	}
	r.ok.Add(1)
	return nil
}

func (r *ingestRunner) processAttach(ctx context.Context, it ingestItem, raw []byte) error {
	sha := osearch.SHA256Hex(raw)
	name := canonicalize.Filename(filepath.Base(it.relPath))
	title := strings.TrimSuffix(name, filepath.Ext(name))
	embInput := title
	embs, err := r.embedder.Embed(ctx, []string{embInput})
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}
	// v2.4: bulk-loaded attachments are classified the same way an
	// MCP brain_attach call would (attachment_stub / semantic) but
	// flagged with the migration source so they're filterable later.
	_, err = r.client.Attach(ctx, brain.AttachRequest{
		SHA:              sha,
		OriginalFilename: name,
		Title:            title,
		BytesB64:         base64.StdEncoding.EncodeToString(raw),
		MIMEType:         guessAttachmentMIME(filepath.Ext(name)),
		Embedding:        embs[0],
		MemoryFields: brain.MemoryFields{
			Kind:       string(osearch.KindAttachmentStub),
			MemoryType: string(osearch.MemorySemantic),
			Source:     []string{"ingest-bulk:" + it.relPath},
		},
	})
	if err != nil {
		return err
	}
	r.ok.Add(1)
	return nil
}

func (r *ingestRunner) report(w writer, elapsed time.Duration) {
	fmt.Fprintf(w, "ingest-bulk done in %s: ok=%d errors=%d\n",
		elapsed.Round(time.Second), r.ok.Load(), r.errCount.Load())
}

// legacyMemoryFields wraps the brain.MemoryFields constructed from
// an Obsidian-style .md plus any extra tag values lifted out of
// type/vendor/category. extraTags is appended onto the doc's
// existing tags[] then deduped.
type legacyMemoryFields struct {
	fields    brain.MemoryFields
	extraTags []string
}

// buildLegacyMemoryFields extracts v2.4 memory classification from a
// legacy Obsidian curated-markdown frontmatter. The "email_import"
// signal fires when any of from_email / vendor / obsidian_note is
// present — characteristic markers of the email-scrape pipeline.
// Returns the constructed MemoryFields + whether this looks like an
// email-import doc (so the caller can set Kind accordingly).
func buildLegacyMemoryFields(doc *vault.Document, relPath string) (legacyMemoryFields, bool) {
	out := legacyMemoryFields{}

	// Lift type/vendor/category into tags[] — they're flat labels in
	// the new schema, not dedicated columns.
	for _, key := range []string{"type", "vendor", "category"} {
		v := strings.TrimSpace(doc.FrontmatterString(key))
		if v != "" {
			out.extraTags = append(out.extraTags, key+":"+v)
		}
	}

	// Source provenance from email metadata + obsidian breadcrumb.
	if fromEmail := strings.TrimSpace(doc.FrontmatterString("from_email")); fromEmail != "" {
		out.fields.Source = append(out.fields.Source, "from_email:"+fromEmail)
	}
	if obsNote := strings.TrimSpace(doc.FrontmatterString("obsidian_note")); obsNote != "" {
		out.fields.Source = append(out.fields.Source, "obsidian_note:"+obsNote)
	}
	if subj := strings.TrimSpace(doc.FrontmatterString("subject")); subj != "" {
		out.fields.Source = append(out.fields.Source, "subject:"+subj)
	}

	// Parse the date field as captured_at if present + parseable.
	// yaml.v3 auto-types `date: 2025-07-15` as time.Time (not string),
	// so FrontmatterString returns "" for those — read the raw value
	// and handle both shapes.
	if dateRaw, ok := doc.Frontmatter["date"]; ok {
		switch v := dateRaw.(type) {
		case time.Time:
			vv := v
			out.fields.CapturedAt = &vv
		case string:
			s := strings.TrimSpace(v)
			for _, layout := range []string{"2006-01-02", "2006-01-02T15:04:05Z", time.RFC3339} {
				if t, err := time.Parse(layout, s); err == nil {
					t := t
					out.fields.CapturedAt = &t
					break
				}
			}
		}
	}

	looksLikeEmailImport := strings.TrimSpace(doc.FrontmatterString("from_email")) != "" ||
		strings.TrimSpace(doc.FrontmatterString("vendor")) != "" ||
		strings.TrimSpace(doc.FrontmatterString("obsidian_note")) != ""
	return out, looksLikeEmailImport
}

// uniqueStrings returns a copy of s with duplicates removed,
// preserving first-seen order. Used to dedup tag lists when we
// append legacy frontmatter values onto the doc's own tags[].
func uniqueStrings(s []string) []string {
	if len(s) == 0 {
		return s
	}
	seen := make(map[string]bool, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// guessAttachmentMIME mirrors internal/mcp/attach.go's guessMIMEType.
// Duplicated rather than exported because the bulk loader runs in a
// different package and the heuristic doesn't merit a shared type.
func guessAttachmentMIME(ext string) string {
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
