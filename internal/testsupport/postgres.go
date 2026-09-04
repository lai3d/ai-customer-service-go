// Package testsupport starts the real dependencies tests run against.
//
// Retrieval is measured against a real pgvector and the real embedding model, never a
// stub. A stubbed embedding model would make these tests fast and meaningless: the
// thing being asserted is that this corpus, embedded by this model, ranks the right
// passage first. None of it needs an API key.
package testsupport

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/store"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// StartPostgres runs a pgvector container and applies the schema. The returned stop
// function terminates it. Call this from TestMain so one container serves a whole
// package: starting one per test is about two seconds each, paid many times over.
func StartPostgres(ctx context.Context, dimensions int) (*pgxpool.Pool, func(), error) {
	container, err := tcpostgres.Run(ctx, "pgvector/pgvector:pg17",
		tcpostgres.WithDatabase("csagent"),
		tcpostgres.WithUsername("csagent"),
		tcpostgres.WithPassword("csagent"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("start pgvector container: %w", err)
	}
	stop := func() { _ = container.Terminate(context.Background()) }

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		stop()
		return nil, nil, err
	}
	pool, err := store.Open(ctx, url, 10, dimensions)
	if err != nil {
		stop()
		return nil, nil, err
	}
	return pool, func() { pool.Close(); stop() }, nil
}

// Postgres is StartPostgres bound to one test's lifetime.
func Postgres(t *testing.T, dimensions int) *pgxpool.Pool {
	t.Helper()
	pool, stop, err := StartPostgres(context.Background(), dimensions)
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(stop)
	return pool
}
