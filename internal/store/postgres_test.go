package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
// The race is not a race: it is transaction visibility, so it can be made deterministic.
//
// An uncommitted CREATE EXTENSION is invisible to another session, so that session's
// IF NOT EXISTS finds nothing, proceeds, and blocks on the catalogue's unique index until
// the first commits -- and then fails. No goroutines, no timing, no flake. Construction
// from the Java implementation's session, which could not force its own concurrency
// reproduction to run and proved the mechanism instead. It is better than the six-starter
// test below: that one shows the failure *happens*, this one shows it *must*.
func TestTheExtensionRaceIsTransactionVisibilityNotTiming(t *testing.T) {
	ctx := context.Background()
	url := coldDatabase(t)

	a, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	b, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)

	tx, err := a.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `CREATE EXTENSION vector`); err != nil {
		t.Fatalf("session A could not create the extension: %v", err)
	}
	// A has not committed, so B cannot see the extension and will not skip.

	failed := make(chan error, 1)
	go func() {
		_, err := b.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
		failed <- err
	}()

	select {
	case err := <-failed:
		t.Fatalf("session B returned before A committed (%v); the premise of this test "+
			"is that it blocks on the catalogue index", err)
	case <-time.After(300 * time.Millisecond):
		// B is blocked, as expected.
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	err = <-failed
	if err == nil {
		t.Fatal("session B succeeded; the unique index on pg_extension did not reject it, " +
			"so IF NOT EXISTS is safe on this Postgres and the lock in store.Open may be " +
			"unnecessary -- re-measure before removing it")
	}
	if !strings.Contains(err.Error(), "pg_extension_name_index") {
		t.Errorf("session B failed with %v, want the pg_extension_name_index violation", err)
	}
	t.Logf("deterministic: %v", err)
}

// coldDatabase starts a pgvector container with no extension created yet.
func coldDatabase(t *testing.T) string {
	t.Helper()
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
	return url
}

// The advisory lock must not outlive Open.
//
// Taking it on a *pooled* connection would be the trap: returning a connection to a pool
// ends no session, so the lock survives until the pool retires the connection. The Java
// implementation's session lost 1,479 seconds to exactly that, with a test named
// "a dropped connection releases the lock" passing while demonstrating the opposite --
// advisory locks are re-entrant, and it got the same connection back. store.Open uses a
// dedicated connection and closes it, so this asserts the property rather than the code.
func TestOpenLeavesNoAdvisoryLockHeld(t *testing.T) {
	ctx := context.Background()
	url := coldDatabase(t)

	pool, err := store.Open(ctx, url, 2, 384)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var held int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 0 {
		t.Errorf("%d advisory lock(s) still held after Open returned", held)
	}
}

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

	// And each migration ran once, not six times. Six starters that all succeed prove
	// the lock serialises them; only the ledger proves they did not each apply the same
	// migration in turn. The insert has a primary key on version, so a second attempt
	// would have failed rather than duplicated -- which means a green suite here is the
	// difference between "the lock works" and "the primary key caught it".
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var versions, rows int
	if err := conn.QueryRow(ctx, `SELECT count(DISTINCT version), count(*) FROM schema_migration`).
		Scan(&versions, &rows); err != nil {
		t.Fatal(err)
	}
	if versions == 0 {
		t.Fatal("no migrations recorded after six starters came up")
	}
	if rows != versions {
		t.Errorf("%d ledger rows for %d versions", rows, versions)
	}
}
