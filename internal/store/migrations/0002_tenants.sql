-- 0002 · tenants, and the keys that name one.
--
-- The first migration written against the tool rather than extracted into it, and the
-- first that is deliberately *not* idempotent: it runs once, the ledger says so, and
-- writing IF NOT EXISTS around statements that can only run once would be decoration
-- pretending to be a guarantee.
--
-- This migration adds the tenant and its keys and touches nothing else. The tenant_id
-- columns come with the code that reads them, one area at a time -- a column nothing
-- filters on is a column that looks like isolation and is not.

CREATE TABLE tenant (
    tenant_id   TEXT        NOT NULL PRIMARY KEY,
    name        TEXT        NOT NULL,
    -- Disabled rather than deleted. A tenant's rows outlive their contract: the
    -- conversations still have to be answerable for, and an erasure is a separate
    -- decision made deliberately rather than a side effect of cancelling an account.
    disabled_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by  TEXT        NOT NULL DEFAULT 'system'
);

-- The tenant everything that exists today belongs to.
--
-- It is a real tenant and not a null: every row this service has ever written belongs to
-- somebody, and "no tenant" as a value is the state that later turns into a query somebody
-- forgot to filter. The bundled corpus is this tenant's, and the parity fixtures that make
-- the two repositories comparable run as this tenant.
INSERT INTO tenant (tenant_id, name) VALUES ('default', 'Default');

-- An API key per tenant, on /api/v1/**.
--
-- Not a subdomain and not a claim in a token: this service has no host identity to assert
-- one with, and the caller is another server rather than a browser.
--
-- The key arrives as `csk_<key_id>_<secret>`. Only key_id is stored in the clear, and it
-- exists so a lookup is an index hit rather than a scan comparing hashes -- the shape a
-- constant-time comparison needs, because the alternative is hashing the candidate against
-- every row and calling the timing difference acceptable. The secret is stored as SHA-256
-- and shown once, at issue.
--
-- SHA-256 rather than bcrypt or argon2 deliberately, and the reason is the input: this is
-- a 32-byte random secret this service generated, not a password a person chose. Key
-- stretching buys resistance to guessing a low-entropy input, and there is nothing to
-- guess here. It is also on the path of every request, where a deliberately slow hash is a
-- deliberately slow service.
CREATE TABLE tenant_api_key (
    key_id       TEXT        NOT NULL PRIMARY KEY,
    tenant_id    TEXT        NOT NULL REFERENCES tenant (tenant_id),
    key_hash     BYTEA       NOT NULL,
    -- What it is for, so revoking the right one does not need a guess.
    label        TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by   TEXT        NOT NULL,
    -- Written at most once a minute rather than on every request: an UPDATE per call would
    -- make one row per tenant the hottest in the database, and "when was this key last
    -- used" is a question nobody asks to the second.
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX tenant_api_key_tenant_idx ON tenant_api_key (tenant_id);
