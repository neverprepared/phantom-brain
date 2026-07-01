package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	"github.com/neverprepared/phantom-brain/internal/osproject"
	"github.com/neverprepared/phantom-brain/internal/pgstore"
	pbserver "github.com/neverprepared/phantom-brain/internal/server"
)

// bindingCmd is the v3.3 operator entry point that collapses the
// historical 5-step "carve out a new binding" recipe (mkdir, write
// auth.toml, write config.toml, mc mb, daemon reload) into one
// command. Lives under `pbrainctl server binding ...`.
func bindingCmd() *cobra.Command {
	c := &cobra.Command{Use: "binding", Short: "Operator workflow for per-binding config (v3.3)"}
	c.AddCommand(bindingCreateCmd(), bindingListCmd(), bindingDeleteCmd())
	return c
}

// --- create ----------------------------------------------------------

func bindingCreateCmd() *cobra.Command {
	var (
		indexPrefix  string
		bucket       string
		createBucket bool
		token        string
	)
	c := &cobra.Command{
		Use:   "create [profile/vault]",
		Short: "Create a new binding (config dir + auth.toml + optional storage overrides)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := vaultArgFromArgs(args)
			if err != nil {
				return err
			}
			if err := validateBindingSegment("profile", key.Profile); err != nil {
				return err
			}
			if err := validateBindingSegment("vault", key.Vault); err != nil {
				return err
			}
			if createBucket && bucket == "" {
				return errors.New("--create-bucket requires --bucket")
			}
			if indexPrefix != "" {
				// Reuse the daemon's registry-level validator so the CLI
				// and the daemon agree on what's a legal prefix.
				if err := pbserver.ValidateStorageOverridePrefix(indexPrefix); err != nil {
					return err
				}
			}

			configDir := resolveConfigDir(cmd)
			bindingDir := filepath.Join(configDir, "profiles", key.Profile, "vaults", key.Vault)
			if _, err := os.Stat(bindingDir); err == nil {
				return fmt.Errorf("binding %s already exists at %s — run `pbrainctl server binding delete %s` first",
					key, bindingDir, key)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("stat %s: %w", bindingDir, err)
			}
			if err := os.MkdirAll(bindingDir, 0o700); err != nil {
				return bindingWriteErr("mkdir", bindingDir, key.Profile, key.Vault, err)
			}

			if strings.TrimSpace(token) == "" {
				generated, err := newBearerToken()
				if err != nil {
					return err
				}
				token = generated
			} else if strings.TrimSpace(token) != token || token == "" {
				return errors.New("--token must be non-empty and contain no whitespace")
			}

			authPath := filepath.Join(bindingDir, "auth.toml")
			authBody := fmt.Sprintf("bearer_token = %q\n", token)
			if err := os.WriteFile(authPath, []byte(authBody), 0o600); err != nil {
				return bindingWriteErr("write", authPath, key.Profile, key.Vault, err)
			}

			cfgPath := ""
			if indexPrefix != "" || bucket != "" {
				cfgPath = filepath.Join(bindingDir, "config.toml")
				body := buildBindingConfigTOML(indexPrefix, bucket)
				if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
					return bindingWriteErr("write", cfgPath, key.Profile, key.Vault, err)
				}
			}

			if createBucket {
				ctx, cancel := signalCancel(cmd.Context())
				defer cancel()
				mb, err := openMinIOForOps(cmd)
				if err != nil {
					return err
				}
				if err := mb.CreateBucket(ctx, bucket); err != nil {
					return err
				}
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "binding %s created\n", key)
			fmt.Fprintf(out, "  auth   : %s\n", authPath)
			if cfgPath != "" {
				fmt.Fprintf(out, "  config : %s\n", cfgPath)
			}
			if createBucket {
				fmt.Fprintf(out, "  bucket : %s (created)\n", bucket)
			} else if bucket != "" {
				fmt.Fprintf(out, "  bucket : %s (pre-existing)\n", bucket)
			}
			fmt.Fprintf(out, "  token  : %s\n", token)
			fmt.Fprintln(out, "restart daemon (or SIGHUP via `pbrainctl server vault reload`) to load this binding")
			return nil
		},
	}
	opsCommonFlags(c)
	c.Flags().StringVar(&indexPrefix, "index-prefix", "", "OS index prefix override (appended to daemon-global prefix)")
	c.Flags().StringVar(&bucket, "bucket", "", "MinIO bucket override for this binding")
	c.Flags().BoolVar(&createBucket, "create-bucket", false, "also call MakeBucket on --bucket")
	c.Flags().StringVar(&token, "token", "", "bearer token (default: generated 32 random bytes hex)")
	return c
}

