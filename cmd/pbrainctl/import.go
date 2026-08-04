package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/ollama"
	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/vaultxfer"
)

// loadEmbeddingSidecar returns a carried vector from <vault>/embeddings/<sha>.json
// when present and dimension-compatible, else (nil, false) so the caller re-embeds.
func loadEmbeddingSidecar(vaultDir, sha string) ([]float32, bool) {
	if sha == "" {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(vaultDir, "embeddings", sha+".json"))
	if err != nil {
		return nil, false
	}
	var v []float32
	if json.Unmarshal(b, &v) != nil || len(v) != osearch.EmbeddingDim {
		return nil, false
	}
	return v, true
}

// importCmd loads a vault produced by `pbrainctl client export` back into the
// token-scoped vault — the import half of the round-trip pair. Idempotent: the
// daemon derives the canonical, kind-aware SHA, so re-importing an unchanged
// vault is a no-op and importing two vaults is a dedup-safe union (the merge
// story). Skips index.md + dotfiles so a `mart build` directory does not inject
// spurious records. Also re-attaches any blobs under attachments/.
func importCmd() *cobra.Command {
	var api, token string
	var dryRun bool
	c := &cobra.Command{
		Use:   "import <vault-dir>",
		Short: "Import a round-trip markdown vault (idempotent, SHA-deduped)",
		Long: `import reads <vault>/records/*.md (as written by 'pbrainctl client export'),
restores each record's metadata + raw body, re-embeds locally via Ollama, and
writes through the daemon. The daemon derives the canonical SHA, so re-import is
idempotent and importing two vaults is a dedup-safe union. index.md and dotfiles
are skipped so a 'mart build' directory does not inject spurious records.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			recordsDir := filepath.Join(args[0], "records")
			entries, err := os.ReadDir(recordsDir)
			if err != nil {
				return fmt.Errorf("read %s: %w", recordsDir, err)
			}
			var files []string
			for _, e := range entries {
				n := e.Name()
				if e.IsDir() || !strings.HasSuffix(n, ".md") {
					continue
				}
				if n == "index.md" || strings.HasPrefix(n, ".") {
					continue // mart index / manifests / dotfiles are not records
				}
				files = append(files, filepath.Join(recordsDir, n))
			}

			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "import: %d record file(s) under %s (dry-run)\n", len(files), recordsDir)
				return nil
			}
			if api == "" {
				api = strings.TrimSpace(os.Getenv("CL_BRAIN_API"))
			}
			if token == "" {
				token = strings.TrimSpace(os.Getenv("CL_BRAIN_API_TOKEN"))
			}
			if api == "" || token == "" {
				return fmt.Errorf("import: --api/--token (or CL_BRAIN_API/CL_BRAIN_API_TOKEN env) required")
			}
			client, err := brain.NewClient(brain.ClientOpts{BaseURL: api, Token: token, Timeout: 600 * time.Second})
			if err != nil {
				return fmt.Errorf("build daemon client: %w", err)
			}
			emb := ollama.New(ollama.OptionsFromEnv())
			if emb.Dims() != osearch.EmbeddingDim {
				return fmt.Errorf("embedder dim %d != %d — daemon would reject vectors", emb.Dims(), osearch.EmbeddingDim)
			}

			ctx := cmd.Context()
			okN, failN := 0, 0
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "read %s: %v\n", filepath.Base(f), err)
					failN++
					continue
				}
				meta, body, err := vaultxfer.Decode(raw)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "decode %s: %v\n", filepath.Base(f), err)
					failN++
					continue
				}
				if strings.TrimSpace(body) == "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "skip %s: empty body\n", filepath.Base(f))
					failN++
					continue
				}
				// Prefer a carried vector (export --with-embeddings) over re-embedding.
				embedding, haveEmb := loadEmbeddingSidecar(args[0], meta.SHA)
				if !haveEmb {
					embs, err := emb.Embed(ctx, []string{strings.TrimSpace(meta.Title + "\n\n" + body)})
					if err != nil {
						return fmt.Errorf("embed %s: %w", filepath.Base(f), err)
					}
					embedding = embs[0]
				}
				// SHA is advisory — the daemon derives the canonical identity. Use
				// the exported checksum when it is a valid 64-hex, else a placeholder.
				sha := meta.SHA
				if len(sha) != 64 {
					sha = osearch.SHA256Hex([]byte(body))
				}
				mf := brain.MemoryFields{
					Kind: meta.Kind, MemoryType: meta.MemoryType, Topic: meta.Topic,
					Reliability: meta.Reliability, Source: meta.Source,
				}
				if meta.CapturedAt != "" {
					if t, e := time.Parse(time.RFC3339, meta.CapturedAt); e == nil {
						mf.CapturedAt = &t
					}
				}
				if _, err := client.Learn(ctx, brain.LearnRequest{
					SHA: sha, Title: meta.Title, Body: body, Tags: meta.Tags,
					Embedding: embedding, MemoryFields: mf,
				}); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "learn %s: %v\n", filepath.Base(f), err)
					failN++
					continue
				}
				okN++
			}

			// Attachments: attachments/<sha> (blob) + <sha>.json (sidecar). The
			// daemon re-verifies sha == sha256(bytes) and recreates the companion
			// stub; idempotent by SHA.
			attOk, attFail := 0, 0
			attDir := filepath.Join(args[0], "attachments")
			if ents, rerr := os.ReadDir(attDir); rerr == nil {
				for _, e := range ents {
					if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
						continue
					}
					sha := strings.TrimSuffix(e.Name(), ".json")
					var side struct {
						OriginalFilename string   `json:"original_filename"`
						MIMEType         string   `json:"mime_type"`
						Tags             []string `json:"tags"`
						ExtractedText    string   `json:"extracted_text"`
					}
					if sj, err := os.ReadFile(filepath.Join(attDir, e.Name())); err == nil {
						_ = json.Unmarshal(sj, &side)
					}
					data, err := os.ReadFile(filepath.Join(attDir, sha))
					if err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "attachment %s: missing blob\n", e.Name())
						attFail++
						continue
					}
					if _, err := client.Attach(ctx, brain.AttachRequest{
						SHA:              osearch.SHA256Hex(data),
						OriginalFilename: side.OriginalFilename,
						MIMEType:         side.MIMEType,
						BytesB64:         base64.StdEncoding.EncodeToString(data),
						ExtractedText:    side.ExtractedText,
						Tags:             side.Tags,
					}); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "attach %s: %v\n", e.Name(), err)
						attFail++
						continue
					}
					attOk++
				}
			}

			tail := ""
			if attOk > 0 || attFail > 0 {
				tail = fmt.Sprintf(", attachments ok=%d failed=%d", attOk, attFail)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "import done: records ok=%d failed=%d%s\n", okN, failN, tail)
			return nil
		},
	}
	c.Flags().StringVar(&api, "api", "", "daemon URL (defaults to $CL_BRAIN_API)")
	c.Flags().StringVar(&token, "token", "", "daemon bearer token (defaults to $CL_BRAIN_API_TOKEN)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "list record files without importing")
	return c
}
