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
docker compose --profile ops up -d     # plus the operations UI on :8090

cd admin-ui && npm ci                  # the operations UI (React, TS, Vite, antd)
npm run dev                            # :5173, pointed at VITE_API_BASE
npm run typecheck && npm test          # vite build does NOT typecheck; run both
npm run build
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
- **An alert that names a series the application does not emit never fires**, and looks
  exactly like a healthy system. `observability/prometheus-rule.yaml` is therefore checked
  against a real `obs.Metrics` by `internal/deployment/observability_test.go` -- names,
  label names, label values read out of the Go source, and the `le` of the latency SLI
  against the histogram's real boundaries -- and `make check-rules` runs promtool so every
  alert has been seen to fire. The quiet case in `observability/rules_test.yaml` is
  deliberately a *healthy but imperfect* service: with no failures in the fixture the
  failure ratios go empty rather than small, and an empty expression fires nothing however
  wrong the threshold is.
- **A refusal at the HTTP edge answers before `chat.Service.Turn` runs, so no turn metric
  moves for it.** The daily budget (503), both rate limiters (429), a missing session (401)
  and somebody else's conversation (404) are all decided in `internal/httpapi/identity.go`,
  and for a day the service could have refused every customer it had while
  `chat_turns_total` stayed flat and green. `chat_edge_refusals_total{reason}` closes it:
  four reasons, **no subject and no conversation id** — a refusal counter is where an
  attacker would choose the label values. Anything else added at that edge needs its own
  increment, because nothing downstream will count it.
- **The assistant declining is not detectable without a second model call, and a phrase
  list must not stand in for one.** `docs/evaluation.md` has the three times a phrase list
  measured wording here. The observable half is the escalation tool, it undercounts, and
  `docs/safety.md` says by what. Moderation is decided against with an argument, not
  forgotten.
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
- **`grep` in this shell is ripgrep, which skips anything `.gitignore` matches.** A
  repo-wide search for a variable name silently missed `k8s/examples/secret.yaml` — a file
  that was on disk, referenced by a README, and not in the repository, because a bare
  `secret.yaml` ignore rule matched it too. Use `command grep`, `git grep` or `rg --no-ignore`
  when the question is "does this exist anywhere", and remember that the filesystem is not
  the repository: `TestEveryPathTheKubernetesReadmeDrawsIsInTheRepository` asks git,
  because an `os.Stat` would have passed the whole time.
- **`ADMIN_CORS_ORIGINS` has no wildcard, and must not grow one.** It permits *other
  pages reading the support inbox*. Origins match whole (a prefix match accepts
  `ops.example.com.evil.test`), `Vary: Origin` goes on every response that could have
  carried the header, CORS wraps authentication rather than the reverse (a preflight
  carries no `Authorization`), and every route needs an `OPTIONS` twin because Go's mux
  matches on method. All six were forced red in `internal/admin`.
- **The UI's dev server must not proxy the API.** A proxy makes development same-origin
  and production cross-origin, which is the difference that hides a CORS mistake until it
  ships. `VITE_API_BASE` points at the real API instead.
- **nginx does not inherit `add_header` into a location that sets one of its own.** The
  UI's `Content-Security-Policy` was declared once at server level and was absent from
  `GET /` — the only response that matters. The headers are an `include` in every
  location; check with `curl -I` on each path, never by reading the config.
- **`config.js` is written at container start-up, under `/tmp`.** Baking the API base in
  at build time means a rebuilt image per environment, and writing it into the web root
  fails as a non-root user and again under a read-only root filesystem.
- **Two contracts now live in two languages.** `NEXT_STATES` in
  `admin-ui/src/api/types.ts` mirrors `allowedTransitions` in `internal/ticket/admin.go`,
  and the no-markup rule is React's default rather than a hand-written renderer.
  `internal/deployment/frontend_test.go` reads the TypeScript from Go for both, because
  nothing re-derives a translation.
- **`kubectl exec ... | grep -q` is broken under `pipefail`** when the exec's own exit
  code is non-zero — which is exactly what a successful "this must fail" assertion
  produces. `verify.sh` has `exec_in_pod` for this; it cost a red assertion against a pod
  that was demonstrably correct.
- **Check that a port is yours before trusting what answers on it.** Two implementations
  of this system run on one machine and both put admin surfaces on nearby ports. A server
  started on an occupied port exits, and a `curl /healthz` loop then passes against the
  *other* session's service — which is how a readiness check came back 200 for a process
  that was not running. `lsof -nP -iTCP:<port> -sTCP:LISTEN` first, and confirm the pid is
  still alive rather than only that something answers.
