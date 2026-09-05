# The operations surface


`/admin` is where people who answer for this service look at what it did: the
conversations, the tickets the assistant raised, and a record of who looked.

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

With `ADMIN_TOKENS` unset, the routes are **never registered**. `/admin` and
`/api/admin/v1/*` return 404 the way any unknown path does — not 401 from a guard. A guard
is a thing that can be misconfigured. An absent route cannot be.

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

**Still not verified:** one operator, one conversation, one ticket, one browser. Nothing has
been driven at a width where a table needs to scroll, and no second operator has had the same
ticket open in another window — the conflict path is covered by tests and by `curl`, not by
two people.

---

[← Back to the README](../README.md)
