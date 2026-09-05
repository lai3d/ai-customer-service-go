// Package store owns the Postgres connection pool and the schema.
//
// Conversation memory and the FAQ vectors live in the same database on purpose: one
// database to run, back up and reason about, and a ticket and the conversation that
// produced it can be written in one transaction if they ever stop being mock data.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// Schema is applied at startup. It is idempotent, and small enough that a migration
// tool would be more machinery than the problem needs; swap one in when it isn't.
//
// conversation_id is varchar(64) rather than unbounded: an id arrives from a client,
// and in the Java implementation an over-long one surfaced as a 500 from a database
// constraint. It is validated at the edge as well, so the column is a backstop.
const Schema = `
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS chat_memory (
    id              BIGSERIAL PRIMARY KEY,
    conversation_id VARCHAR(64) NOT NULL,
    role            TEXT        NOT NULL CHECK (role IN ('user', 'assistant')),
    content         TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS chat_memory_conversation_idx
    ON chat_memory (conversation_id, id);

-- Readable primary keys: "faq:returns-window:en" rather than an opaque UUID, so a row
-- can be traced back to its corpus entry by eye.
CREATE TABLE IF NOT EXISTS faq_document (
    id         TEXT NOT NULL PRIMARY KEY,
    entry_id   TEXT NOT NULL,
    language   TEXT NOT NULL,
    category   TEXT NOT NULL,
    question   TEXT NOT NULL,
    answer     TEXT NOT NULL,
    content    TEXT NOT NULL,
    embedding  vector(%d) NOT NULL
);

CREATE INDEX IF NOT EXISTS faq_document_embedding_idx
    ON faq_document USING hnsw (embedding vector_cosine_ops);
`

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
		return pgxvec.RegisterTypes(ctx, conn)
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

	if _, err := conn.Exec(ctx, fmt.Sprintf(Schema, dimensions)); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}
