# Goroutines, measured — and what cgo does to them


The Java implementation of this system published a benchmark to justify running on
virtual threads. This is the same benchmark on the same machine, so the two can be read
side by side: **1000 concurrent requests, a stubbed model with a fixed 1000 ms delay, the
full production request path** — validation, conversation memory in Postgres, query
embedding, a pgvector search, tool definitions and metrics — and one fresh conversation
per request.

```
make bench
```

Apple M5 Max (18 cores), Go 1.26.1, `GOMAXPROCS=18`. The load driver shares the process,
as it did in the Java measurement.

| runtime | wall | req/s | p50 | p95 | p99 | OS threads |
| --- | --- | --- | --- | --- | --- | --- |
| Java, platform threads | 6254 ms | 160 | 4037 ms | 6118 ms | 6174 ms | 202 Tomcat / 246 total |
| Java, virtual threads | 2000 ms | 500 | 1616 ms | 1955 ms | 1986 ms | 2 Tomcat / 52 total |
| **Go, goroutines** | **1667 ms** | **600** | **1648 ms** | **1663 ms** | **1665 ms** | **13 → 135** |
| Go, goroutines, embedding bounded | 1876 ms | 533 | 1448 ms | 1845 ms | 1871 ms | 13 → 40 |
| Go, goroutines, embedding stubbed | 1156 ms | 865 | 1128 ms | 1152 ms | 1154 ms | 12 → 27 |

Peak goroutines is ~5,000 in every Go row: one per in-flight request plus the driver's
own, and they cost about 8 KB of stack each rather than an OS thread.

Read the ratios, not the absolute timings. Run-to-run variance is a few tens of
milliseconds on wall time and much larger on the thread count — see below.

### The headline is not the throughput

Go serves the same workload about 20% faster than Java's virtual threads (1667 ms
against 2000 ms) and holds a far tighter latency distribution: p50, p95 and p99 land
within 17 ms of each other, against Java's 370 ms spread. Both are a long way from
platform threads, which hit the pool ceiling at 200 and queued the remaining 800 into
four more waves.

The interesting number is the thread count, and it goes the *other* way. Java's
virtual-thread run held a thousand in-flight requests on 52 platform threads. Go's
equivalent run reached 135 OS threads on the run above — and 276 on another run of the
same code. That is not noise in the measurement; it is the measurement.

### Where the threads come from, and why the number moves

A goroutine waiting on the network parks and costs no thread. A goroutine waiting on the
stubbed model's `time.Sleep` parks and costs no thread. The third row of the table is
what happens when the embedding model is stubbed out: the whole path — HTTP, Postgres,
pgvector, metrics — holds a thousand concurrent requests on **27 OS threads**.

The rest is cgo. **A goroutine inside a cgo call blocks the OS thread it is running on,
and the Go scheduler's answer to a blocked thread is to create another one.** The
in-process embedding model is a native call, roughly 2 ms of CPU per query, and under a
thousand simultaneous arrivals the runtime spins up threads for as many of them as
happen to overlap. How many overlap depends on scheduling, which is why the count ranges
from 135 to 276 across runs while the wall time barely moves.

This is the same in-process ONNX model the Java implementation runs, and Java does not
have this problem — not because the JVM is cleverer about native calls, but because it
is *less* clever. A native call from a virtual thread pins its carrier, and the carrier
pool is bounded at the core count by default, so the pinning calls queue against a fixed
set of threads. Go has no such bound: its scheduler treats "this thread is blocked" as a
reason to make another thread, up to 10,000 of them.

So the two runtimes sit at opposite ends of the same trade-off, and neither is wrong.
Go's default spends threads to keep every core busy; the JVM's default caps threads and
queues.

### Asking Go for the JVM's behaviour

The fourth row is the same code with the embedding calls bounded to `GOMAXPROCS`
([`rag.Bounded`](../internal/rag/bounded.go), which is what the server runs). The work is
CPU-bound, so admitting more goroutines than there are cores was never buying
throughput — only threads.

