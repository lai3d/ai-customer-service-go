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

| | Go | Java |
| --- | --- | --- |
| Process memory (`anon`), at rest | **960 MiB** | — |
| Process RSS, at rest | **1004 MiB** | 1.65 GiB *(published, "steady state")* |
| cgroup peak, including page cache | **1347–1527 MiB** | 2.82 GiB *(published, "peak RSS")* |
| OOMKilled at | 1152Mi and below | 2560Mi and below |
| Starts at | 1280Mi (99% of limit) | 3Gi (94% of limit) |
| Deployed `requests` / `limits` | 1536Mi / 2Gi | 3Gi / 4Gi |
| Image | 1.1 GB | 1.92 GB |

Roughly half, on every row. The dominant term in both is the same 470 MB fp32 ONNX model
loaded into the process; the difference is everything around it.

**Two caveats, and the second is why the Java column is in italics.**

*Page cache is not a fixed cost.* The Go rows show 1347 MiB in one replica and 1527 MiB in
the other, from the *same deployment*: `anon` is 960 MiB in both, and the difference is
page cache for the model file, charged by the kernel to whichever cgroup faulted it in
first. It is reclaimable. A cgroup peak is therefore an upper bound that depends on which
replica you look at, which is why the Go column gives three numbers rather than one.

*The Java numbers are published by that repository, not re-measured here*, and its README
calls them "peak RSS" without saying whether that is the process's RSS or the cgroup's
working set. Those differ by 300–500 MiB on the Go side, so the honest reading is that Go
uses roughly half as much by either definition — not that the exact ratio is known.
Re-measuring both under one harness is the way to settle it and has not been done.

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
