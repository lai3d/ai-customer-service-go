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
| 1 | [Identity, session ownership, rate limiting, global budget](#1-anyone-can-read-anyone-elses-conversation) | launch | both | not started | 3–4 h |
| 2 | [Knowledge as a knowledge base, not a fixture](#2-the-corpus-is-a-test-fixture) | launch | both | not started | 4–6 h |
| 3 | [The loop back to a human](#3-a-ticket-is-a-row-and-nothing-else-happens) | launch | both | not started | 3–5 h |
| 4 | [Real tools instead of the mock](#4-the-tools-are-fiction) | week 1 | both | not started | 2–3 h |
| 5 | [Retention and deletion of customer data](#5-there-is-no-way-to-delete-a-customer) | week 1 | both | not started | 2–3 h |
| 6 | [An answer-quality regression set](#6-nothing-tells-you-a-prompt-change-made-it-worse) | week 1 | both | not started | 3–4 h |
| 7 | [Feedback from customers and operators](#7-nothing-comes-back) | week 2 | both | not started | 2–3 h |
| 8 | [Alerting and an SLO](#8-there-are-metrics-and-nothing-watches-them) | week 2 | Go | not started | 2 h |
| 9 | [A schema migration path](#9-the-first-change-to-a-live-schema-is-manual) | week 2 | Go | not started | 1–2 h |
| 10 | [The admin list pages lie past one page](#10-the-admin-lists-lie-past-the-first-page) | week 2 | Go | not started | 0.5 h |
| 11 | [Provider failover](#11-three-providers-are-supported-and-one-runs) | scale | both | not started | 1–2 h |
| 12 | [Multi-tenancy](#12-one-corpus-one-config-one-price-list) | scale | both | not started | 4–6 h |
| 13 | [The deployment is a demo deployment](#13-the-manifests-stop-where-a-real-cluster-starts) | scale | Go | not started | 2–3 h |
| 14 | [Abuse and content safety](#14-the-system-prompt-is-the-whole-of-the-safety-story) | scale | both | not started | 2–3 h |

Estimates are Claude session hours: the work, not the calendar. Things only you can do —
registering with providers, deciding a retention period with whoever owns that decision,
getting access to the real order system — are called out per item and are not in the
numbers.

---

## Launch blockers

### 1. Anyone can read anyone else's conversation

`POST /api/v1/chat` has no authentication, and `conversationId` comes from the client with
only a length check (`internal/httpapi/api.go`). Chat memory is keyed on that id. So
sending a message with somebody else's conversation id appends to *their* history, and the
model's reply is composed with their context in the prompt.

That is a confidentiality break, not a missing feature. The absence of authentication is
documented as deliberate scope, and it is the right scope for a comparison repository —
but it is the first thing that stops being acceptable when a real customer types into it.

The budget makes the second half worse rather than better: `CONVERSATION_TOKEN_BUDGET` is
per conversation, and conversation ids are free, so anyone who wants to spend your model
budget rotates the id. Nothing rate-limits anything.

**Done looks like:** the server issues the conversation id and binds it to an
authenticated subject; a request carrying an id that is not the subject's is a 404, not a
403 (a 403 confirms the id exists); rate limits per subject and per IP; a global daily
spend ceiling that trips a circuit breaker rather than a page in a dashboard.

**Watch for:** the id is currently echoed in `X-Conversation-Id` and used by the demo page
across turns — whatever replaces it has to survive a page reload, and "survives a reload"
is where people accidentally put a bearer token in `localStorage`.

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

**Done looks like:** knowledge entries in Postgres with versions; an editor in the
operations UI; a publication step that embeds a new index and switches atomically, with
in-flight requests finishing against the old one; a rollback; and retrieval filtering on
the published version. Plus: tool calls that a retrieved passage can influence are
constrained by the caller's identity rather than by the model's judgement.

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

`admin-ui` fetches 100 conversations (200 tickets) and paginates them in the browser. The
API returns `total`, and the UI never passes it to the table, so Ant Design derives the
total from the rows it has. Past one page, the footer states a number that is wrong and
the remaining rows are gone with no indication.

**Done looks like:** page and page size drive `limit`/`offset`, the response's `total`
drives the footer, and a test with more rows than one page asserts the displayed total
equals the API's rather than the page length. The ticket filters the API already supports
(assignee, conversation) and the overview window (the API takes 1 hour to 90 days; the UI
hard-codes 168) come along with it.

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
