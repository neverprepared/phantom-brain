package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/neverprepared/mcp-phantom-brain/internal/ollama"
	"github.com/neverprepared/mcp-phantom-brain/internal/osearch"
	pbserver "github.com/neverprepared/mcp-phantom-brain/internal/server"
)

// backfillEmbedder is the minimal slice of *ollama.Client the backfill
// subcommand needs. Mirrors ingest-bulk's pattern so tests can substitute
// an in-memory fake without an Ollama process.
type backfillEmbedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	Dims() int
}

// backfillStubClient is the minimal slice of *osearch.Client the
// backfill subcommand needs. Declared as an interface so tests can
// substitute an in-memory fake without standing up a live OpenSearch
// cluster.
type backfillStubClient interface {
	ScrollAttachments(ctx context.Context, profile, vault string, batchSize int, fn func(osearch.AttachmentDoc) error) error
	GetSummary(ctx context.Context, profile, vault, sha string) (*osearch.SummaryDoc, error)
	UpsertSummary(ctx context.Context, doc osearch.SummaryDoc, waitForRefresh bool) error
}

// backfillAttachmentStubsCmd creates the operator subcommand that
// retrofits a SummaryDoc stub for every existing AttachmentDoc that
// doesn't have one. Required after upgrading to v2.5.1 so the 1960
// attachments ingested under v2.5.0 become recallable via the
// summaries-only snapshot export path.
//
// Talks to OpenSearch directly using the daemon's [opensearch] config
// from server.toml. Does not go through the HTTP attach handler —
// the raw bytes aren't available here, and the handler's SHA-vs-bytes
// validation would fire on every doc.
//
// Idempotent: GetSummary on the same SHA short-circuits.
func backfillAttachmentStubsCmd() *cobra.Command {
	var (
		profile     string
		vault       string
		dryRun      bool
		batchSize   int
		concurrency int
	)
	c := &cobra.Command{
		Use:   "backfill-attachment-stubs",
		Short: "Write SummaryDoc stubs for existing AttachmentDocs (v2.5.1 retrofit)",
		Long: `Scrolls pb_attachments scoped to (profile, vault) and, for each
AttachmentDoc that lacks a matching SummaryDoc by SHA, upserts a stub
SummaryDoc (kind=attachment_stub) into pb_summaries. The stub carries
the attachment's title, description (mirrored from ExtractedText until
the v2.5.1 description split lands), tags, source[], and a back-link
via Attachments=[sha]. Reliability is fixed at medium with a
"curated (attachment)" gate reason so the daemon's synth queue skips
the LLM gate on these stubs.

Idempotent: stubs are keyed by the AttachmentDoc's SHA, so re-running
against an already-backfilled vault is a no-op (each candidate is
checked via GetSummary before write).

Run once per vault after upgrading the daemon to v2.5.1. Then either
restart the daemon (snapshot rebuild on next write) or trigger one
manually so agents pull a snapshot that contains the new stubs.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if profile == "" || vault == "" {
				return errors.New("--profile and --vault are required")
			}
			if batchSize <= 0 {
				batchSize = 500
			}
			if concurrency <= 0 {
				concurrency = 4
			}

			cfg, err := pbserver.LoadServerConfig(resolveConfigDir(cmd))
			if err != nil {
				return fmt.Errorf("load server config: %w", err)
			}
			if !cfg.OpenSearch.Enabled() {
				return errors.New("server.toml has no [opensearch] block — nothing to backfill against")
			}

			ctx, cancel := signalCancel(cmd.Context())
			defer cancel()

			oc, err := osearch.Open(ctx, osearch.Config{
				Addresses:          cfg.OpenSearch.Addresses,
				Username:           cfg.OpenSearch.Username,
				Password:           cfg.OpenSearch.Password,
				InsecureSkipVerify: cfg.OpenSearch.InsecureSkipVerify,
				IndexPrefix:        cfg.OpenSearch.IndexPrefix,
				RequestTimeout:     time.Duration(cfg.OpenSearch.RequestTimeoutSecs) * time.Second,
			})
			if err != nil {
				return fmt.Errorf("opensearch open: %w", err)
			}

			emb := ollama.New(ollama.OptionsFromEnv())
			if emb.Dims() != osearch.EmbeddingDim {
				return fmt.Errorf("embedder dim %d != osearch.EmbeddingDim %d",
					emb.Dims(), osearch.EmbeddingDim)
			}

			res, err := runBackfillAttachmentStubs(ctx, oc, emb, backfillOpts{
				Profile:     profile,
				Vault:       vault,
				DryRun:      dryRun,
				BatchSize:   batchSize,
				Concurrency: concurrency,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"backfill-attachment-stubs: walked=%d created=%d skipped=%d errors=%d (dry_run=%v)\n",
				res.Walked, res.Created, res.Skipped, res.Errors, dryRun)
			if res.Errors > 0 {
				return fmt.Errorf("backfill completed with %d errors", res.Errors)
			}
			return nil
		},
	}
	opsCommonFlags(c)
	c.Flags().StringVar(&profile, "profile", "", "profile to backfill (required)")
	c.Flags().StringVar(&vault, "vault", "", "vault to backfill (required)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "scan + report without writing stubs")
	c.Flags().IntVar(&batchSize, "batch-size", 500, "OS scroll page size")
	c.Flags().IntVar(&concurrency, "concurrency", 4, "concurrent stub upserts in flight")
	return c
}

type backfillOpts struct {
	Profile     string
	Vault       string
	DryRun      bool
	BatchSize   int
	Concurrency int
}

type backfillResult struct {
	Walked  int64
	Created int64
	Skipped int64
	Errors  int64
}

// runBackfillAttachmentStubs is the testable core: scroll attachments,
// skip those that already have a SummaryDoc, build + upsert a stub for
// the rest. Concurrency-limited so a large vault doesn't stampede OS.
//
// The embedder is required because v2.5.0 attachments may carry empty
// embeddings, and the snapshot exporter drops zero-vector docs. Without
// re-embedding here, backfilled stubs would never reach brain_recall.
func runBackfillAttachmentStubs(ctx context.Context, client backfillStubClient, emb backfillEmbedder, opts backfillOpts) (backfillResult, error) {
	var res backfillResult

	jobs := make(chan osearch.AttachmentDoc, opts.Concurrency*2)
	var wg sync.WaitGroup
	var firstErr error
	var firstErrOnce sync.Once

	worker := func() {
		defer wg.Done()
		for att := range jobs {
			if ctx.Err() != nil {
				return
			}
			created, err := processBackfillOne(ctx, client, emb, att, opts.DryRun)
			if err != nil {
				atomic.AddInt64(&res.Errors, 1)
				firstErrOnce.Do(func() { firstErr = err })
				continue
			}
			if created {
				atomic.AddInt64(&res.Created, 1)
			} else {
				atomic.AddInt64(&res.Skipped, 1)
			}
		}
	}
	for i := 0; i < opts.Concurrency; i++ {
		wg.Add(1)
		go worker()
	}

	scrollErr := client.ScrollAttachments(ctx, opts.Profile, opts.Vault, opts.BatchSize, func(att osearch.AttachmentDoc) error {
		atomic.AddInt64(&res.Walked, 1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case jobs <- att:
			return nil
		}
	})
	close(jobs)
	wg.Wait()

	if scrollErr != nil {
		return res, scrollErr
	}
	if firstErr != nil {
		// Errors already counted; surface the first one so the CLI
		// exits non-zero with a meaningful message.
		return res, firstErr
	}
	return res, nil
}

// processBackfillOne returns (created, error). created=false means
// the stub already existed (idempotent skip).
func processBackfillOne(ctx context.Context, client backfillStubClient, emb backfillEmbedder, att osearch.AttachmentDoc, dryRun bool) (bool, error) {
	if att.SHA == "" || att.Profile == "" || att.Vault == "" {
		return false, fmt.Errorf("attachment missing identity: profile=%q vault=%q sha=%q",
			att.Profile, att.Vault, att.SHA)
	}
	existing, err := client.GetSummary(ctx, att.Profile, att.Vault, att.SHA)
	if err != nil {
		return false, fmt.Errorf("get summary %s: %w", att.SHA, err)
	}
	if existing != nil {
		return false, nil
	}

	stub := buildAttachmentStub(att)
	if len(stub.Embedding) == 0 {
		embInput := stub.Title
		if stub.RawBody != "" {
			embInput = stub.Title + "\n\n" + stub.RawBody
		}
		vecs, err := emb.Embed(ctx, []string{embInput})
		if err != nil {
			return false, fmt.Errorf("embed %s: %w", att.SHA, err)
		}
		if len(vecs) != 1 {
			return false, fmt.Errorf("embed %s: got %d vectors, want 1", att.SHA, len(vecs))
		}
		stub.Embedding = vecs[0]
	}
	if dryRun {
		return true, nil
	}
	if err := client.UpsertSummary(ctx, stub, false); err != nil {
		return false, fmt.Errorf("upsert summary %s: %w", att.SHA, err)
	}
	return true, nil
}

// buildAttachmentStub composes the SummaryDoc stub per the v2.5.1
// field-mapping table. Mirrors what the planned handleAttach will
// write for new attachments — same shape so a backfilled stub and a
// fresh-attach stub are indistinguishable downstream.
//
// Body source priority: the v2.5.1 core fix splits Description out of
// ExtractedText, but at backfill time on existing data we don't know
// which one a given doc carries. Both today's ExtractedText (which is
// really the caller's description per #47) and a future PDF-extracted
// ExtractedText make valid RawBody seeds. We prefer ExtractedText →
// Title fallback; OriginalFilename as the last resort so the stub is
// never empty.
func buildAttachmentStub(att osearch.AttachmentDoc) osearch.SummaryDoc {
	now := time.Now().UTC()

	title := att.Title
	if title == "" {
		title = att.OriginalFilename
	}

	rawBody := att.ExtractedText
	if rawBody == "" {
		rawBody = title
	}

	tags := append([]string{}, att.Tags...)
	tags = append(tags, "attachment")
	if att.MIMEType != "" {
		tags = append(tags, "mime:"+att.MIMEType)
	}
	tags = uniqueStrings(tags)

	memType := att.MemoryType
	if memType == "" {
		memType = osearch.MemorySemantic
	}

	stub := osearch.SummaryDoc{
		Profile:     att.Profile,
		Vault:       att.Vault,
		SHA:         att.SHA,
		Kind:        osearch.KindAttachmentStub,
		MemoryType:  memType,
		SourcePath:  "attachment://" + att.SHA,
		Source:      append([]string{}, att.Source...),
		CreatedAt:   now,
		UpdatedAt:   now,
		CapturedAt:  att.CapturedAt,
		Title:       title,
		RawBody:     rawBody,
		Body:        rawBody,
		Tags:        tags,
		Reliability: osearch.ReliabilityMedium,
		GateReason:  "curated (attachment)",
		Attachments: []string{att.SHA},
		References:  append([]string{}, att.References...),
		Embedding:   att.Embedding,
		Synthesised: true,
	}
	return stub
}