| | unbounded | bounded to 18 |
| --- | --- | --- |
| OS threads | 135–276, varying | **40, stable** |
| req/s | 600 | 533 |
| p50 | 1648 ms | **1448 ms** |
| p95 | 1663 ms | 1845 ms |

Threads drop by a factor of three to seven and stop moving between runs, for about 11%
of throughput. The p50 improvement was not the goal and is worth naming: a queue forms,
most requests get through it sooner, and the tail gets longer. That is the ordinary
shape of admission control, and it is the same shape the JVM's bounded carrier pool
produces for free.

The bound is on by default because an unbounded thread count under a load spike is a
resource-exhaustion shape, and 11% of throughput on a path whose real cost is a
multi-second model call is not the constraint. `EMBEDDING_MAX_CONCURRENCY` changes it;
`0` means `GOMAXPROCS`.

### A constant delay flatters everything

Every row above uses a fixed 1000 ms model delay, because that is what the Java
implementation published and the comparison is the point. It is also unrealistic in a
specific way: when every request takes exactly as long as every other, no request ever
waits behind a slow one. Real model latency is heavy-tailed.

The same run with the delay drawn from `300 ms + Exp(mean 700 ms)`, capped at 8 s — the
same 1000 ms mean, a median near 785 ms, and a tail of several seconds:

| | wall | req/s | p50 | p95 | p99 | OS threads |
| --- | --- | --- | --- | --- | --- | --- |
| fixed 1000 ms | 1667 ms | 600 | 1648 ms | 1663 ms | 1665 ms | 13 → 135 |
| heavy-tailed, same mean | 4544–5963 ms | 168–220 | 1372–1489 ms | 2933–3157 ms | 3800–4586 ms | 13 → 101–133 |

**Read only p50 and p95 from that second row.** Wall time and req/s are not throughput
when the delay is heavy-tailed: a thousand requests finish when the slowest one does, so
"wall" is measuring the worst draw from the distribution and "req/s" is measuring the
same thing upside down.

What the row does say is worth having. p50 *improves* — 1372 ms against 1648 ms — because
most requests draw a shorter delay than the constant one, and the queueing that a
constant delay hides is not, at this concurrency, the dominant effect. And the OS thread
count *falls*, to 101–133 from 135–276.

That last one sharpens the cgo finding rather than softening it: **the thread count is a
function of how concentrated the arrivals are, not of how many there are.** Spreading the
same thousand embedding calls across 5 seconds instead of packing them into 1.7 means
fewer of them are inside the native call simultaneously. The 276-thread figure is the
worst case — a thousand genuinely simultaneous arrivals — and a realistic latency spread
roughly halves it. It also means the number will not reproduce if your traffic has any
shape to it at all, which is a good reason to bound it rather than to tune a limit to a
measured peak.

### What this is not

The model is stubbed, so this measures scheduling rather than an assistant. Everything
else is the production path, which is why the fastest row still takes 1.16 s for an
operation that sleeps for 1.00 s — the retrieval work is real work.

Two measurement mistakes are worth recording, one inherited and one new:

**The thread counter never goes down.** Go's `threadcreate` profile counts threads
*created* over the life of the process. Running two variants in one process makes the
second one inherit the first one's threads and report 112 before serving a request. The
variants run in separate processes for that reason, which `make bench` does by invoking
`go test` three times. The Java implementation hit the same class of error from the other
direction: its test-context cache kept two servers alive, and the idle one's 200-thread
pool was counted against whichever run went second.

**Whole-process counts include the load driver.** Go exposes no per-component thread
count — there is no Tomcat pool to interrogate — so these numbers are an upper bound on
what serving costs. The stubbed-embedding row is the control that makes them
interpretable: the driver and the entire non-cgo path together account for 27 threads,
and everything above that is the embedding model.

---

[← Back to the README](../README.md)
