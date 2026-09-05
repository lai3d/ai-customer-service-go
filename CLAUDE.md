# CLAUDE.md

Guidance for Claude Code working in this repository.

## What this repository is

The Go half of a pair. The Java half is `../ai-customer-service-java` on this machine and
`github.com/lai3d/ai-customer-service-java` publicly. **It is not a port**, and the most
valuable thing here is the comparison — same system, same corpus, same measurements, two
runtimes, honest numbers where they differ.

Two rules follow from that:

- **`corpus/faq.json` is byte-identical to the Java repository's. Never edit it.** A
  reworded corpus makes every retrieval number on both sides incomparable.
- **Do not translate Java code.** Where Spring AI needs an advisor, a `BeanPostProcessor`
  or a `@ConditionalOnProperty`, the Go equivalent is an ordinary function call. Three of
  the Java implementation's runtime hazards are compile errors or unrepresentable here;
  that asymmetry is the README's most interesting section and rebuilding the framework
  would erase it.

## Toolchain

The embedding model runs in-process through cgo, so the build has native dependencies.

```bash
make deps      # libtokenizers.a into third_party/lib, the 470 MB model into model-cache/
```

`go build ./...` then works with nothing exported: `internal/rag/cgo.go` carries the
linker flag as a `#cgo LDFLAGS` directive with `${SRCDIR}`. ONNX Runtime is loaded at
runtime, not linked — `brew install onnxruntime` on macOS, or `make deps` fetches it on
Linux. `ONNXRUNTIME_LIB_PATH` overrides the search.

Docker must be running: every integration test starts a real `pgvector/pgvector:pg17`.

## Commands

```bash
make test                              # full suite, no API key needed
make test-race
go test ./internal/rag/ -run TestNoSimilarityThresholdIsUseful -v
make bench                             # opt-in; four runs in four processes

docker compose up -d postgres jaeger   # dependencies only
make run                               # the app from source, on :8081
docker compose up -d                   # the whole stack, app included
```

Ports avoid the Java stack's on purpose: Postgres 5433, app 8081, Jaeger 16687/4319, and
the Compose project is `ai-customer-service-go`. Container names are global — two projects
cannot both claim `csagent-postgres`.

## Architecture

One turn is `chat.Service.Turn`, and it does five things in an order that is the design:

```
1. memory.Append(user)      the customer's own words, before anything rewrites them
2. retriever.Retrieve       passages returned, not spliced into the message
3. memory.History           windowed at 40
4. the tool loop            one Stream call == one model call == one bill
5. memory.Append(assistant) whatever was said, however the turn ended
```

**Never put retrieved passages into memory.** They belong to one request. In the Java
implementation retrieval rewrote the user message and memory stored whatever it was
handed, so the wrong composition wrote every passage into the customer's history and
re-sent it forever — silently. `TestRetrievedPassagesNeverEnterMemory` and
`TestASecondTurnDoesNotResendTheFirstTurnsPassages` hold the line.

**`llm.Client.Stream` makes exactly one model call and returns exactly one call's usage.**
The caller sums. Do not add a heuristic that reconstructs call boundaries from usage
frames — that is a Spring AI workaround, and `docs/reliability.md` has the frame counts
that show why it is not needed here.

## Constraints that fail silently

- **Never set `temperature`, `top_p` or `top_k`.** Claude Opus 5 returns HTTP 400 for any
  of them; GPT-5 accepts only its own default. There is no field for one in `llm.Request`
  or `config.Chat`, and a test asserts that stays true.
- **The OpenAI protocol reports no usage in a streamed response** unless the request sets
  `stream_options.include_usage`. Anthropic sends it unasked, which is how the omission
  hides: the budget never fires and the cost meters read zero.
- **Prices key on the model the provider reports.** `gpt-5` comes back as
  `gpt-5-2025-08-07`. `chat_unpriced_model_calls_total` exists so a permanently-zero cost
  meter is visible rather than plausible.
- **`pgvector.Vector` must be registered on every pooled connection.** Without
  `pgxvec.RegisterTypes` in `AfterConnect`, query parameters still work (pgx falls back to
  text) but `CopyFrom` uses the binary protocol and Postgres rejects a 384-dimension
  vector with *"vector cannot have more than 16000 dimensions"*.
- **A zero vector has NaN cosine distance**, and `1 - NaN >= threshold` is false, so a
  search silently returns nothing. Test doubles must return a non-zero vector.
- **`resource.Merge` fails at startup** if the resource pins a semconv version different
  from the one `resource.Default()` carries. Use `resource.NewSchemaless`.
- **Go's `threadcreate` profile only ever grows.** Benchmark variants must run in separate
  processes or the second inherits the first's threads.
- **Compose does not inject an undeclared variable.** Anything in `.env.example` must be
  listed in the app service's `environment:`; `internal/deployment` asserts it. Never dump
  `docker compose config` — it interpolates real secrets.
- **Metrics are tagged by model, never by conversation id.** Per-conversation tags are
  unbounded cardinality — and so is anything the *model* writes. A tool name is validated
  against `s.tools` before it can become a metric label or a span name; a span name is an
  aggregated dimension just like a label.
- **Never return early from a client `Stream` on error.** Anthropic reports the input
  count at `message_start`, so an abandoned stream has already been billed. Build the
  result from whatever accumulated and return it alongside the error. The contract is
  asserted in `internal/llm/stream_test.go` against an `httptest` provider, not in a
  stub — a stub can satisfy any contract, which is how this shipped in the first place.
- **The ticket table and the budget table are both bounded LRUs.** A map keyed by
  conversation id that nothing removes from is a memory leak with a long fuse.

## Measurements, and how to change one

`docs/` holds one document per decision and every number in it was produced by a test or
a live call. **When a measured value changes, re-run the measurement and update it — do
not edit the number.** The tests that carry measurements are:

| Measurement | Test |
| --- | --- |
| 20/20 paraphrases, 4/4 cross-lingual | `internal/rag/retrieval_test.go` |
| No threshold separates the three score populations | `TestNoSimilarityThresholdIsUseful` |
| Both native libraries are concurrency-safe | `TestONNXEmbedderIsConcurrencySafe` (run with `-race`) |
| The ticket cap holds under concurrency | `TestTheCapHoldsUnderConcurrentCalls` |
| Throughput, latency and OS threads | `make bench` |

`app.rag.similarity-threshold` is **0**, and that is a measurement, not an omission:
relevant, off-topic and degenerate inputs all score in an overlapping band. If you change
the embedding model, re-measure the threshold, the dimensions, and the corpus embeddings
together.

## Verified live

`claude-opus-5`, `gpt-5` and `grok-4.6`: each answers from the corpus, calls
`lookup_order_status` and uses its result, and reports usage that reaches the budget, the
meters and the spans. A Chinese question retrieves Chinese passages and is answered in
Chinese, through the container as well as from source. Traces arrive in Jaeger with
`gen_ai.usage.*` and per-tool spans, and carry no customer text — checked by grepping the
backend.

**Not verified:** the demo page has not been driven in a real browser. It serves and its
event contract is tested; the rendering is not.

## Scope

No authentication, no multi-tenancy, no MCP. No Gemini — three providers, and
`CHAT_PROVIDER=gemini` fails at startup by name. Do not add Kubernetes manifests; the
Java repository has them and a duplicate carries no signal.
