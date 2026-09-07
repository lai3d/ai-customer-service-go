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
| 7 | [Feedback from customers and operators](#7-nothing-comes-back) | week 2 | both | not started | 2–3 h |
| 8 | [Alerting and an SLO](#8-there-are-metrics-and-nothing-watches-them) | week 2 | Go | **done** 2026-09-07 | 2 h |
| 9 | [A schema migration path](#9-the-first-change-to-a-live-schema-is-manual) | week 2 | Go | not started | 1–2 h |
| 10 | [The admin list pages lie past one page](#10-the-admin-lists-lie-past-the-first-page) | week 2 | Go | **done** 2026-09-06 | 0.5 h |
| 11 | [Provider failover](#11-three-providers-are-supported-and-one-runs) | scale | both | **done** 2026-09-07 | 1–2 h |
| 12 | [Multi-tenancy](#12-one-corpus-one-config-one-price-list) | scale | both | not started | 4–6 h |
| 13 | [The deployment is a demo deployment](#13-the-manifests-stop-where-a-real-cluster-starts) | scale | Go | not started | 2–3 h |
| 14 | [Abuse and content safety](#14-the-system-prompt-is-the-whole-of-the-safety-story) | scale | both | not started | 2–3 h |
| 15 | [A turn a dead process left behind stays in_flight for ever](#15-a-turn-a-dead-process-left-behind-stays-in_flight-for-ever) | week 2 | Go | not started | 1 h |

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

**What this found:** a refusal at the HTTP edge emits no metric at all. The daily token
budget (503) and both rate limiters (429) refuse before `chat.Service.Turn` runs, so
`chat_turns_total` never moves — the service can be refusing every customer it has while
every meter stays flat and green. There is a `slog.Warn` and nothing else. It is the
largest hole on the page and it needs instrumentation at the edge rather than another rule
over the series that already exist.

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
