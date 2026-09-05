# The operations surface


The operations surface is where people who answer for this service look at what it did:
the conversations, the tickets the assistant raised, and a record of who looked.

It is **two applications**. `admin-ui/` is a React and TypeScript bundle served by nginx
from its own image; the Go service exposes `/api/admin/v1/*` and no page at all. They meet
across an origin, which turns two things that were implicit into configuration you can get
wrong: an allowlist saying which origin may read these responses, and a contract in two
languages that nothing re-derives.

It exists because the Java implementation of this system built one and this repository
was asked to match it. It was argued against first, and the argument is answered rather
than dropped — which is most of what this page is about.

## It does not exist unless you configure it

Everything else in this service works to keep customer text out of the places it leaks
to: no query content on vector-store spans, no customer words in any span attribute, a
tool name validated before it can become a metric label, and a
[trace backend grepped for a credit card number](observability.md#the-customers-words-are-not-in-the-trace-and-that-was-checked-rather-than-assumed)
to prove it.

This page displays that text on purpose. An agent working a ticket has to read what the
customer wrote. So it is the one surface where all of that care is concentrated in a
single place, and whatever protects it protects everything the tracing work protected.

With `ADMIN_TOKENS` unset, the routes are **never registered**. `/api/admin/v1/*` returns
404 the way any unknown path does — not 401 from a guard. A guard is a thing that can be
misconfigured. An absent route cannot be.

The kind harness used to assert this against `/admin/`. That assertion is gone, because
`/admin/` is now 404 whatever the configuration says — it would have gone on passing while
checking nothing, which is the failure this repository keeps finding in its own checks.

```bash
ADMIN_TOKENS="alex:$(openssl rand -hex 24):operator,dana:$(openssl rand -hex 24)"
```

| Decision | Reason |
| --- | --- |
| Tokens carry an operator name | An audit trail whose every entry says "admin" records *when* something happened and not *who* did it, which is most of the point of having one. |
| Two roles, `viewer` and `operator` | This release has two kinds of action. A permission model with more entries than the actions it governs is a design document, not a control. |
| An omitted role means `viewer` | Least privilege is the safe direction for a typo. |
| Tokens shorter than 16 characters are refused at startup | This is the credential for every customer conversation in the database. |
| Comparison is constant-time, across every configured token | The time a rejection takes should not say which operator exists or how much of a guess was right. |
| Permission is checked on the server for every request | Hiding a button is a user-interface decision. |

## Two origins, and what the browser decides

The UI runs on its own origin, so every request it makes is cross-origin and the browser —
not the server — decides whether the page may read the response. `ADMIN_CORS_ORIGINS` is
that decision, and it is a security control rather than plumbing: what it permits is other
pages reading the support inbox.

| Rule | Why it is that way |
| --- | --- |
| No wildcard is reachable from configuration | `*` is the value that makes the error disappear in development and reappear as "any page can read the conversations". `ADMIN_CORS_ORIGINS=*` is treated as an origin literally named `*`, which nothing is. |
| Origins match whole, never by prefix | A prefix match on `https://ops.example.com` also accepts `https://ops.example.com.evil.test`, a domain anyone can register. |
| `Vary: Origin` on every response that could have carried the header | Without it a shared cache can hand one origin's allowed response to another — the same hole, arrived at by accident. |
| CORS wraps authentication, not the reverse | A browser sends no `Authorization` header on a preflight. Authenticating first rejects every cross-origin request before the real one is sent, and the page sees an opaque network error with nothing in the server log. |
| Every route has an `OPTIONS` twin | Go's mux matches on method, so a preflight without one is a 405 that the browser reports as a CORS failure — the least informative error in the browser. |
| Empty means no CORS at all | Correct behind a reverse proxy that serves both from one origin, and the reason the empty value is not a permissive default. |

All six were forced red before being trusted: a prefix match let the lookalike through,
authenticating first made the preflight a 401, dropping `Vary` went unnoticed by everything
else, and honouring a literal `*` let an arbitrary origin read a response.

**The dev server deliberately does not proxy the API.** A proxy makes development
same-origin and production cross-origin, and that difference is exactly what hides a CORS
mistake until the day it ships. `VITE_API_BASE` points the dev server at the real API, so
the preflight, the allowlist and the `Vary` header are exercised from the first minute.

## Two copies of the same contract

Splitting the UI out gave it its own copy of two things the server also knows, in a
language the server's compiler never sees. Both are checked by Go tests that read the
TypeScript:

- **The ticket state machine.** `NEXT_STATES` in `admin-ui/src/api/types.ts` against
  `allowedTransitions` in `internal/ticket/admin.go`. Drift in one direction offers a move
  the server answers with 422 — annoying, but visible. Drift in the other hides a legal
  move, and nobody reports a button that was never there.
- **The markup rule.** React escapes by default, so the only way to put a model's words
  into the DOM as markup is to ask for it by name. A test walks every `.ts` and `.tsx` for
  `dangerouslySetInnerHTML` and friends, and for `localStorage`, since the token reads
  every conversation in the database and `localStorage` outlives the tab.

The second test's first run failed on the *comment* in `Markdown.tsx` that explains why the
prop is never used — the third detector in this repository to measure the prose instead of
the code. It matches a use now.

## Reading is an action

Opening a conversation writes an audit entry. Who looked is most of what an audit trail
is for here, because looking is the sensitive operation on this surface — the writes are
about tickets and the reads are about people.

**Walking the workflow live is what found the hole in that.** A viewer refused a `PATCH`
got a clean `403` and left no trace whatsoever: the deny path returned before anything
recorded it, so the audit trail contained every action that succeeded and none that were
refused. A rejected attempt is exactly what an audit trail is for. It is recorded now,
with the actor, the path and the role that was insufficient.

Unauthenticated requests are logged rather than audited — there is no operator to
attribute them to, and a table of "who did what" should not fill with rows saying nobody.

There is deliberately no endpoint that edits or deletes the audit table, and a test
asserts that every mutating method against it is refused.

## Tickets became real before the page existed

Tickets used to live in a map in each process. The three-per-conversation cap was
therefore `replicas × 3`, and deduplication held only within whichever replica served the
request. Both were written down as known limits, which was honest while nobody could see
them — an operations page is exactly the thing that shows two operators two different sets
of tickets and lets neither of them be wrong.

They are in Postgres now, with the cap and the deduplication enforced by a transaction
holding an advisory lock and backed by a unique index.
`TestTheCapHoldsAcrossReplicas` runs twenty differently-worded requests through two pools
and gets three tickets; without the lock it gets seventeen.

The state machine is small: `OPEN → IN_PROGRESS → RESOLVED → CLOSED`, with reopening
allowed from `RESOLVED` and `CLOSED`. Resolving requires a conclusion and reopening
requires a reason, because a `RESOLVED` ticket with no record of what was done is
indistinguishable from an abandoned one, and a ticket that comes back is the interesting
case.

Every mutation takes an `expectedVersion` — required, never defaulted. Two operators with
the same ticket open otherwise overwrite each other and the loser is told nothing. A
conflict is a `409` so the page can say "somebody changed this, refresh"; a rejected
transition is a `422`, because that is the operator's mistake rather than the server's.

## The turn record is not the chat memory

`chat_memory` is the model's context: windowed at 40 messages, holding what the model
needs to see next time. A history that disappears when the window slides is not a history,
and it cannot answer what an operator is actually asked — did this fail or did the
customer close the tab, what did retrieval return, what did it cost.

So there is one `turn` row per turn, with its retrieval evidence and its tool calls. The
passages are kept because the corpus can change and *why did it answer that* is
unanswerable from a corpus that has since moved.

It is written where the turn executes rather than from the event stream that feeds the
browser, because that stream feeds a page which may already be gone. Two boundaries,
deliberately asymmetric:

- The **opening** record is written before the model is called, and its failure fails the
  turn. A model call this service cannot account for is worse than a turn that did not
  happen; the alternative is finding the gap a month later, on a bill.
- The **closing** record runs in the same deferred block that persists the partial reply,
  on a context detached from the request, and its failure is logged rather than raised.
  By then the money is spent and the customer has their answer.

**The test for the disconnect case found a real defect.** A client that goes away while
the history read is in flight made that read return `context.Canceled`, and the turn was
recorded as `memory_failed` — the record said the database broke, which is the single
question it exists to answer correctly. Cancellation is now classified before the step
that noticed it, on every early return.

## Cost is labelled an estimate, and says when it is incomplete

The overview totals tokens and cost. A turn on a model with no price entry contributes its
tokens and no cost, so the overview reports how many such turns there were rather than
quietly omitting them. A total that silently drops some rows is worse than one that says
how many it dropped.

## Not built: knowledge editing and publication

The largest part of the Java proposal, and deferred rather than forgotten.

It changes the corpus, and the corpus is the one fixture that makes every retrieval number
in this pair comparable — that repository's own proposal says not to change the bundled
corpus on either side as part of admin development. Doing it properly needs a versioned
index, an atomic switch that live retrieval filters on, and a rollback that accounts for
in-flight requests. That is its own piece of work with its own way of going wrong, and
half of it would be worse than none: a Publish button wired to the startup importer is
exactly the shape that looks finished and is not.

## Where the two implementations diverge, and where one of them is better

The Java implementation built its operations surface at the same time, and the two are not
the same design. Some of that is taste. One of it is not.

| | Go | Java |
| --- | --- | --- |
| States | `OPEN → IN_PROGRESS → RESOLVED → CLOSED`, reopen from either terminal state | `open → claimed → resolved → closed`, plus release back to `open`; reopen clears the owner |
| Concurrency | version on every mutation, `409` on stale, `SELECT ... FOR UPDATE` inside the transaction | the same, with claim made atomic across replicas by row lock |
| Where the conclusion lives | a `resolution` column on `support_ticket` | on the resolving `ticket_event`, never on the row |
| Opening a conversation | writes an audit row | not audited today |
| A refused action | writes an audit row | writes nothing today |

The third row is the one worth reading twice. Storing the conclusion on the event rather
than on the ticket means **a reopen has nothing to carry forward** — the defect this page's
browser run found is not a bug you can fix there, it is a state you cannot construct. It
also keeps every conclusion a ticket ever had, in order, instead of the most recent one.

Here the column survives because the ticket dialog fills its resolution box from it, and
that is the whole of what reads it: the list does not show it, and nothing else in the
service touches it. So this side has a denormalised copy of one event's text, kept correct
by a test that was written after the copy went wrong once. That is a weaker guarantee than
not having the copy, and it is recorded here rather than quietly evened out, because a pair
of implementations that converge on every decision stops being able to show anything.

## Two defects that only a browser could find

The page was driven in a real Chrome on 2026-09-05, signed in as an operator, through
overview, conversations, a conversation, tickets, a ticket dialog and the audit tab. No
console error, no failed request, and the run recorded its own `read conversation` row —
which the audit tab then displayed, three rows above the refusal from the `curl` walk.

It found two things that no test here could have.

The first is the demo page's defect, in a place where it matters more. The reply showed the
customer a ticket number as `**TKT-4700**` — literal asterisks, because the operations page
was appending the model's text as a plain string. An operator reading a complaint is reading
it to see what the customer saw, and the customer saw bold. So this page renders the same
deliberately small markdown subset the demo page does, by the same means: DOM nodes only, no
`innerHTML`, and no links, because `[text](javascript:...)` is the one markdown construct
that does something rather than looks like something. The renderer is duplicated rather than
shared — the two pages are embedded assets of different packages — and
`TestBothPagesRenderMarkdownIdentically` compares the two sources, because the failure mode
of a copy is that only one copy ever gets fixed.

The second was visible only because the dialog was open on screen. The resolution box is
filled from the row, so a `RESOLVED` ticket shows its conclusion — and an operator reopening
that ticket resubmits that text without touching it. The store then kept the conclusion
across the reopen, leaving a ticket that was `IN_PROGRESS` and also claimed to have been
concluded. A reopen disputes the conclusion; the row now stops asserting one, and nothing is
lost, because the state change that resolved it carries the text in the history.

Both are the same shape as the finding this repository has now recorded five times: the data
was correct at every seam a test can reach, and the defect lived in what a person would see.

Both survive the rewrite. The React application renders the same markdown subset, refuses
links for the same reason, and its resolution box is deliberately **not** filled from the
row — the conclusion a ticket already has is in the history below it, where resubmitting it
by accident is not possible.

## What the container found, which a laptop would not have

Two more, from the same habit applied one layer down.

**Writing `config.js` into the web root fails as a non-root user**, and would fail again
under Kubernetes' read-only root filesystem. The API's origin is written at container
start-up rather than baked in by `vite build`, because a build-time value means a rebuilt
image per environment — and then the image that was tested is not the image that ships. It
is written under `/tmp` and served by an `alias`, so the same file works on a laptop, in
Compose, and in a pod that cannot write to its own image.

**nginx does not inherit `add_header` into a location that sets one of its own.** The
`Content-Security-Policy` was declared once at server level and was silently absent from
`GET /` — the only response where it matters, because the location for `index.html` sets a
`Cache-Control` and that is enough to drop every inherited header. The config read
correctly. `curl -I` on each path did not, and the headers are an `include` now.

## Verified across two origins, in a real browser

Driven in Chrome on 2026-09-06 with the UI on `localhost:8090` and the API on
`localhost:8082`, against a live model that raised `TKT-4701` from a Chinese complaint:
the reply rendered with its ticket number in bold and no literal asterisks; the resolution
box came up empty; a claim moved the ticket to `IN_PROGRESS` and wrote its audit row; and
signing in with a `viewer` token produced a dialog with the history and no Save button.
No console error, no failed request — which is also how the CORS configuration was checked,
because a browser is the only thing that enforces it.

**Still not verified:** one browser, one operator at a time. Nothing has been driven at a
width where a table needs to scroll, and no second operator has had the same ticket open in
another window — the 409 path is covered by tests and by `curl`, not by two people.

---

[← Back to the README](../README.md)