// bindingWriteErr wraps a filesystem write failure from `binding
// create`. When the cause is EROFS it returns an actionable hint
// instead of the bare syscall error: in production the daemon
// container bind-mounts /config read-only, so this subcommand has to
// be run against a writeable path (the bind-mount source on the host,
// or a local config dir whose result is copied into the config root).
func bindingWriteErr(op, path, profile, vault string, err error) error {
	if errors.Is(err, syscall.EROFS) {
		return fmt.Errorf("%s %s: config dir is read-only (typical in production: /config is bind-mounted ro into the daemon container).\n"+
			"Run this subcommand against a writeable path:\n"+
			"  - on the storage box host: --config-dir <path-on-host> (the bind-mount source)\n"+
			"  - on a workstation: write to a local --config-dir, then copy the resulting profiles/%s/vaults/%s/ subtree into the daemon's config root",
			op, path, profile, vault)
	}
	return fmt.Errorf("%s %s: %w", op, path, err)
}

// buildBindingConfigTOML writes the minimal [storage_overrides] body.
// We do this by hand rather than via a TOML encoder so the file stays
// human-readable + diff-friendly (operators edit these by hand).
func buildBindingConfigTOML(indexPrefix, bucket string) string {
	var b strings.Builder
	b.WriteString("[storage_overrides]\n")
	if indexPrefix != "" {
		fmt.Fprintf(&b, "index_prefix = %q\n", indexPrefix)
	}
	if bucket != "" {
		fmt.Fprintf(&b, "bucket = %q\n", bucket)
	}
	return b.String()
}

// newBearerToken returns 32 bytes of crypto/rand hex-encoded — a
// 64-character token, plenty of entropy for an auth.toml secret.
func newBearerToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate bearer token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// --- list ------------------------------------------------------------

func bindingListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "Show every binding with its resolved storage targets (v3.3)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := loadRegistryForOps(resolveConfigDir(cmd))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "PROFILE/VAULT\tINDEX_PREFIX\tBUCKET")
			cfg, _ := pbserver.LoadServerConfig(resolveConfigDir(cmd))
			defaultPrefix := ""
			defaultBucket := ""
			if cfg != nil {
				defaultPrefix = cfg.OpenSearch.IndexPrefix
				defaultBucket = cfg.Storage.MinIOBucket
			}
			for _, b := range r.Vaults() {
				prefix := b.Storage.IndexPrefix
				if prefix == defaultPrefix {
					prefix = "<shared>"
				}
				bucket := b.Storage.Bucket
				if bucket == defaultBucket {
					bucket = "<default>"
				}
				fmt.Fprintf(out, "%s\t%s\t%s\n",
					b.Key, prefix, bucket)
			}
			return nil
		},
	}
	opsCommonFlags(c)
	return c
}

// --- delete ----------------------------------------------------------

