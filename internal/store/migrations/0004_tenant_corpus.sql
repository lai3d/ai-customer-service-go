-- 0004 · the tenant on the corpus.
--
-- Two primary keys change here, and both were flagged in the readiness item before any of
-- this was built: `corpus_active` has a primary key on a constant, which is precisely the
-- shape that does not survive tenancy, and `knowledge_entry` is keyed by
-- (entry_id, language), which would make two tenants fight over `returns-window`.

ALTER TABLE faq_document
    ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default' REFERENCES tenant (tenant_id);
ALTER TABLE faq_document ALTER COLUMN tenant_id DROP DEFAULT;

-- The document key becomes (tenant_id, id) rather than staying on id alone.
--
-- It would have *worked* alone: a published document's id is `<version>:<entry>:<lang>`
-- and version names are globally unique, so ids do not collide across tenants. But that is
-- an argument about how ids happen to be built, made in a different file, and it stops
-- being true the day somebody shortens a version name. A document belongs to a tenant, so
-- the key says so.
ALTER TABLE faq_document DROP CONSTRAINT faq_document_pkey;
ALTER TABLE faq_document ADD PRIMARY KEY (tenant_id, id);

-- Retrieval filters on tenant and version together, so the index carries both. The
-- single-column version index it replaces would leave the tenant to a heap filter, which
-- on an HNSW post-filter is candidates spent on another tenant's documents.
DROP INDEX faq_document_version_idx;
CREATE INDEX faq_document_tenant_version_idx ON faq_document (tenant_id, corpus_version);

-- Version *names* stay globally unique, and that is deliberate rather than an oversight:
-- a version name appears in log lines, in the operations UI and in an audit row, and a
-- name that means two different things depending on who is reading is a name that has to
-- be qualified everywhere it is used.
ALTER TABLE corpus_version
    ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default' REFERENCES tenant (tenant_id);
ALTER TABLE corpus_version ALTER COLUMN tenant_id DROP DEFAULT;
CREATE INDEX corpus_version_tenant_idx ON corpus_version (tenant_id, created_at DESC);

-- One active version per tenant, and the primary key is what says so -- the same job the
-- primary key on a constant was doing for one tenant. `only_one` goes: a boolean column
-- that is always true, next to a real key, is a thing somebody eventually filters on.
ALTER TABLE corpus_active
    ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default' REFERENCES tenant (tenant_id);
ALTER TABLE corpus_active ALTER COLUMN tenant_id DROP DEFAULT;
ALTER TABLE corpus_active DROP CONSTRAINT corpus_active_pkey;
ALTER TABLE corpus_active DROP COLUMN only_one;
ALTER TABLE corpus_active ADD PRIMARY KEY (tenant_id);

-- The drafts. Two tenants can both have an entry called `returns-window` and they are
-- different entries; without the tenant in the key the second one to be saved would
-- overwrite the first, and the operator who typed it would see their own text.
ALTER TABLE knowledge_entry
    ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default' REFERENCES tenant (tenant_id);
ALTER TABLE knowledge_entry ALTER COLUMN tenant_id DROP DEFAULT;
ALTER TABLE knowledge_entry DROP CONSTRAINT knowledge_entry_pkey;
ALTER TABLE knowledge_entry ADD PRIMARY KEY (tenant_id, entry_id, language);
