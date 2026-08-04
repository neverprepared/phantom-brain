package main

import (
	"context"
	"encoding/json"
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

// isAttachmentKind matches an attachment stub. ListRecords returns the raw SoR
// kind "attachment"; other read paths translate it to KindAttachmentStub, so
// accept both.
func isAttachmentKind(k string) bool {
	return k == "attachment" || k == string(osearch.KindAttachmentStub)
}

// attachMeta is the sidecar written next to an exported blob so import can
// reconstruct the AttachRequest. The blob's identity is its content SHA (the
// file is named by it); the daemon re-verifies sha == sha256(bytes) on import.
type attachMeta struct {
	SHA              string   `json:"sha"`
	OriginalFilename string   `json:"original_filename,omitempty"`
	MIMEType         string   `json:"mime_type,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	ExtractedText    string   `json:"extracted_text,omitempty"`
}

// exportAttachment downloads a stub record's blob (streamed through the daemon,
// so it works from a host client) into <dest>/attachments/<sha> plus a
// <sha>.json sidecar.
func exportAttachment(ctx context.Context, client *brain.Client, dest string, rec brain.RecordDTO) error {
	data, err := client.AttachBytes(ctx, rec.SHA)
	if err != nil {
		return err
	}
	attDir := filepath.Join(dest, "attachments")
	if err := os.MkdirAll(attDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(attDir, rec.SHA), data, 0o644); err != nil {
		return err
	}
	side, _ := json.MarshalIndent(attachMeta{
		SHA: rec.SHA, OriginalFilename: rec.OriginalFilename, MIMEType: rec.MimeType,
		Tags: rec.Tags, ExtractedText: rec.Body,
	}, "", "  ")
	return os.WriteFile(filepath.Join(attDir, rec.SHA+".json"), side, 0o644)
}

// exportCmd materializes the token-scoped vault into a portable, re-importable
// markdown vault — the export half of the round-trip pair. Unlike `mart build`
// (a human-facing read-only projection: distilled body, cosmetic framing, an
// index.md), export writes the RAW body verbatim with full metadata under
// records/, so `pbrainctl client import` recomputes the same canonical identity
// and dedups. Attachment blobs go to attachments/. Basis for merge, portability,
// and archival.
func exportCmd() *cobra.Command {
	var api, token string
	var pageSize int
	c := &cobra.Command{
		Use:   "export <dest-dir>",
		Short: "Export a vault to a re-importable markdown vault (round-trip half)",
		Long: `export writes the token-scoped vault to <dest>/records/ — one file per
record: metadata frontmatter (kind, tags, captured_at, source, …) then the RAW
body verbatim — and attachment blobs to <dest>/attachments/. Pair with
'pbrainctl client import' for a faithful, idempotent (SHA-deduped) round-trip.
Prefer this over 'mart build' for anything you intend to re-import: mart is a
lossy human projection that does not round-trip.`,
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
			written, attachN := 0, 0
			// Enumerate BOTH synthesised states: pending-synth records and fresh
			// attachment stubs are Synthesised=false and the server defaults the
			// filter to true, so a single pass would miss them.
			for _, synth := range []bool{true, false} {
				s := synth
				var afterID int64
				for {
					resp, err := client.ListRecords(ctx, brain.ListRecordsRequest{AfterID: afterID, Limit: pageSize, Synthesised: &s})
					if err != nil {
						return fmt.Errorf("list records: %w", err)
					}
					for _, rec := range resp.Records {
						// Attachment stubs export as a blob + sidecar under
						// attachments/, not as a record file.
						if isAttachmentKind(rec.Kind) {
							if err := exportAttachment(ctx, client, dest, rec); err != nil {
								return fmt.Errorf("export attachment %s: %w", rec.SHA[:12], err)
							}
							attachN++
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
			}

			tail := ""
			if attachN > 0 {
				tail = fmt.Sprintf(" + %d attachment(s)", attachN)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "exported %d record(s)%s → %s\n", written, tail, dest)
			return nil
		},
	}
	c.Flags().StringVar(&api, "api", "", "daemon URL (defaults to $CL_BRAIN_API)")
	c.Flags().StringVar(&token, "token", "", "daemon bearer token (defaults to $CL_BRAIN_API_TOKEN)")
	c.Flags().IntVar(&pageSize, "page-size", 100, "records fetched per daemon request")
	return c
}
