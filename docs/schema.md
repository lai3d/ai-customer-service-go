# Changing a schema that has data in it

The schema was a Go constant applied at start-up with `CREATE TABLE IF NOT EXISTS` under a
Postgres advisory lock. That is correct for an empty database and says nothing about the
second one: adding a column to a table with rows in it was a hand-run statement and a hope,
and nothing anywhere recorded which statements a given database had seen.

It is now versioned files with a ledger. The Java implementation uses Flyway; this side
deliberately does not, and the whole mechanism is about two hundred lines because the hard
parts of a migration tool are the decisions, not the code.

## The baseline is applied, not assumed

Every migration tool has to answer one question first: what do you do about the databases
that already exist?

The usual answer is a marker — `flyway baseline`, an `--assume-applied` flag, a row
inserted by hand — and every one of them is a claim somebody has to get right. This
repository does not need one, because **`0001_baseline.sql` is the previous start-up
schema verbatim, and that schema was already idempotent**: every statement is
`CREATE … IF NOT EXISTS` or `ADD COLUMN IF NOT EXISTS`. Applying it to a database that
already has the schema does nothing; applying it to a cold one creates everything.

So adoption is just running it. No flag, no marker, nothing to get wrong.

That is a property of this schema rather than of migrations in general, and it is asserted
rather than assumed: `TestADatabaseWithTheSchemaAndNoLedgerAdoptsTheBaseline` builds the
pre-migration state — schema applied straight from the file, a row of data in it, no
ledger — and then runs the migrations against it. Removing one `IF NOT EXISTS` from the
baseline makes it red with `relation "chat_memory" already exists`.

**The first version of that test proved nothing**, and it is worth saying why. It called
`Open` twice and checked the data survived. It passed with the `IF NOT EXISTS` removed —
because the first `Open` wrote the ledger, so the second skipped the baseline entirely. It
was measuring that a restart is a no-op, which nobody doubted. Both tests are kept: the
one that covers adoption, and `TestOpeningTwiceKeepsTheDataAndTheLedger` for the restart,
which is a different claim.

## What the ledger records, and why the checksum is in it

```sql
CREATE TABLE schema_migration (
    version     INTEGER     NOT NULL PRIMARY KEY,
    name        TEXT        NOT NULL,
    checksum    TEXT        NOT NULL,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    duration_ms BIGINT      NOT NULL
)
```

The version records that *a* file with that number ran. The **checksum** records that it
was *this* file — and editing an applied migration is exactly what somebody does when
writing another one feels like overkill. A build whose `0004` no longer hashes to what the
database says it applied refuses to start, naming the file, because what the database
contains and what the repository says it contains have diverged and every guess about
which is right is worse than stopping.

The other refusal is **out of order**: a migration numbered below one already applied.
That is what two branches both adding `0007` and merging looks like from the database's
side — the deployed database would silently skip one of them, and a developer machine that
had seen neither would run both. Both outcomes are defensible, which is the problem.

## One transaction, and the half of it Postgres already does

A migration and its ledger row commit together. One that ran and was not recorded runs
again on the next start-up against a database it has already changed; one recorded without
running is a lie that only surfaces in the migration after it.

Two tests cover that, and **they are not the same test**:

| Test | What actually makes it pass |
| --- | --- |
| `TestAFailingMigrationLeavesNeitherTheChangeNorTheRecord` | Postgres's *implicit* transaction around a multi-statement simple query. It stays green with the explicit transaction removed — verified — and the test says so in its own comment rather than claiming otherwise. |
| `TestAMigrationThatCannotBeRecordedIsRolledBackWithItsRecord` | the explicit transaction. A `CHECK` constraint on the ledger refuses the record after the DDL succeeded, and without one transaction around both the table exists and nothing says so. |

Getting the second one to fail took three attempts, and the two that did not work are the
useful part:

- Swapping `tx.Exec` for `conn.Exec` **changes nothing**. pgx runs both on the same
  connection, and a connection inside a transaction block is inside it however you address
  it. The perturbation looked like it removed the transaction and did not.
- The multi-statement migration rolls back on its own, so the first test cannot see the
  explicit transaction at all.

What finally worked was committing the DDL *before* opening the transaction, which is the
only shape that genuinely separates the two writes.

The cost of one transaction per migration is stated rather than hidden: everything runs
inside one, so `CREATE INDEX CONCURRENTLY` cannot. A migration that needs one needs a
different mechanism, and building that mechanism before there is a table big enough to need
it would be inventing the requirement too.

## `:dimensions`, and why it is not `%d`

The baseline carries `vector(:dimensions)`, substituted by a plain string replace. It was
`%d` and a `fmt.Sprintf` when the schema was a Go constant, and that is safe only for as
long as no migration contains a percent sign — the first `LIKE '%…%'` in a data backfill
would turn the substitution into a silent corruption of the statement beside it.

`TestTheEmbeddedMigrationsAreWellFormed` fails on any printf verb in any migration. It
found one immediately: the comment at the top of the baseline explaining why verbs are not
used contained two.

## What this does not do

- **No down migrations.** A rollback of a schema change is a decision about data, not a
  file that can be written in advance, and a `down` nobody has run is worse than none.
- **No `CREATE INDEX CONCURRENTLY`**, as above.
- **Changing `EMBEDDING_DIMENSIONS` on an existing database still does nothing**, exactly
  as before: `0001` has already been applied and will not run again. Different dimensions
  mean re-embedding the corpus, which is a decision this pair of repositories takes
  deliberately and not as a side effect of a config change — see
  [Knowledge](knowledge.md#the-bundled-corpus-is-adopted-not-rebuilt).
- **No separate binary.** Migrations run at start-up under the same advisory lock that
  already serialised the DDL, so six replicas starting against a cold database still come
  up — and the ledger now shows each migration applied exactly once rather than six
  replicas each having been saved by a primary key.

---

[← Back to the README](../README.md)
