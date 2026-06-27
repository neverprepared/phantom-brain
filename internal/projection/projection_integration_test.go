//go:build integration

// Integration coverage for the transactional outbox + River projection worker
// against a real pgvector Postgres. Build-tagged OFF by default so `make test`
// neither compiles this file nor needs a Docker daemon. Run with:
//
//	GOFLAGS="-tags=sqlite_fts5,integration" go test ./internal/projection/ -run Integration -count=1 -v
package projection

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// fakeProjector records every Project / DeleteProjection call. It can be told
// to fail the first failUntil Project attempts to exercise River retries.
type fakeProjector struct {
	mu        sync.Mutex
	projected []pgdb.Record
	deleted   [][3]string // {profile, vault, sha}
	failUntil int         // fail the first N Project calls, then succeed
	attempts  int         // total Project calls (incl. failures)
}

func (f *fakeProjector) Project(_ context.Context, rec pgdb.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.attempts <= f.failUntil {
		return errTransient
	}
	f.projected = append(f.projected, rec)
	return nil
}

func (f *fakeProjector) DeleteProjection(_ context.Context, profile, vault, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, [3]string{profile, vault, sha})
	return nil
}

func (f *fakeProjector) projectedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.projected)
}

func (f *fakeProjector) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *fakeProjector) snapshotProjected() []pgdb.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]pgdb.Record, len(f.projected))
	copy(out, f.projected)
	return out
}

var errTransient = &transientErr{}

type transientErr struct{}

func (*transientErr) Error() string { return "transient projector failure (test)" }

// fastRetryPolicy retries almost immediately so the retry sub-test doesn't sit
// through River's default ~attempt^4-second backoff.
type fastRetryPolicy struct{}

func (fastRetryPolicy) NextRetry(job *rivertype.JobRow) time.Time {
	return time.Now().Add(100 * time.Millisecond)
}

// --- container harness -----------------------------------------------------

func startPG(t *testing.T) (baseDSN string) {
	t.Helper()
	ctx := context.Background()

	const (
		dbUser = "pbrain"
		dbPass = "pbrain"
		dbName = "phantom_brain"
	)

	req := testcontainers.ContainerRequest{
		Image:        "pgvector/pgvector:pg17",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     dbUser,
			"POSTGRES_PASSWORD": dbPass,
			"POSTGRES_DB":       dbName,
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(2 * time.Minute),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start pgvector container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}
	return "postgres://" + dbUser + ":" + dbPass + "@" + host + ":" + port.Port() + "/" + dbName + "?sslmode=disable"
}

// provisionAndOpen provisions a profile DB, migrates River into it, and returns
// an open pool. The profile name is unique per sub-test to isolate state.
func provisionAndOpen(t *testing.T, baseDSN, profile string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	if err := pgstore.Provision(ctx, baseDSN, profile); err != nil {
		t.Fatalf("Provision(%s): %v", profile, err)
	}
	dsn, err := pgstore.DSNForProfile(baseDSN, profile)
	if err != nil {
		t.Fatalf("DSNForProfile: %v", err)
	}
	pool, err := pgstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := MigrateRiver(ctx, pool); err != nil {
		t.Fatalf("MigrateRiver: %v", err)
	}
	return pool
}

// newEmbed768 builds a deterministic non-zero 768-dim vector (pgvector
// rejects all-zero vectors under cosine; non-zero also proves real values
// round-trip through the binary codec).
func newEmbed768(seed float32) pgvector.Vector {
	v := make([]float32, 768)
	for i := range v {
		v[i] = seed + float32(i%7)*0.001
	}
	return pgvector.NewVector(v)
}

func sampleParams(sha string) pgdb.UpsertRecordParams {
	return pgdb.UpsertRecordParams{
		Profile: "tctest",
		Vault:   "main",
		Sha:     sha,
		Kind:    "note",
		Title:   "outbox test note",
		RawBody: pgtype.Text{String: "the body", Valid: true},
		// source/tags are NOT NULL in the schema; the column DEFAULT '{}'
		// only applies when omitted from the INSERT, but UpsertRecord passes
		// them explicitly — so a nil slice would send SQL NULL and violate
		// the constraint. Send empty (non-nil) slices.
		Source: []string{},
		Tags:   []string{},
	}
}

// --- the test --------------------------------------------------------------

