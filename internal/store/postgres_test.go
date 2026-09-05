package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/store"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Several replicas starting at once against a cold database must all come up.
//
// `CREATE EXTENSION IF NOT EXISTS` is not concurrency-safe -- it checks the catalogue
// and then inserts, with nothing holding the gap -- so without a lock one starter dies
// with `duplicate key value violates unique constraint "pg_extension_name_index"` and
// the pod restart-loops until another replica has finished the DDL.
//
// This needs its own container, deliberately: the shared test fixture applies the schema
// while starting, so by the time any other test runs the extension already exists and
// the race cannot happen. That is exactly why the kind harness reported this as passing
// for two days -- the condition never arose, and an untested condition reported as a PASS
// is worse than no check.
func TestConcurrentStartersAgainstAColdDatabaseAllSucceed(t *testing.T) {
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "pgvector/pgvector:pg17",
		tcpostgres.WithDatabase("csagent"),
		tcpostgres.WithUsername("csagent"),
		tcpostgres.WithPassword("csagent"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		t.Fatalf("start pgvector: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	// The extension does not exist yet: this is the cold-database case.
	const starters = 6
	errs := make([]error, starters)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range starters {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together, to make the gap as narrow as possible
			pool, err := store.Open(ctx, url, 2, 384)
			if err != nil {
				errs[i] = err
				return
			}
			pool.Close()
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("starter %d failed: %v", i, err)
		}
	}
}
