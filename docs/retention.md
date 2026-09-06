# Deleting customer data

Until recently there was no `DELETE` against customer data anywhere in this service. Every
word a customer had typed sat in `chat_memory` and `turn` in plain text for ever, and a
deletion request would have been a hand-written SQL statement by whoever had the password.

Two things exist now, and they answer different questions:

| | Question it answers | Where |
| --- | --- | --- |
| **Expiry** | "why do we still have this?" | `RETENTION_DAYS`, swept on a schedule |
| **Erasure** | "please delete my data" | `DELETE /api/admin/v1/conversations/{id}`, operator only, audited |

Expiry is off by default and the server warns about it at every start-up. That is
deliberate: *"we never deleted anything"* is not a position anyone means to hold, it is one
they discover when somebody asks.

## What is deleted, and what is not

```
deleted    chat_memory        the customer's words, and the model's
           turn               question, reply, failure detail
             turn_passage     which passages were retrieved (cascade)
             turn_tool_call   which tools ran (cascade)
           conversation_owner the mapping from subject to conversation
           chat_session       on subject erasure

redacted   support_ticket     summary, order number, resolution -> [erased]
           ticket_event       detail -> [erased]

untouched  admin_audit        every row, including the ones about this conversation
```

### The audit trail survives, entirely

An audit row that the subject of the audit can erase is not an audit row. This is the one
table an erasure must not touch, and it costs nothing to hold that line here: the trail
records *who did what to which object* — an operator's name, a ticket number, a
conversation id — and no customer text. There is nothing in it to erase.

The erasure itself writes one. It names what was removed:

```
alex | erase conversation | ok | memory=2 turns=1 conversations=1 sessions=0 tickets_redacted=1 events_redacted=1
```

"Somebody erased something" would record that it happened and not what, which is the
failure the trail exists to prevent. The entry is written even when the erasure found
nothing, because *somebody asked us to erase a conversation that did not exist* is exactly
the kind of thing an investigation later wants to know happened.

### Tickets are redacted, not deleted

A support ticket is a business record. Deleting an `OPEN` one erases the fact that somebody
is owed a refund along with the words that asked for it, and the customer who asked to be
forgotten is usually not asking to forfeit their refund.

So the words go and the record stays: the ticket keeps its number, state, timestamps and
version; its history keeps every actor, action and time and loses only what was written.
The order number goes with the text, because an order number identifies a person as much
as it identifies a parcel.

`[erased]` rather than an empty string, because a blank summary reads as a bug and an
operator should be able to tell *erased on request* from *the model wrote nothing*.

### Expiry leaves tickets alone too

The sweep takes turns and memory past the window and does not touch tickets at all. A
ticket ages on its own lifecycle, and expiring it because the conversation that raised it
got old would delete an obligation because the request for it got old. Retention for
tickets is a separate decision with a different owner; folding it in here would make the
sweep quietly wrong.

An owner row is removed only when it is both past the window **and** has nothing left
pointing at it — no turns, no tickets. An old conversation that still has a ticket keeps
its attribution.

## Erasure is not in the operations UI

There is no button. A one-click irreversible erase needs a confirmation design that has not
been thought about, and shipping the button first is how that thinking gets skipped. An
operator does it with `curl` today, and the API is the deliverable:

```sh
curl -X DELETE "$API/api/admin/v1/conversations/$ID" -H "Authorization: Bearer $TOKEN"
```

A viewer gets `403`, and the refusal is audited like any other.

## What was checked, and how

Every survival property was forced red before being trusted — deleting tickets instead of
redacting them, letting the erasure reach `admin_audit`, letting the sweep expire tickets,
registering the endpoint as a read so a viewer could call it, and dropping the report from
the audit entry.

Two of those five perturbations did not fire on the first attempt, and both times the
perturbation was wrong rather than the test: one deleted audit rows keyed on a bare
conversation id when the trail stores `conversation/<id>`, and one expired tickets by
`created_at` when the fixture back-dates the conversation rather than the ticket. An
assertion that has not actually been seen red is a claim.

Live, against a real model: a Chinese complaint raising `TKT-4702`, a viewer refused with
`403`, an operator's erasure returning its report, `chat_memory` and `turn` emptied, the
ticket surviving as `OPEN` with `summary=[erased]` and its history intact, and the audit
trail holding both the refusal and the erasure. Then a sixty-day-old turn planted while the
service ran with a thirty-day window and a five-second interval, swept on the next tick.

## What this does not do

- **No encryption at rest, and no field-level encryption.** Deletion is not the same as
  confidentiality; this addresses the first only.
- **No export.** "Give me my data" is the other half of most regulations and is not built.
- **Erasure by subject exists in the store and not on the API** (`EraseSubject`), because
  the API has no way to name a subject yet — the operations surface lists conversations,
  not the anonymous subjects behind them. It is one endpoint away when there is a reason.
- **Nothing propagates.** If a ticket has been exported to another system, erasing it here
  erases it here.

---

[← Back to the README](../README.md)
