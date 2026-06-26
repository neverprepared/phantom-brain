package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/backfill"
	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
	pbserver "github.com/neverprepared/phantom-brain/internal/server"
)

// backfillToPGCmd is the Phase B2 one-shot loader (daemon-cutover plan):
// load a profile's EXISTING legacy OpenSearch corpus into the new Postgres
// System-of-Record + pb_records projection, reusing the legacy embeddings
// (NO re-embed) and reconstructing the entity graph. Additive tooling — it
// talks to OS + Postgres directly (no daemon dependency), changes no live
// path, and is idempotent + resumable (re-running is safe).
//
// Run on the daemon host (it reads the same server.toml + registry the
// daemon uses for OS config + per-binding index prefixes). Requires
// `pbrainctl server db provision <profile>` to have created the profile DB.
func backfillToPGCmd() *cobra.Command {
	var (
		dsn    string
		vault  string
		dryRun bool
		noEnts bool
		batch  int
	)
	c := &cobra.Command{
		Use:   "backfill-to-pg <profile>",
		Short: "Load a profile's legacy OpenSearch corpus into Postgres + pb_records (Phase B2)",
		Long: `Scroll a profile's existing pb_summaries (and pb_entities) out of the
legacy OpenSearch store and load them into the per-profile Postgres
System-of-Record (records / entities / record_entities) plus the
pb_records search projection, so recall parity can be validated BEFORE
the read cutover.

Reuses the existing 768-dim embeddings (no re-embed) and reconstructs
the entity graph by inverting the legacy MentionedBy[] backlinks into
record_entities links. Idempotent + resumable — re-running re-scrolls
and no-ops the rows that already exist.

The --dsn flag is the BASE/maintenance DSN; the per-profile database
(pb_<profile>) is derived from it. Requires the profile DB to exist
(run 'pbrainctl server db provision <profile>' first).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile := args[0]

			base, err := resolveDBDSN(dsn)
			if err != nil {
				return err
			}

			// Load the same server config + registry the daemon uses, so
			// OS connection settings + per-binding index prefixes match.
			configDir := resolveConfigDir(cmd)
			cfg, err := pbserver.LoadServerConfig(configDir)
			if err != nil {
				return fmt.Errorf("backfill: load server config: %w", err)
			}
			if !cfg.OpenSearch.Enabled() {
				return fmt.Errorf("backfill: server.toml has no [opensearch] block — backfill needs OS access")
			}
			reg, err := loadRegistryForOps(configDir)
			if err != nil {
				return fmt.Errorf("backfill: load registry: %w", err)
			}

			// Resolve the profile's vault binding(s) → VaultRef with the
			// per-binding index prefix. Filter to --vault when given.
			var refs []backfill.VaultRef
			var known []string
			for _, b := range reg.Vaults() {
				if b.Key.Profile != profile {
					continue
				}
				known = append(known, b.Key.Vault)
				if vault != "" && b.Key.Vault != vault {
					continue
				}
				refs = append(refs, backfill.VaultRef{
					Vault:       b.Key.Vault,
					IndexPrefix: b.Storage.IndexPrefix,
				})
			}
			if len(known) == 0 {
				return fmt.Errorf("backfill: profile %q has no vaults in the registry — check --config-dir and that auth.toml exists", profile)
			}
			if len(refs) == 0 {
				return fmt.Errorf("backfill: vault %q not found for profile %q (known vaults: %s)",
					vault, profile, strings.Join(known, ", "))
			}

			ctx, cancel := signalCancel(cmd.Context())
			defer cancel()

			// OpenSearch — same credentials the daemon uses.
			osc, err := osearch.Open(ctx, osearch.Config{
				Addresses:          cfg.OpenSearch.Addresses,
				Username:           cfg.OpenSearch.Username,
				Password:           cfg.OpenSearch.Password,
				InsecureSkipVerify: cfg.OpenSearch.InsecureSkipVerify,
				IndexPrefix:        cfg.OpenSearch.IndexPrefix,
				RequestTimeout:     time.Duration(cfg.OpenSearch.RequestTimeoutSecs) * time.Second,
			})
			if err != nil {
				return fmt.Errorf("backfill: opensearch unreachable (check [opensearch].addresses in server.toml): %w", err)
			}

			// Postgres — the per-profile SoR database.
			profileDSN, err := pgstore.DSNForProfile(base, profile)
			if err != nil {
				return err
			}
			pool, err := pgstore.Open(ctx, profileDSN)
			if err != nil {
				return fmt.Errorf("backfill: postgres unreachable — provision the profile DB "+
					"('pbrainctl server db provision %s') and check --dsn host is reachable "+
					"(e.g. localhost:5433 from the storage host): %w", profile, err)
			}
			defer pool.Close()

			stats, err := backfill.Run(ctx, backfill.Options{
				OS:              osc,
				PG:              pool,
				Profile:         profile,
				Vaults:          refs,
				DryRun:          dryRun,
				IncludeEntities: !noEnts,
				BatchSize:       batch,
			})
			if err != nil {
				return err
			}

			printBackfillToPGStats(cmd, profile, dryRun, stats)
			return nil
		},
	}
	c.Flags().StringVar(&dsn, "dsn", "",
		"base/maintenance Postgres DSN (default: $PB_POSTGRES_DSN, then $DATABASE_URL); per-profile db derived")
	c.Flags().StringVar(&vault, "vault", "", "limit to one vault (default: all vaults of the profile)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "scroll + count only; write nothing to Postgres or pb_records")
	c.Flags().BoolVar(&noEnts, "no-entities", false, "skip Pass 2 (entity-graph reconstruction); records only")
	c.Flags().IntVar(&batch, "batch", 500, "OpenSearch scroll page size")
	opsCommonFlags(c)
	return c
}

// printBackfillToPGStats renders the per-vault + total tally as a table.
func printBackfillToPGStats(cmd *cobra.Command, profile string, dryRun bool, s backfill.Stats) {
	out := cmd.OutOrStdout()
	mode := "applied"
	if dryRun {
		mode = "DRY-RUN (no writes)"
	}
	fmt.Fprintf(out, "backfill-to-pg %s — %s\n", profile, mode)
	fmt.Fprintf(out, "%-20s %8s %8s %8s %9s %8s %6s %7s %7s\n",
		"vault", "ins", "dup", "synth", "entities", "aliases", "links", "misses", "errors")
	row := func(v backfill.VaultStats) {
		fmt.Fprintf(out, "%-20s %8d %8d %8d %9d %8d %6d %7d %7d\n",
			v.Vault, v.RecordsInserted, v.RecordsDup, v.RecordsSynthed,
			v.EntitiesUpserted, v.AliasesAdded, v.LinksCreated, v.EntityLinkMisses, v.Errors)
	}
	for _, v := range s.PerVault {
		row(v)
	}
	total := s.Total
	total.Vault = "TOTAL"
	row(total)

	// Surface sample errors (per-item failures that were skipped, not
	// aborted) so the operator can see WHAT was dropped, not just a count.
	for _, v := range s.PerVault {
		if len(v.SampleErrors) == 0 {
			continue
		}
		fmt.Fprintf(out, "\n%s: %d error(s) skipped; first %d:\n", v.Vault, v.Errors, len(v.SampleErrors))
		for _, e := range v.SampleErrors {
			fmt.Fprintf(out, "  - %s\n", e)
		}
	}
}
