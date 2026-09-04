# Observability


Metrics on `/metrics`, traces over OTLP. `docker compose up` starts Jaeger alongside the
app and points the exporter at it; the UI is at **http://localhost:16687**. Jaeger ingests
OTLP directly, so no separate collector is needed locally — a real deployment would put
an OpenTelemetry Collector in front.

Export is off by default, so `make run` on its own does not fill the log with failed
exports.

### Why traces matter more here than in an ordinary service

A single turn is retrieval, then a model call, then possibly a tool call and a second
model call. Metrics can tell you a turn took eleven seconds; only a trace tells you which
of those it was. A real turn, read out of Jaeger:

```
POST /api/v1/chat                     11099 ms
└─ chat turn                          11099 ms
   ├─ retrieve                            7 ms
   │  ├─ embed query                      5 ms
   │  └─ pgvector similarity search       2 ms
   ├─ chat claude-opus-5               3715 ms
   ├─ tool lookup_order_status            0 ms
   └─ chat claude-opus-5               7372 ms
```

Retrieval is 7 ms of an 11-second turn. Everything else is the model, and it is *two*
model calls, because the first one asked for a tool — which is the same finding the
[token accounting](reliability.md#a-turn-is-not-a-model-call) rests on, visible here as
two spans instead of one. A trace that collapsed them would hide half the turn's cost.

Attribute names follow OpenTelemetry's GenAI semantic conventions — `gen_ai.system`,
`gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.usage.input_tokens`,
`gen_ai.usage.output_tokens`, `gen_ai.response.finish_reasons` — so nothing here invents
a vocabulary.

Sampling is 1.0, not a fraction. At a lower rate most conversations produce no trace at
all, which reads as "tracing is broken" rather than "tracing is sampled". Lower it
deliberately under real traffic.

### The customer's words are not in the trace, and that was checked rather than assumed

Nothing in this codebase puts customer text on a span: not the question, not the reply,
not the tool arguments the model wrote from what the customer said. The conversation id
is there; the content is not.

That is easy to claim and worth verifying, because the Java implementation of this system
found the opposite by accident — Spring AI attached the search query to every vector-store
span unconditionally, with no property to disable it, and it was discovered by reading a
customer's question back out of Jaeger rather than from any documentation. A support
question is often the most sensitive thing in a request, and traces are retained and read
far more widely than a database is.

So the check is the same one: send a turn containing something that must not leak, then
search what actually arrived at the backend.

```
POST /api/v1/chat
  {"message":"订单 ORD-10045 的退款什么时候到？我的信用卡号是 4111-1111-1111-1111"}

$ curl -s http://localhost:16687/api/traces/$TRACE_ID > trace.json
$ grep -c 4111 trace.json          0
$ grep -c ORD-10045 trace.json     0
$ grep -c 信用卡 trace.json          0
$ grep -c 退款 trace.json           0
```

Zero, including the order number that was in the tool's arguments and the fragments of
the question that reached the model. What is kept is everything that makes a span useful:
top-k, how many passages came back, the similarity threshold, the dimensions, the model,
the token counts, the finish reason, and the timing.

The general form of this is the part worth carrying: **check what arrives at the backend,
not what the documentation says is included.** A library that adds one helpful attribute
is doing something reasonable for a library and something unacceptable for a support
system, and it will not be listed under the switches that turn content logging off.

### Metrics

```
chat_tokens_total{model="claude-opus-5",type="input"}
chat_cost_usd_total{model="claude-opus-5"}
chat_unpriced_model_calls_total{model="gpt-5-2025-08-07"}
chat_model_calls_total{model="claude-opus-5",outcome="success"}
chat_turns_total{outcome="completed"}
chat_turn_duration_seconds{model="claude-opus-5"}
chat_tool_invocations_total{tool="lookup_order_status",outcome="found"}
chat_retrieval_duration_seconds
```

Everything is tagged by model and **never** by conversation id. Per-conversation tags
grow cardinality without limit and take the metrics backend down long before the bill
does; there is deliberately no way to pass a conversation id into any of them.

`chat_unpriced_model_calls_total` exists because the alternative failure is silent. Prices
key on the model id the provider *reports* — asking for `gpt-5` yields
`gpt-5-2025-08-07` — so a price table keyed on the requested id never matches, tokens
keep counting, and the cost meter stays at zero. A flat cost meter is indistinguishable
from a cheap month unless something counts the misses.

---

[← Back to the README](../README.md)
