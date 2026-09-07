-- 0003 · the tenant on the identity tables.
--
-- The three steps the Java side's ADR 002 names, in order and for the reason it gives:
-- add the column with a default so the backfill is the ALTER itself, then **drop the
-- default**, so a row written without a tenant is a constraint violation rather than a
-- silent orphan on whichever tenant happened to be first.
--
-- Postgres 11 and later backfill an ADD COLUMN ... NOT NULL DEFAULT without rewriting the
-- table, so this is a catalogue change on any size of table. That is worth knowing rather
-- than assuming: the same statement on Postgres 10 rewrites every row under an ACCESS
-- EXCLUSIVE lock, which is an outage rather than a migration.
--
-- The foreign key is the point of `default` being a real row. Without it "tenant_id" is a
-- string column that happens to contain tenant ids, and the first typo in a backfill
-- creates a tenant nobody can see and nobody can log into.

ALTER TABLE chat_session
    ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default' REFERENCES tenant (tenant_id);
ALTER TABLE chat_session ALTER COLUMN tenant_id DROP DEFAULT;

ALTER TABLE conversation_owner
    ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default' REFERENCES tenant (tenant_id);
ALTER TABLE conversation_owner ALTER COLUMN tenant_id DROP DEFAULT;
CREATE INDEX conversation_owner_tenant_idx ON conversation_owner (tenant_id, subject);

-- The rate limiter's windows.
--
-- The per-subject bucket would have been safe without this -- subject ids are 128 bits of
-- randomness and do not collide across tenants. The per-IP session bucket would not: its
-- key is a client address, and one tenant's chatty office would spend another tenant's
-- session allowance. The column is on the table rather than folded into the key so that
-- the sweep, the abuse gauge and any future per-tenant limit all read the same column.
ALTER TABLE rate_window
    ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default' REFERENCES tenant (tenant_id);
ALTER TABLE rate_window ALTER COLUMN tenant_id DROP DEFAULT;
ALTER TABLE rate_window DROP CONSTRAINT rate_window_pkey;
ALTER TABLE rate_window ADD PRIMARY KEY (tenant_id, bucket, subject, window_start);
