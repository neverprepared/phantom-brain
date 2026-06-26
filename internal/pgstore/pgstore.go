// Package pgstore is the operator tooling for Postgres-as-System-of-Record
// (epic #92). Postgres is isolated PER PROFILE: one engine, one DATABASE per
// profile (pb_personal, pb_gsa, …), resolved like the existing per-binding
// MinIO bucket / OS index-prefix.
//
// This package provisions a profile's database, enables the per-database
// extensions (vector, pg_trgm), and applies the SoR migrations. It is
// operator tooling only — the daemon (`server serve`) does NOT import or
// auto-connect through this package.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	// pgx5 database driver, registered for its side effect (the "pgx5://"
	// scheme golang-migrate dispatches on).
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"

	"github.com/neverprepared/phantom-brain/migrations"
)

// profileRe is the strict identifier guard. Database names CANNOT be
// parameterized in SQL, so a validated profile is the first line of defense
// against injection (pgx.Identifier.Sanitize() is the second). We accept
// only lowercase ASCII letters, digits, and underscore.
var profileRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// ProfileDBName derives the per-profile database name (pb_<profile>).
//
// Validation rule: the profile is lowercased first, then must match
// ^[a-z0-9_]+$ — i.e. after lowercasing it may contain ONLY lowercase ASCII
// letters, digits, and underscore, and must be non-empty. Hyphens, dots,
// spaces, semicolons, and any other character are rejected. (Lowercasing is
// a normalization, not a relaxation: "GSA" → "gsa" is accepted, but "a-b"
// or "a b" or "a;b" are rejected.)
func ProfileDBName(profile string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(profile))
	if p == "" {
		return "", fmt.Errorf("pgstore: empty profile name")
	}
	if !profileRe.MatchString(p) {
		return "", fmt.Errorf("pgstore: invalid profile %q: must match ^[a-z0-9_]+$ (lowercase letters, digits, underscore)", profile)
	}
	return "pb_" + p, nil
}

// DSNForProfile rewrites baseDSN's database name (the URL path) to the
// per-profile database, preserving every other part (user, password, host,
// port, query params). baseDSN points at the maintenance database; the
// returned DSN points at pb_<profile>.
func DSNForProfile(baseDSN, profile string) (string, error) {
	dbName, err := ProfileDBName(profile)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(baseDSN)
	if err != nil {
		return "", fmt.Errorf("pgstore: parse DSN: %w", err)
	}
	u.Path = "/" + dbName
	return u.String(), nil
}

// Provision creates the per-profile database (if absent), enables the
// per-database extensions, and applies the SoR migrations. It is idempotent:
// re-running against an existing, migrated profile is a no-op.
//
// baseDSN must point at an existing maintenance database (e.g. phantom_brain
// or postgres) — it is used as-is to CREATE DATABASE, then DSNForProfile
// rewrites it to target the new database for extensions + migrations.
func Provision(ctx context.Context, baseDSN, profile string) error {
	dbName, err := ProfileDBName(profile)
	if err != nil {
		return err
	}

	// 1. Connect to the maintenance db and CREATE DATABASE if absent.
	//    CREATE DATABASE can't run in a transaction and can't be
	//    parameterized, hence the SELECT-then-Exec with Sanitize().
	maint, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		return fmt.Errorf("pgstore: connect to maintenance db: %w", err)
	}
	var exists bool
	err = maint.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", dbName).Scan(&exists)
	if err != nil {
		maint.Close(ctx)
		return fmt.Errorf("pgstore: check database existence: %w", err)
	}
	if !exists {
		ident := pgx.Identifier{dbName}.Sanitize()
		if _, err := maint.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
			maint.Close(ctx)
			return fmt.Errorf("pgstore: create database %s: %w", dbName, err)
		}
	}
	maint.Close(ctx)

	// 2. Connect to the new db and enable per-database extensions.
	dsn, err := DSNForProfile(baseDSN, profile)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("pgstore: connect to %s: %w", dbName, err)
	}
	for _, ext := range []string{"vector", "pg_trgm"} {
		if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+ext); err != nil {
			conn.Close(ctx)
			return fmt.Errorf("pgstore: enable extension %s in %s: %w", ext, dbName, err)
		}
	}
	conn.Close(ctx)

	// 3. Apply migrations against the new db.
	if err := Migrate(ctx, dsn); err != nil {
		return err
	}
	return nil
}

// Migrate applies all pending UP migrations to the database addressed by dsn
// using the embedded migration source. dsn may use a postgres://,
// postgresql://, or pgx5:// scheme — it is normalized to pgx5:// for the
// golang-migrate pgx5 driver. ErrNoChange (already up-to-date) is treated as
// success.
func Migrate(_ context.Context, dsn string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("pgstore: open migration source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, toPgxURL(dsn))
	if err != nil {
		return fmt.Errorf("pgstore: init migrator: %w", err)
	}
	defer m.Close() // closes both source and database instances.

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("pgstore: migrate up: %w", err)
	}
	return nil
}

// toPgxURL rewrites a Postgres DSN scheme to "pgx5://", which is what the
// golang-migrate pgx5 driver dispatches on. postgres:// and postgresql://
// are rewritten; an already-pgx5:// URL is returned unchanged. Anything else
// is returned as-is (NewWithSourceInstance will surface the error).
func toPgxURL(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "pgx5://"):
		return dsn
	case strings.HasPrefix(dsn, "postgresql://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgresql://")
	case strings.HasPrefix(dsn, "postgres://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgres://")
	default:
		return dsn
	}
}
