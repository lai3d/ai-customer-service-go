-- 0005 · the tenant on the operational records.
--
-- The turn, the model's context, the tickets, and the audit trail. These are the tables
-- the operations surface reads, which is the one surface that shows customer text on
-- purpose -- so this is the migration that decides whether one operator can read another
-- tenant's conversations.
--
-- The child tables of `turn` (turn_passage, turn_tool_call, turn_feedback) deliberately do
-- **not** get a column. They are reachable only through a turn, which carries one, and a
-- copy of the tenant on a child row is a second value that can disagree with its parent --
-- with nothing to say which is right. The queries join.

ALTER TABLE turn
    ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default' REFERENCES tenant (tenant_id);
ALTER TABLE turn ALTER COLUMN tenant_id DROP DEFAULT;
CREATE INDEX turn_tenant_started_idx ON turn (tenant_id, started_at DESC);

ALTER TABLE chat_memory
    ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default' REFERENCES tenant (tenant_id);
ALTER TABLE chat_memory ALTER COLUMN tenant_id DROP DEFAULT;

-- Ticket *numbers* stay globally unique and the sequence stays global. A ticket number is
-- not a secret and is quoted to customers, in emails, and across systems; a number that
-- means two different things depending on who is reading is a number that has to be
-- qualified everywhere it appears. What the tenant does here is decide who can read it.
ALTER TABLE support_ticket
    ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default' REFERENCES tenant (tenant_id);
ALTER TABLE support_ticket ALTER COLUMN tenant_id DROP DEFAULT;
CREATE INDEX support_ticket_tenant_idx ON support_ticket (tenant_id, created_at DESC);

-- The one table that must never lose a row, so this migration only adds a column.
--
-- Backfilled to `default` like the rest. That is a claim about history -- every audited
-- action so far was taken against the only tenant there was -- and it is true here because
-- the tenant column arrives in the same release as the second tenant.
ALTER TABLE admin_audit
    ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default' REFERENCES tenant (tenant_id);
ALTER TABLE admin_audit ALTER COLUMN tenant_id DROP DEFAULT;
CREATE INDEX admin_audit_tenant_idx ON admin_audit (tenant_id, at DESC);
