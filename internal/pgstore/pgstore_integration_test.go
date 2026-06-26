//go:build integration

// Integration coverage for Provision against a real Postgres (pgvector
// image). Build-tagged OFF by default so `make test` neither compiles this
// file nor needs a Docker daemon. Run with:
//
//	GOFLAGS=-tags='sqlite_fts5 integration' go test ./internal/pgstore/ -run Integration -count=1
package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestProvisionIntegration(t *testing.T) {
	ctx := context.Background()

	const (
		dbUser = "pbrain"
		dbPass = "pbrain"
		dbName = "phantom_brain" // maintenance db created by the image
	)

	req := testcontainers.ContainerRequest{
		Image:        "pgvector/pgvector:pg17",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     dbUser,
			"POSTGRES_PASSWORD": dbPass,
			"POSTGRES_DB":       dbName,
		},
		// The image logs the readiness line twice: once for the bootstrap
		// (init scripts) start, then again for the real listener. Wait for
		// the second occurrence so we don't connect during the restart.
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

	baseDSN := "postgres://" + dbUser + ":" + dbPass + "@" + host + ":" + port.Port() + "/" + dbName + "?sslmode=disable"

	// Provision a fresh profile.
	if err := Provision(ctx, baseDSN, "tctest"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Idempotency: a second Provision must be a clean no-op.
	if err := Provision(ctx, baseDSN, "tctest"); err != nil {
		t.Fatalf("Provision (second call, idempotency): %v", err)
	}

	// The pb_tctest database must now exist.
	maint, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatalf("connect maintenance db: %v", err)
	}
	var exists bool
	if err := maint.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname='pb_tctest')").Scan(&exists); err != nil {
		t.Fatalf("query pg_database: %v", err)
	}
	maint.Close(ctx)
	if !exists {
		t.Fatal("expected database pb_tctest to exist after Provision")
	}

	// Connect to the provisioned db and verify the schema.
	profileDSN, err := DSNForProfile(baseDSN, "tctest")
	if err != nil {
		t.Fatalf("DSNForProfile: %v", err)
	}
	conn, err := pgx.Connect(ctx, profileDSN)
	if err != nil {
		t.Fatalf("connect pb_tctest: %v", err)
	}
	defer conn.Close(ctx)

	// All six tables from migrations 0001-0003 must exist.
	wantTables := []string{
		"records", "entities", "entity_aliases",
		"record_entities", "facts", "fact_history",
	}
	for _, tbl := range wantTables {
		var present bool
		if err := conn.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name=$1)",
			tbl).Scan(&present); err != nil {
			t.Fatalf("query for table %s: %v", tbl, err)
		}
		if !present {
			t.Errorf("expected table %q to exist", tbl)
		}
	}

	// records.embedding_model proves migration 0004 ran.
	var colPresent bool
	if err := conn.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='records' AND column_name='embedding_model')").
		Scan(&colPresent); err != nil {
		t.Fatalf("query records.embedding_model column: %v", err)
	}
	if !colPresent {
		t.Error("expected records.embedding_model column (migration 0004) to exist")
	}
}
