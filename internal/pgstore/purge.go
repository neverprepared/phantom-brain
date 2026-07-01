package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PurgeCounts reports how many rows each scoped delete removed. Used by
// the operator CLI (`binding delete --purge-data`) so the summary can be
// honest about what left the System of Record.
type PurgeCounts struct {
	Records     int64
	Entities    int64
	FactHistory int64
}

// PurgeBinding deletes one binding's rows (profile + vault) from the
// per-profile System of Record. baseDSN is the base/maintenance DSN; the
// per-profile database (pb_<profile>) is derived from it.
//
// This is a SCOPED ROW DELETE, deliberately NOT a DROP DATABASE:
// pb_<profile> is shared across every vault of the profile, so dropping
// it would nuke sibling vaults. We delete only rows matching
// (profile, vault) and let foreign keys cascade the rest:
//
//   - records      → cascades record_entities (record side); NULLs
//     facts.source_record_id (ON DELETE SET NULL).
//   - entities     → cascades facts, entity_aliases, record_entities
//     (entity side).
//   - fact_history → no FK (it is an immutable belief log), so it is
//     deleted explicitly.
//
// All three run in one transaction: a binding purge is all-or-nothing.
// The delete order is chosen so cascades don't fight the explicit
// deletes. Returns per-table counts for the operator summary.
func PurgeBinding(ctx context.Context, baseDSN, profile, vault string) (PurgeCounts, error) {
	var counts PurgeCounts

	dsn, err := DSNForProfile(baseDSN, profile)
	if err != nil {
		return counts, err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return counts, fmt.Errorf("pgstore: connect for purge: %w", err)
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return counts, fmt.Errorf("pgstore: begin purge tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit

	// Order matters only for clarity — the FKs make any order correct, but
	// deleting records first drops the record-side join rows, then entities
	// drops facts + entity-side joins, then the FK-less history is cleaned.
	for _, step := range []struct {
		table string
		sql   string
		out   *int64
	}{
		{"records", `DELETE FROM records WHERE profile = $1 AND vault = $2`, &counts.Records},
		{"entities", `DELETE FROM entities WHERE profile = $1 AND vault = $2`, &counts.Entities},
		{"fact_history", `DELETE FROM fact_history WHERE profile = $1 AND vault = $2`, &counts.FactHistory},
	} {
		tag, err := tx.Exec(ctx, step.sql, profile, vault)
		if err != nil {
			return counts, fmt.Errorf("pgstore: purge %s for %s/%s: %w", step.table, profile, vault, err)
		}
		*step.out = tag.RowsAffected()
	}

	if err := tx.Commit(ctx); err != nil {
		return counts, fmt.Errorf("pgstore: commit purge: %w", err)
	}
	return counts, nil
}