func bindingDeleteCmd() *cobra.Command {
	var (
		purgeData     bool
		confirm       bool
		confirmTarget string
		allowShared   bool
		dsn           string
	)
	c := &cobra.Command{
		Use:   "delete [profile/vault]",
		Short: "Delete a binding's config (and, with --purge-data, its OS projection index, MinIO bucket, and Postgres rows)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := vaultArgFromArgs(args)
			if err != nil {
				return err
			}
			configDir := resolveConfigDir(cmd)
			bindingDir := filepath.Join(configDir, "profiles", key.Profile, "vaults", key.Vault)
			if _, err := os.Stat(bindingDir); errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("binding %s not found at %s", key, bindingDir)
			}

			cfg, err := pbserver.LoadServerConfig(configDir)
			if err != nil {
				return fmt.Errorf("load server config: %w", err)
			}
			r, err := loadRegistryForOps(configDir)
			if err != nil {
				return err
			}
			binding, ok := r.LookupByVault(key)
			if !ok {
				return fmt.Errorf("binding %s present on disk but not in registry — fix config or rerun with the file removed", key)
			}

			hasOverride := binding.Storage.IndexPrefix != cfg.OpenSearch.IndexPrefix ||
				binding.Storage.Bucket != cfg.Storage.MinIOBucket
			usesSharedBucket := binding.Storage.Bucket == cfg.Storage.MinIOBucket
			usesSharedIndices := binding.Storage.IndexPrefix == cfg.OpenSearch.IndexPrefix

			// The irreversible purge is gated on a typed-name match, not a
			// bare boolean: --confirm-target must equal the exact binding
			// key. This guards against confirming a delete of the WRONG
			// binding (a bool can't). The recoverable config-only delete
			// keeps the lighter --confirm bool.
			purgeConfirmed := purgeData && confirmTarget == key.String()
			doDelete := confirm || purgeConfirmed

			out := cmd.OutOrStdout()
			action := "DRY-RUN"
			if doDelete {
				action = "DELETE"
			}
			fmt.Fprintf(out, "%s binding %s\n", action, key)
			fmt.Fprintf(out, "  config dir: %s\n", bindingDir)
			if purgeData {
				// Structural safety first — these configs are never safe to
				// purge regardless of confirmation, so reject before asking
				// the operator to type the name.
				if !hasOverride {
					return fmt.Errorf("--purge-data refused: binding %s has no [storage_overrides] (deleting shared resources is never safe)", key)
				}
				if usesSharedIndices && !allowShared {
					return errors.New("--purge-data refused: binding writes to shared OS indices; re-run with --allow-shared to confirm you understand the blast radius")
				}
				if usesSharedBucket && !allowShared {
					return errors.New("--purge-data refused: binding writes to the shared default MinIO bucket; re-run with --allow-shared (will skip bucket purge regardless)")
				}
				// Typed-name confirmation for the irreversible purge.
				if !purgeConfirmed {
					if confirmTarget == "" {
						return fmt.Errorf("--purge-data is irreversible (drops the OS index, MinIO bucket, and Postgres rows).\n"+
							"Re-run with --confirm-target=%s to confirm you mean this exact binding.", key)
					}
					return fmt.Errorf("--confirm-target %q does not match the binding %s — aborting to avoid deleting the wrong binding", confirmTarget, key)
				}
				ctx, cancel := signalCancel(cmd.Context())
				defer cancel()
				if err := purgeBindingData(ctx, cmd, cfg, binding, dsn, out); err != nil {
					return err
				}
			} else if confirm {
				fmt.Fprintln(out, "  (config only — data left in place; add --purge-data to drop index + bucket + Postgres rows)")
			} else {
				// dry-run: show what would happen
				if hasOverride {
					fmt.Fprintf(out, "  would-purge-with-flag: index=%spb_records bucket=%s + Postgres rows for %s\n",
						binding.Storage.IndexPrefix, binding.Storage.Bucket, key)
				}
				fmt.Fprintf(out, "  add --confirm to remove the config dir; add --purge-data --confirm-target=%s to also drop OS + MinIO + Postgres state\n", key)
				return nil
			}

			if err := os.RemoveAll(bindingDir); err != nil {
				return fmt.Errorf("remove %s: %w", bindingDir, err)
			}
			fmt.Fprintf(out, "removed %s\n", bindingDir)
			fmt.Fprintln(out, "restart daemon (or SIGHUP via `pbrainctl server vault reload`) to drop the binding")
			return nil
		},
	}
	opsCommonFlags(c)
	c.Flags().BoolVar(&purgeData, "purge-data", false, "also drop the OS projection index, (non-default) MinIO bucket, and the binding's Postgres rows")
	c.Flags().BoolVar(&confirm, "confirm", false, "actually delete the config (without this flag the command is a dry-run); not sufficient for --purge-data")
	c.Flags().StringVar(&confirmTarget, "confirm-target", "", "typed-name confirmation for the irreversible --purge-data: must equal <profile/vault>")
	c.Flags().BoolVar(&allowShared, "allow-shared", false, "permit --purge-data on a binding sharing default indices/bucket")
	c.Flags().StringVar(&dsn, "dsn", "", "base/maintenance Postgres DSN for the row purge (default: $PB_POSTGRES_DSN, then $DATABASE_URL, then server.toml [postgres] dsn)")
	return c
}

