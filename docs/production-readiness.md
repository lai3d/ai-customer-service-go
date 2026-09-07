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
| 2 | [Knowledge as a knowledge base, not a fixture](#2-the-corpus-is-a-test-fixture) | launch | both | **done** 2026-09-07 | 4–6 h |
| 3 | [The loop back to a human](#3-a-ticket-is-a-row-and-nothing-else-happens) | launch | both | **done** 2026-09-06 | 3–5 h |
| 4 | [Real tools instead of the mock](#4-the-tools-are-fiction) | week 1 | both | **seam done** 2026-09-07, integration blocked on access | 2–3 h (0.5 h left) |
| 5 | [Retention and deletion of customer data](#5-there-is-no-way-to-delete-a-customer) | week 1 | both | **done** 2026-09-06 | 2–3 h |
| 6 | [An answer-quality regression set](#6-nothing-tells-you-a-prompt-change-made-it-worse) | week 1 | both | **done** 2026-09-06 | 3–4 h |
| 7 | [Feedback from customers and operators](#7-nothing-comes-back) | week 2 | both | **done** 2026-09-07 | 2–3 h |
| 8 | [Alerting and an SLO](#8-there-are-metrics-and-nothing-watches-them) | week 2 | Go | **done** 2026-09-07 | 2 h |
| 9 | [A schema migration path](#9-the-first-change-to-a-live-schema-is-manual) | week 2 | Go | not started | 1–2 h |
| 10 | [The admin list pages lie past one page](#10-the-admin-lists-lie-past-the-first-page) | week 2 | Go | **done** 2026-09-06 | 0.5 h |
| 11 | [Provider failover](#11-three-providers-are-supported-and-one-runs) | scale | both | **done** 2026-09-07 | 1–2 h |
| 12 | [Multi-tenancy](#12-one-corpus-one-config-one-price-list) | scale | both | queued behind 9; design from the Java side | 4–6 h |
| 13 | [The deployment is a demo deployment](#13-the-manifests-stop-where-a-real-cluster-starts) | scale | Go | not started | 2–3 h |
| 14 | [Abuse and content safety](#14-the-system-prompt-is-the-whole-of-the-safety-story) | scale | both | **partly done** 2026-09-07 | 2–3 h (moderation not built, deliberately) |
| 15 | [A turn a dead process left behind stays in_flight for ever](#15-a-turn-a-dead-process-left-behind-stays-in_flight-for-ever) | week 2 | Go | **done** 2026-09-07 | 1 h |

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

What it does not settle is whether that holds as retention grows — and the Java side then
measured that too, which is worth reading before this design is copied.

**It is bounded by workload, not structurally immune.** Raising retention from 3 to 10 to
30 changed nothing (8 of 8 each time, at 4% active rows), because pgvector's HNSW stores
**one graph element per distinct vector** with a list of heap tids: a publication that
re-embeds unchanged entries to bit-identical vectors costs one candidate for all of its
copies, not one per copy. With *random* vectors — entries that actually changed — the same
shape gives 1 of 8 at 5% selectivity and 0 of 8 at scale. So the two conditions are that
re-embedding is deterministic (true for an in-process ONNX model, not guaranteed for an API
embedder returning slightly different floats) and that few entries change between retained
versions. A knowledge base where most entries change every publication gets the bad number.

That also explains why the synthetic probe here was uninformative: its 36 orthogonal
vectors were duplicated across versions, so the whole index was two distinct elements.

**Correction to something this document said.** It stated that `hnsw.iterative_scan` does
not exist in the `pgvector/pgvector:pg17` image. It does — 0.8.6 ships it, defaulting to
`off`. The check that produced the wrong answer ran `SHOW hnsw.iterative_scan` in a fresh
session; the extension's GUCs register when its library is loaded, so the parameter is
genuinely unrecognised until any vector operation or `LOAD 'vector'` happens in that
session. `strict_order` and `relaxed_order` both exist and the Java side measured
`relaxed_order` turning 0 of 8 into 8 of 8 on a 40,000-row table.

**It is not being set here, and the reason is a measurement rather than an oversight.**
`Store.Search` post-filters by language, which is the same mechanism, so this repository has
the shape. At 4,000 rows with 5% matching, `EXPLAIN (ANALYZE)` confirming
`Index Scan using ..._embedding_idx` with `Filter: (language = 'zh')`:

```
Limit (actual rows=8)
  ->  Index Scan using lang_probe_embedding_idx (actual rows=8)
        Filter: (language = 'zh'::text)
        Rows Removed by Filter: 173
```

**`Rows Removed by Filter` is the number that matters, not the eight.** It says the scan
walked 181 candidates to find its eight, so the filter is costing candidates exactly as the
mechanism predicts — it simply had enough of them. Eight rows returned tells you nothing
about how close that was; this tells you the margin, and it is why the Java side's earlier
"immune" measurements were misleading (their planner had been choosing a sequential scan,
so no candidates were being spent at all).

A setting with no case that needs it is the kind of configuration this repository does not
add. The Java side has the case: with every entry's text changed each publication and the
index forced, twenty publications gave 26 dead of 40 candidates and a top-8 of 1.

**Core done, 2026-09-06** — [the reasoning](knowledge.md). Versions are built, activated
under an expected-revision check, rolled back and retained; retrieval reads the one active
version; and the bundled corpus is adopted as the first version **without re-embedding**, so
`corpus/faq.json` stays byte-identical and the pair stays comparable. The numbers confirm it:
20/20 and 4/4 retrieval unchanged, and 34–35/35 on the answer eval — measured through the
versioned read path, after a first attempt measured the unversioned fallback that no
deployment uses.

`hnsw.iterative_scan = strict_order` is set, and **argued rather than evidenced here**: three
measurements in this stack failed to reproduce the starvation the Java side sees, including
twenty published versions with retention and autovacuum off. It is kept because the cost is
nothing and the failure is silent, and the test written to justify it now says in its own
comment that it does not. Why the two stacks differ is written down as an open question
rather than a conclusion.

**Done, 2026-09-07.** The editor is in the operations UI: drafts, a publication with a
note, a version history with rollback, and every edit audited with *what* changed. Saving
and publishing are visibly different actions, because they have different consequences.

The injection question landed with it, and is measured rather than claimed:
`injection-in-a-corpus-entry` in `make eval` writes an entry that orders the assistant to
ignore its instructions and call a tool. `withPassages` labels retrieved text as documents
rather than instructions — **argued, not evidenced**, since the case passes with and without
it; the probe is too weak to discriminate. The real constraint, tool calls bounded by the
caller's identity rather than the model's judgement, is still not built.

### 3. A ticket is a row, and nothing else happens

`TKT-4700` gets created, deduplicated, capped and audited — and then nothing. No queue, no
notification to anyone, no SLA, and no path back to the customer when a human does reply.
The operations UI is where a human *reads* it; there is nothing that makes a human *see*
it.

This is the line between an assistant that escalates and a chat box that files tickets
into a drawer.

**Done, 2026-09-06.** `internal/handoff`, and [the reasoning](handoff.md).

Outbound: `HANDOFF_WEBHOOK_URL` is told when a ticket is raised and when an operator
replies, asynchronously, with one retry and every outcome recorded in `handoff_delivery` —
because a notification's failure mode is silence, and the operations overview shows the
undelivered count in red. The body carries the ticket number, the conversation id and what
happened, and **no customer text**: a webhook is outside this service's control, and whoever
receives it can open the operations UI where reading is audited.

Inbound: an operator replies in the ticket dialog, and the text goes into the customer's
conversation as well as the ticket's history — attributed in its own words, `alex (support):
…`, because the customer cannot see a database column. `GET /api/v1/conversations/{id}`
(session-scoped) is where they read it.

Writing it into the conversation is the part that matters. Live, after an operator said a
refund had been released manually, the next turn answered *"你这边不用再做任何操作。alex
已经手动放行了这笔退款"* — without it, the assistant would have told the customer to wait for
a human who had already answered.

**Still your decision:** which system the webhook points at. That was the part this could
not decide, so it built the half that does not depend on it. Nothing pushes to the customer
either — no email, no wake-up for a closed tab — which is the same decision seen from the
other end.

---

## Week one

### 4. The tools are fiction

`internal/tools/orders.go` answered from `mockOrders`, a hard-coded map with `ORD-10045` in
it. Ticket creation is real; order lookup was not.

**Seam done, 2026-09-07. The integration is not, and cannot be from here** — see
[Tool calling](tools.md#where-an-order-comes-from) for the reasoning and the evidence.

What exists now:

| | |
| --- | --- |
| `tools.OrderSource` | one interface, two implementations, chosen by configuration |
| `MemoryOrders` | the five-order fixture, still the default, still what the tests, the benchmark, the eval and the demo drive — they measure the *model's* behaviour, and a real lookup would put variance into every number |
| `HTTPOrders` | `GET {ORDER_SERVICE_URL}/orders/{number}`, bearer token, a 3 s budget for the whole lookup, one retry for the failures a moment fixes |
| `ORDER_SERVICE_*` | in `.env.example`, `docker-compose.yml` and `k8s/configmap.yaml`; unset means the fixture, and the server logs a **warning** saying exactly that at every start-up |
| Six outcomes | `found`, `not_found`, `timed_out`, `unavailable`, `unreadable`, `denied` — a closed set, so they are safe as metric labels, and none of them is an exception |

The contract the item asked to keep is kept, and slightly strengthened: `lookup_order_status`
now has **no** path that returns an error. A ticket storage failure still does, because
that is this service's own database and the model has nothing useful to say about it; an
order service failing is somebody else's outage, and the model has something different and
useful to say about each kind.

**What is not done, and why it cannot be done here.** There is no order service to point
this at, so the wire contract — the URL shape, the status codes, the field names — is a
guess. It is written down in `orders_http.go` so that it can be corrected in one place, and
`docs/tools.md` labels it unverified rather than describing it as a design. Every *failure*
path is verified against an `httptest` server, and every assertion in that file was forced
red before being trusted; the table of what was broken to make each one fail is in
`docs/tools.md`.

**You must still provide:** access to the real order service, a non-production instance to
test against, and the credential. Expect the first run against it to change the request
shape and the response field names. The parts that should not change are the ones this item
was actually about: failures are values, the outcomes stay distinct, and the budget covers
the whole lookup.

**One thing found on the way that is not about orders.** A test written to prove the order
number cannot escape its URL path asserted on `r.URL.Path` — which is what net/http
*decoded* — and so reported a correctly-escaped `%2F..%2F` as a traversal. It was red
against code that was right. `r.RequestURI` is what went on the wire; this is the same
shape as the regex that measured the language rather than the bug.

### 5. There is no way to delete a customer

**Done, 2026-09-06.** `internal/retention`, and [the reasoning](retention.md).

Expiry by age (`RETENTION_DAYS`, swept on a schedule, off by default with a start-up
warning) and erasure on request (`DELETE /api/admin/v1/conversations/{id}`, operator only,
audited with a report of what it removed).

The interesting half was what survives. `admin_audit` is untouched — an audit row the
subject of the audit can erase is not an audit row, and it costs nothing to hold that line
because the trail holds names, ids and outcomes rather than customer text. Tickets are
redacted rather than deleted, because deleting an `OPEN` one erases the fact that somebody
is owed a refund along with the words that asked for it; the ticket keeps its number,
state and history, and the summary, order number and event details become `[erased]`.

Verified live: a viewer refused with `403` and the refusal audited, an operator's erasure
emptying `chat_memory` and `turn` while `TKT-4702` survived as `OPEN` with its history
intact, and a sixty-day-old turn planted into a running service swept on the next tick.

**Still your decision:** the retention period itself, and who signs off on the policy.
`RETENTION_DAYS=0` is a choice the service now says out loud rather than one it hides.

**Not built:** export ("give me my data" is the other half of most regulations),
encryption at rest, and erasure by subject over the API — `EraseSubject` exists in the
store, but the operations surface has no way to name an anonymous subject yet.

### 6. Nothing tells you a prompt change made it worse

**Done, 2026-09-06.** `make eval`, and [the reasoning](evaluation.md).

35 cases against the real model: **35/35, $0.52, 2m08s** on `claude-opus-5`. Facts from the
corpus in both languages, tool use and escalation, five questions the corpus does not cover,
and a multi-intent case. Every check is mechanical — no model grading another model — and
numbers carry the most weight, because a wrong number is a hallucination and a wrong tone is
an opinion.

**The number that makes the first one mean something is the control: 15/35 (42.9%) with the
corpus left out** (`make eval-control`). A suite that scores 100% has said nothing until the
same harness is shown to produce a bad number; a 57-point collapse is what "grounded in this
corpus" looks like as a measurement.

It is opt-in rather than in CI, for the same reason `make bench` is: $0.52 a run is cheap
for a person about to change a prompt and expensive for a job that fires on every push. The
runner refuses to start if the corpus version has moved.

**The first run scored 34/35, and the failure was the assertion rather than the answer** —
a grounding case banned any mention of a time of day, and the model correctly quoted the
support hours while saying it knew nothing about a shop. Recorded in the doc, because the
eval's own cases are not exempt from the mistake this repository keeps making.

**Not measured:** whether the answer is any *good*, tone, variance (each case runs once),
and the other two providers.

### 7. Nothing comes back

A customer cannot say the answer was wrong. An operator reading a bad answer in the
conversation view cannot mark it. Nothing flows into the corpus or into the eval set from
either.

**Done looks like:** a rating on the demo page's answer; a "this answer was wrong" action
in the operations UI that captures the turn; and a queue of those that feeds item 6's
regression set and item 2's knowledge editor. The value is the loop, not the widget.

**The operator half is done, 2026-09-07.** `internal/feedback` records a verdict per turn
per source, the conversation view has a Judge action, and the Feedback page is the queue —
each item carrying the question, the reply, the model and the entries it was answered from,
so a reported-wrong answer arrives as a piece of work rather than a complaint. The two
sources are stored separately and never averaged: a customer knows whether they were
helped and nothing about correctness, an operator knows the opposite, and one number would
lose the distinction that decides what to do.

**The customer half is done, 2026-09-07.** `POST /api/v1/turns/{id}/feedback` takes one
verdict from the session that owns the turn, and the demo page has three buttons on the
answer. That is the half that scales: operators read a sample, customers read everything.

Everything interesting about the endpoint is the check the operator one does not make. A
rating names a turn, and a turn belongs to somebody, so the turn is resolved to its
conversation and the conversation is checked against the session — with the *same* 404 for
a turn that is not yours and a turn that does not exist, because a status code that
separates them is an oracle for which turns exist. Four perturbations were each seen red:
dropping the ownership check (another session's rating returned 204 and was written),
dropping the session check (404 rather than 401 — the endpoint still refused, with the
wrong answer), letting an invented verdict fall through to the store (503 rather than 422),
and sharing the turn limiter's bucket (ratings spent the customer's turns).

The turn id reaches the page in the usage event, and it is not the authorisation. Ratings
have their own rate-limit bucket at the turn limit's number: a write anybody with a session
can make needs a ceiling, and sharing the *bucket* would let a rating cost a turn.

The customer's own words in a note are erased with the conversation, by the cascade from
`turn` and by nothing else — asserted in `internal/retention`, and seen red by removing the
foreign key.

### 8. There are metrics and nothing watches them

**Done, 2026-09-07.** `observability/`, and [the reasoning](observability.md#an-slo-on-the-turn).

Traces reached Jaeger and metrics reached Prometheus, and no alert rule existed anywhere
in this repository. `chat_unpriced_model_calls_total` was the sharpest example: a counter
built precisely so that a permanently-zero cost meter is visible rather than plausible,
with nothing looking at it.

**An SLO on the turn**, written down with its reasoning: 99% of turns end `completed` and
95% finish inside 16 seconds, both over 28 days. **Neither number is measured** — there is
no production traffic here — and the document says so first rather than last. What they are
anchored to is what has been watched: an 11.1 s tool-calling turn in Jaeger, a 5.065 s
usage card in the browser, and 35 eval cases at a 3.7 s mean. The first real week should
replace both by deriving them.

**Eleven alerts** as a `PrometheusRule`, with a `ServiceMonitor` beside it because the
operator ignores the `prometheus.io/*` annotations the Deployment carries: the two SLO
burn rates, two latency ones, the target being scraped at all, unpriced model calls,
provider errors, budget-exceeded turns, turns that hit the tool-round cap, undelivered
handoff notifications, and cost per hour. They live outside `k8s/` because
`kubectl apply -f k8s/` would fail on both files on any cluster without the
monitoring.coreos.com CRDs — including the kind cluster the harness builds.

One metric was added to make the last of those possible: `chat_handoff_notifications_total`.
Delivery outcomes were rows in `handoff_delivery` and a red number on the operations
overview, both of which need a person to go and look.

**The part that makes the rest worth having is the test**, and it is the Java side's
`DashboardMetricsTest` idea aimed at rules instead of dashboards. `internal/deployment`
exercises every metric through a real registry, reads them back out of a real `Gather`,
and checks every expression in the manifest against that: metric names, label names, label
values (from the Go source — the literals that reach `WithLabelValues`, including through
a variable), and the `le` of the latency SLI against the histogram's actual boundaries.
Four perturbations were forced red, one at a time; a matcher whose value cannot be
resolved fails rather than passes; and the test was shown non-vacuous three separate ways
— an empty manifest, a crippled source scan and a crippled metric-exercising loop each
produce a specific complaint rather than a pass. The wiring is checked too, because none
of it errors when it is wrong: namespaces, the monitor's selector against the Service's
labels, the port name, the scraped path against the route `main.go` registers, and the
`job` value against the monitor's `jobLabel`.

`make check-rules` runs Prometheus's own promtool in a container: the PromQL parses, and
**all eleven alerts have been seen to fire** on synthetic series. The case worth more than
those is the quiet one — a healthy but imperfect service raises nothing — and it is
imperfect deliberately: with no failures at all in the fixture the failure ratios go
*empty* rather than small, and an empty expression fires nothing however wrong the
threshold is. The first version of that test passed against a rule loosened to `> -1`.

**Two things in the original sketch were not built, and both are named in the document
rather than quietly dropped.** There is no alert on the audit trail not growing while
operators work: there is no metric for it and "operators are working" is not a signal this
service has. And there is no Grafana dashboard, because nothing in this repository runs
Grafana — the metric-name test would cover one the day one exists, which is the condition
for adding it.

**What this found:** a refusal at the HTTP edge emitted no metric at all. The daily token
budget (503) and both rate limiters (429) refuse before `chat.Service.Turn` runs, so
`chat_turns_total` never moved — the service could be refusing every customer it had while
every meter stayed flat and green, with a `slog.Warn` as the only record. It was the
largest hole on the page, it needed instrumentation at the edge rather than another rule
over the series that already existed, and it was closed the next day as the first half of
item 14: `chat_edge_refusals_total{reason}`, three more alerts, and fourteen rules where
there were eleven.

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

**Done, 2026-09-07.** `CHAT_FALLBACK_PROVIDER`.

Anthropic, OpenAI and xAI were all implemented and verified live, and `CHAT_PROVIDER`
picked one at start-up. If that provider was down, the service was down — and a replica
count does not help, because every pod calls the same API.

`llm.Failover` is a `Client` wrapping two `Client`s, so nothing above it changed: the tool
loop, the meters, the budget and the spans still talk to one client and still see one
`Result` per `Stream` call. The secondary is configured in full and separately — its own
key, model and base URL — and two things are start-up failures rather than warnings: a
fallback whose key is missing, and a fallback naming the same provider as the primary.
Both are only ever exercised while something else is already broken.

The list of failures worth a second provider, the reasoning for each, and the two
questions below are in [Chat providers](providers.md#failover-a-second-provider-and-when-it-is-used).
The short version is that the default is **no**: 429 and 5xx after the client's own
retries, a transport failure, and a stall are the exceptions. A customer's cancellation is
not one, a 401 is not one, and neither is anything at all once a token has reached the
customer.

**The two questions this item said were part of the work, answered.**

*What happens to the voice.* A turn picks a provider at its first model call and keeps it.
The switch happens before the first byte reaches the customer or not at all — `onText` is
watched and a single forwarded token settles it — and a tool round never switches, because
its request carries the assistant turn the primary produced, including tool-call ids and
the thinking blocks Anthropic requires echoed back unchanged. The cost is stated rather
than hidden: a provider that dies half-way through an answer produces a failed turn with a
working provider sitting right there. One visibly failed answer beats an answer in two
voices, because the failure is legible and the seam is not. Restarting the turn at the
secondary is the other defensible choice and is not built.

*What happens to the money already spent.* The abandoned attempt has usually been billed —
Anthropic reports the input count at `message_start`, before a single token, which is
exactly the window a failover acts in. Both attempts' tokens therefore come back in one
`Result`, because `chat.Service` sums `Result.Usage` and that is the only route to the
budget. What that costs is attribution: one `Result` carries one model id, so the primary's
tokens land in `chat_tokens_total` under the fallback's model and at its prices.
`chat_failover_abandoned_tokens_total{provider,model,type}` records the same tokens at the
model that really billed them, so the skew is a number rather than an assumption, and
`chat_provider_failovers_total{from,to,reason}` counts the failovers themselves — a
failover is otherwise a *silent success*, and the first sign of an outage would be an
invoice from a provider nobody meant to use.

**How it was checked.** Every rule is a test in `internal/llm/failover_test.go`, driving
the two real clients against two `httptest` providers rather than a stub of `llm.Client` —
the decision to spend money at a second provider is made *from* the error the first one
returned, and a stub choosing that error would be choosing the answer. Twenty-four
perturbations of the source were each observed red on the test that should catch them,
including one that only reorders two guards: moving the caller-context check after the
error check makes every timed-out turn call a second provider, and the test for that
ordering exists because the first version of the cancellation test could not see the guard
at all. That first version was left in place with a comment saying what it does and does
not prove.

**Closed at the merge, having been flagged rather than carried.** An alert
(`RunningOnTheFallbackProvider`) now names `chat_provider_failovers_total`, seen firing on
a fifth of turns failing over and seen *not* firing on one in a hundred;
`k8s/examples/secret.yaml` names the fallback provider's key, which `envFrom` carries into
the pod; and both READMEs' opening paragraph says a fallback exists. A capability that only
the docs behind it know about is the failure mode CLAUDE.md names in *the front door does
not get re-read*.

**Not done, and known.** `chat_model_calls_total` counts one call per `Stream`, so it
undercounts by one per failover — provider calls attempted is that plus
`chat_provider_failovers_total`. Metering each attempt separately needs a change in
`internal/chat`, which this work did not make. There is no health check and no circuit
breaker, so every turn pays the primary's failure latency before falling over. And nothing
has been exercised against two real providers in a real outage: the 529, the truncated
stream and the stall are all `httptest`.

### 12. One corpus, one config, one price list

Everything is single-tenant: one FAQ, one system prompt, one budget, one set of prices,
one set of operator tokens. Nothing is keyed by a tenant.

**Done looks like:** a tenant column on the tables that need one, retrieval filtered by it,
per-tenant budgets and prices, and operator tokens scoped to a tenant. The audit trail
becomes more important here, not less.

**The Java implementation is building this**, which settles the question this item used to
turn on. It was going to be argued against here — multi-tenancy is a product decision rather
than an engineering one, and a single-brand service may never need it — but that argument
dies the moment the pair diverges: the comparison is the point of these two repositories,
and one of them having tenants and the other not is a bigger loss than the work.

So it will be built, following their shape rather than inventing a second one. Their design
has been asked for; adopting it is what made the corpus-versioning work comparable and it
answered an objection that had been written into this very item.

**Two things about it are already visible from here.** `corpus_active` has a primary key on
a constant, so exactly one version can be active — which is precisely the shape that does
not survive tenancy. And `admin_audit` has no tenant column, so it needs a migration on the
one table that must never lose a row.

**Ordered behind item 9.** This is the largest schema change on the list, and doing it with
`CREATE TABLE IF NOT EXISTS` and no migration tool is how the first `ALTER` on a live
database becomes a hand-run statement and a hope.

### The shape to follow, from the Java side's ADR 002

Written down here rather than left in a conversation, because it is the design this
repository will implement and the ADR lives in the other one (`ai-customer-service-java`
PR #50, not yet merged).

| Decision | The shape |
| --- | --- |
| Where the tenant comes from | An **API key per tenant** on `/api/v1/**`, resolved before anything else into a request context. Not a subdomain and not a claim: there is no host identity to assert one. Keys stored as SHA-256 with a short `key_id` prefix for lookup, shown once at issue. No key, revoked key, disabled tenant → 401 **before any model call**. |
| For this repository specifically | **The tenant is resolved before the session, and a session belongs to a tenant.** `internal/identity` issues anonymous sessions today; a tenant becomes the thing a session hangs off rather than a property discovered later. |
| Where it lives | One database, `tenant_id` on every row that belongs to a customer, one `faq_document` table and one HNSW graph filtered on **tenant *and* corpus version**. Schema- and database-per-tenant deferred with a named reopening condition: a regulator, or a tenant big enough to need its own index. |
| Enforcement | A predicate every query carries, plus an isolation test per module — write as A, read as B, find nothing — and a check that no store method takes a conversation id without a tenant. They suggest **RLS as a second guard on this side**, which with a pool means `SET LOCAL app.tenant` per *transaction*, not per connection. Same columns either way, so the two stay comparable. |
| Per tenant | Conversations, turns and their retrieval/tool rows, tickets, feedback, knowledge entries and versions and the active pointer, staff accounts, audit rows, sessions. |
| Global | The ticket number sequence (a number is not a secret), prices, the model provider, the bundled-corpus import record, the per-conversation token budget. Per-tenant budgets and providers are deferred to billing. |
| Metrics | A **bounded** `tenant` label, dropped above a configured count. Unbounded cardinality is the rule this repository already has. |
| Operators | One tenant per staff account and no account sees two, plus a tenant-less `platform` role that creates tenants and issues keys **and sees no customer content**. Usernames stay globally unique, because they are the audit actor. |
| `admin_audit` | Gains `tenant_id` by a migration that backfills `default`. Append-only, so the migration only adds a column — the one table that must never lose a row. |
| The corpus | `corpus_active` becomes **one row per tenant**, `tenant_id` as the primary key, replacing the primary key on a constant. `Activate` keeps its expected-version check on the tenant's row, so the shape survives with the key changed. Version ids stay globally unique; retention becomes "newest N **per tenant**", because today's global N would retire a neighbour's. |
| The bundled corpus | Belongs to the **`default` tenant** and is not a template. `corpus/faq.json` stays byte-identical, bootstrap adopts it as that tenant's first version without re-embedding, and the parity fixtures run as the default tenant. A new tenant starts with nothing and gets grounded refusals. |
| The migration | Create `default`; add the columns **with a default of `default`**; backfill; then **drop the column defaults**, so a row without a tenant becomes a bug rather than a silent orphan. |

Two things this settles that were open here. `corpus_active`'s primary key on a constant —
flagged above as the shape that does not survive tenancy — becomes a primary key on
`tenant_id`, which is a smaller change than it looked. And the property to protect is the
same one as before: the import path and the file untouched, with the default tenant owning
what the import produces.

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

**Partly done, 2026-09-07.** [The reasoning, in full](safety.md).

Of the four gaps this item named, one is built (a per-customer abuse signal), one is half
built and half argued (refusal logging: the service refusing is now counted, the assistant
declining is not, and the document says why approximating it would be worse than the gap),
and two are decisions written down rather than features — moderation with its argument,
jailbreak detection with the control that would actually bound it.

**A refusal at the HTTP edge is counted.** It was the hole item 8 found: the daily budget
(503), both rate limiters (429), a missing session (401) and somebody else's conversation
(404) all answer before `chat.Service.Turn` runs, so `chat_turns_total` never moved for any
of them. `chat_edge_refusals_total{reason}` now does — four reasons, no other labels,
because the subject and the conversation id are unbounded and a refusal counter is exactly
where an attacker chooses the label values. Three alerts sit on it, including one that
fires on a *single* `daily_budget` refusal: the budget is service-wide, so one refusal
means everybody is refused until midnight UTC. Each of the four increments was forced red
on its own, and so was the mistake of counting one refusal under two spellings.

**A per-subject abuse signal, out of counting that already happens.** The rate limiter
writes a row per (bucket, subject, window) to decide whether to refuse; that table already
knows who was over the limit and in how many separate minutes.
`chat_rate_limited_subjects` is a `GROUP BY` over it, sampled once a minute — no new table,
no write on the hot path. It answers what a per-minute limit cannot: a hundred customers
refused once and one client refused a hundred times are the same number of 429s. The
subject ids go to a log line and never to a label, and the signal scores nobody and bans
nobody — a repeat offender is a scraper, a retry loop, a shared NAT or a limit set too low,
and those need a person.

**Refusal recording, in the sense of the assistant declining, is not built and should not
be approximated.** Whether a reply says "I don't know, shall I fetch a human" is a property
of text; classifying text takes a model or a phrase list, and
[docs/evaluation.md](evaluation.md) records what phrase lists have cost this repository
three times — an assertion that measured any sentence with a clock in it, a list of five
ways to say "not included" that the model beat twice, and a regex that measured Chinese
punctuation. The observable half is the *action*: `chat_tool_invocations_total{tool="create_support_ticket"}`
is the model escalating, and it has been counted since the handoff loop was built. It
undercounts by an unknown amount, which is said in the document rather than papered over.
The honest fix is an offline judge over sampled `turn` rows — the question, reply and
passages are already stored — and it waits on there being real traffic to sample.

**Moderation was decided against, with the argument written down.** Two extra API calls per
turn against a second vendor; output moderation is incompatible with streaming unless the
whole reply is buffered, and the demo page's own numbers (first word at 3629 ms) are what
that would cost. It would have to fail open, which makes it a filter for accidents rather
than a boundary. And the harms this design actually has — an instruction hidden in an
edited knowledge entry, and money — are not the ones a moderation API is trained on; the
budgets and limits bound the second, and the first is
[argued rather than evidenced](knowledge.md). `docs/safety.md` names what would change the
decision (content flowing between customers, file input, a tool a third party reads, a
compliance requirement) and the terms it would be built on if it is: off by default, fail
open *with a counter*, moderate the input before the call and the stored reply after, and
publish the measured latency.

**Still open in this item:** jailbreak detection, which is the same classification problem
against an adversary and which no phrase list bounds — what would actually bound it is
constraining tool calls by the caller's identity, and `lookup_order_status` still takes any
order number from any session (item 4 meets item 14 there).

### 15. A turn a dead process left behind stays in_flight for ever

`Recorder.Begin` writes the turn as `in_flight` before the model call and `Finish` closes
it afterwards. A process that dies in between leaves the row `in_flight` permanently:
nothing sweeps it, and the operations overview counts it under "not completed" as though
the customer had abandoned the conversation. A crash and a closed tab are exactly the two
things that record is supposed to tell apart.

**Done, 2026-09-07.** `Recorder.Sweep` marks a turn `unknown` once it has been in flight
past `TURN_LEASE` — never `failed`, because a failure is something this service observed and
described, and this is the absence of an observation; never `completed`, for the obvious
reason.

The race the item warned about is answered by a relation rather than a large number, which
is the Java side's answer too: **`TURN_LEASE` must exceed `HTTP_READ_TIMEOUT`**, and the
server refuses to start otherwise. A turn still running past its own request timeout has
already lost its client, so anything older than the lease is abandoned rather than slow. A
start-up error rather than a clamp, because a clamp is a value nobody chose taking effect
silently.

Forced red four ways: sweeping without regard to the lease took a one-second-old turn; the
outcome as `completed`; a zero lease sweeping everything instead of nothing; and the
config relation removed, which accepted a 30-second lease against a 120-second timeout.

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
