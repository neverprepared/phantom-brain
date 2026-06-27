package projection

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// defaultMaxWorkers caps concurrent projection jobs on the default queue.
// Modest by design — projection is I/O to the search store, not CPU-bound, and
// the SoR is the bottleneck of record.
const defaultMaxWorkers = 5

// MigrateRiver runs River's own schema migration (the river_job table and
// friends) Up against pool. River's tables live alongside the SoR tables in
// each per-profile database (pb_<profile>), so this is called once per profile
// DB. It is idempotent: a no-op when already at the latest version.
func MigrateRiver(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("projection: init river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("projection: river migrate up: %w", err)
	}
	return nil
}

// NewWorkers builds the River worker registry for this package, registering
// ProjectRecordWorker against q (SoR access) and proj (projection target).
func NewWorkers(q *pgdb.Queries, proj Projector) *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, NewProjectRecordWorker(q, proj))
	return workers
}

// NewClient constructs a River client over pool with the default queue running
// at a modest worker count. The caller owns the lifecycle: client.Start(ctx)
// to begin draining, client.Stop(ctx) to shut down gracefully.
//
// Pass workers from NewWorkers. The same pool backs both job storage and the
// driver, so InsertTx can enqueue inside an SoR transaction (the outbox).
func NewClient(pool *pgxpool.Pool, workers *river.Workers) (*river.Client[pgx.Tx], error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: defaultMaxWorkers},
		},
		Workers: workers,
	})
	if err != nil {
		return nil, fmt.Errorf("projection: new river client: %w", err)
	}
	return client, nil
}

// EnqueueProjectTx is THE transactional-outbox primitive. It inserts a
// project_record (upsert) job on the SAME pgx.Tx the caller used to write the
// record. Because River's job insert participates in that transaction:
//
//   - the tx COMMITS  ⇒ the job is durably enqueued and will be worked;
//   - the tx ROLLS BACK ⇒ no job exists, nothing is projected.
//
// This is what lets a caller atomically write a record AND schedule its
// projection without dual-write divergence: there is no window where the
// record exists but the projection job does not (or vice-versa). River will
// not start the job until the transaction commits (snapshot visibility).
func EnqueueProjectTx(ctx context.Context, client *river.Client[pgx.Tx], tx pgx.Tx, recordID int64) error {
	_, err := client.InsertTx(ctx, tx, ProjectRecordArgs{
		RecordID: recordID,
		Op:       OpUpsert,
	}, nil)
	if err != nil {
		return fmt.Errorf("projection: enqueue project job for record %d: %w", recordID, err)
	}
	return nil
}

// EnqueueDeleteTx is the delete sibling of EnqueueProjectTx: it schedules
// removal of the projection for (profile, vault, sha), carrying the identity
// inline because the SoR record may already be gone by the time the job runs.
// Same transactional guarantee — commit enqueues, rollback does not.
func EnqueueDeleteTx(ctx context.Context, client *river.Client[pgx.Tx], tx pgx.Tx, profile, vault, sha string) error {
	_, err := client.InsertTx(ctx, tx, ProjectRecordArgs{
		Op:      OpDelete,
		Profile: profile,
		Vault:   vault,
		Sha:     sha,
	}, nil)
	if err != nil {
		return fmt.Errorf("projection: enqueue delete job for %s/%s/%s: %w", profile, vault, sha, err)
	}
	return nil
}

// WriteRecordAndEnqueue is the canonical "write + outbox" path the synth/ingest
// layer calls. In a single transaction it upserts the record, enqueues its
// projection job, then commits. Any error rolls the whole thing back (so
// neither the record nor the job lands).
//
// Enqueue semantics: UpsertRecord uses ON CONFLICT (profile, vault, sha) DO
// UPDATE (it backfills a missing embedding without clobbering existing fields),
// so it RETURNS the row on BOTH a fresh insert AND a re-ingest. We enqueue a
// projection whenever UpsertRecord returns a row. This is intentional: a
// re-ingest may have backfilled the embedding, so re-projecting keeps the
// pb_records projection consistent with the SoR. Re-projection is idempotent —
// the worker upserts keyed on (profile, vault, sha) / _id, so an extra job
// converges to the same state. The pgx.ErrNoRows branch is now effectively
// unreachable (DO UPDATE always returns a row); it is retained as a defensive
// fallback in case the query ever reverts to DO NOTHING.
func WriteRecordAndEnqueue(
	ctx context.Context,
	pool *pgxpool.Pool,
	client *river.Client[pgx.Tx],
	params pgdb.UpsertRecordParams,
) (rec pgdb.Record, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return pgdb.Record{}, fmt.Errorf("projection: begin tx: %w", err)
	}
	// Rollback is a no-op after a successful Commit; this guarantees rollback
	// on every early return / panic path.
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	q := pgdb.New(tx)

	rec, upErr := q.UpsertRecord(ctx, params)
	if upErr != nil {
		if !errors.Is(upErr, pgx.ErrNoRows) {
			err = fmt.Errorf("projection: upsert record: %w", upErr)
			return pgdb.Record{}, err
		}
		// Defensive fallback (unreachable with ON CONFLICT DO UPDATE, which
		// returns the row on conflict): fetch the existing record so the
		// caller still gets it back, then enqueue its projection like any
		// other write (idempotent).
		existing, getErr := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{
			Profile: params.Profile,
			Vault:   params.Vault,
			Sha:     params.Sha,
		})
		if getErr != nil {
			err = fmt.Errorf("projection: fetch existing record after dedup: %w", getErr)
			return pgdb.Record{}, err
		}
		rec = existing
	}

	// Enqueue the projection in the same tx (the outbox). With ON CONFLICT DO
	// UPDATE, UpsertRecord returns the row on a fresh insert AND a re-ingest,
	// so this fires on both. Re-projection is idempotent (the worker upserts
	// the projection keyed on (profile, vault, sha) / _id), and re-ingest may
	// have backfilled the embedding, so re-projecting keeps pb_records in sync.
	if err = EnqueueProjectTx(ctx, client, tx, rec.ID); err != nil {
		return pgdb.Record{}, err
	}

	if err = tx.Commit(ctx); err != nil {
		err = fmt.Errorf("projection: commit: %w", err)
		return pgdb.Record{}, err
	}
	return rec, nil
}
