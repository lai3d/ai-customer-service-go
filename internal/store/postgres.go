// Package store owns the Postgres connection pool and the schema.
//
// Conversation memory and the FAQ vectors live in the same database on purpose: one
// database to run, back up and reason about, and a ticket and the conversation that
// produced it can be written in one transaction if they ever stop being mock data.
package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// Open applies the schema on a single connection, then opens a pool whose connections
// know about the vector type.
//
// The two steps are in that order because they depend on each other in opposite
// directions: registering the type looks up the OID `CREATE EXTENSION vector` creates,
// so the extension has to exist first, and the pool has to register on every connection
// because a pool hands out more than one.
//
// Registering is not optional, and skipping it fails in a way that does not point at
// the cause. Query parameters would still work -- pgx falls back to the text format --
// but CopyFrom always uses the binary protocol, so an unregistered vector is encoded as
// something else and Postgres rejects the bytes with "vector cannot have more than
// 16000 dimensions" for a 384-dimension vector.
func Open(ctx context.Context, url string, maxConns int32, dimensions int) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse postgres url: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if err := pgxvec.RegisterTypes(ctx, conn); err != nil {
			return err
		}
		// A guard against the version filter starving the HNSW scan, and it is **argued
		// rather than evidenced here** -- which is the honest label, and the same one the
		// stream-close fix in internal/llm carries.
		//
		// The shape: retrieval filters on the active corpus version, and that filter is
		// applied after the scan has chosen its candidates, so candidates spent on retired
		// documents are candidates not spent on live ones. The Java implementation of this
		// system measured exactly that -- 40 candidates, 26 dead, a top-8 of 1 -- and
		// closed it with this setting.
		//
		// It could not be reproduced in this stack. Measured here: 4,000 rows at 5%
		// selectivity walked 181 candidates to return a full 8 (EXPLAIN ANALYZE, `Rows
		// Removed by Filter: 173`), and twenty published versions with retention down to
		// two returned 8 with *no* rows removed by the filter at all. The churn test in
		// internal/rag passes with this line and without it, so it does not justify it and
		// does not pretend to.
		//
		// Kept anyway, because the measured cost is nothing -- the extra work happens only
		// when a scan has to resume, and a single-version search never exhausts its first
		// candidates, so the retrieval numbers are unchanged -- and the failure it guards
		// against is silent. strict_order rather than relaxed_order because this service
		// shows retrieval scores to operators and ranks by them; an order that is
		// approximately right is a number that is approximately meaningless.
		//
		// Set here rather than in postgresql.conf so it travels with the application: a
		// database somebody else administers is still correct. See docs/knowledge.md.
		if _, err := conn.Exec(ctx, "SET hnsw.iterative_scan = strict_order"); err != nil {
			return fmt.Errorf("set hnsw.iterative_scan: %w", err)
		}
		return nil
	}

	if err := applySchema(ctx, cfg.ConnConfig, dimensions); err != nil {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

// schemaLockKey is an arbitrary constant, shared by every replica of this service and
// by nothing else. Postgres advisory locks live in one 64-bit keyspace for the whole
// database, so the value matters only in that two different applications must not
// collide on it.
const schemaLockKey = 0x41_49_43_53_47_4F_01 // "AICSGO" + 1

func applySchema(ctx context.Context, cfg *pgx.ConnConfig, dimensions int) error {
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer conn.Close(ctx)

	// Serialise DDL across replicas.
	//
	// `CREATE EXTENSION IF NOT EXISTS` is not concurrency-safe: it checks the catalogue
	// and then inserts, with nothing holding the gap. Two replicas starting together
	// against a cold database and one of them dies with
	//
	//   duplicate key value violates unique constraint "pg_extension_name_index"
	//
	// which fails startup and puts the pod into a restart loop until the other replica
	// has finished. Reproduced on kind with two replicas and a freshly dropped
	// extension; both replicas restarted.
	//
	// The advisory lock is released when this connection closes, but it is released
	// explicitly so the window is the DDL rather than the rest of Open.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(schemaLockKey)); err != nil {
		return fmt.Errorf("take the schema lock: %w", err)
	}
	defer func() {
		// A failure to unlock is not worth failing startup for: closing the connection
		// releases it, and the deferred Close above always runs.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(schemaLockKey))
	}()

	ran, err := migrate(ctx, conn, Migrations, dimensions)
	if err != nil {
		return err
	}
	if ran > 0 {
		slog.Info("schema migrations applied", "count", ran)
	}
	return nil
}
