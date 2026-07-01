package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	pbserver "github.com/neverprepared/phantom-brain/internal/server"

	"github.com/neverprepared/phantom-brain/internal/pgstore"
)

// dbCmd groups the Postgres-as-System-of-Record provisioning tooling
// (epic #92). Postgres is isolated per profile: one engine, a DATABASE per
// profile (pb_<profile>). These are OPERATOR commands — the daemon never
// runs them and never auto-connects to Postgres.
//
// The --dsn flag carries the BASE/MAINTENANCE DSN (it should point at an
// existing database such as phantom_brain or postgres). `provision` connects
// there to CREATE DATABASE; `migrate` rewrites it to the per-profile db.
func dbCmd() *cobra.Command {
	var dsn string
	c := &cobra.Command{
		Use:   "db",
		Short: "Provision and migrate per-profile Postgres databases (epic #92)",
		Long: `Provision and migrate the per-profile Postgres System of Record.

Postgres is isolated per profile: one engine, a DATABASE per profile
(pb_personal, pb_gsa, …). The --dsn flag is the base/maintenance DSN and
should point at an existing maintenance database (e.g. phantom_brain or
postgres); the per-profile database name is derived as pb_<profile>.`,
	}
	c.PersistentFlags().StringVar(&dsn, "dsn", "",
		"base/maintenance Postgres DSN (default: $PB_POSTGRES_DSN, then $DATABASE_URL, then server.toml [postgres] dsn)")
	c.PersistentFlags().String("config-dir", "", "override PHANTOM_BRAIN_CONFIG_DIR (read [postgres] dsn from server.toml when --dsn/env are unset)")
	c.AddCommand(dbProvisionCmd(&dsn))
	c.AddCommand(dbMigrateCmd(&dsn))
	return c
}

// resolveDBDSN returns the base/maintenance DSN, preferring the explicit
// --dsn, then $PB_POSTGRES_DSN, then $DATABASE_URL, and finally the
// [postgres] dsn from the daemon's server.toml (resolved via --config-dir
// or the default config dir). The config fallback means `db provision`
// works with zero extra flags on the daemon host — the same server.toml
// the daemon reads is authoritative. Returns an actionable error when no
// source yields a DSN.
func resolveDBDSN(cmd *cobra.Command, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if v := os.Getenv("PB_POSTGRES_DSN"); v != "" {
		return v, nil
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v, nil
	}
	// Last resort: read the same server.toml the daemon uses. A missing or
	// unparseable config is not fatal here — fall through to the actionable
	// error so the operator sees the DSN guidance, not a config-read stack.
	if cfg, err := pbserver.LoadServerConfig(resolveConfigDir(cmd)); err == nil {
		if v := strings.TrimSpace(cfg.Postgres.DSN); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("no DSN: set --dsn, PB_POSTGRES_DSN / DATABASE_URL, " +
		"or [postgres] dsn in server.toml (via --config-dir); " +
		"it should point at the maintenance database, e.g. " +
		"postgres://pbrain:***@localhost:5433/phantom_brain")
}

func dbProvisionCmd(dsn *string) *cobra.Command {
	return &cobra.Command{
		Use:   "provision <profile>",
		Short: "Create the profile's database, enable extensions, and migrate",
		Long: `Create the per-profile database (if absent), enable the vector and
pg_trgm extensions in it, and apply all SoR migrations. Idempotent —
re-running on an existing, migrated profile is a no-op.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, err := resolveDBDSN(cmd, *dsn)
			if err != nil {
				return err
			}
			profile := args[0]
			dbName, err := pgstore.ProfileDBName(profile)
			if err != nil {
				return err
			}
			ctx, cancel := signalCancel(cmd.Context())
			defer cancel()
			if err := pgstore.Provision(ctx, base, profile); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "provisioned %s\n", dbName)
			return nil
		},
	}
}

func dbMigrateCmd(dsn *string) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate <profile>",
		Short: "Apply pending migrations to the profile's database",
		Long: `Apply all pending UP migrations to the per-profile database. The base
DSN is rewritten to target pb_<profile>. Idempotent — an up-to-date
database is a no-op. Does NOT create the database (use provision first).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, err := resolveDBDSN(cmd, *dsn)
			if err != nil {
				return err
			}
			profile := args[0]
			dbName, err := pgstore.ProfileDBName(profile)
			if err != nil {
				return err
			}
			profileDSN, err := pgstore.DSNForProfile(base, profile)
			if err != nil {
				return err
			}
			ctx, cancel := signalCancel(cmd.Context())
			defer cancel()
			if err := pgstore.Migrate(ctx, profileDSN); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "migrated %s\n", dbName)
			return nil
		},
	}
}
