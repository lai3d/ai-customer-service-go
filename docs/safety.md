# Safety, abuse, and the parts deliberately not built

Until this document existed, the system prompt was the whole of the safety story:
`internal/chat/service.go` asks the model to ground every factual claim, to treat
retrieved passages and tool results as data rather than instructions, and to escalate
rather than guess. All three are requests. Grounding holds in practice because the corpus
is small and closed and the two tools are narrow — and neither of those is a control.

This is what was added, what was measured, and — the longer half — what was decided
against and why.

## What a refusal is, and which half of it is countable

Two different things get called a refusal, and only one of them is observable.

**The service refusing** is a fact about a request: a 429 from a rate limiter, a 503 when
the day's token budget is spent, a 401 with no session, a 404 for somebody else's
conversation. Those are decisions this code makes, and until this item they emitted no
metric at all — the service could refuse every customer it had while `chat_turns_total`
stayed flat and every dashboard read green, because all four answer before
`chat.Service.Turn` runs. That is now `chat_edge_refusals_total{reason}`, with four
reasons and no other labels; the reasoning, the alerts and the red tests are in
[Observability](observability.md#refused-before-a-turn-ever-started).

**The assistant declining** is a property of an answer: *"I don't have information about
that, but I can put you through to a human."* It is what the prompt asks for when there is
no grounding, and its rate is worth watching — a jump means retrieval broke, or the corpus
lost an entry people ask about, long before anybody complains.

**It is not detectable from the reply without a second model call, and this repository
will not approximate it with a phrase list.** Deciding whether a sentence declines is
classifying free text in two languages, and the two ways to classify free text are a
model and a list of phrases. [The evaluation notes](evaluation.md) record what the list
costs here: `ungrounded-store` measured any sentence with a clock in it rather than the
fabrication, `international-duties` listed five ways of saying "not included" and the
model found a sixth, then listed ten and it found an eleventh, and a regex for
run-together sentences turned out to be measuring Chinese punctuation. Three separate
detectors in this repository have measured a surface that usually accompanies the thing
instead of the thing. A `refusals_total` counter incremented by matching *"I don't have"*
would be the fourth, and it would be worse than the others because it would look like an
operational metric rather than a heuristic.

What *is* observable without another model call is the action the model takes when it
declines:

```
chat_tool_invocations_total{tool="create_support_ticket"}   the model escalated
chat_turns_total{outcome="tool_limit"}                      it gave up mid-tool-loop
chat_turns_total{outcome="budget_exceeded"}                 the conversation's ceiling
```

An escalation is a refusal the model did something about, and it has been counted since
the handoff loop was built. **It undercounts, and by an unknown amount**: an assistant
that says "I don't know" and stops has refused and called nothing. That gap is named here
rather than filled, because filling it wrongly is how the three failures above happened.

The shape that would close it honestly is an **offline judge over sampled turn records** —
the `turn` table already stores the question, the reply, the passages and the outcome, so a
sample can be graded after the fact, on a schedule, by a model, without adding a call to
the hot path or a vendor to the request path. That is the same machinery
[`make eval`](evaluation.md) already uses against fixed cases, pointed at real traffic
instead. It is not built: there is no real traffic behind this repository to sample.

## A per-subject abuse signal, from counting that already happens

`internal/identity` writes one row per (bucket, subject, window) to decide whether to
refuse a request. That table already knows which subject was over its limit and in how
many separate minutes, so "is this one client, again?" is a `GROUP BY` over rows that
exist rather than a new thing to record — no table, no write on the hot path, no second
store. `internal/identity/abuse.go` is ninety lines of code and that is the whole of it.

```
chat_rate_limited_subjects   subjects over TURNS_PER_MINUTE in >= 3 separate windows
                             of the last hour, sampled once a minute
```

The question it answers is the one a per-minute limit cannot: **a hundred customers
refused once and one client refused a hundred times produce the same number of 429s**, and
only one of them is worth waking up for. `RepeatedlyRateLimitedSubjects` alerts on more
than two of them sustained for half an hour.

Four things about it are deliberate:

- **The subject id is in a log line and never in a label.** Subjects are unbounded, and an
  unbounded label value takes a metrics backend down long before the bill does — the same
  rule that keeps conversation ids out of every other metric here. The gauge carries the
  count; `slog.Warn` carries up to five ids for whoever goes looking.
- **`count > limit` is the same comparison `Allow` makes.** A row is over the limit here
  exactly when the request that wrote it was refused; reconstructing the threshold as
  `>= limit` would count subjects that never saw a 429 and quietly disagree with what
  customers experienced.
- **Three windows, not one.** One window over the limit is a burst, which is precisely what
  a per-minute limit is for and what it handled. Three separate minutes is a client that
  did not back off.
- **It scores nobody and bans nobody.** A repeat offender here is a scraper, a retry loop,
  a shared NAT, or a limit set below what the product legitimately does. Those are four
  different things that need a person to tell apart.

The gauge is per replica against the same rows, so an alert takes `max()` and not `sum()`.
On a query failure the sampler leaves the last reading and logs an error rather than
writing zero: zero is a measurement — *nobody is being throttled* — and publishing it
because a query failed is the one available answer that says stop looking.

**One adjacent gap, found while writing this and not fixed here:** `Limits.SweepWindows`
and `Sessions.Sweep` exist and nothing calls them. `rate_window` and `chat_session`
therefore grow for ever in a long-lived deployment. It does not affect this signal — the
query is bounded by an indexed `window_start` — but it is the same shape as the bounded
LRUs that `CLAUDE.md` insists on, in Postgres instead of in a map.

## Content moderation: decided against, for now

Not built, and this is the argument rather than an omission.

**What it would cost.** A moderation pass on input and output is two more API calls per
turn against a second vendor. The input call is on the critical path before the model call,
so it is added latency on every turn. The output call is worse than that: this service
**streams**, and the demo page has the numbers — the first word reaches the customer at
3629 ms and the answer keeps arriving until 5065 ms. Moderating an answer the customer has
already read is theatre; moderating it before they read it means buffering the whole reply
and giving up streaming, which is most of what the page is for.

**What it would protect.** The realistic harms here are not the ones a moderation API is
trained on. This assistant answers from a closed corpus about orders and returns; it does
not accept images, and shows no customer's words to another customer — the one surface that
displays them on purpose is the operations UI, to an operator, where reading a conversation
writes an audit row ([operations](operations.md)). It has exactly two tools, both bounded:
a read-only order lookup and a capped, deduplicated ticket. The harms that
actually exist in this design are: an instruction hidden in an edited knowledge entry
(`docs/knowledge.md` — the corpus stopped being a reviewed file the moment operators could
edit it, and the eval's injection case passes with and without the prompt's defensive
wording, which means the probe is too weak to discriminate rather than that the wording
works), and money (bounded by the per-conversation budget, the per-subject limit and the
daily ceiling). A moderation API addresses neither.

**And it must fail open.** A moderation service that fails closed turns its own outage into
this service's outage. Failing open is correct, and it also means the control can be
routed around by whatever makes it fail — so it is a filter for accidents, not a boundary
against an attacker. Calling it a control would overstate it.

**What would change the decision**: user-visible content flowing between customers, image
or file input, a tool that writes something a third party reads, or a compliance
requirement that names moderation. Any of those and the trade goes the other way.

**If it is built, these are the terms** — written down now so the decision is not made
again from scratch:

- `MODERATION_ENABLED`, off by default, one provider, one timeout.
- **Fail open, loudly**: a `slog.Error` and its own counter, so "moderation has been down
  for a week and everything passed" is visible rather than plausible. A fail-open path with
  no counter is the failure this repository keeps writing down.
- Moderate the **input before the model call**, and the **stored reply after** the customer
  has it. Never buffer the stream.
- Measure the added latency and put the number in this document. `chat_turn_duration_seconds`
  already exists to make it visible; an unmeasured latency claim is not one this repository
  makes.
- Never send the customer's text anywhere the traces do not already go —
  [the trace carries no customer text](observability.md#the-customers-words-are-not-in-the-trace-and-that-was-checked-rather-than-assumed),
  and a moderation vendor is a new destination for exactly the words that were kept out of
  the backend.

## Still not built, and named

- **Jailbreak detection.** Same problem as refusal detection, one step harder: it is
  classification of adversarial text, and a phrase list is worth even less against an
  adversary than against a customer. What actually bounds a persuaded model here is what
  its tools can do — and constraining tool calls by the caller's identity (a customer
  cannot look up an order that is not theirs) is the control that is missing.
  `lookup_order_status` takes an order number and the caller's session does not constrain
  it, which is [production-readiness item 4](production-readiness.md#4-the-tools-are-fiction)
  meeting this one.
- **A refusal rate for the assistant declining**, per the first section: it needs a judge,
  and the sampled-offline shape is the one to build.
- **Anything per-customer beyond the session.** A subject is a session token, not a person.
  Two sessions from one customer are two subjects, and this service has no way to know
  otherwise. Identity asserted by the host product — a JWT, an OIDC subject — is the seam
  `internal/identity`'s package comment names `Authenticator`, and there is no such type:
  it is a named gap, not a stub.

---

[← Back to the README](../README.md)
