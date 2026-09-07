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

-- Which published version a document belongs to.
--
-- ADD COLUMN IF NOT EXISTS rather than a new table, and that is the whole migration: the
-- bundled corpus already in a running database keeps its rows and its embeddings, and gets
-- adopted as the first version by stamping this column. Re-embedding it would change the
-- vectors that every retrieval number in this pair of repositories was measured against.
ALTER TABLE faq_document ADD COLUMN IF NOT EXISTS corpus_version TEXT;
CREATE INDEX IF NOT EXISTS faq_document_version_idx ON faq_document (corpus_version);

-- The versions themselves, and which one live retrieval reads.
--
-- A version is immutable once built: its documents are written, then it is activated, and
-- it is never edited in place. Editing in place is what makes a half-published corpus
-- reachable by a customer mid-write.
CREATE TABLE IF NOT EXISTS corpus_version (
    version     TEXT        NOT NULL PRIMARY KEY,
    source      TEXT        NOT NULL,     -- 'bundled' or 'published'
    documents   INT         NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by  TEXT        NOT NULL DEFAULT 'system',
    note        TEXT
);

-- Exactly one row, ever. The primary key on a constant is the constraint: "the active
-- version" is a thing there is one of, and a table that can hold two of them is a table
-- that eventually does.
CREATE TABLE IF NOT EXISTS corpus_active (
    only_one    BOOLEAN     NOT NULL PRIMARY KEY DEFAULT true CHECK (only_one),
    version     TEXT        NOT NULL REFERENCES corpus_version (version),
    activated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    activated_by TEXT       NOT NULL DEFAULT 'system',
    -- Bumped on every switch. An operator publishing from a stale page loses the race
    -- instead of overwriting whoever won it.
    revision    INT         NOT NULL DEFAULT 1
);

-- What operators edit. Drafts, not the live corpus: nothing here is retrievable until a
-- publication turns it into a corpus_version.
CREATE TABLE IF NOT EXISTS knowledge_entry (
    entry_id    TEXT        NOT NULL,
    language    TEXT        NOT NULL,
    category    TEXT        NOT NULL,
    question    TEXT        NOT NULL,
    answer      TEXT        NOT NULL,
    -- Soft delete: a published version still references what it was built from, and an
    -- entry removed from the drafts must not disappear from the version that shipped it.
    deleted     BOOLEAN     NOT NULL DEFAULT false,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by  TEXT        NOT NULL,
    PRIMARY KEY (entry_id, language)
);

