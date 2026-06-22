package osearch

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/neverprepared/mcp-phantom-brain/internal/index"
)

// ExportOptions configures one snapshot export from OS to a tar.zst.
type ExportOptions struct {
	// Profile and Vault scope the export — only docs matching this
	// pair are included. Mandatory; cross-vault snapshots are not
	// supported (and would violate the auth boundary anyway).
	Profile string
	Vault   string

	// OutputPath is the destination .tar.zst file. The tarball
	// contains a single `vectors.db` at the root with the agent's
	// internal/index schema populated from OS.
	OutputPath string

	// BatchSize controls the OS scroll page size. 500 is a sane
	// default — large enough to amortise round-trips, small enough
	// not to blow request limits.
	BatchSize int

	// ScrollKeepAlive is how long the scroll context stays open on
	// the OS side between paged calls. 1m is fine for any reasonable
	// vault size.
	ScrollKeepAlive time.Duration

	// IncludeRawOnly controls whether docs without a synthesised
	// summary are exported. Default false — agents prefer to recall
	// against curated content. Raw-only docs always have RawBody and
	// usually an Embedding (agent computed locally before POSTing).
	IncludeRawOnly bool
}

// ExportManifest summarises what Export wrote. Persisted alongside
// the tarball by Day 6's snapshot wiring.
type ExportManifest struct {
	Profile      string    `json:"profile"`
	Vault        string    `json:"vault"`
	NumDocs      int       `json:"num_docs"`
	NumSkipped   int       `json:"num_skipped"`
	SHA256       string    `json:"sha256"`
	SizeBytes    int64     `json:"size_bytes"`
	GeneratedAt  time.Time `json:"generated_at"`
	EmbeddingDim int       `json:"embedding_dim"`
}

// Export pulls every (profile, vault)-scoped summary from OS, writes
// them into a fresh sqlite-vec+FTS5 database matching the agent's
// internal/index schema, and tars+zstd's the result to opts.OutputPath.
//
// The returned manifest is suitable for sidecar persistence; the
// caller is responsible for placing the .sha256 / .manifest.json
// files alongside the tarball (Day 6 handles that wiring).
func (c *Client) Export(ctx context.Context, opts ExportOptions) (ExportManifest, error) {
	if opts.Profile == "" || opts.Vault == "" || opts.OutputPath == "" {
		return ExportManifest{}, errors.New("osearch.Export: profile, vault, output_path required")
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}
	if opts.ScrollKeepAlive == 0 {
		opts.ScrollKeepAlive = time.Minute
	}

	stage, err := os.MkdirTemp("", "pbrain-export-*")
	if err != nil {
		return ExportManifest{}, fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(stage)

	// vectors.db must land inside _index/ inside the tarball — that's
	// the path internal/index.Open looks at when the agent's birth
	// extracts the snapshot under brain_dir/. Writing it at the stage
	// root produced a tarball that extracted to brain_dir/vectors.db,
	// which the index opener never reads, leaving the local cache
	// empty (Phase 6 regression vs v1 BuildSnapshot's reflinked _index/).
	indexStage := filepath.Join(stage, "_index")
	if err := os.MkdirAll(indexStage, 0o755); err != nil {
		return ExportManifest{}, fmt.Errorf("mkdir _index: %w", err)
	}
	idx, err := index.Open(indexStage, EmbeddingDim)
	if err != nil {
		return ExportManifest{}, fmt.Errorf("index.Open: %w", err)
	}
	defer idx.Close()

	numDocs, numSkipped, err := c.scrollSummariesIntoIndex(ctx, opts, idx)
	if err != nil {
		return ExportManifest{}, err
	}

	// Force the SQLite WAL into the main DB file before tarring;
	// otherwise the tarball captures only the pre-WAL snapshot.
	if err := closeAndCheckpoint(idx); err != nil {
		return ExportManifest{}, fmt.Errorf("checkpoint: %w", err)
	}

	sum, size, err := writeTarZst(stage, opts.OutputPath)
	if err != nil {
		return ExportManifest{}, fmt.Errorf("tar.zst: %w", err)
	}

	return ExportManifest{
		Profile:      opts.Profile,
		Vault:        opts.Vault,
		NumDocs:      numDocs,
		NumSkipped:   numSkipped,
		SHA256:       sum,
		SizeBytes:    size,
		GeneratedAt:  time.Now().UTC(),
		EmbeddingDim: EmbeddingDim,
	}, nil
}

// scrollSummariesIntoIndex paginates pb_summaries via OS scroll API
// and writes each hit into the local index. Returns (written, skipped).
func (c *Client) scrollSummariesIntoIndex(ctx context.Context, opts ExportOptions, idx *index.Index) (int, int, error) {
	query := map[string]any{
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []map[string]any{
					{"term": map[string]any{"profile": opts.Profile}},
					{"term": map[string]any{"vault": opts.Vault}},
				},
			},
		},
		"size": opts.BatchSize,
		"sort": []map[string]any{{"_doc": map[string]any{"order": "asc"}}},
	}
	body, err := json.Marshal(query)
	if err != nil {
		return 0, 0, fmt.Errorf("marshal initial query: %w", err)
	}

	resp, err := c.api.Search(ctx, &osapi.SearchReq{
		Indices: []string{c.IndexName(IndexSummaries)},
		Body:    bytes.NewReader(body),
		Params:  osapi.SearchParams{Scroll: opts.ScrollKeepAlive},
	})
	if err != nil {
		return 0, 0, fmt.Errorf("initial search: %w", err)
	}

	written, skipped := 0, 0
	scrollID := ""
	if resp.ScrollID != nil {
		scrollID = *resp.ScrollID
	}
	// Always clear the scroll cursor on exit, even on partial writes —
	// dangling scrolls hold heap on the OS coordinator.
	defer func() {
		if scrollID != "" {
			_, _ = c.api.Scroll.Delete(context.Background(), osapi.ScrollDeleteReq{ScrollIDs: []string{scrollID}})
		}
	}()

	hits := resp.Hits.Hits
	for len(hits) > 0 {
		for _, h := range hits {
			ok, err := writeHitToIndex(ctx, h, opts, idx)
			if err != nil {
				return written, skipped, err
			}
			if ok {
				written++
			} else {
				skipped++
			}
		}
		if scrollID == "" {
			break
		}
		nextResp, err := c.api.Scroll.Get(ctx, osapi.ScrollGetReq{
			ScrollID: scrollID,
			Params:   osapi.ScrollGetParams{Scroll: opts.ScrollKeepAlive},
		})
		if err != nil {
			return written, skipped, fmt.Errorf("scroll get: %w", err)
		}
		hits = nextResp.Hits.Hits
		if nextResp.ScrollID != nil {
			scrollID = *nextResp.ScrollID
		}
	}
	return written, skipped, nil
}