- **Measure a filtered vector search with `EXPLAIN (ANALYZE)` and read `Rows Removed by
  Filter`, not the row count.** An HNSW scan spends candidates on rows the filter rejects;
  eight results say nothing about how close the scan came to running out, and the number of
  rejected rows is the margin. Two separate sessions reached wrong conclusions here by
  reading the result count — one because the planner had quietly chosen a sequential scan.
- **A score is not a measurement until the harness has been seen to produce a bad one.**
  `make eval` scores 35/35; `make eval-control` runs the same cases with no corpus and
  scores 15/35. Without the second number the first says only that a large model sounds
  plausible. Keep the control working when adding cases.
- **Never `git checkout <file>` to undo a temporary edit.** It discards *everything*
  uncommitted in that file, not the perturbation you just made. It ate uncommitted work
  three times in one session -- twice during red-tests, and once silently enough that a
  README row was missing from a commit and only found two commits later. Copy the file to
  the scratchpad first and copy it back.
- **An operator's reply goes into `chat_memory`, not only onto the ticket.** The model's
  next turn composes from that history; without it the assistant tells the customer to wait
  for a human who has already answered, in the same conversation where the answer is
  sitting. Attribute it in the text (`alex (support): …`) -- there is one assistant role and
  the customer cannot see a column.
- **The handoff webhook carries no customer text, and its failures are rows.** It is a
  destination outside this service's control. A notification fails silently by nature, so
  `handoff_delivery` records every outcome and the overview shows the undelivered count;
  and delivery never fails the reply, because the customer's answer must not depend on a
  chat room being up.
- **An erasure must never touch `admin_audit`, and must write to it.** An audit row the
  subject of the audit can erase is not an audit row. `internal/retention` deletes
  conversations and *redacts* tickets — a deleted `OPEN` ticket erases an obligation along
  with the words that asked for it — and the entry it writes names what it removed, because
  "somebody erased something" records that it happened and not what.
- **A pgvector GUC does not exist until the extension's library is loaded in that session.**
  `SHOW hnsw.iterative_scan` in a fresh psql session fails with *unrecognized configuration
  parameter*, which reads exactly like "this version does not have it" -- and that wrong
  conclusion was published here before a peer corrected it. Run any vector operation, or
  `LOAD 'vector'`, before asking.
- **Saving a knowledge entry and publishing it are different actions, and must stay
  visibly different.** A save changes a draft; a publication changes what customers are
  told. `knowledge_entry` is seeded from the bundled corpus once, by count rather than by a
  flag -- if there are drafts at all somebody has edited, and re-seeding resurrects what
  they deleted.
- **A published version's name carries random bytes, not just a timestamp.** Two
  publications inside one second collided on the document primary key with a raw constraint
  violation; a double-clicked button is enough. Clock resolution is not a uniqueness source.
- **Retrieved passages are labelled as documents rather than instructions, and that is
  argued rather than evidenced.** `make eval`'s injection-in-a-corpus-entry case passes with
  and without the wording -- the probe is too weak to discriminate, which is not the same as
  the wording being useless. What would actually bound it is constraining tool calls by the
  caller's identity, which is not built.
- **The bundled corpus is adopted as the first version, never re-embedded.** Its vectors
  are what every retrieval number in this pair was measured against. `AdoptBundled` stamps
  `corpus_version` on the rows already there and is a no-op once a version is active; a
  second adoption would stamp published documents with the bundled name.
- **Retrieval reads one active version, and `corpus_version IS NULL` is the deploy path.**
  A database mid-rollout, or a test that ingests without versioning, keeps working instead
  of answering nothing. Do not tighten that predicate without a plan for both.
- **Clear `faq_document` with `TRUNCATE`, never `DELETE`.** An HNSW scan collects its
  candidates from the graph and only then drops the dead ones, so rows deleted by previous
  ingestions crowd out the live ones and a `LIMIT 8` search quietly returns fewer than
  eight — measured at 2 of 8 after 60 reloads with autovacuum off. `TRUNCATE` rebuilds the
  index empty. `TestRetrievalSurvivesManyCorpusReloads` pins it and needs sixty cycles:
  thirty passed either way.
- **A check that cannot be seen to fail is a claim, not a check.** `k8s/README.md` keeps an
  inventory of which harness assertions have actually been observed red. Three separate
  detectors in this repository have been silently blind — a `CREATE EXTENSION` check whose
  condition never arose, a capacity check that parsed nothing and passed, and a regex that
  measured the language rather than the bug. Before trusting a green check, make it red.
