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

### Attributes are not the only way into a backend

The check above searches for customer *text*. A cross-review from the Java implementation
pointed out that it was the wrong question — or rather, too narrow a one. Every
`SetAttributes` call in this repository carries a literal, a model id, a count, a tool
name or the conversation id, and none of that is customer-typed. But **a span name is
also an aggregated dimension**, and so is a metric label.

The tool name is written by the model. It reached both — `tool <name>` as a span name and
`{tool=<name>}` as a Prometheus label — including on the branch taken precisely when the
name is one the model invented. That is unbounded cardinality arriving through a
different door than the one `metrics.go` carefully shut against conversation ids, and it
is attacker-influenced rather than merely unbounded: a retrieved passage can carry an
instruction to call a tool that does not exist.

The name is now validated against the tool table before it can become either, and the
literal `unknown` is emitted when it does not match. The model is still told the name it
asked for, in the tool result, because it needs that to recover.
`TestAnInventedToolNameNeverBecomesAMetricLabel` sends a 200-character invented name and
asserts exactly one bounded label value.

The general form: **ask what reaches the backend, not what is in the attribute list.**

### Metrics

```
chat_tokens_total{model="claude-opus-5",type="input"}
chat_cost_usd_total{model="claude-opus-5"}
chat_unpriced_model_calls_total{model="gpt-5-2025-08-07"}
chat_model_calls_total{model="claude-opus-5",outcome="success"}
chat_turns_total{outcome="completed"}
chat_turn_duration_seconds{model="claude-opus-5"}
chat_tool_invocations_total{tool="lookup_order_status",outcome="found"}
chat_handoff_notifications_total{type="ticket.created",outcome="failed"}
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

`chat_handoff_notifications_total{type,outcome}` is the newest of them and exists for the
same reason. A webhook that does not arrive is silent by nature: `handoff_delivery` has
recorded every outcome as a row since the handoff loop was built, and the operations
overview shows the undelivered count in red, but both need a person to go and look. The
counter is those same events where an alert can reach them. It is per process and resets
when a replica restarts, which the rows do not — the row is the record, the counter is the
smoke detector. `TestAnUndeliveredNotificationIsCountedAsWellAsRecorded` drives the real
notifier at a destination that refuses and then at one that accepts, because an assertion
made against the counter itself would prove only that a counter counts; it was forced red
by incrementing on delivery alone.

### An SLO on the turn

Two objectives, both on the customer's turn rather than on any component of it:

| | Objective | Measured as |
| --- | --- | --- |
| Success | **99%** of turns end `completed`, over 28 days | `chat_turns_total{outcome="completed"}` against all outcomes |
| Latency | **95%** of turns finish inside **16 s**, over 28 days | `chat_turn_duration_seconds_bucket{le="16"}` against `_count` |

**Both numbers are chosen, not measured, and that is the most important thing about
them.** There is no production traffic behind this repository, so nothing here was derived
from a distribution of real turns. What the numbers *are* anchored to is what has been
observed:

- A real tool-calling turn read out of Jaeger took **11.1 s** — 7 ms of retrieval and two
  model calls for the rest (the trace is above).
- The demo page, timed in a headless browser, had the usage card at **5.065 s** on a
  tool-calling turn.
- `make eval` runs 35 cases in **2m08s**, a mean of about 3.7 s a turn against the real
  model.

So 16 s is roughly three times a typical two-call turn and above every turn anyone here
has watched — and it is a boundary the histogram already has, which matters (below). The
99% is convention rather than evidence: it is the number people usually start at, and the
first real week of traffic should replace it with one derived from the distribution
instead. **When that happens, re-derive both — do not edit the numbers.**

Two deliberate choices in how the SLI is computed:

- **The latency SLI counts observations inside a bucket, rather than taking
  `histogram_quantile`.** 16 is an actual edge of the histogram in `internal/obs`, so the
  ratio is exact; a quantile interpolates between edges and would make the objective
  depend on how the buckets happen to be laid out. The cost is that the objective can only
  be set at a boundary, which is a fair trade for a number people will argue about.
- **The failure ratio counts failures, not the shortfall from success.** Written as
  `1 - completed/all`, the expression is *empty* when nothing completes at all — the
  `completed` series does not exist, the division is empty, and `1 - empty` is empty — so
  the one case that would not alert is a total outage. Counting the failures has no such
  hole. This is asserted rather than argued: `observability/rules_test.yaml` has a case
  with only failing turns and no completed series, and the fast-burn alert fires on it.

### Alerts

`observability/prometheus-rule.yaml` is a `PrometheusRule` for the prometheus-operator,
and `observability/servicemonitor.yaml` is what makes the operator scrape this app at all
— the `prometheus.io/*` annotations on the Deployment are for a hand-configured
Prometheus, and the operator ignores them.

| Alert | Fires when | Why it is here |
| --- | --- | --- |
| `TurnFailureBudgetBurningFast` / `Slow` | >14.4% of turns fail over 1 h; >6% over 6 h | The success objective, at the two burn rates that spend a 28-day budget in two days and in five |
| `TurnsSlowerThanTheObjective` / `FarSlower` | >10% of turns over 16 s for 30 m; >25% for 10 m | The latency objective: a drift and a step change |
| `AppTargetDown` | `up == 0` for 5 m | Not that the app is unhealthy — that every other rule here has no data |
| `UnpricedModelCalls` | any, in an hour | A permanently-zero cost meter, which reads as a cheap month |
| `ProviderCallsFailing` | >5% of model calls error over 10 m | The provider, and there is no failover |
| `BudgetExceededTurns` | >3 turns refused in 15 m | Customers being cut off by the per-conversation token budget |
| `TurnsHittingTheToolRoundCap` | any, in an hour | The model still wanted a tool; the customer may have got nothing, which looks like a completed turn |
| `HandoffNotificationsUndelivered` | any, in 15 m | A ticket was raised and the destination was never told |
| `CostBurnRate` | >$5/hour for 15 m | A placeholder threshold; set it from the invoice you are prepared to explain |

Thresholds are starting points on the same terms as the SLO: `>$5/hour` and `>3 refused
turns` are numbers to argue with once there is traffic, not measurements.

### The rules are checked against the application, and the check was forced red

An alert naming a series nobody emits never fires. It is applied, healthy, listed by the
operator, and worth nothing — indistinguishable from a service with no problems, which is
the same shape as the three silently blind detectors `CLAUDE.md` records.

So `internal/deployment/observability_test.go` reads every expression in the manifest and
checks it against a real `obs.Metrics`: the metrics are exercised through a real registry
and read back out of a real `Gather`, so the names, label names and bucket boundaries it
compares against are the ones a scrape would produce rather than a list somebody wrote
down. Four things are checked, and each was forced red before it was believed:

| Perturbation | What it printed |
| --- | --- |
| `chat_unpriced_model_calls_total` → `chat_unpriced_model_calls` | *UnpricedModelCalls names chat_unpriced_model_calls, which this application does not emit* — followed by the ten it does |
| `{outcome="budget_exceeded"}` → `{result="budget_exceeded"}` | *that metric has no such label. It has: outcome* |
| `budget_exceeded` → `budget-exceeded` | *the code never writes that value into outcome. It writes: budget_exceeded, completed, failed, tool_limit* |
| `le="16"` → `le="15"` | *chat_turn_duration_seconds has no such bucket boundary. The selector matches no series at all, so the alert can never fire* — with the nine boundaries listed |

The third of those needs explaining, because there is no runtime way to ask a `CounterVec`
what values it *could* take. The label values come from the Go source: the literals passed
to `WithLabelValues`, plus the literals assigned to the variable that is passed, which is
how `outcome` is written in `chat.Service.Turn`. A matcher whose value cannot be resolved
that way is a **failure**, not a pass — an unverifiable matcher is exactly the one worth
verifying.

And the check was shown not to be vacuous, which is a separate question from whether it
can fail:

- Pointed at a manifest with no `groups:`, it reports *only 0 alerts were read*, *only 0
  metric references were extracted*, *only 0 label values were checked*, and *no le=
  matcher was checked* — four separate guards, because a check that finds nothing to
  disagree with agrees with everything.
- With the source scan crippled, it reports *the source scan found 0 values for
  chat_turns_total's outcome label; chat.Service.Turn writes four*, and every value
  matcher degrades to *this test cannot tell*, loudly, rather than to silence.
- With the metric-exercising loop crippled, it reports *chat_turns_total is registered but
  produced no sample*. A new metric that this loop does not know how to exercise fails the
  test rather than being skipped.

The same file checks the wiring that makes all of this reach Prometheus, because none of
it produces an error anywhere when it is wrong: the `PrometheusRule` and the
`ServiceMonitor` are in the app's namespace, the monitor's selector matches the app
Service's labels (**both** of them — the operations UI Service carries the same
`app.kubernetes.io/name` and serves no `/metrics`, so a name-only selector adds a target
that is permanently down), the scraped port name exists on the Service, the scraped path
is a route `cmd/server/main.go` actually registers, and `up{job="ai-customer-service-go"}`
is the job the monitor's `jobLabel` will produce. Forced red by pointing `jobLabel` at
`app.kubernetes.io/component`: *AppTargetDown matches job="ai-customer-service-go"; the
ServiceMonitor's jobLabel (app.kubernetes.io/component) makes it "app"*.

### Every alert has been seen to fire

The Go test proves the rules name real series. It cannot prove they fire: an expression can
be valid, name real metrics, and still have the comparison the wrong way round.

```
make check-rules        # promtool, from the Prometheus image, in a container
```

`observability/rules_test.yaml` drives each alert with synthetic series through
Prometheus's own `promtool test rules`: **11 rules parse, and all 11 alerts fire on data
that should trip them.** The assertions are written against the `ALERTS` series rather
than with `alert_rule_test`, which compares annotations word for word and would put a
second copy of every description in the test file.

The half worth more than the firing cases is the quiet one: a *healthy* service — about
one turn in a hundred failing, one model call in a hundred erroring, every turn inside
16 s, $3/hour — raises nothing at all. That case is why the healthy fixture is not a
perfect one. With no failures in it, the failure ratios go **empty** rather than small,
and an empty expression fires nothing however wrong the threshold is; the first version of
this test passed happily against a `ProviderCallsFailing` rule loosened to `> -1`.
Loosening it to `> 0.005` instead now fails, printing the alert it should not have raised.

It is not in CI, for the reason `make bench` is not: it needs a container image pulled on
every run, and the Go test — which is in CI — is the half that catches the failure this
whole file is about.

### Applying it

```sh
# Needs the monitoring.coreos.com CRDs: kube-prometheus-stack, or the prometheus-operator.
kubectl apply -f observability/servicemonitor.yaml
kubectl apply -f observability/prometheus-rule.yaml
```

These two live outside `k8s/` on purpose. `kubectl apply -f k8s/` is the documented way to
deploy this, and it would fail on both files on any cluster without those CRDs — including
the kind cluster `k8s/kind/verify.sh` builds, where the operator is not installed. The
manifests are checked against `k8s/` by the test above rather than by being next to it.

Both carry `release: kube-prometheus-stack`, which is the label that chart's default
selectors look for. **Check what your Prometheus actually selects on** rather than
trusting that default: a `PrometheusRule` the operator does not select is applied,
healthy, and evaluated by nobody — the same silence as an alert that names nothing.

### What nothing watches, and why

Named rather than left to be discovered, in the order they would hurt:

- **A refusal at the HTTP edge emits no metric at all.** The daily token budget
  (`DAILY_TOKEN_BUDGET`, a 503) and the rate limiters (`TURNS_PER_MINUTE`,
  `SESSIONS_PER_HOUR_PER_IP`, a 429) both refuse *before* `chat.Service.Turn` runs, so no
  turn is counted and `chat_turns_total` does not move. The service can be refusing every
  customer it has while every meter in this document stays flat and green. There is a
  `slog.Warn` per refusal and nothing else. **This is the largest hole in this page**, and
  closing it is instrumentation at the edge (`internal/httpapi`) plus its own alert, not
  another rule over the existing series.
- **No alert on the audit trail.** The original sketch of this item wanted one on
  `admin_audit` not growing while operators are working. There is no metric for it — the
  trail is rows, read through the operations API — and "operators are working" is not a
  signal this service has. Left undone deliberately rather than approximated.
- **A turn a dead process left behind stays `in_flight` for ever** (production-readiness
  item 15). It is not counted as a failure, so it does not spend the error budget; it sits
  in the operations overview as though the customer had walked away.
- **No Grafana dashboard**, and no Grafana. The Java implementation has both, with a
  Compose profile that runs the stack; nothing in this repository runs Grafana, and a
  dashboard nothing here can open is an artifact that rots without anyone noticing. The
  metric-name test would cover one the day one is added — that is the condition, not the
  dashboard.
- **The handoff counter is per process.** It resets on restart and it is not written by a
  replica that was not the one delivering. `handoff_delivery` is the durable record; the
  counter is only how an alert learns about it.
- **None of the thresholds are measured.** Said once more here because it is the thing
  most likely to be forgotten: they are starting points, and the first week of real
  traffic should replace every one of them.

---

[← Back to the README](../README.md)
