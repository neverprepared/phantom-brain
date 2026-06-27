package pgstore

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
)

// These are the SoR column mappers shared by the live write path
// (internal/server/dual_write.go) and the bulk loader
// (internal/backfill/backfill.go). They live here, next to
// SanitizeText/SanitizeTexts (which they depend on), so there is a single
// definition rather than two copies kept manually "in step".

// OptText returns a NULL pgtype.Text for an empty string, else a valid
// one. Input is sanitized first, so empty optional fields land in the SoR
// as SQL NULL.
func OptText(s string) pgtype.Text {
	s = SanitizeText(s)
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// OptTimestamptz returns a NULL pgtype.Timestamptz for a nil time, else a
// valid one.
func OptTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// OptInt8 returns a NULL pgtype.Int8 for a non-positive value, else a
// valid one. An absent/zero value lands as SQL NULL rather than 0.
func OptInt8(n int64) pgtype.Int8 {
	if n <= 0 {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: n, Valid: true}
}

// OptVector returns nil for an empty embedding (pgvector column stays
// NULL), else a *pgvector.Vector. An empty slice maps to NULL, never a
// zero vector.
func OptVector(emb []float32) *pgvector.Vector {
	if len(emb) == 0 {
		return nil
	}
	v := pgvector.NewVector(emb)
	return &v
}

// NonNilStrings guarantees a non-nil slice for NOT NULL DEFAULT '{}'
// columns (records.source / records.tags). A nil input becomes an empty
// (non-nil) slice so pgx sends '{}' rather than NULL. Each element is
// sanitized.
func NonNilStrings(in []string) []string {
	return SanitizeTexts(in)
}