- **Ticket dedupe and the per-conversation cap are database guarantees, not process
  state.** A transaction with an advisory lock plus a unique index on
  `(conversation_id, dedupe_key)`. Do not reintroduce an in-memory fast path: the cap was
  `replicas x 3` for as long as it lived in a map, and the test that proves it now gets
  seventeen tickets instead of three when the lock is removed.
- **`turn` is the operational record and `chat_memory` is the model's context.** They are
  not interchangeable: the second is windowed. Write the turn record at the service
  boundary, never from the event stream — that stream feeds a page which may already be
  gone. A failure to open the record fails the turn; a failure to close it is logged.
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
- **A customer's rating is scoped by the turn's conversation, not by the turn id.** The id
  is in the usage event and reaches the browser; what refuses somebody else's turn is
  resolving it to a conversation and checking the session owns that. A turn that is not
  yours and a turn that does not exist return the same 404, on the same rule as the
  conversation endpoints. Ratings have their own rate-limit bucket at the turn limit's
  number: sharing the bucket lets a rating spend a turn the customer has not had.
- **A `fetch` whose response body is never read is reported by Chrome as
  `net::ERR_ABORTED`.** The request succeeded and the row was written; only the report was
  wrong. Measured by calling one 204 endpoint twice from one page, read and unread. Drain
  the body — the script that drives a page fails on any failed request, and a check that
  cries wolf is a check somebody turns off.
- **Read a page with `innerText`, not `textContent`.** `textContent` has no block
  boundaries, so two correctly rendered `<p>` elements come back run together and look
  exactly like the two-model-calls-run-together defect this repository already had. That is
  the second detector here to measure its own reading rather than the page.
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
| Answer quality: 35–36/36 over ten runs, and 15/35 with no corpus | `make eval` and `make eval-control` |

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

**The customer rating, driven in headless Chrome on 2026-09-07** against `claude-opus-5`
and a real Postgres: the three buttons appear on the usage card, clicking one writes the
row — read back out of the database, `customer/wrong`, attributed to the session's subject
— and the page says so. That run found the `ERR_ABORTED` above.

## Driving a page in a browser

Neither the claude-in-chrome extension nor the MCP Playwright server attaches in this
workspace. The way through, from the Java implementation's session: a plain Node script
importing Playwright from the npx cache (`~/.npm/_npx/<hash>/node_modules/playwright`) and
launching with `executablePath` pointed at the installed Chrome. The cached
`ms-playwright` Chromium builds are the wrong build numbers for that package, which is
what the confusing "Executable doesn't exist" error actually means.

Worth the trouble every time it has been done. On the demo page it found two defects
nothing else could: the model emits markdown and the page was showing the asterisks, and
the text of two model calls was run together. On the admin page it found the same markdown
defect again — this time showing an operator a ticket number the customer had seen in
bold — and one that was visible only with the dialog open: the resolution box is filled
from the row, so a reopen resubmits the old conclusion, and the store was keeping it, which
left a ticket `IN_PROGRESS` and still claiming to be concluded.

**Do this for any page after changing it.** Four of the defects in this repository lived
where a person looks and nowhere a test can reach; the data was correct at every seam. The
script that does it is a few lines — sign in, click each tab, read `#main`'s text, take a
screenshot, and fail on any console error, page error or failed request.

## Scope

No multi-tenancy, no MCP. No Gemini — three providers, and `CHAT_PROVIDER=gemini` fails at
startup by name. Authentication exists only for `/admin`: the chat endpoints are open, and
an operator login is not customer identity.

**The operations surface is two applications.** `internal/admin` serves
`/api/admin/v1/*` and no page; `admin-ui/` is a React/TypeScript bundle on its own image
and its own origin. With `ADMIN_TOKENS` unset the API's routes are never registered — a
404, not a guarded 401. Never change that to "register the routes and reject at the
guard": a guard can be misconfigured and an absent route cannot. It is the one surface
that displays customer text on purpose, which is why reading a conversation writes an
audit row and why refused actions do too.

**Knowledge editing and publication are built, and the corpus is still the fixture.** The
bundled `corpus/faq.json` is *adopted* as the first version rather than re-embedded, so
every retrieval number in this pair still refers to the same vectors; a publication writes
a new version and switches one row. Do not wire a Publish button to the startup importer —
that was the shape that looked finished and was not, and `docs/knowledge.md` says what
replaced it.

`k8s/` exists and every number in it was measured on kind by `k8s/kind/verify.sh` before
being committed — the Java repository's manifests were committed unapplied and two were
wrong, which is the reason the harness is there. If you change `resources`, re-run the
sweep; and remember that on Go the CPU limit sets `GOMAXPROCS`, which sets the embedding
concurrency bound.
