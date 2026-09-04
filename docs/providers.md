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
