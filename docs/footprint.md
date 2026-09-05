# Footprint

What the two implementations cost to *run*, as opposed to how fast they run. Both are
measurements; they answer different questions and neither substitutes for the other.

> **This is not the benchmark.** [The benchmark](benchmark.md) measures throughput and
> latency under a stated load — 1000 concurrent requests, a stubbed model, the full
> production path. This page measures the resources a pod holds. A memory number with no
> stated moment is meaningless, so every row below says *when* it was taken: at rest after
> startup, or at the startup peak.

## Memory

Measured on a kind cluster, `replicas: 2`, one node, from each container's own cgroup.
Both columns are `/sys/fs/cgroup/memory.stat` and `memory.peak`, taken the same way on
both sides.

| | Go | Java |
| --- | --- | --- |
| `anon` at rest — the real requirement | **951 MiB** | 1409–1527 MiB |
| `file` at rest — page cache, reclaimable | 124–379 MiB | 10–18 MiB |
| `memory.current` at rest | **1082–1337 MiB** | 1437–1547 MiB |
| `memory.peak` — what a limit must accommodate | **1394–1655 MiB** | 2874–2889 MiB |
| peak ÷ current — the startup spike | **1.24×** | ~1.9× |
| OOMKilled at | 1152Mi and below | 2560Mi and below |
| Deployed `requests` / `limits` | 1536Mi / 2Gi | 3Gi / 4Gi |
| Image | 1.1 GB | 1.92 GB |

**The ratio depends entirely on which row you read, and that is the finding.**

At rest the two are close: 1.1–1.2× on `memory.current`, and about 1.5× on the process
memory that is actually required. At the peak they are 1.8–2.1× apart. The gap between
those two ratios is the JVM's startup spike — its peak is roughly double its steady state,
where this one's is a quarter above. A comparison that quotes only the peak makes the
runtimes look twice as far apart as they are once running; one that quotes only the steady
state hides the number a Kubernetes limit actually has to survive.

Both are true. Neither is "the memory usage".

**Two things that were believed and are not true.**

*It is not page cache.* An earlier version of this page attributed the difference to the
470 MB model file being charged as page cache. Both sides then measured `anon` and `file`
separately: the Java side is 10–18 MiB of file cache, essentially none, and its peak is
almost entirely anonymous. On this side `file` varies from 18 to 379 MiB between replicas
and over a pod's life — real, reclaimable, and not the explanation for anything.

*Neither implementation maps the model.* The hypothesis was that the Go binding `mmap`s
the file while DJL reads it into native buffers. Checked: `/proc/1/maps` in the Go pod
contains **no mapping naming model.onnx at all**. Both read it into anonymous memory. The
`file` difference is unexplained, and is labelled unexplained rather than given a story.

## Startup

| | Go | Java |
| --- | --- | --- |
| Time to `Ready` | **4.4 s** | not published |
| CPU consumed to reach `Ready` | **8.0 s** | not published |
| Corpus ingestion (36 documents) | 3.0 s throttled to 2 CPU, 166 ms unthrottled | not published |
| Startup probe budget in the manifest | 30 × 2 s = 60 s | 30 × 5 s = 150 s |

The probe budgets are the two authors' estimates rather than a measurement of either
runtime, but the difference in what each thought it needed is itself informative.

## CPU at rest

Both sit at approximately zero: a chat backend spends its life blocked on a model API, and
neither runtime burns CPU waiting. This row exists to say that a "CPU usage" comparison at
rest measures nothing, and that the CPU number worth comparing is under a stated load —
which is [the benchmark](benchmark.md), where Go serves 600 req/s against Loom's 500 on
the same path and machine, and spends 3–7× the OS threads doing it.

## Why the Go side needs a CPU limit and the Java side does not

The Java manifest sets no CPU limit deliberately, reasoning that startup is CPU-hungry and
throttling it makes the startup probe flap. That is sound for a JVM.

On Go it has a second consequence. Go 1.25+ derives `GOMAXPROCS` from the cgroup CPU
limit — verified in the pod: 18 CPUs on the node, `limits.cpu: "2"`, `GOMAXPROCS=2` — and
`GOMAXPROCS` is what the in-process embedding concurrency defaults to. A goroutine inside
a cgo call blocks an OS thread, so removing the CPU limit does not only remove a throttle;
it silently raises the embedding concurrency bound to the node's core count. See
[k8s/README.md](../k8s/README.md#the-go-specific-part-gomaxprocs-is-the-embedding-concurrency).

---

[← Back to the README](../README.md)
