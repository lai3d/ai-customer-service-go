# Knowledge that can be edited

The corpus was a test fixture. Eighteen bilingual entries in `corpus/faq.json`, loaded once
at start-up, and changing one meant a code change, a rebuild and a redeploy. There was no
versioning, no publication step, and no way for the people who actually know the answers to
change one.

It is now a versioned knowledge base, and `corpus/faq.json` is still byte-identical to the
Java implementation's.

## The bundled corpus is adopted, not rebuilt

At start-up, the documents already in the database are **stamped** with the bundled corpus's
version and that version is activated. Nothing is re-embedded.

That is the load-bearing decision. The bundled corpus's vectors are what every retrieval
number in this pair of repositories was measured against — 20/20 paraphrases, 4/4
cross-lingual, a similarity threshold measured to zero. Recomputing them would move the
measurement while claiming to preserve it, and the two implementations would stop being
comparable without either of them noticing.

Adoption is idempotent and runs every start-up. On a database that already has an active
version it does nothing at all: a service that has published edits does not get its corpus
stamped back to the bundled one on the next restart.

**This design came from the Java implementation**, which built it first and measured it.
Adopting their shape rather than inventing a second one is what keeps the pair a comparison.

## A version is built, then switched to, and never edited

```
publish ─ write the documents under a new corpus_version   (invisible: nothing reads it)
        └ insert the version row
        └ UPDATE corpus_active … WHERE revision = $expected  (one row, atomic)
```

A customer searching during a publication reads the old version completely and then the new
version completely, never a mixture. The switch is one row.

`corpus_active` has a primary key on a constant, so there is exactly one active version and
a table that could hold two of them does not exist. Its `revision` is what makes two
operators publishing from two stale pages a **409 for the loser** rather than a silent
overwrite — the same optimistic concurrency the ticket workflow uses, for the same reason.

Rollback is `Activate` pointed backwards. Retention keeps the newest few versions' documents
and deletes the rest, never the active one — and `Activate` **refuses a version whose
documents were swept**, because a rollback that silently activates an empty corpus turns an
incident into an outage.

## Retrieval reads one version

```sql
WHERE (corpus_version IS NULL OR corpus_version = (SELECT version FROM corpus_active …))
```

The `IS NULL` is what makes this safe to deploy: a database whose corpus has not been
adopted yet — mid-rollout, or a test that ingests without versioning — keeps working exactly
as before, rather than answering nothing.

An operator can search a specific version instead of the active one. That is what a preview
is, and it is the only reason `SearchOptions.Version` exists.

## `hnsw.iterative_scan`: argued, not evidenced here

The version predicate is a **post-filter** on an HNSW scan — the index walks the graph,
collects candidates, and only then discards the ones from other versions. Candidates spent
on retired documents are candidates not spent on live ones, and a `LIMIT 8` search can
return fewer than eight.

The Java implementation measured exactly that: 40 candidates, 26 dead, 14 reaching the
filter, 4 of the active version, top-8 of **1**. They set `hnsw.iterative_scan` on every
pooled connection and their test is red without it.

**This repository could not reproduce it.** What was measured here, on the same
`pgvector/pgvector:pg17` image:

| Case | Result |
| --- | --- |
| 4,000 rows, 5% selectivity, `EXPLAIN ANALYZE` | 8 of 8, `Rows Removed by Filter: 173` — 181 candidates walked |
| 20 published versions × 36 distinct vectors, retention to 2, autovacuum off | 8 of 8, **no** rows removed by the filter |
| the same through `Store.Search`, with the setting and without it | passes both ways |

So the setting is **kept and labelled**: the cost is nothing (the extra work happens only
when a scan resumes, and a single-version search never exhausts its first candidates — the
retrieval numbers and the answer eval are unchanged), and the failure it guards against is
silent. It is `strict_order` rather than `relaxed_order` because this service shows retrieval
scores to operators and ranks passages by them; an order that is approximately right is a
number that is approximately meaningless.

The test that was written to justify it is kept **with its claim corrected**, not deleted:
it measures that version churn plus retention does not degrade retrieval, which is worth
having, and it says in its own comment that it does not justify the setting. The alternative
— a test whose name implies it proves something it passes without — is the shape this
repository has thrown away twice before.

What remains unexplained is why the two stacks differ. Both are pgvector 0.8.6 in the same
image. The candidate difference is that their retired rows are *dead tuples* discarded
inside the index scan, while these measurements had live rows discarded by the executor,
which can ask the index for more — but that is a hypothesis, and it is written here as one.

## What the numbers did after all this

Unchanged, which is the point:

- Retrieval: 20/20 paraphrases, 4/4 cross-lingual, every language of every entry indexed.
- Answers: 34–35 of 35, the same range as before versioning, measured through the
  **versioned** read path rather than the unversioned fallback.

## Not built here

- **The editing surface.** `knowledge_entry` exists and the operations UI has no editor yet;
  publication is a store method rather than a button. That is the next slice, and it is
  ordinary CRUD on top of a versioning core that has been tested.
- **Prompt-injection defences for edited knowledge.** The moment entries are editable by
  many people, retrieved text is attacker-influenced input to a model holding tools. The
  system prompt is the whole of the defence today, and constraining tool calls by the
  caller's identity rather than by the model's judgement is written down in
  [the readiness list](production-readiness.md#2-the-corpus-is-a-test-fixture) and not done.
- **Per-version evaluation.** `make eval` scores the active version. Scoring a draft before
  activating it is what would make a publication safe rather than merely reversible.

---

[← Back to the README](../README.md)
