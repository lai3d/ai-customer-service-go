# What is missing before this runs a real product

This repository is a working system with an honest operational spine, and it is not a
product. This document is the difference between those two, written down so it can be
worked through rather than rediscovered.

It is a live document. Each item names what is missing, what goes wrong if it ships
without it, what "done" looks like, and roughly what it costs. When an item lands, its row
moves to **done** and the section says what was actually built rather than what was
planned.

**Most of this is not Go-specific.** The Java implementation of this system has the same
product gaps, because they are the same product. Items marked *Go* are this
implementation's own; items marked *both* apply to the pair, and doing one first is a good
way to find out what the other should do.

## What already exists, so the list below is fair

The parts that usually get skipped are the parts this repository has: a `turn` record that
tells a cancelled customer apart from a failed database, cost metered by the model the
provider actually reports (with a counter for the calls it could not price), ticket dedupe
and per-conversation caps that are database guarantees rather than process state, an
append-only audit trail with no mutation path, traces that were grepped to prove they
carry no customer text, and Kubernetes manifests whose every number was measured on a real
cluster. Retrieval is evaluated: 20/20 paraphrases and 4/4 cross-lingual, with the
similarity threshold measured to zero rather than guessed.

What is missing is almost entirely product, not scaffolding.

## The list

| # | Item | Blocks | Scope | Status | Estimate |
| --- | --- | --- | --- | --- | --- |
| 1 | [Identity, session ownership, rate limiting, global budget](#1-anyone-can-read-anyone-elses-conversation) | launch | both | **done** 2026-09-06 | 3–4 h |
| 2 | [Knowledge as a knowledge base, not a fixture](#2-the-corpus-is-a-test-fixture) | launch | both | not started | 4–6 h |
| 3 | [The loop back to a human](#3-a-ticket-is-a-row-and-nothing-else-happens) | launch | both | not started | 3–5 h |
| 4 | [Real tools instead of the mock](#4-the-tools-are-fiction) | week 1 | both | not started | 2–3 h |
| 5 | [Retention and deletion of customer data](#5-there-is-no-way-to-delete-a-customer) | week 1 | both | not started | 2–3 h |
| 6 | [An answer-quality regression set](#6-nothing-tells-you-a-prompt-change-made-it-worse) | week 1 | both | not started | 3–4 h |
| 7 | [Feedback from customers and operators](#7-nothing-comes-back) | week 2 | both | not started | 2–3 h |
| 8 | [Alerting and an SLO](#8-there-are-metrics-and-nothing-watches-them) | week 2 | Go | not started | 2 h |
| 9 | [A schema migration path](#9-the-first-change-to-a-live-schema-is-manual) | week 2 | Go | not started | 1–2 h |
| 10 | [The admin list pages lie past one page](#10-the-admin-lists-lie-past-the-first-page) | week 2 | Go | **done** 2026-09-06 | 0.5 h |
| 11 | [Provider failover](#11-three-providers-are-supported-and-one-runs) | scale | both | not started | 1–2 h |
| 12 | [Multi-tenancy](#12-one-corpus-one-config-one-price-list) | scale | both | not started | 4–6 h |
| 13 | [The deployment is a demo deployment](#13-the-manifests-stop-where-a-real-cluster-starts) | scale | Go | not started | 2–3 h |
| 14 | [Abuse and content safety](#14-the-system-prompt-is-the-whole-of-the-safety-story) | scale | both | not started | 2–3 h |
| 15 | [A turn a dead process left behind stays in_flight for ever](#15-a-turn-a-dead-process-left-behind-stays-in-flight-for-ever) | week 2 | Go | not started | 1 h |

Estimates are Claude session hours: the work, not the calendar. Things only you can do —
registering with providers, deciding a retention period with whoever owns that decision,
getting access to the real order system — are called out per item and are not in the
numbers.

---

## Launch blockers

### 1. Anyone can read anyone else's conversation

**Done, 2026-09-06.** `AUTH_MODE=session`.

`conversationId` came from the client with only a length check and chat memory is keyed on
it, so a message sent with somebody else's id appended to their history and composed the
model's reply from their context. The budget made the other half worse rather than better:
`CONVERSATION_TOKEN_BUDGET` is per conversation and conversation ids are free.

What exists now:

| | |
| --- | --- |
| `POST /api/v1/session` | issues an opaque token; the row stores its SHA-256, so a database dump is not a set of live sessions |
| Conversation ownership | server-issued ids, claimed atomically; someone else's conversation and a non-existent one return the *same* 404, because a 403 confirms an id exists |
| `TURNS_PER_MINUTE` | per subject, a fixed window in Postgres, with `Retry-After` |
| `SESSIONS_PER_HOUR_PER_IP` | the endpoint that mints subjects, because a per-subject limit is worth nothing if subjects are free |
| `DAILY_TOKEN_BUDGET` | the whole service, per UTC day, fed by every finished turn on both the blocking and the streaming path; refused as 503 rather than 429, because it is the service saying no and telling the customer to slow down would be a lie about whose problem it is |

Sessions are anonymous on purpose. This service does not know who a customer is and the
product it embeds in does; what is needed here is only that two customers are different
subjects and that a subject cannot be guessed. **Verifying an identity the host product
asserts — a JWT, an OIDC subject — is deliberately not built.** It is a fork that depends
on what that product already has, and `identity.Subject` plus the resolve step in
`internal/httpapi/identity.go` is the seam it arrives through. That is the remaining work
on this item, and it needs a decision rather than an hour.

`AUTH_MODE=off` keeps the old behaviour, because the benchmark and the cross-repository
comparison drive it and changing that changes what the two implementations measure. It is
not a production mode and the server logs a warning saying so at every start-up.

**How it was checked.** Every property was forced red first: SELECT-then-INSERT let three
of twelve concurrent subjects claim one conversation; removing the ownership check let
another session in on both paths; a 403 instead of a 404 made the endpoint an oracle for
which ids exist; storing the token unhashed put a credential in every dump; a read-then-
write limiter let the fourth request through; checking the limit after the turn ran it
anyway. Live, against a real model: two sessions and an identical 404 for another's
conversation and for a made-up one, `429` with `Retry-After: 8` on the third turn of a
minute, and `503` with `Retry-After` to the end of the UTC day once the day's ledger
passed the ceiling — including for a brand-new session and a brand-new conversation, which
is the thing the per-conversation budget could never do.

Two things found on the way. A test that aged a session with a negative TTL passed while
proving only that the zero-TTL guard exists. And `SSE_KEEPALIVE=0` panicked
`time.NewTicker` inside the handler on every streamed turn — `Load` refuses it now, and
the handler clamps, because a panic mid-response is a worse failure than a wrong heartbeat.

### 2. The corpus is a test fixture

`corpus/faq.json` is about twenty bilingual entries loaded once at start-up. Editing it
means a code change, a rebuild, and a redeploy. There is no versioning, no publication
step, and no way for the people who actually know the answers to change one.

This is deliberately not built here, for a reason that expires the moment this is a
product: the corpus is byte-identical to the Java repository's, and that is what makes
every retrieval number in the pair comparable. A product does not care about that.

The part that is easy to underestimate is what changes *about the model's safety* when the
corpus becomes editable. Today retrieved passages are trusted because they are a fixture
under review. As soon as knowledge is editable by many people — or, worse, imported from
tickets or a wiki — retrieved text is attacker-influenced input to the model, and
`create_support_ticket` and `lookup_order_status` are the blast radius.

**Done looks like** — and this is the Java implementation's shape, adopted here rather than
re-invented, because it answers the objection above rather than accepting it: knowledge
entries and revisions in Postgres; an editor in the operations UI; a publication that
snapshots the drafts, embeds them under a new corpus version, and only then switches a
single active-version row under a lock with an expected-version check; retrieval confined
to the active version; rollback by re-activating a retained version; retention keeping the
newest few.

The part that matters for this pair: **the bundled corpus is adopted as the first version
without re-embedding**, so `faq.json` stays byte-identical and every retrieval number stays
comparable while the product gets editable knowledge. The caveat above is answered, not
traded away.

**One thing to measure rather than inherit.** The Java side tested this design against the
HNSW dead-entry defect — twenty publications with autovacuum off, `n_dead_tup` over 300,
and a top-8 that still returned eight live rows for four questions in both languages. That
is stronger evidence than anything argued, and it says the design survives what `DELETE`
did not.

What it does not settle is whether that holds as retention grows, because versioning
replaces one problem with a related one: the search is filtered to the active version, so
candidates spent on *retired but live* rows are lost the same way candidates spent on dead
ones are. A synthetic probe here — three live versions of 36 rows, ~600 deleted — returned
8 of 8 unfiltered and **4 of 8 with a version filter**, but its vectors are duplicated
across versions, which is the pathological case for exactly that measurement, so it settles
nothing. When this is built, measure the filtered path against the real corpus at the
retention count that will actually be configured, and treat `hnsw.ef_search` as a parameter
of the design rather than a default. Note that `hnsw.iterative_scan` does not exist in the
`pgvector/pgvector:pg17` image both repositories use, so it is not available as a mitigation.

Plus, and separately: tool calls that a retrieved passage can influence must be constrained
by the caller's identity rather than by the model's judgement.

**Do not** wire a Publish button to the start-up importer. It is the shape that looks
finished and is not.

### 3. A ticket is a row, and nothing else happens

`TKT-4700` gets created, deduplicated, capped and audited — and then nothing. No queue, no
notification to anyone, no SLA, and no path back to the customer when a human does reply.
The operations UI is where a human *reads* it; there is nothing that makes a human *see*
it.

This is the line between an assistant that escalates and a chat box that files tickets
into a drawer.

**Done looks like:** tickets land somewhere humans already are (your existing ticketing
system, or a channel); assignment notifies; a reply from the operator reaches the customer
through whatever channel they arrived on; and the conversation shows that it happened, so
the model does not tell the customer to wait for something that already came.

**You must decide:** which system is the destination. That choice changes most of the
work, and it is not a technical decision.

---

## Week one

### 4. The tools are fiction

`internal/tools/orders.go` answers from `mockOrders`, a hard-coded map with `ORD-10045` in
it. Ticket creation is real; order lookup is not.

**Done looks like:** the real order service behind the same interface, with its own
credentials, a timeout shorter than the turn's, and a story for when it is down. Keep the
current design when you do it: a tool failure is a *value* the model can act on, never an
exception — an exception's message ends up in front of a customer.

**You must provide:** access to that service, and a non-production instance to test
against.

### 5. There is no way to delete a customer

There is no `DELETE` against customer data anywhere in `internal/` — only test cleanup and
the corpus replacement. Customer text sits in `chat_memory` and `turn` in plaintext,
forever, and the audit trail is append-only by design.

A deletion request today is a hand-written SQL statement by whoever has the password.

**Done looks like:** a retention period that expires `chat_memory` and `turn` payloads on
a schedule; a deletion path keyed by subject that covers both, and that says explicitly
what it does *not* delete, because the audit trail must survive — an audit row that can be
erased by the person being audited is not one. Deleting a subject's audit *content* while
keeping the fact of the action is usually the resolution.

**You must decide:** the retention period, and who signs off on the deletion policy.

### 6. Nothing tells you a prompt change made it worse

Retrieval is measured. Answers are not. There is no set of real questions with expected
properties, no scoring, and therefore no way to know that a prompt edit, a model upgrade
or a corpus change made the product worse. For something customers talk to, this is the
measurement that decides whether it is usable, and it is the one that is missing.

**Done looks like:** thirty to a hundred real questions with what a good answer must and
must not contain (grounding: it says it does not know when the corpus does not cover it;
tool use: it looks up the order rather than inventing a date; language: it answers in the
language asked); a runner that scores them; and a number in CI that a prompt change has to
not regress. The existing retrieval eval is the model for how to write it.

**Costs money to run:** each pass calls the real model. Budget it deliberately.

### 7. Nothing comes back

A customer cannot say the answer was wrong. An operator reading a bad answer in the
conversation view cannot mark it. Nothing flows into the corpus or into the eval set from
either.

**Done looks like:** a rating on the demo page's answer; a "this answer was wrong" action
in the operations UI that captures the turn; and a queue of those that feeds item 6's
regression set and item 2's knowledge editor. The value is the loop, not the widget.

### 8. There are metrics and nothing watches them

Traces reach Jaeger, metrics reach Prometheus, and no alert rule exists in this
repository. There is no `observability/` directory here at all. The Java implementation
has one — dashboards, a `PrometheusRule`, a `ServiceMonitor`, and a test that fails when a
dashboard references a metric the application does not emit.

`chat_unpriced_model_calls_total` exists precisely so a permanently-zero cost meter is
visible rather than plausible, and today nothing looks at it.

**Done looks like:** an SLO on the turn (success rate and latency), alerts on unpriced
calls, on budget-exceeded turns, on provider 5xx, and on the audit table not growing when
operators are working — and each alert forced red once before it is trusted, the way the
kind harness assertions are.

### 9. The first change to a live schema is manual

The schema is created by `CREATE TABLE IF NOT EXISTS` under a Postgres advisory lock at
start-up. That is correct for an empty database and says nothing about the second one:
there is no migration tool, so adding a column to a table with data in it is a hand-run
statement and a hope. The Java side uses Flyway; this side deliberately does not.

**Done looks like:** a migration tool with versioned files, adopting the existing schema as
a baseline, holding the same advisory lock so two replicas starting together still
serialise. `TestConcurrentStartersAgainstAColdDatabaseAllSucceed` is the test that must
still pass.

### 10. The admin lists lie past the first page

**Done, 2026-09-06.**

`admin-ui` fetched 100 conversations (200 tickets) and paginated them in the browser. The
API returned `total`, the UI never passed it to the table, and Ant Design fell back to
counting the rows it had. Past one page the footer stated a number that was wrong and the
rest of the data was gone with no indication that anything was missing.

Page and page size now drive `limit`/`offset`, and the response's `total` drives the
footer. `usePaging` holds it in one place because the mistake is per-table and there are
three of them; it also caps the page size at the server's own limit of 200, since both
stores silently substitute 50 for anything outside their range — a page size the server
will not honour is a table that disagrees with its own footer.

The audit endpoint had no paging at all: `AuditTrail` took a limit and returned rows. It
takes an offset and returns a total now, which is the same fix one layer down.

Three things came with it, all of them capability the API already had and nothing offered:
the ticket list filters by assignee, and the overview window is selectable (the server
accepts an hour to ninety days; the page hard-coded 168).

**How it was checked.** Two of the three React tests failed on the old code before the fix
— the footer assertion could not find "250 conversation(s)", and the page-2 assertion
found `limit: 100` where it wanted an offset. The Go test for the audit total was forced
red twice: once with the total computed as `least(count(*), limit)`, which is exactly the
lie being fixed, and once with the offset multiplied by zero, which produced "offset
changed nothing; the second page repeats the first". Then 137 synthetic conversations in a
real database, driven in Chrome: the footer said 139, page 7 requested `offset=120` and
showed different rows, and changing the page size re-requested from the server rather than
re-slicing what was on screen.

---

## Before scale

### 11. Three providers are supported and one runs

Anthropic, OpenAI and xAI are all implemented and verified live. `CHAT_PROVIDER` picks
one at start-up. If that provider is down, the service is down.

**Done looks like:** a secondary provider, a health signal that decides when to use it,
and an explicit answer to the question the failover raises — the two providers do not
produce the same answers, so a conversation that switches mid-way changes voice. Deciding
that is part of the work.

### 12. One corpus, one config, one price list

Everything is single-tenant: one FAQ, one system prompt, one budget, one set of prices,
one set of operator tokens. Nothing is keyed by a tenant.

**Done looks like:** a tenant column on the tables that need one, retrieval filtered by it,
per-tenant budgets and prices, and operator tokens scoped to a tenant. The audit trail
becomes more important here, not less.

### 13. The manifests stop where a real cluster starts

`k8s/` has a Namespace, two Deployments, two Services and two ConfigMaps. It has no
Ingress or TLS, no HorizontalPodAutoscaler, no PodDisruptionBudget, and no NetworkPolicy —
the Java side has one of those. Images are not published to a registry. Secrets are plain
Kubernetes Secrets, which are base64, not encryption.

**Done looks like:** the above, plus an image published with an immutable tag, and secrets
from your cloud's manager rather than a manifest. Re-run `k8s/kind/verify.sh` after each,
and add an assertion for each thing added — the harness exists because the Java
repository's manifests were committed unapplied and two were wrong.

### 14. The system prompt is the whole of the safety story

There is no content moderation, no jailbreak detection, no refusal logging, and no
per-customer abuse signal. Grounding holds today because the corpus is small and the
prompt is careful, and neither of those is a control.

**Done looks like:** a moderation pass on input and output, refusals recorded so their rate
is visible, and an abuse signal per identity — which is only possible after item 1.

### 15. A turn a dead process left behind stays in_flight for ever

`Recorder.Begin` writes the turn as `in_flight` before the model call and `Finish` closes
it afterwards. A process that dies in between leaves the row `in_flight` permanently:
nothing sweeps it, and the operations overview counts it under "not completed" as though
the customer had abandoned the conversation. A crash and a closed tab are exactly the two
things that record is supposed to tell apart.

**Done looks like:** a lease, and a sweeper that marks a row still running past it —
`unknown`, never `completed`, because what happened to it is not known. The Java
implementation has this (`TurnRecordSweeper`) and this repository does not; the shape is
worth copying rather than redesigning.

**Watch for:** the sweep and a slow `Finish` racing each other. `Finish` runs on a detached
context after the response, so the lease has to be comfortably longer than the longest
finish, or a live turn gets marked `unknown`.

---

## How to work through this

One item per branch, one commit per item, and this document updated in the same commit:
the row's status, and the section rewritten to say what was built rather than what was
planned. If an item turns out to be wrong — the wrong shape, or unnecessary — say that
here instead of deleting the row. The reasoning is the useful part.

Two items change the pair's comparability and should be discussed with the Java side
before starting: item 2 (the corpus stops being a shared fixture) and item 12 (multi-tenancy
changes every table). Everything else can proceed independently.

---

[← Back to the README](../README.md)
