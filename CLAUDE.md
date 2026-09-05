# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this
repository.

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
make lint                              # go vet + gofmt; CI runs this and fails on it
go test ./internal/rag/ -run TestNoSimilarityThresholdIsUseful -v
go test ./internal/chat/ -run 'TestRetrievedPassagesNeverEnterMemory' -v
make bench                             # opt-in; four runs in four processes
make build                             # bin/server
make fmt

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

### A turn's events reach the client through three files

`chat.Service.Turn` takes an `emit func(Event)` and pushes typed events -- `retrieval`,
`tool`, `message`, `usage`, `error` -- rather than returning a string. Adding or changing
one means touching all three of:

```
internal/chat/events.go        the Event type and what each carries
internal/httpapi/sse.go        the turn runs in a goroutine, events arrive on a channel,
                               and the heartbeat interleaves with them
internal/httpapi/web/index.html  the only consumer that exercises the whole contract
```

The channel is not incidental. It is what makes the turn consumed exactly once while a
heartbeat interleaves; merging a heartbeat into a reactive stream can subscribe twice and
run the whole turn twice -- two model calls, two bills -- while the response still looks
correct.

`httpapi.NewServer` takes a `Turner` interface rather than `*chat.Service`, so the edge --
validation, status codes, SSE framing -- is tested with no database, no model and no
embedding model. `internal/chat` is where the turn itself is tested, against a real
Postgres.

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
- **A script must not open the user's kubeconfig at all.** `k8s/kind/verify.sh` exports its
  own `KUBECONFIG`. `kubectl config use-context` changes global state in a file that holds
  production-shaped contexts; `--context` per call is not enough, because
  `kind create cluster` writes into `$KUBECONFIG` anyway; and save/restore is not a fix
  because `trap ... EXIT` replaces the previous handler rather than adding to it. Verify by
  hashing the file, not by reading `current-context` — the weaker check passes for a script
  that rewrites the file and puts the context back.
- **A check that cannot be seen to fail is a claim, not a check.** `k8s/README.md` keeps an
  inventory of which harness assertions have actually been observed red. Three separate
  detectors in this repository have been silently blind — a `CREATE EXTENSION` check whose
  condition never arose, a capacity check that parsed nothing and passed, and a regex that
  measured the language rather than the bug. Before trusting a green check, make it red.
- **Schema creation takes a Postgres advisory lock.** `CREATE EXTENSION IF NOT EXISTS` is
  not concurrency-safe — it checks the catalogue and then inserts, with nothing holding the
  gap — so two replicas starting against a cold database crash one of them with
  `duplicate key value violates unique constraint "pg_extension_name_index"`.
  `TestConcurrentStartersAgainstAColdDatabaseAllSucceed` needs its own container, because
  the shared fixture has already created the extension by the time any other test runs.
- **A turn holds a per-conversation lock for its whole length** (`internal/chat/serialize.go`).
  History read, model call, budget record and reply persistence are only coherent together;
  without it two browser tabs on one conversation interleave and the second request loses
  its retrieved passages silently. Do not move work outside the lock without checking what
  it reads.
- **The demo page dispatches on the SSE `event:` name, never on a payload field.** Chat
  events carry a `type`; a post-commit failure carries problem+json whose `type` is a URI.
  Switching on the payload silently drops every error, and the server-side test still
  passes.
- **The ticket table and the budget table are both bounded LRUs.** A map keyed by
  conversation id that nothing removes from is a memory leak with a long fuse.
- **`README.md` and `README.zh.md` are a pair.** Adding, removing or moving a section in
  one without the other fails `TestBothReadmesHaveTheSameSectionStructure`. Nothing
  re-derives a translation, so the test compares heading-level sequences -- the drift that
  actually happens.

## Four things that came out of being wrong

The first two are rules. The last two are not, and saying so is the point: they are
failure modes nobody in this exchange found a technique for, and inventing one would be
worse than naming the gap.

**A test written from the same understanding as the code confirms the understanding, not
the code.** Three defects here were covered by passing tests that asserted against
fixtures built to satisfy the very claim being tested — a stub returning usage where no
real client did, a mocked model standing in for a database row, a four-sample assertion
cited as a regression guard. When a test and the code it covers were written together from
one mental model, the test is evidence about the model. Push the assertion below the seam:
drive the real client against an `httptest` server, read the actual row, widen the sample.

**A detector that disagrees with a fix you have just verified is evidence about the
detector at least as often as about the fix.** A regex for run-together sentences reported
three failures in Chinese after the fix demonstrably worked in English. The regex was
measuring the language — Chinese prose puts no space after 。 — not the bug.

**The front door does not get re-read when the thing behind it changes.** Docs are
revisited when the code they describe moves, because that is what makes someone open them.
A lead paragraph, a repository description, a topic list — these are revisited when
someone writes them, and then never, and they go on reading fine because they were true
once. The Java implementation's README opened by naming one provider while its status line
four lines below named four; this one's lead is current only because it was written after
the providers existed. **When a capability changes, re-read the first paragraph of both
READMEs and the GitHub description and topics.** Nothing else will prompt you to.

**Verification is a claim about a moment, not a property of the thing.** The cross-repo
link in `README.zh.md` was checked live and returned 200; ninety seconds later the other
repository renamed the file and it was a 404. The check was not careless — care is what
produced it. There is no rule here for when a verified fact needs re-verifying, only the
knowledge that "I checked" has a timestamp on it.

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

**The demo page, verified in a headless Chromium** by the Java implementation's session on
2026-09-05: the panes fill in event order — retrieval on screen at 123 ms, the tool pill at
1933 ms, the first word of the answer at 3629 ms, the usage card at 5065 ms reporting
**two model calls** on a tool-calling turn. So "retrieval appears while the model is still
thinking" is a measurement, not a claim: 3.5 seconds before the first word. Independently
timed on the wire, the page adds no reordering of its own.

That run found the page was showing the model's markdown to the customer as literal
asterisks and hyphens, which is fixed. **Still not covered:** the run was headless with a
throwaway profile, so font fallback and anything gated on a real display are unverified.

## Driving the demo page in a browser

Neither the claude-in-chrome extension nor the MCP Playwright server attaches in this
workspace. The way through, from the Java implementation's session: a plain Node script
importing Playwright from the npx cache (`~/.npm/_npx/<hash>/node_modules/playwright`) and
launching with `executablePath` pointed at the installed Chrome. The cached
`ms-playwright` Chromium builds are the wrong build numbers for that package, which is
what the confusing "Executable doesn't exist" error actually means.

Worth the trouble: driving the page found two defects nothing else could. The model emits
markdown and the page was showing the asterisks; and the text of two model calls was run
together, which only appears when the model narrates before asking for a tool.

## Scope

No authentication, no multi-tenancy, no MCP. No Gemini — three providers, and
`CHAT_PROVIDER=gemini` fails at startup by name.

`k8s/` exists and every number in it was measured on kind by `k8s/kind/verify.sh` before
being committed — the Java repository's manifests were committed unapplied and two were
wrong, which is the reason the harness is there. If you change `resources`, re-run the
sweep; and remember that on Go the CPU limit sets `GOMAXPROCS`, which sets the embedding
concurrency bound.