// purgeBindingData drops the binding's data across all three stores: the
// prefixed OS projection index (post-#92 that is the single pb_records
// index, NOT the retired pb_summaries/pb_entities/pb_attachments trio),
// its own non-default MinIO bucket, and its rows in the per-profile
// Postgres SoR. Shared resources are NEVER dropped wholesale — callers
// route here only after the safety predicates pass, and the Postgres
// purge is a scoped (profile, vault) row delete rather than a database
// drop (pb_<profile> is shared across the profile's vaults).
func purgeBindingData(ctx context.Context, cmd *cobra.Command, cfg *pbserver.ServerConfig, binding pbserver.VaultBinding, dsnFlag string, out writer) error {
	if cfg.OpenSearch.Enabled() && binding.Storage.IndexPrefix != cfg.OpenSearch.IndexPrefix {
		oc, err := osearch.Open(ctx, osearch.Config{
			Addresses:          cfg.OpenSearch.Addresses,
			Username:           cfg.OpenSearch.Username,
			Password:           cfg.OpenSearch.Password,
			InsecureSkipVerify: cfg.OpenSearch.InsecureSkipVerify,
			IndexPrefix:        binding.Storage.IndexPrefix,
			RequestTimeout:     time.Duration(cfg.OpenSearch.RequestTimeoutSecs) * time.Second,
		})
		if err != nil {
			return fmt.Errorf("opensearch open: %w", err)
		}
		full := osearch.IndexNameWithPrefix(binding.Storage.IndexPrefix, osproject.LogicalRecords)
		if err := oc.DeleteIndex(ctx, osproject.LogicalRecords); err != nil {
			return fmt.Errorf("delete index %s: %w", full, err)
		}
		fmt.Fprintf(out, "  dropped OS index %s\n", full)
	}

	if binding.Storage.Bucket != "" && binding.Storage.Bucket != cfg.Storage.MinIOBucket && cfg.Storage.Backend == "minio" {
		mb, err := openMinIOForOps(cmd)
		if err != nil {
			return err
		}
		if err := mb.RemoveBucketWithObjects(ctx, binding.Storage.Bucket); err != nil {
			return err
		}
		fmt.Fprintf(out, "  dropped MinIO bucket %s\n", binding.Storage.Bucket)
	}

	// Postgres SoR: delete only this binding's rows. Skipped (not failed)
	// when no DSN is resolvable — the operator may run a legacy OS-only
	// daemon, and a missing DSN shouldn't block the config/OS/bucket purge.
	if base, err := resolveDBDSN(cmd, dsnFlag); err == nil {
		counts, err := pgstore.PurgeBinding(ctx, base, binding.Key.Profile, binding.Key.Vault)
		if err != nil {
			return annotatePGConnectErr(err)
		}
		fmt.Fprintf(out, "  purged Postgres rows for %s: %d records, %d entities, %d fact-history\n",
			binding.Key, counts.Records, counts.Entities, counts.FactHistory)
	} else {
		fmt.Fprintln(out, "  skipped Postgres purge: no DSN (set --dsn / PB_POSTGRES_DSN, or add [postgres] dsn to server.toml)")
	}
	return nil
}

