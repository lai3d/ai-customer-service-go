# Tool calling


The model can call two tools.

| Tool | What it does | What is behind it |
| --- | --- | --- |
| `lookup_order_status` | Reads one order by number. Case- and whitespace-tolerant, because customers paste order numbers out of emails. | A seam, `tools.OrderSource`, with two implementations: a five-order fixture (the default) and an HTTP adapter for a real order service. **The adapter has never been run against a real order system** — see [Where an order comes from](#where-an-order-comes-from). |
| `create_support_ticket` | Raises a ticket for a human agent, attributed to the conversation it came from. | Postgres. Dedupe and the per-conversation cap are database guarantees, not process state. |

**Tool descriptions are prompt, not documentation.** They are the entire basis on which
the model decides whether to call a tool instead of answering from retrieved FAQ text, so
they say what each tool is *not* for as well as what it is for.
`TestToolDefinitionsSayWhatTheToolIsNotFor` asserts on the generated schema — names,
descriptions, which parameters are required, and that every parameter has a description —
because a rename or a dropped description changes model behaviour without changing
anything else a test would notice.

### A missing thing is a value, not an error

`lookup_order_status` returns `found: false` with a plain explanation. Throwing would put
an internal error string in front of a customer — whatever a tool layer does with a
returned error, the model reads the result and writes an answer from it — and would give
the model nothing to reason about. `found: false` lets the assistant say "I can't find
that order number, could you check it?"

The same applies to arguments the model got wrong, and to a refused ticket. A refusal
returned as an error would reach the model as the generic tool-failure sentence — *"offer
to raise a support ticket"* — which is precisely the wrong thing to say when the problem
is that too many tickets already exist.

Anything that does still return an error is replaced by one fixed sentence and the real
error is logged. There is exactly one string a customer can ever see from a failing tool,
and it was written for them.

## Where an order comes from

Order lookup used to answer from `mockOrders`, a map with five orders in it, and nothing
else was possible. It is now a seam:

```go
type OrderSource interface {
    Lookup(ctx context.Context, orderNumber string) (Order, Outcome, error)
}
```

The signature is deliberately the one `internal/ticket` uses — a value, an outcome, and an
error that is for the log rather than for the caller's control flow.

| Source | When | What it is |
| --- | --- | --- |
| `MemoryOrders` | `ORDER_SERVICE_URL` unset — the default | ORD-10042 to ORD-10046, hard-coded, dates matching the Java implementation's so a conversation replays against either. Every other number is `not_found`. |
| `HTTPOrders` | `ORDER_SERVICE_URL` set | `GET {base}/orders/{number}` with a bearer token. |

**The server says which one it is using at every start-up**, and the fixture case is a
`WARN` rather than an `INFO`:

```
level=WARN msg="ORDER_SERVICE_URL is unset: lookup_order_status answers from a five-order
fixture (ORD-10042 to ORD-10046) and reports every other number as not found. This is a
demo source, not the order system."
```

That line is the point of the whole item. A service answering from a fixture is
indistinguishable from one that works — it returns orders, the meters read `found`, the
model composes a fluent answer — right up until a customer asks about an order that
exists. Nothing else in the system can notice, so the service has to say it out loud, which
makes it load-bearing code rather than decoration. `cmd/server/main_test.go` captures the
log and asserts it: the fixture case is `WARN` and names the fixture, the configured case
names the URL and reports `authenticated=true` **without printing the token**.

### What has and has not been verified

**Not verified: the wire contract.** There is no order service to point this at. The
request shape, the status codes and the field names in `orders_http.go` are this
repository's guess, and they are the first thing that will be wrong when a real service
appears. Treat that file's doc comment as a starting point, not a specification.

**Verified: every way the adapter can fail**, driven against an `httptest` server in
`internal/tools/orders_http_test.go` — not against a stub of `OrderSource`, because a stub
satisfies whatever contract it was written beside, which is how a defect shipped here
before (`CLAUDE.md`, *"Never return early from a client `Stream` on error"*). The failures
that exist are net/http's: a body that stops mid-read, a status code nobody planned for, a
server that accepts the connection and then says nothing. None of them is reachable above
the seam.

| Assertion | Forced red by |
| --- | --- |
| A 200 becomes an order, uppercased and trimmed on the way out | dropping the normalisation: `GET /orders/ord-77001%20%20` |
| A 404 is `not_found`, not retried | mapping 404 to `unavailable`: three failures collapsed to two |
| A 500 is `unavailable` and is retried exactly once | making the retry unconditional-false: 1 request, want 2 |
| A timeout is `timed_out`, costs one attempt and returns inside the budget | moving the deadline from the lookup to the attempt: 2 requests and 706 ms against a 300 ms budget |
| A body that is not JSON, `{}`, or an order with no status is `unreadable` | removing the empty-status check: `found:true` with `"status":""` |
| A 128 KiB body is refused rather than billed | removing the `io.LimitReader`: 131,188 bytes reached the model's context |
| A 401/403 is `denied`, and says nothing about credentials to the model | mapping it to `unavailable` |
| Nothing the deployment knows reaches the model on any failing path | appending the source error to the explanation: the host, the port and `check ORDER_SERVICE_TOKEN` all appeared |
| A credential in the URL does not reach the log | removing `scrub`: the token appeared in an `ERROR` line |
| An order number cannot steer the request | dropping `url.PathEscape` |
| The default source reports itself as a fixture | `Fixture() bool { return false }` |
| `ORDER_SERVICE_TIMEOUT` must be shorter than `HTTP_READ_TIMEOUT` | removing the check: a 30 s tool budget was accepted against a 30 s turn |
| The start-up line announces the fixture at `WARN` | downgrading it to `INFO` |
| The start-up line says whether a credential is set, and never what it is | logging `"token", cfg.Token`: `token=s3cret-order-token` |
| Every `ORDER_SERVICE_*` variable reaches the container | deleting two of them from `docker-compose.yml`: `TestEveryDocumentedVariableReachesTheContainer` named both |

One of those was red for the wrong reason first, and it is worth writing down: the
path-escaping test originally asserted on `r.URL.Path`, which is what net/http *decoded*.
It reported an escaped `%2F..%2F` as a traversal that had not happened. The assertion is on
`r.RequestURI` now — what actually went on the wire.

**One branch is not covered**, and naming it is cheaper than a flaky test: the case where
the pause *between* retries would outrun the budget (`ORDER_SERVICE_ATTEMPTS` of 3 or more
against a service failing fast). It returns `timed_out` rather than sleeping past the
deadline and reporting a stale `unavailable`. Reaching it needs a test tuned to a 100 ms
internal gap, which would be a timing assertion pretending to be a behavioural one. With
the default of 2 attempts the branch is unreachable.

### A timeout, a 404 and a 500 are three different things

They are three different things to a customer, and collapsing them is a single line of
code: `return errors.New("lookup failed")`. So the outcome is a closed vocabulary, it is a
metric label and a span attribute, and it also travels into the model's own input as
`reason`:

| Outcome | What happened | What the model is told |
| --- | --- | --- |
| `found` | the order is in the result | — |
| `not_found` | the source answered; there is no such order | may have been mistyped, or belongs to another account |
| `timed_out` | no answer inside the budget | says nothing about the order; offer to try again in a moment |
| `unavailable` | 5xx, 429, or the service could not be reached | a problem on our side; do not guess; offer a support ticket |
| `unreadable` | a 200 this service cannot use, or a status code outside the contract | as `unavailable` |
| `denied` | 401 or 403 | as `unavailable` |
| `bad_arguments` | the model's arguments did not fit the schema | ask the customer to repeat the number |

`unreadable` and `denied` share the `unavailable` wording, and that is the one deliberate
collapse. They are three different things to an operator — an outage, a contract
disagreement and a rotated token get fixed by three different people — and one thing to a
customer, which is *we cannot check right now*. In particular the model is never told that
a credential was rejected: its context is one turn away from a customer's screen, and
`check ORDER_SERVICE_TOKEN` is not a sentence anyone should be able to read out. That goes
in the log and the metric label, where the person who can fix it is looking.

Every explanation says **do not guess at a status**. An invented *"it's out for delivery"*
is much worse than an apology, and a model with no answer and a helpful disposition will
produce one.

### The budget covers the whole lookup

`ORDER_SERVICE_TIMEOUT` (3 s) bounds the lookup including retries, not one attempt. A tool
call happens *inside* a turn the customer is watching, after the model has already spent
seconds deciding to make it, so the number that matters is how long they wait — and an
adapter with a 3 s per-attempt timeout and two attempts is a 6 s adapter that every
document will describe as a 3 s one.

Two consequences follow, and both are asserted:

- A **timeout is not retried.** The budget that would have paid for the second attempt is
  exactly what the first one spent. What is retried is a 5xx, a 429 and a connection that
  could not be made — the failures where the second attempt costs milliseconds.
- `config.Load` **refuses to start** if `ORDER_SERVICE_TIMEOUT` is not shorter than
  `HTTP_READ_TIMEOUT`. A clamp would be a value nobody chose taking effect silently, and
  the symptom of getting this wrong is not an error anywhere: it is a slow assistant, which
  everyone reads as a slow model.

### The order service is input to the model

Whatever it returns ends up in the model's context, and `Summary` is free text. The body is
bounded at 64 KiB — an upstream bug that streams megabytes should cost a decode error
rather than a bill — and the order number is `url.PathEscape`d, because it is written by
the model from what the customer typed and concatenating it into a URL makes *"what shall
I ask the order service for"* a decision the customer gets to make.

**Not verified, and worth saying:** if a real order system's fields can contain
customer-supplied text, this becomes the same attacker-influenced-input problem that
[item 2](production-readiness.md#2-the-corpus-is-a-test-fixture) describes for an editable
corpus. Nothing here defends against that yet, and the fixture cannot produce it.

### Conversation identity is a parameter

`Tool.Invoke` takes the conversation id as an argument. Spring AI passed it through an
ambient `ToolContext`, which created a contract with teeth: a code path that reached the
model without populating it broke ticket creation, and broke it *only once a conversation
had escalated far enough for the model to try*. The Java implementation needed a test
covering both entry points to catch that.

Here a caller that forgets does not compile. That is not cleverness; it is what happens
when a dependency is passed rather than found.

### Ticket creation is idempotent, and capped

A model can call the same tool twice in one turn, and a retried request replays the
conversation. Without a guard, one frustrated customer becomes three tickets in the human
agents' queue. Asking twice returns the existing ticket flagged `alreadyExisted`, so the
assistant says "I've already raised that" rather than inventing a second reference number.

The cap is three, enforced in the tool, and the reasoning is in
[Cost and failure](reliability.md#bound-tool-side-effects-in-the-tool).

### Tools run in parallel, and their results go back together

A model may ask for several tools in one response. They are executed concurrently and
**all** the results go back in a single message. Splitting them across messages is
accepted by the API and quietly teaches the model to stop asking for tools in parallel.

---

[← Back to the README](../README.md)