// writeHitToIndex decodes one OS hit into an index.Record and
// upserts it. Returns false when the doc was skipped (missing
// embedding, etc.) without an error so the caller can keep going.
func writeHitToIndex(ctx context.Context, h osapi.SearchHit, opts ExportOptions, idx *index.Index) (bool, error) {
	var doc SummaryDoc
	if err := json.Unmarshal(h.Source, &doc); err != nil {
		return false, fmt.Errorf("decode hit %s: %w", h.ID, err)
	}
	if len(doc.Embedding) == 0 {
		// Without a vector the doc can't go into vec_embeddings (and
		// the agent's hybrid recall depends on both halves). Day 5's
		// synth queue ensures the daemon fills embeddings for every
		// indexed doc, so this branch is mostly the raw-only case.
		return false, nil
	}
	if len(doc.Embedding) != EmbeddingDim {
		return false, fmt.Errorf("doc %s: embedding dim %d, want %d", h.ID, len(doc.Embedding), EmbeddingDim)
	}
	if !opts.IncludeRawOnly && !doc.Synthesised {
		return false, nil
	}

	// Prefer the synthesised Body; fall back to RawBody for legacy
	// or raw-only exports.
	body := doc.Body
	if body == "" {
		body = doc.RawBody
	}

	rec := index.Record{
		SHA:        doc.SHA,
		SourcePath: doc.SourcePath,
		Title:      doc.Title,
		Tags:       strings.Join(doc.Tags, " "),
		Body:       body,
		Embedding:  doc.Embedding,
	}
	if rec.SourcePath == "" {
		// internal/index requires a non-empty source_path; synthesise
		// a deterministic one when the original was a URL-only source.
		rec.SourcePath = fmt.Sprintf("os://%s/%s/%s", doc.Profile, doc.Vault, doc.SHA)
	}
	if err := idx.Upsert(ctx, rec); err != nil {
		return false, fmt.Errorf("index upsert %s: %w", doc.SHA, err)
	}
	return true, nil
}

// closeAndCheckpoint closes the index but first issues a WAL
// checkpoint so the on-disk vectors.db file is self-contained when
// we tar it. internal/index has no public checkpoint hook, so we
// just close — sqlite-vec's wrapper truncates the WAL on close
// when no other connections hold it open.
func closeAndCheckpoint(idx *index.Index) error {
	return idx.Close()
}

// WriteTarZst tars stage's tree and zstd-compresses to outPath using
// a temp file + rename for atomicity. Returns (sha256_hex, size_bytes).
// Exported so callers that build their own sqlite-vec databases (e.g.
// pbrainctl ingest-bulk staging, test fixtures) can produce snapshot-
// compatible tarballs without re-implementing the format.
func WriteTarZst(stage, outPath string) (string, int64, error) { return writeTarZst(stage, outPath) }

func writeTarZst(stage, outPath string) (string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return "", 0, err
	}
	tmp := outPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", 0, err
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}

	hash := sha256.New()
	mw := io.MultiWriter(f, hash)

	zw, err := zstd.NewWriter(mw, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		cleanup()
		return "", 0, fmt.Errorf("zstd writer: %w", err)
	}
	tw := tar.NewWriter(zw)

	if err := addTreeToTar(tw, stage); err != nil {
		_ = tw.Close()
		_ = zw.Close()
		cleanup()
		return "", 0, err
	}
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		cleanup()
		return "", 0, fmt.Errorf("tar close: %w", err)
	}
	if err := zw.Close(); err != nil {
		cleanup()
		return "", 0, fmt.Errorf("zstd close: %w", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return "", 0, fmt.Errorf("fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", 0, fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		_ = os.Remove(tmp)
		return "", 0, fmt.Errorf("rename: %w", err)
	}
	st, err := os.Stat(outPath)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), st.Size(), nil
}

func addTreeToTar(tw *tar.Writer, stage string) error {
	return filepath.Walk(stage, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stage, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// SQLite leaves WAL/SHM sidecars when the DB is closed without
		// a checkpoint; skip them since the main DB is self-contained
		// after Close().
		if strings.HasSuffix(rel, "-wal") || strings.HasSuffix(rel, "-shm") {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = strings.ReplaceAll(rel, string(filepath.Separator), "/")
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, src)
		_ = src.Close()
		return err
	})
}
