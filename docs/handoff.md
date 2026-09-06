# The loop back to a human

A ticket used to be a row and nothing else happened. It was created, deduplicated, capped
and audited — and then it sat there. Nothing told a person it existed, and when a person
did eventually reply there was no path back to the customer who had asked.

That is the line between an assistant that escalates and a chat box that files tickets into
a drawer, and both halves of it are here now.

```
customer asks for a human
  └─ create_support_ticket        the ticket exists      (was already true)
     └─ POST to HANDOFF_WEBHOOK_URL   somebody is told   (new)
        └─ operator replies in the operations UI
           └─ the reply is written into the conversation (new)
              └─ GET /api/v1/conversations/{id}          the customer reads it (new)
                 └─ and the model's next turn composes from it
```

## Outbound: a webhook, and the decision it does not make for you

`HANDOFF_WEBHOOK_URL` is any URL that accepts a JSON POST — a chat webhook, a ticketing
system's inbox, your own relay. **Which system tickets should land in is your decision and
not a technical one**, and a webhook is the one shape every candidate accepts, so this
builds the half that does not depend on the answer.

Empty means a ticket is created and nobody is told. The server warns about it at every
start-up, because that is the state this was in before and it looked like it worked.

### The body carries no customer text

```json
{"type":"ticket.created","ticket":"TKT-4703","conversation":"f3afa880-…","category":"payment","at":"…"}
```

Everything else in this service works to keep customer text out of the places it leaks to —
no query content on spans, no customer words in metric labels, a
[trace backend grepped to prove it](observability.md#the-customers-words-are-not-in-the-trace-and-that-was-checked-rather-than-assumed).
A webhook is by definition a place outside this service's control, and usually a chat room
with a search box and a wide audience. Whoever receives this opens the operations UI, where
reading the conversation is [an audited action](operations.md#reading-is-an-action).

### A failed notification is recorded, because its failure mode is silence

Delivery is asynchronous with one retry, and every outcome lands in `handoff_delivery`.
Nobody chases a message they do not know was sent, so "we were never told about that
ticket" has to be an answerable question rather than an argument. The operations overview
shows the count of undelivered notifications in the window, in red.

Asynchronous is also what stops a slow chat room becoming a slow ticket. The model has
already promised the customer a human; failing the turn because the notification could not
be delivered would break the part that worked because the part that did not, failed.

## Inbound: the reply goes into the conversation, not just onto the ticket

An operator replies in the ticket dialog. The text lands in three places in one
transaction — the ticket's history, the customer's conversation, and the ticket's updated
timestamp — and a fourth thing happens afterwards: the destination is told again.

The message is attributed **in its own words**: `alex (support): …`. The conversation has
one assistant role and the customer cannot see a database column, so without the name in
the text a person's answer is indistinguishable from the machine's.

Writing it into `chat_memory` rather than only onto the ticket is the part that is easy to
skip and expensive to have skipped. The model's next turn composes from that history. Live,
after an operator replied that a refund had been released manually:

> **customer:** 那我还需要做什么吗？
> **assistant:** 不需要，你这边不用再做任何操作。**alex 已经手动放行了这笔退款**，48 小时内会到账…

Without the reply in the history, the assistant would have told the customer to wait for a
human who had already answered — confidently, and in the same conversation where the answer
was sitting.

## What the customer reads

`GET /api/v1/conversations/{id}`, session-scoped like everything else. **With
`AUTH_MODE=off` the endpoint does not exist**: a transcript readable by anyone who guesses a
conversation id would be the [confidentiality hole](production-readiness.md#1-anyone-can-read-anyone-elses-conversation)
this service just closed, reopened for convenience.

## What was checked, and how

Five properties, each forced red first:

| Property | What made it fail |
| --- | --- |
| The reply reaches the conversation | removing the `chat_memory` insert — the ticket event alone left the transcript one message short |
| The webhook carries no customer text | putting the reply text in a field — the assertion printed the whole leaked body |
| A failed delivery is recorded | logging without the insert — the test timed out waiting for a row |
| A failing webhook does not fail the reply | making `Reply` wait for delivery and return its error |
| An unknown ticket or an empty reply is refused | (structural; both return before anything is written) |

The fourth perturbation took two attempts. Making delivery synchronous did not fail
anything, because `Send` returns nothing either way — it only made the reply slow. The
perturbation that actually tests the property is one that propagates the delivery error,
and until it was written the test was passing for a reason unrelated to what it claimed.

Live, end to end: a Chinese complaint raised `TKT-4703` and the destination received
`ticket.created`; an operator replied through the API and the destination received
`ticket.replied`; the customer's transcript showed their question, the model's answer and
`alex (support): …`; and the next turn answered from the human's words rather than around
them.

## What is not built

- **No push.** The customer sees the reply when they come back and ask, or when a client
  polls the transcript. Nothing emails them, and nothing wakes a closed tab. That needs a
  channel this service does not have, which is the same decision as the destination one.
- **The demo page does not poll.** It keeps its conversation in memory and starts fresh on
  reload, so the loop is demonstrated with `curl` rather than on screen.
- **No signature on the webhook.** A receiver cannot verify the POST came from here. An
  HMAC header is small and belongs with whatever destination is chosen, since the
  verification has to happen on that side.
- **One retry, then a record.** No queue, no backoff schedule, no replay. The record is
  what makes a missed notification recoverable by hand, and a retry loop against a
  destination that is down turns one outage into two.

---

[← Back to the README](../README.md)
