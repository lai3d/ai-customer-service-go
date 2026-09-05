# Kubernetes manifests

Namespace, ConfigMap, Service, Deployment. No Postgres and no Secret — see
[Before you apply](#before-you-apply).

These were written against the Java implementation's manifests, which are good and worth
reading. What is *not* copied is the part that matters: every number here was measured on
a kind cluster before it was committed. That repository's README says its manifests were
committed without ever being applied and that two of them were wrong; copying the files
without copying the harness would have been copying the half that did not work.

```
k8s/
├── namespace.yaml
├── configmap.yaml
├── service.yaml
├── deployment.yaml
├── examples/secret.yaml     a template, deliberately not in the directory apply path
└── kind/
    ├── postgres.yaml        a Postgres for the throwaway cluster only
    └── verify.sh            create a cluster, deploy, assert twelve things
```

## Apply

```sh
kubectl apply -f k8s/namespace.yaml

# Create the Secret imperatively so real values never touch a file git can see.
kubectl -n ai-customer-service-go create secret generic ai-customer-service-go-secrets \
  --from-literal=ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY" \
  --from-literal=POSTGRES_USER='csagent' \
  --from-literal=POSTGRES_PASSWORD="$PGPASSWORD"

kubectl apply -f k8s/
kubectl -n ai-customer-service-go rollout status deploy/ai-customer-service-go
```

## Before you apply

1. `deployment.yaml` → `image` — currently `ghcr.io/lai3d/ai-customer-service-go:0.1.0`.
   Point it at your registry and, in anything you care about, an immutable tag or digest.
2. `configmap.yaml` → `POSTGRES_HOST` / `POSTGRES_PORT` / `POSTGRES_DB`. The database
   needs the `vector` extension available.

## Verify on kind, before a real cluster

```sh
k8s/kind/verify.sh          # create a throwaway cluster, deploy, assert
k8s/kind/verify.sh --keep   # skip the image build if the tag is already present
k8s/kind/verify.sh --down   # delete it
```

It applies `k8s/` **unmodified** and adds only the two things the manifests deliberately
do not ship. Twelve assertions: both replicas ready, nothing OOMKilled, the Secret
untouched by the directory apply, no replica losing the `CREATE EXTENSION` race, uid
10001, a read-only root filesystem, no writable volume needed at all, health and readiness
through the Service, Go metrics, the demo page, and a bad key surfacing as `502` rather
than as a healthy pod returning errors. No API key needed; export `ANTHROPIC_API_KEY` to
check the model call too.

## What running them found

| | |
| --- | --- |
| A placeholder `requests: 4Gi` left the second replica `Pending` forever | 2 × 4Gi against a 7.75 GiB node. `Insufficient memory`, not a crash — a rollout that simply never finishes. |
| The first memory measurement was self-inconsistent and I nearly published it | A row read `peak 1380 MiB` under a `1280Mi` limit, which is impossible. `kubectl rollout status` returns while the previous pod is still `Running`, and the sampler picked it. Every row now reads `memory.max` from inside the same pod it measures, so a row cannot lie about which pod it describes. |
| Two assertions passed on the first run and failed on the second, against an identical pod | `producer \| grep -q` under `set -o pipefail` fails *because it matched*: grep exits at the first hit, the producer takes SIGPIPE, pipefail reports failure. The Java harness documents this trap for its own detector. This script copied the harness, kept the comment, and fell into it somewhere else. |
| Per-replica memory looked unequal and was not | 1347 MiB against 1527 MiB — entirely page cache for the 470 MB model file, charged to whichever cgroup faulted it in first. `anon` is 960 MiB in both. |

## Sizing, measured

Limits swept on a kind cluster, one replica, each row reading its own cgroup:

| limit | outcome | cgroup peak | process RSS | % of limit |
| --- | --- | --- | --- | --- |
| 1Gi | **OOMKilled** | — | — | — |
| 1152Mi | **OOMKilled** | — | — | — |
| 1280Mi | started | 1272 MiB | 1002 MiB | 99% |
| 1536Mi | started | 1269 MiB | 993 MiB | 82% |
| 2Gi | started | 1269 MiB | 994 MiB | 61% |
| 3Gi | started | 1270 MiB | 1004 MiB | 41% |

**The number to size against is `anon`, and it is 960 MiB.** That is the process itself,
overwhelmingly the 470 MB fp32 embedding model loaded into memory, and it is identical
across replicas. The rest of the cgroup total is page cache from reading the model file
out of the image layer, which is reclaimable and which the kernel charges to whichever
container faults it in first — so the *same deployment* shows 1347 MiB in one replica and
1527 MiB in the other.

Sizing against `anon` alone would be wrong in the other direction: 1152Mi is above 960 MiB
and still OOMKills, because the page cache churn while reading a 470 MB file cannot all be
reclaimed in time during startup. Hence `requests: 1536Mi` — covering the *startup* peak,
because the peak is at boot and a node packed to requests would crash-loop the pod rather
than degrade it — and `limits: 2Gi`, which leaves the worst observed peak at 75%.

## Startup

| | |
| --- | --- |
| Time to `Ready` | 4.4 s (the startup probe polls every 2 s, so this is quantised) |
| CPU consumed to reach `Ready` | 8.0 s of CPU |
| Corpus ingestion | 3.0 s for 36 documents, throttled to 2 CPU — it is 166 ms unthrottled on the host |
| OS threads at rest | 26 |

Nothing is downloaded at startup: the model is baked into the image. That is why the image
is 1.1 GB and why a cold pod needs no egress.

## The Go-specific part: GOMAXPROCS is the embedding concurrency

Go 1.25+ derives `GOMAXPROCS` from the cgroup CPU limit. Verified in the pod: the node has
18 CPUs, `limits.cpu: "2"`, and the process reports `GOMAXPROCS=2`.

That matters more here than in most services, because `GOMAXPROCS` is what the embedding
concurrency bound defaults to — and a goroutine inside a cgo call blocks an OS thread
(see [the benchmark](../docs/benchmark.md)). So on this runtime the CPU limit is not only
a throttle: it also decides how many threads the embedding path can consume. Remove the
CPU limit and the bound silently becomes the node's core count.

`EMBEDDING_MAX_CONCURRENCY` is therefore set explicitly in the ConfigMap, so the bound
does not move when someone edits `resources`.

## What this deployment does not fix

**The per-conversation lock is per process.** Turns are serialised within a replica, so
one replica cannot interleave two requests on one conversation — but two replicas can.
`sessionAffinity` would paper over it; the real fix is Postgres advisory locks on the
conversation id. Same shape as the ticket cap, which is `replicas × 3` rather than 3.

## Deliberately not included

- **Ingress / Gateway.** The app has no authentication. Exposing it needs a decision about
  what sits in front, which belongs with whoever owns the edge.
- **HorizontalPodAutoscaler.** The useful signal is in-flight model calls, not CPU; a
  CPU-based HPA on a workload that spends its life blocked on an API would scale on the
  wrong thing. `/metrics` is exposed for a KEDA/Prometheus HPA once someone picks the
  metric.
- **PodDisruptionBudget.** Worth adding (`minAvailable: 1`) on a cluster with real node
  churn.
- **NetworkPolicy.** Depends entirely on the CNI and the cluster's conventions.
- **A Postgres.** Conversation memory and the pgvector embeddings share one database, so
  it wants a real managed instance with backups, not a StatefulSet nobody owns.

---

[← Back to the README](../README.md)
