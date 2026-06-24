package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/osearch"
	pbserver "github.com/neverprepared/phantom-brain/internal/server"
)

// configCmd groups daemon-config inspection levers. Today just
// `validate`; a `show`/`lint` sibling could land here later.
func configCmd() *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "Validate or inspect daemon config"}
	c.AddCommand(configValidateCmd())
	return c
}

// configValidateCmd runs the exact registry-load path the daemon runs
// at startup (server.toml parse, per-binding TOML parse, override-prefix
// regex, cross-binding bearer-token dedup, and the v3.2 storage-override
// footgun guard) WITHOUT binding an HTTP listener or starting a worker.
//
// It lets an operator catch a bad hand-edit before `docker compose
// restart` crash-loops the daemon and strands every binding (including
// unaffected ones). Issue #70.
func configValidateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "validate [profile/vault]",
		Short: "Dry-run the daemon's startup config load without serving (issue #70)",
		Long: `Validate the daemon config the same way startup does, but without serving.

Checks: server.toml parse, per-binding TOML parse, [storage_overrides]
index-prefix regex, cross-binding bearer-token dedup, and (when OpenSearch
is reachable) the v3.2 storage-override footgun guard.

With no argument, validates every binding. With profile/vault, validates
just that binding in isolation and skips the cross-binding token-dedup
check (which needs the full set).

Exits 0 on a clean load; non-zero with the same error the daemon would
emit at startup.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			configDir := resolveConfigDir(cmd)
			out := cmd.OutOrStdout()

			cfg, err := pbserver.LoadServerConfig(configDir)
			if err != nil {
				return err
			}

			var bindings []pbserver.VaultBinding
			single := false

			if len(args) == 1 {
				// Single-binding mode: load just this binding's files in
				// isolation. Mirrors the per-vault resolution in
				// Registry.Load (internal/server/registry.go) — keep in
				// sync. Cross-binding token dedup is intentionally skipped
				// (it needs the full set; a sibling binding may be absent
				// or unreadable from where this runs).
				key, err := vaultArgFromArgs(args)
				if err != nil {
					return err
				}
				overrides, auth, err := pbserver.LoadVaultFiles(configDir, key.Profile, key.Vault)
				if err != nil {
					return err
				}
				if err := pbserver.ValidateStorageOverridePrefix(overrides.StorageOverrides.IndexPrefix); err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
				storage := pbserver.ResolvedStorage{
					IndexPrefix: cfg.OpenSearch.IndexPrefix + overrides.StorageOverrides.IndexPrefix,
					Bucket:      cfg.Storage.MinIOBucket,
				}
				if overrides.StorageOverrides.Bucket != "" {
					storage.Bucket = overrides.StorageOverrides.Bucket
				}
				bindings = []pbserver.VaultBinding{{
					Key:      key,
					Auth:     auth,
					Defaults: pbserver.MergedDefaults(cfg.Defaults, overrides),
					Storage:  storage,
				}}
				single = true
			} else {
				// Full load: parse every binding, override regex, and the
				// cross-binding token dedup — exactly what the daemon does
				// at startup. Any error here is the same one it would emit.
				reg := pbserver.NewRegistry()
				if _, err := reg.Load(pbserver.LoadOpts{
					ConfigDir:          configDir,
					Defaults:           cfg.Defaults,
					DefaultIndexPrefix: cfg.OpenSearch.IndexPrefix,
					DefaultBucket:      cfg.Storage.MinIOBucket,
				}); err != nil {
					return err
				}
				bindings = reg.Vaults()
			}

			// Storage-override footgun guard (v3.2) needs OpenSearch.
			// Best-effort: if OS is off or unreachable, warn and skip
			// rather than fail — the pure checks above already passed and
			// the operator may be validating syntax from a box without OS.
			footgun := false
			if cfg.OpenSearch.Enabled() {
				ctx, cancel := signalCancel(cmd.Context())
				defer cancel()
				oc, oerr := osearch.Open(ctx, osearch.Config{
					Addresses:          cfg.OpenSearch.Addresses,
					Username:           cfg.OpenSearch.Username,
					Password:           cfg.OpenSearch.Password,
					InsecureSkipVerify: cfg.OpenSearch.InsecureSkipVerify,
					IndexPrefix:        cfg.OpenSearch.IndexPrefix,
					RequestTimeout:     time.Duration(cfg.OpenSearch.RequestTimeoutSecs) * time.Second,
				})
				if oerr != nil {
					fmt.Fprintf(out, "warning: OpenSearch unreachable, skipped storage-override footgun guard: %v\n", oerr)
				} else if err := pbserver.VerifyStorageOverrides(ctx, oc, cfg.OpenSearch.IndexPrefix, bindings); err != nil {
					return err
				} else {
					footgun = true
				}
			} else {
				fmt.Fprintln(out, "warning: OpenSearch not configured, skipped storage-override footgun guard")
			}

			if single {
				fmt.Fprintf(out, "OK: binding %s validates (TOML, override prefix", bindings[0].Key)
			} else {
				fmt.Fprintf(out, "OK: %d binding(s) validate (TOML, override prefixes, token dedup", len(bindings))
			}
			if footgun {
				fmt.Fprint(out, ", footgun guard")
			}
			fmt.Fprintln(out, ")")
			if single {
				fmt.Fprintln(out, "note: cross-binding token-dedup check skipped in single-binding mode")
			}
			return nil
		},
	}
	opsCommonFlags(c)
	return c
}
