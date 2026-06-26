package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"

	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// Open returns a pgxpool connected to dsn with the pgvector type codecs
// registered on every pooled connection. Without the AfterConnect hook the
// vector(768) columns fall back to text encode/decode; registering the
// binary codec lets pgvector.Vector params and result scans use the native
// protocol. dsn typically comes from DSNForProfile (the per-profile db).
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pgstore: parse pool DSN: %w", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if err := pgxvec.RegisterTypes(ctx, conn); err != nil {
			return fmt.Errorf("pgstore: register pgvector types: %w", err)
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgstore: open pool: %w", err)
	}
	return pool, nil
}

// New wraps a pgx connection-or-pool in the generated sqlc query set. The
// pool satisfies pgdb.DBTX directly, so this is a thin constructor — callers
// hold the *pgdb.Queries and don't touch the pool except to Close it.
func New(db pgdb.DBTX) *pgdb.Queries {
	return pgdb.New(db)
}
