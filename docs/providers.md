# Chat providers


The provider is configuration, not code. Everything around the model — memory, retrieval,
both tools, the tool loop, SSE streaming, metrics and spans — is written against
[`llm.Client`](../internal/llm/llm.go), a three-method interface.

```bash
CHAT_PROVIDER=anthropic  ANTHROPIC_API_KEY=…   # default, claude-opus-5
CHAT_PROVIDER=openai     OPENAI_API_KEY=…      # gpt-5
CHAT_PROVIDER=xai        XAI_API_KEY=…         # grok-4.6
```

Only the selected provider's key is required, and startup fails immediately if it is
missing. That is deliberate: a service that starts without credentials, reports itself
healthy, is marked ready by Kubernetes and then 401s every customer request is the worse
failure.

All three are verified live: each answers a question from the corpus, calls
`lookup_order_status` and uses its result, and reports usage that reaches the budget, the
meters and the spans. Two of the three were asked in Chinese and answered in Chinese.

A second provider can be configured as a fallback for the first — see
[Failover](#failover-a-second-provider-and-when-it-is-used) for what counts as a failure,
and for the two questions it raises that nothing else answers: what happens to the money
already spent on the attempt that was thrown away, and what happens to the voice.

## Failover: a second provider, and when it is used

`CHAT_PROVIDER` picks one provider at start-up. Unset `CHAT_FALLBACK_PROVIDER` and that is
the whole story: if that provider is down, this service is down, and no replica count
changes it because every pod calls the same API.

```bash
CHAT_PROVIDER=anthropic          ANTHROPIC_API_KEY=…
CHAT_FALLBACK_PROVIDER=openai    OPENAI_API_KEY=…     # its own key, model and base URL
```

The secondary is configured **in full and separately**. Sharing the primary's credentials
would let `CHAT_FALLBACK_PROVIDER=xai` quietly send an Anthropic key to x.ai and find out
during the first outage. Two things are start-up failures rather than warnings: a fallback
whose key is not set, and a fallback naming the same provider as the primary. Both are
only ever exercised while something else is already broken, which is the worst moment to
discover them — and the second one is not a harmless no-op either, since the SDK has
already retried that provider with backoff by the time the client returns an error, so a
"failover" to it is a fourth attempt wearing a resilience badge.

### What counts as a failure

A 429 is not a 500 is not a timeout is not a customer closing their laptop, and only some
of those are worth spending money at a second provider. The default is **no**, and every
exception is argued in [`internal/llm/failover.go`](../internal/llm/failover.go).

| The primary returned | Failover? | Why |
| --- | --- | --- |
| 429, 500, 502, 503, 504, 529 after the SDK's own retries | **yes**, `reason=unavailable` | the provider is answering, and the answer is that it cannot serve this |
| No HTTP response at all — DNS, refused, a connection cut mid-stream | **yes**, `reason=transport` | the same failure one layer down |
| The attempt's own request timeout expired, caller still waiting | **yes**, `reason=stalled` | a provider that took the request and went silent is down in the way a status page admits last |
| The caller's context was cancelled or its deadline passed | no | the customer is gone, or the turn's own clock ran out |
| 400, 401, 403, 404, 422 | no | this service's request or credentials; the fallback rejects the same thing one invoice later |
| Anything, once a token has reached the customer | no | see *Never mid-answer* |
| Anything, part-way through a tool-calling turn | no | the request is not portable |

The last four are the ones that matter, because each of them looks like an outage from
inside a client.

**A cancellation and a stall arrive as the same error.** Both are
`context.DeadlineExceeded`, and one of them is worth a second provider. Nothing in the
error distinguishes them; what does is *whose clock ran out*, which only the caller's
context can say. So `decide` asks `ctx.Err()` before it looks at the error at all. Get
that order wrong and every turn that times out spends a second provider's tokens on a
customer who has already given up — at exactly the moment the service is slowest and least
able to afford it. There is a test for the ordering, not just for the guard.

**A 401 must not be papered over.** Failing over on one would answer every customer
correctly while the primary's credentials were broken, and the first thing anyone would
learn from is a bill from the wrong provider.

### Never mid-answer, and never mid-turn

The question failover raises that nothing else answers: **the two providers do not write
the same answer.** Claude and GPT-5 differ in voice, length and hedging. A conversation
that switches provider between turns reads as a person having a slightly different day; a
turn that switches provider half-way through reads as two people typing into one box —
and `chat.Service` inserts a paragraph break between model calls, so the seam would be
presented as if it were deliberate.

The rule is therefore **a turn picks a provider at its first model call and keeps it**:

- The switch happens before the first byte reaches the customer, or not at all. `onText` is
  watched, and a single forwarded token settles it.
- A tool round never switches. Its request carries the assistant turn the primary
  produced — that provider's tool-call ids, and on Anthropic the thinking blocks a
  continuation must echo back unchanged in `Message.Native`. Another provider's client
  cannot read any of that, so what it would send is a different request wearing the same
  name. `startOfTurn` reads the messages rather than being told, because `llm.Client` has
  no notion of a turn and inventing one would be a second source of truth about something
  the request already says.

**What that costs, said plainly:** a provider that dies after streaming half an answer
produces a failed turn even though a working provider was sitting right there. One
visibly failed answer beats an answer in two voices, because the failure is legible and
the seam is not. Restarting the turn from the beginning at the secondary — losing the text
already on screen, paying the input tokens twice — is the other defensible choice, and it
is **not** built. Nothing here measures which customers prefer.

Two further things this does not do, and both are deliberate: there is no health check and
no circuit breaker, so every turn tries the primary first and pays its failure latency;
and nothing ever fails *back*, because there is nothing to fail back from — the secondary
is used for one turn at a time, not switched to.

### The money of an attempt that was thrown away

`llm.Client.Stream` makes exactly one model call and returns exactly one call's usage; the
caller sums. A failover breaks that shape, because two providers were called and one
`Result` comes back — and the abandoned attempt has usually already been billed. Anthropic
reports the input count at `message_start`, before a single token of the answer, so a
primary that dies in that window — which is *exactly* the window a failover acts in, since
a token on screen forbids one — has spent real money on history and retrieved passages
that nobody will ever read.

So the returned `Result` carries **both** attempts' tokens. That is the only way they reach
the budget at all: `chat.Service` sums `Result.Usage` and hands the total to
`cost.Budget.Record`, and dropping them here would put the same defect one layer below the
comment in `llm.go` that promises otherwise.

**What that costs is attribution, and it is written down rather than hidden.** One `Result`
carries one model id, so the primary's tokens arrive in `chat_tokens_total` labelled with
the *fallback's* model and priced at the fallback's rate. Two counters make that visible
rather than silent:

| Metric | Labels | What it is for |
| --- | --- | --- |
| `chat_provider_failovers_total` | `from`, `to`, `reason` | a failover is a *silent success* — the customer is answered and the turn completes — so without this the first sign of a provider outage is an invoice from a provider nobody meant to use |
| `chat_failover_abandoned_tokens_total` | `provider`, `model`, `type` | the same tokens, at the model that really billed them, so the skew above is a number somebody can subtract rather than an assumption |

Neither is labelled by conversation id, and a test reads the label names back out of a real
`Gather` to keep it that way.

One more consequence: `chat_model_calls_total` counts one call per `Stream`, so it
undercounts by one per failover. **Provider calls attempted = `chat_model_calls_total` +
`chat_provider_failovers_total`.** Fixing that properly needs `chat.Service` to meter each
attempt separately, which needs a change in `internal/chat` that this work did not make.

### What is verified, and what is not

Every rule above is a test in
[`internal/llm/failover_test.go`](../internal/llm/failover_test.go), driving the two real
clients against two `httptest` providers — never a stub of `llm.Client`, because the
decision to spend money at a second provider is made *from* the error the first one
returned, and a stub choosing that error would be choosing the answer. Each assertion was
forced red by perturbing the source and observed failing.

**Not verified live.** No two-provider outage has been exercised against real APIs; the
529, the truncated stream and the stall are all `httptest`. And nobody has measured what a
mid-conversation change of voice actually reads like to a customer — the rule above is
argued, not evidenced.

### No sampling parameters, for any provider

Claude Opus 5 returns HTTP 400 for `temperature`, `top_p` or `top_k` — any of them. GPT-5
returns *"Unsupported value: 'temperature' does not support 0.7 with this model. Only the
default (1) value is supported."*

There is no field in `llm.Request` to set one, and `config.Chat` has none either. A test
asserts that, which sounds like testing nothing until you know the shape of the bug it
prevents: Spring AI set a temperature in a *field initialiser* on each provider's
properties class — 0.8, 0.7, 0.7 — that configuration could not null out, and the Java
implementation needed a `BeanPostProcessor` to strip it back off. There is nothing to strip
here, and the test is there so that stays true.

### xAI is a provider, not a base-URL trick

Spring AI ships no xAI starter, and there are three ways to respond to that. Two are
wrong, and Go does not change which.

The shortcut is to select `openai`, put the xAI key in `OPENAI_API_KEY` and override the
base URL. It works, and it lies: the configuration says OpenAI everywhere while talking to
xAI, the two cannot be configured side by side, and whoever reads the deployment later has
to know the trick. The opposite mistake is writing an xAI client from scratch — xAI speaks
OpenAI's wire protocol, so that reimplements streaming, tool calling and retry for no gain
and a permanent maintenance cost.

What is actually true is that xAI is a **separate provider** reached over a **shared
protocol**, and those are different things.
[`openAIProtocol`](../internal/llm/openai_protocol.go) is one implementation with two
constructors: `NewOpenAI` and `NewXAI` differ in the provider name they report and the
credentials, base URL and model they are given. `CHAT_PROVIDER=xai` sits alongside
`openai`, and OpenAI's own slot stays free.

The one thing this does not paper over: xAI's compatibility is xAI's to maintain. If they
diverge from OpenAI's protocol this breaks, and the file says so rather than hiding behind
its name.

### What only a live call found

**Streamed usage arrives differently per provider, and the disagreements are silent.**
The OpenAI protocol reports no usage at all in a streamed response unless the request sets
`stream_options.include_usage`. Anthropic sends it unasked, which is exactly how that
stays hidden until someone looks at a budget that never fires. Details, and the frame
counts, in [Cost and failure](reliability.md#a-turn-is-not-a-model-call).

**The model id in the response is not the one you asked for.** `gpt-5` comes back as
`gpt-5-2025-08-07`. Metrics and prices key on what the provider reports.

### What this does not claim

Nothing here calls three APIs and compares them. The abstraction covers the request shape;
tool-call reliability, streaming chunk granularity, and how each provider treats a system
prompt differ in ways only live traffic reveals. A cross-provider contract test would need
three sets of credentials and would cost money on every run, so it does not belong in CI.

**Gemini is not implemented.** The Java implementation of this system supports it and
records that choosing a model took four attempts — `gemini-3-pro-preview` 404s without
preview access, `gemini-2.5-pro` is listed by the models API and still 404s as "no longer
available to new users", and `gemini-3.1-pro-preview` returns 429 because the free tier's
pro quota is zero. That finding is theirs and is **not re-verified here.**

---

[← Back to the README](../README.md)