-- ---------------------------------------------------------------------------------
-- Operational records. Everything above serves a customer turn; everything below
-- serves the people who have to answer for it afterwards.
--
-- chat_memory is deliberately not that record. It is the model's context: windowed,
-- trimmed, and holding only what the model needs to see. An operational history that
-- disappears when the window slides is not a history.
-- ---------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS support_ticket (
    ticket_number   TEXT        NOT NULL PRIMARY KEY,
    conversation_id VARCHAR(64) NOT NULL,
    -- The normalised summary. A unique index on it is what makes deduplication a
    -- guarantee rather than a per-replica convention: two replicas racing on the same
    -- request now collide in the database instead of each creating a ticket.
    dedupe_key      TEXT        NOT NULL,
    category        TEXT        NOT NULL,
    summary         TEXT        NOT NULL,
    order_number    TEXT,
    state           TEXT        NOT NULL DEFAULT 'OPEN'
                    CHECK (state IN ('OPEN','IN_PROGRESS','RESOLVED','CLOSED')),
    assignee        TEXT,
    resolution      TEXT,
    -- Optimistic concurrency for the admin surface: two operators editing one ticket
    -- must not silently overwrite each other.
    version         INT         NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS support_ticket_dedupe_idx
    ON support_ticket (conversation_id, dedupe_key);
CREATE INDEX IF NOT EXISTS support_ticket_state_idx
    ON support_ticket (state, updated_at DESC);
CREATE SEQUENCE IF NOT EXISTS support_ticket_number_seq START 4700;

-- Every transition, attributed. A state machine with no history is a current value.
CREATE TABLE IF NOT EXISTS ticket_event (
    id            BIGSERIAL   PRIMARY KEY,
    ticket_number TEXT        NOT NULL REFERENCES support_ticket (ticket_number) ON DELETE CASCADE,
    at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor         TEXT        NOT NULL,
    action        TEXT        NOT NULL,
    detail        TEXT
);
CREATE INDEX IF NOT EXISTS ticket_event_ticket_idx ON ticket_event (ticket_number, id);

-- One row per customer turn, written at the service boundary rather than from the
-- event stream that feeds the browser -- so a turn whose client disconnected still has
-- a terminal outcome recorded.
-- A session is anonymous: it says two customers are different people, not who they are.
-- The token is stored as a hash, so a database dump does not hand its reader a working
-- session for every customer in it.
CREATE TABLE IF NOT EXISTS chat_session (
    token_hash   BYTEA       NOT NULL PRIMARY KEY,
    subject      TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS chat_session_expiry_idx ON chat_session (expires_at);

-- Which subject a conversation belongs to. Without this the conversation id was the whole
-- of the authorisation: anyone who knew one could append to that history and have the
-- model answer with its context.
--
-- The row outlives the session deliberately. A customer coming back tomorrow is a new
-- session and cannot resume the conversation, which is the correct trade for a service
-- that has no idea who they are -- but the operations surface must still be able to read
-- what was said, and a dangling owner row is how it stays attributable.
CREATE TABLE IF NOT EXISTS conversation_owner (
    conversation_id VARCHAR(64) NOT NULL PRIMARY KEY,
    subject         TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS conversation_owner_subject_idx ON conversation_owner (subject);

-- A fixed-window counter, shared because a counter in a process is replicas x limit.
-- The bucket column keeps the per-subject turn limit and the per-IP session limit in one
-- table rather than two that would need the same sweep.
CREATE TABLE IF NOT EXISTS rate_window (
    bucket       TEXT        NOT NULL,
    subject      TEXT        NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    count        INT         NOT NULL,
    PRIMARY KEY (bucket, subject, window_start)
);
CREATE INDEX IF NOT EXISTS rate_window_start_idx ON rate_window (window_start);

-- What the whole service has spent today, in tokens, so a ceiling can exist that a new
-- conversation id does not reset. The per-conversation budget cannot do this job:
-- conversation ids are free.
CREATE TABLE IF NOT EXISTS daily_spend (
    day    DATE   NOT NULL PRIMARY KEY,
    tokens BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS turn (
    id              TEXT        NOT NULL PRIMARY KEY,
    conversation_id VARCHAR(64) NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL,
    ended_at        TIMESTAMPTZ,
    -- completed | cancelled | failed | tool_limit | budget_exceeded | retrieval_failed
    -- | memory_failed | in_flight | unknown. Never a bare success/failure: a turn the customer
    -- abandoned and a turn the provider rejected are different events.
    outcome         TEXT        NOT NULL,
    question        TEXT        NOT NULL,
    reply           TEXT,
    model           TEXT,
    model_calls     INT         NOT NULL DEFAULT 0,
    input_tokens    BIGINT      NOT NULL DEFAULT 0,
    output_tokens   BIGINT      NOT NULL DEFAULT 0,
    cost_usd        NUMERIC(14,8),
    trace_id        TEXT,
    detail          TEXT
);
CREATE INDEX IF NOT EXISTS turn_conversation_idx ON turn (conversation_id, started_at);
CREATE INDEX IF NOT EXISTS turn_started_idx      ON turn (started_at DESC);
CREATE INDEX IF NOT EXISTS turn_outcome_idx      ON turn (outcome, started_at DESC);

-- What retrieval actually returned for that turn, kept because the corpus can change
-- and "why did it answer that" is unanswerable from a corpus that has since moved.
CREATE TABLE IF NOT EXISTS turn_passage (
    turn_id  TEXT NOT NULL REFERENCES turn (id) ON DELETE CASCADE,
    rank     INT  NOT NULL,
    entry_id TEXT NOT NULL,
    language TEXT NOT NULL,
    score    DOUBLE PRECISION NOT NULL,
    question TEXT NOT NULL,
    PRIMARY KEY (turn_id, rank)
);

CREATE TABLE IF NOT EXISTS turn_tool_call (
    turn_id TEXT NOT NULL REFERENCES turn (id) ON DELETE CASCADE,
    seq     INT  NOT NULL,
    name    TEXT NOT NULL,
    outcome TEXT NOT NULL,
    PRIMARY KEY (turn_id, seq)
);

-- Who did what through the admin surface. Operators cannot edit this table through
-- any endpoint; there is deliberately no update or delete path to it.
-- What somebody thought of an answer.
--
-- Two sources with different weight and the same shape: a customer knows whether they were
-- helped and nothing about whether the answer was correct; an operator knows the opposite.
-- Recording which one said it is the difference between a signal and a number.
--
-- One verdict per turn per source: a customer changing their mind replaces their own
-- verdict and leaves the operator's alone.
CREATE TABLE IF NOT EXISTS turn_feedback (
    turn_id    TEXT        NOT NULL REFERENCES turn (id) ON DELETE CASCADE,
    source     TEXT        NOT NULL CHECK (source IN ('customer','operator')),
    verdict    TEXT        NOT NULL CHECK (verdict IN ('helpful','wrong','unclear')),
    note       TEXT,
    actor      TEXT        NOT NULL,
    at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Whether this has been turned into an eval case or a knowledge edit. The point of
    -- collecting it is the work it causes; a table nobody clears is a suggestion box.
    handled_at TIMESTAMPTZ,
    handled_by TEXT,
    PRIMARY KEY (turn_id, source)
);
CREATE INDEX IF NOT EXISTS turn_feedback_open_idx
    ON turn_feedback (at DESC) WHERE verdict <> 'helpful' AND handled_at IS NULL;

-- Whether the people who answer tickets were actually told.
--
-- The failure mode of a notification is silence, and silence is indistinguishable from
-- "nothing happened" -- nobody chases a message they do not know was sent. This row is
-- what makes "we were never told about that ticket" an answerable question.
CREATE TABLE IF NOT EXISTS handoff_delivery (
    id            BIGSERIAL   PRIMARY KEY,
    at            TIMESTAMPTZ NOT NULL,
    type          TEXT        NOT NULL,
    ticket_number TEXT        NOT NULL,
    status        INT         NOT NULL DEFAULT 0,
    failure       TEXT
);
CREATE INDEX IF NOT EXISTS handoff_delivery_failed_idx
    ON handoff_delivery (at DESC) WHERE failure IS NOT NULL;

CREATE TABLE IF NOT EXISTS admin_audit (
    id      BIGSERIAL   PRIMARY KEY,
    at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor   TEXT        NOT NULL,
    action  TEXT        NOT NULL,
    object  TEXT        NOT NULL,
    outcome TEXT        NOT NULL,
    detail  TEXT
);
CREATE INDEX IF NOT EXISTS admin_audit_at_idx ON admin_audit (at DESC);
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

	if _, err := conn.Exec(ctx, fmt.Sprintf(Schema, dimensions)); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}
