package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/vaultxfer"
)

// exportCmd materializes the token-scoped vault into a portable, re-importable
// markdown vault — the export half of the round-trip pair (import is the other).
// Unlike `mart build` (a human-facing read-only projection that emits a distilled
// body + an index and injects cosmetic framing), export writes the RAW body
// verbatim with full metadata under records/, so `pbrainctl client import`
// recomputes the same canonical identity and dedups. This is the basis for
// merge, portability, and archival.
func exportCmd() *cobra.Command {
	var api, token string
	var pageSize int
	c := &cobra.Command{
		Use:   "export <dest-dir>",
		Short: "Export a vault to a re-importable markdown vault (round-trip half)",
		Long: `export writes the token-scoped vault to <dest>/records/ — one file per
record: metadata frontmatter (kind, tags, captured_at, source, …) then the RAW
body verbatim. Pair with 'pbrainctl client import' for a faithful, idempotent
(SHA-deduped) round-trip. Prefer this over 'mart build' for anything you intend
to re-import: mart is a lossy human projection (distilled body, cosmetic framing,
an index.md) that does not round-trip.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dest := args[0]
			if api == "" {
				api = strings.TrimSpace(os.Getenv("CL_BRAIN_API"))
			}
			if token == "" {
				token = strings.TrimSpace(os.Getenv("CL_BRAIN_API_TOKEN"))
			}
			if api == "" || token == "" {
				return fmt.Errorf("export: --api/--token (or CL_BRAIN_API/CL_BRAIN_API_TOKEN env) required")
			}
			if pageSize <= 0 {
				pageSize = 100
			}
			client, err := brain.NewClient(brain.ClientOpts{BaseURL: api, Token: token, Timeout: 60 * time.Second})
			if err != nil {
				return fmt.Errorf("build daemon client: %w", err)
			}
			recordsDir := filepath.Join(dest, "records")
			if err := os.MkdirAll(recordsDir, 0o755); err != nil {
				return err
			}

			ctx := cmd.Context()
			var afterID int64
			written, skipped := 0, 0
			for {
				resp, err := client.ListRecords(ctx, brain.ListRecordsRequest{AfterID: afterID, Limit: pageSize})
				if err != nil {
					return fmt.Errorf("list records: %w", err)
				}
				for _, rec := range resp.Records {
					// Attachment stubs have no re-importable text body — blob
					// export is a follow-up. Skip for now.
					if rec.Kind == string(osearch.KindAttachmentStub) {
						skipped++
						continue
					}
					body := rec.RawBody // the exact bytes the SHA is derived over
					if body == "" {
						body = rec.Body
					}
					meta := vaultxfer.Meta{
						SHA: rec.SHA, Kind: rec.Kind, Title: rec.Title, Tags: rec.Tags,
						Topic: rec.Topic, Reliability: rec.Reliability, MemoryType: rec.MemoryType,
						Source: rec.Source, SourceURL: rec.SourceURL,
					}
					if rec.CapturedAt != nil {
						meta.CapturedAt = rec.CapturedAt.UTC().Format(time.RFC3339)
					}
					raw, err := vaultxfer.Encode(meta, body)
					if err != nil {
						return err
					}
					fn := vaultxfer.Filename(rec.Title, rec.SHA)
					if err := os.WriteFile(filepath.Join(recordsDir, fn), raw, 0o644); err != nil {
						return err
					}
					written++
				}
				if resp.NextAfterID == 0 || len(resp.Records) == 0 {
					break
				}
				afterID = resp.NextAfterID
			}

			tail := ""
			if skipped > 0 {
				tail = fmt.Sprintf(" (%d attachment stub(s) skipped)", skipped)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "exported %d record(s)%s → %s\n", written, tail, recordsDir)
			return nil
		},
	}
	c.Flags().StringVar(&api, "api", "", "daemon URL (defaults to $CL_BRAIN_API)")
	c.Flags().StringVar(&token, "token", "", "daemon bearer token (defaults to $CL_BRAIN_API_TOKEN)")
	c.Flags().IntVar(&pageSize, "page-size", 100, "records fetched per daemon request")
	return c
}
