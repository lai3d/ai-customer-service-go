# Tool calling


The model can call two tools. Both are mock implementations: the point is the calling
contract, not an order system.

| Tool | What it does |
| --- | --- |
| `lookup_order_status` | Reads one order by number. Case- and whitespace-tolerant, because customers paste order numbers out of emails. |
| `create_support_ticket` | Raises a ticket for a human agent, attributed to the conversation it came from. |

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