func TestProjectionIntegration(t *testing.T) {
	baseDSN := startPG(t)
	ctx := context.Background()

	t.Run("MigrateIdempotent", func(t *testing.T) {
		// provisionAndOpen already migrated once; migrate again -> no-op.
		pool := provisionAndOpen(t, baseDSN, "mig")
		if err := MigrateRiver(ctx, pool); err != nil {
			t.Fatalf("MigrateRiver (second call, idempotency): %v", err)
		}
		// river_job table must exist.
		var present bool
		if err := pool.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='river_job')").
			Scan(&present); err != nil {
			t.Fatalf("query river_job existence: %v", err)
		}
		if !present {
			t.Fatal("expected river_job table after MigrateRiver")
		}
	})

	t.Run("OutboxHappyPath", func(t *testing.T) {
		pool := provisionAndOpen(t, baseDSN, "happy")
		fake := &fakeProjector{}
		q := pgstore.New(pool)

		client, err := NewClient(pool, NewWorkers(q, fake))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if err := client.Start(ctx); err != nil {
			t.Fatalf("client.Start: %v", err)
		}
		defer stopClient(t, client)

		rec, err := WriteRecordAndEnqueue(ctx, pool, client, sampleParams("sha-happy-1"))
		if err != nil {
			t.Fatalf("WriteRecordAndEnqueue: %v", err)
		}
		if rec.ID == 0 {
			t.Fatal("expected a non-zero record ID from fresh insert")
		}

		waitFor(t, 10*time.Second, func() bool { return fake.projectedCount() == 1 })

		got := fake.snapshotProjected()
		if len(got) != 1 {
			t.Fatalf("expected exactly 1 projection, got %d", len(got))
		}
		if got[0].ID != rec.ID || got[0].Sha != "sha-happy-1" {
			t.Fatalf("projected wrong record: got id=%d sha=%q want id=%d sha=%q",
				got[0].ID, got[0].Sha, rec.ID, "sha-happy-1")
		}
	})

	t.Run("RollbackNoJob", func(t *testing.T) {
		pool := provisionAndOpen(t, baseDSN, "rollback")
		fake := &fakeProjector{}
		q := pgstore.New(pool)

		client, err := NewClient(pool, NewWorkers(q, fake))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}

		// Begin a tx, write + enqueue, then ROLL BACK.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		txq := pgdb.New(tx)
		rec, err := txq.UpsertRecord(ctx, sampleParams("sha-rollback-1"))
		if err != nil {
			t.Fatalf("UpsertRecord in tx: %v", err)
		}
		if err := EnqueueProjectTx(ctx, client, tx, rec.ID); err != nil {
			t.Fatalf("EnqueueProjectTx: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("Rollback: %v", err)
		}

		// No job should have been durably enqueued.
		var jobCount int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job").Scan(&jobCount); err != nil {
			t.Fatalf("count river_job: %v", err)
		}
		if jobCount != 0 {
			t.Fatalf("expected 0 jobs after rollback, got %d", jobCount)
		}
		// And the record itself must not exist.
		var recCount int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM records WHERE sha='sha-rollback-1'").Scan(&recCount); err != nil {
			t.Fatalf("count records: %v", err)
		}
		if recCount != 0 {
			t.Fatalf("expected 0 records after rollback, got %d", recCount)
		}

		// Start the worker and confirm the fake is never called.
		if err := client.Start(ctx); err != nil {
			t.Fatalf("client.Start: %v", err)
		}
		defer stopClient(t, client)
		time.Sleep(1500 * time.Millisecond)
		if c := fake.attemptCount(); c != 0 {
			t.Fatalf("expected 0 projector calls after rollback, got %d", c)
		}
	})

	t.Run("RetryOnProjectorFailure", func(t *testing.T) {
		pool := provisionAndOpen(t, baseDSN, "retry")
		fake := &fakeProjector{failUntil: 1} // fail attempt 1, succeed attempt 2
		q := pgstore.New(pool)

		// Custom client with a fast retry policy so attempt 2 lands quickly.
		client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
			Queues:      map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: defaultMaxWorkers}},
			Workers:     NewWorkers(q, fake),
			RetryPolicy: fastRetryPolicy{},
		})
		if err != nil {
			t.Fatalf("NewClient (retry): %v", err)
		}
		if err := client.Start(ctx); err != nil {
			t.Fatalf("client.Start: %v", err)
		}
		defer stopClient(t, client)

		if _, err := WriteRecordAndEnqueue(ctx, pool, client, sampleParams("sha-retry-1")); err != nil {
			t.Fatalf("WriteRecordAndEnqueue: %v", err)
		}

		// Eventually projects despite the first-attempt failure.
		waitFor(t, 15*time.Second, func() bool { return fake.projectedCount() == 1 })
		if a := fake.attemptCount(); a < 2 {
			t.Fatalf("expected at least 2 projector attempts (1 fail + 1 success), got %d", a)
		}
	})

	t.Run("IdempotentReProject", func(t *testing.T) {
		pool := provisionAndOpen(t, baseDSN, "idem")
		fake := &fakeProjector{}
		q := pgstore.New(pool)

		// Seed a record directly (no enqueue) so we can drive the worker path
		// twice for the same record and prove double-delivery safety.
		rec, err := q.UpsertRecord(ctx, sampleParams("sha-idem-1"))
		if err != nil {
			t.Fatalf("seed UpsertRecord: %v", err)
		}

		worker := NewProjectRecordWorker(q, fake)
		job := &river.Job[ProjectRecordArgs]{
			Args: ProjectRecordArgs{RecordID: rec.ID, Op: OpUpsert},
		}
		if err := worker.Work(ctx, job); err != nil {
			t.Fatalf("Work (1st): %v", err)
		}
		if err := worker.Work(ctx, job); err != nil {
			t.Fatalf("Work (2nd): %v", err)
		}
		if c := fake.projectedCount(); c != 2 {
			t.Fatalf("expected fake to handle both projections, got %d", c)
		}
	})

	t.Run("EmbeddingPersistAndBackfill", func(t *testing.T) {
		pool := provisionAndOpen(t, baseDSN, "embed")
		fake := &fakeProjector{}
		q := pgstore.New(pool)

		client, err := NewClient(pool, NewWorkers(q, fake))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if err := client.Start(ctx); err != nil {
			t.Fatalf("client.Start: %v", err)
		}
		defer stopClient(t, client)

		emb := newEmbed768(0.25)

		// (1) A fresh write carrying an embedding persists it on the raw
		// record (no synth needed) — this is what restores kNN recall.
		withEmb := sampleParams("sha-embed-1")
		withEmb.Embedding = &emb
		if _, err := WriteRecordAndEnqueue(ctx, pool, client, withEmb); err != nil {
			t.Fatalf("WriteRecordAndEnqueue (with embedding): %v", err)
		}
		got, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{
			Profile: withEmb.Profile, Vault: withEmb.Vault, Sha: withEmb.Sha,
		})
		if err != nil {
			t.Fatalf("GetRecordBySHA: %v", err)
		}
		if got.Embedding == nil {
			t.Fatal("raw write with an embedding must persist records.embedding non-null")
		}
		if len(got.Embedding.Slice()) != 768 {
			t.Errorf("embedding dim = %d, want 768", len(got.Embedding.Slice()))
		}

		// (2) A fresh write with NO embedding leaves it NULL; a later
		// re-ingest carrying an embedding BACKFILLS it (DO UPDATE +
		// COALESCE), and the re-write returns the existing row.
		noEmb := sampleParams("sha-embed-2")
		first, err := WriteRecordAndEnqueue(ctx, pool, client, noEmb)
		if err != nil {
			t.Fatalf("WriteRecordAndEnqueue (no embedding): %v", err)
		}
		nullRec, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{
			Profile: noEmb.Profile, Vault: noEmb.Vault, Sha: noEmb.Sha,
		})
		if err != nil {
			t.Fatalf("GetRecordBySHA (null): %v", err)
		}
		if nullRec.Embedding != nil {
			t.Fatal("write without an embedding should leave records.embedding NULL")
		}

		reEmb := sampleParams("sha-embed-2")
		reEmb.Embedding = &emb
		reWritten, err := WriteRecordAndEnqueue(ctx, pool, client, reEmb)
		if err != nil {
			t.Fatalf("WriteRecordAndEnqueue (re-ingest backfill): %v", err)
		}
		if reWritten.ID != first.ID {
			t.Errorf("re-ingest returned id %d, want existing %d", reWritten.ID, first.ID)
		}
		backfilled, err := q.GetRecordBySHA(ctx, pgdb.GetRecordBySHAParams{
			Profile: reEmb.Profile, Vault: reEmb.Vault, Sha: reEmb.Sha,
		})
		if err != nil {
			t.Fatalf("GetRecordBySHA (backfilled): %v", err)
		}
		if backfilled.Embedding == nil {
			t.Fatal("re-ingest with an embedding must backfill the previously-NULL embedding")
		}
	})

	t.Run("UpsertRecordGoneIsNoop", func(t *testing.T) {
		pool := provisionAndOpen(t, baseDSN, "gone")
		fake := &fakeProjector{}
		q := pgstore.New(pool)

		// A record ID that does not exist -> pgx.ErrNoRows path -> nil, no
		// projection.
		worker := NewProjectRecordWorker(q, fake)
		job := &river.Job[ProjectRecordArgs]{
			Args: ProjectRecordArgs{RecordID: 999999, Op: OpUpsert},
		}
		if err := worker.Work(ctx, job); err != nil {
			t.Fatalf("Work on missing record should be nil, got: %v", err)
		}
		if c := fake.projectedCount(); c != 0 {
			t.Fatalf("expected 0 projections for missing record, got %d", c)
		}
	})
}

// --- helpers ---------------------------------------------------------------

func stopClient(t *testing.T, client *river.Client[pgx.Tx]) {
	t.Helper()
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Stop(stopCtx); err != nil {
		t.Logf("client.Stop: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
