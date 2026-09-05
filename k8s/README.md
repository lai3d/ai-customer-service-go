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
    └── verify.sh            create a cluster, deploy, assert sixteen things
```

## Apply

```sh
kubectl apply -f k8s/namespace.yaml

# Create the Secret imperatively so real values never touch a file git can see.
# Add ADMIN_TOKENS here too if you want the operations surface -- see below.
kubectl -n ai-customer-service-go create secret generic ai-customer-service-go-secrets \
  --from-literal=ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY" \
  --from-literal=POSTGRES_USER='csagent' \
  --from-literal=POSTGRES_PASSWORD="$PGPASSWORD"

kubectl apply -f k8s/
kubectl -n ai-customer-service-go rollout status deploy/ai-customer-service-go
```

## The operations surface is off unless you put a token in the Secret

`/admin` reads customer conversations, so it is opt-in and its credential belongs in the
Secret rather than the ConfigMap. `deployment.yaml` takes the whole Secret through
`envFrom`, so adding the key is the entire deployment change:

```sh
kubectl -n ai-customer-service-go patch secret ai-customer-service-go-secrets --type=merge \
  -p "{\"stringData\":{\"ADMIN_TOKENS\":\"alex:$(openssl rand -hex 24):operator\"}}"
kubectl -n ai-customer-service-go rollout restart deploy/ai-customer-service-go
```

`name:token[:role]`, comma separated; the roles are `viewer` (read) and `operator`
(read and write), and an omitted role is a viewer. Tokens shorter than 16 characters are
refused at startup rather than accepted and weak.

**With the key absent, `/admin` is a 404 rather than a guarded 401** — the routes are never
registered. `verify.sh` asserts both halves of that, because "documented but never
deployed" is exactly how two of the Java implementation's manifests were wrong.

## Before you apply

1. `deployment.yaml` → `image` — currently `ghcr.io/lai3d/ai-customer-service-go:0.1.0`.
   Point it at your registry and, in anything you care about, an immutable tag or digest.
2. `configmap.yaml` → `POSTGRES_HOST` / `POSTGRES_PORT` / `POSTGRES_DB`. The database
   needs the `vector` extension available.

**Known limitation:** telling you to hand-edit two tracked files is a drift generator —
your edits collide with every `git pull`, and nothing records what you changed. A Kustomize
base plus an overlay is the fix, and the Java implementation of this system has one
(`base/` + `overlays/example/`). This directory is deliberately flat and has not been
restructured; the harness's guarantee that it applies `k8s/` *unmodified* is what makes
these manifests the ones that were actually verified, and an overlay would need the same
guarantee — an assertion that it reproduces every base document verbatim — before it was
an improvement rather than a second thing to trust.

## Verify on kind, before a real cluster

```sh
k8s/kind/verify.sh          # create a throwaway cluster, deploy, assert
k8s/kind/verify.sh --keep   # skip the image build if the tag is already present
k8s/kind/verify.sh --down   # delete it
```

It applies `k8s/` **unmodified** and adds only the two things the manifests deliberately
do not ship. Sixteen assertions: both replicas ready, nothing OOMKilled, the Secret
untouched by the directory apply, no replica losing the `CREATE EXTENSION` race, uid
10001, a read-only root filesystem, no writable volume needed at all, health and readiness
through the Service, Go metrics, the demo page, the operations surface both off and on
(four of them), and a bad key surfacing as `502` rather than as a healthy pod returning
errors. No API key needed; export `ANTHROPIC_API_KEY` to check the model call too.

The four admin assertions are the only ones that change the cluster: they patch
`ADMIN_TOKENS` into the Secret the harness created itself and restart the deployment, so
that the documented way to turn the surface on is the way it was tested. The token is
generated per run and never printed.

## The harness never touches your kubeconfig

`verify.sh` exports a `KUBECONFIG` of its own (`k8s/kind/.kubeconfig`, gitignored) and
never opens yours. That is a safety property rather than a courtesy: this machine's
kubeconfig has production-shaped contexts in it, and a script that changes the current
context decides where somebody's next `kubectl delete` lands.

Three versions, and the first two were both incomplete:

1. `kubectl config use-context` — switched the caller's context and left it switched. It
   did this from the day the script was written, including in the run that switched the
   sibling Java session's context out from under it.
2. `--context` on every call — better, and still wrong on a *fresh* run, because
   `kind create cluster` writes the new context into `$KUBECONFIG` and switches to it. The
   claim "never touches your kubeconfig" was true only for runs that reused a cluster.
3. A dedicated `KUBECONFIG`. Nothing to restore.

Save-and-restore was never attempted, deliberately. `trap ... EXIT` **replaces** the
previous handler rather than adding to it, so a second `trap` below silently disables the
restore and nothing errors — that is how the Java harness lost a restore it had just
added, tested and announced. What this guards is which cluster a `kubectl delete` reaches,
and a mechanism that has to be right is worse than one that cannot be wrong.

**Verified by hash, not by reading the context.** The user's kubeconfig is byte-identical
by `sha256` before and after a full run *including a fresh cluster build* — the case
version 2 did not cover. Checking `current-context` would have passed for a script that
rewrote the file and put the context back, and the first attempt at even that weaker check
was worthless: it switched to a context that no longer existed, the switch failed
silently, and the "before" value was already the one the script would have set. A negative
test that could not fail.

## Which assertions have been seen to fail

An assertion nobody has seen go red is a claim, not a check — the `CREATE EXTENSION` one
passed for two days without its condition ever arising. So this is the honest inventory.

| Assertion | Seen red? |
| --- | --- |
| both replicas ready | **yes** — a placeholder `requests: 4Gi` left one `Pending` |
| no container was OOMKilled | **yes** — forced with a 512Mi limit; note that *both replicas ready* stayed green at the same time, because a container can OOM, restart and recover. That is why they are separate checks. |
| no replica lost the `CREATE EXTENSION` race | **yes** — reproduces on every cold start without the advisory lock |
| runs as uid 10001 / read-only root filesystem | **yes, but spuriously** — pod churn, not the property. The property itself has never been observed failing. |
| the directory apply left the Secret alone | no |
| no writable volume is needed at all | no |
| health, readiness, metrics, the demo page | no |
| a bad key surfaces as 502 | the branch has been exercised (run without `ANTHROPIC_API_KEY`) and passed; it has never failed |
| the node has room for the replicas | **yes** — forced with `requests: 4Gi`, which fits the node's 7931 MiB and not the 7641 MiB actually free |
| with no `ADMIN_TOKENS` there is no admin surface (404) | **yes** — run with the clearing step removed against a cluster an earlier run had enabled: *`/admin/` returned 200 with no ADMIN_TOKENS, want 404*. That red is also what the clearing step is for: without it the assertion passes on a fresh cluster and fails on every `--keep` rerun after. |
| the admin page serves, refuses a tokenless request (401), accepts an operator token (200) | **yes, all three** — run with the enabling patch removed: the page check got an empty body and both API checks got `404` where they wanted `401` and `200`. That is the specific way they could have been vacuous: passing because the surface was already on from a previous run rather than because this run turned it on. |

Four remain unproven: the directory apply leaving the Secret alone, no writable volume,
the four service-level checks, and the 502. They are worth keeping — a check that has
never fired is not the same as a check that cannot — but they should not be read as
evidence until something has made each of them red.

## What running them found

| | |
| --- | --- |
| A placeholder `requests: 4Gi` left the second replica `Pending` forever | 2 × 4Gi against a 7.75 GiB node. `Insufficient memory`, not a crash — a rollout that simply never finishes. |
| The first memory measurement was self-inconsistent and I nearly published it | A row read `peak 1380 MiB` under a `1280Mi` limit, which is impossible. `kubectl rollout status` returns while the previous pod is still `Running`, and the sampler picked it. Every row now reads `memory.max` from inside the same pod it measures, so a row cannot lie about which pod it describes. |
| Two assertions failed roughly one run in three, against pods that were correct | **Three wrong fixes before the right one, and the change that found the cause was not a fix for it.** First blamed on `set -o pipefail` turning `producer \| grep -q` into a failure when it matched — a real trap, but not this one: pipefail does not propagate into `sh -c`, which every one of those checks used. Then on API-server pressure, also unproven. The actual cause: the pod was selected with `phase == "Running"`, which is true of a pod that is shutting down, and `kubectl exec` then fails with *"cannot exec into a container in a completed pod; current phase is Succeeded"*. It became visible only because the helper had been changed to print what it got instead of discarding it — a change made for the wrong reason that paid for itself immediately. Selecting a `Ready` pod was still not enough: Ready and terminating are both true of an old pod for a few seconds, and it can begin terminating between being chosen and being exec'd into. The check now excludes anything with a `deletionTimestamp` and re-resolves the pod on a stale-name error. Four consecutive clean runs, against roughly one failure in three before. |
| A cold-database reset that invented a bug | `DROP EXTENSION vector CASCADE` takes the `embedding` column with it and leaves the table, so `CREATE TABLE IF NOT EXISTS` does nothing and the app serves 500s from a table with no vector column. No deployment reaches that state on its own. The reset drops and recreates the schema instead. |
| The `CREATE EXTENSION` check had been passing without ever running | It only fires on a *cold* database, and no run had ever had two replicas start against one — the first run had a replica stuck `Pending`, and every later run reused an extension that already existed. Forced cold, it fails: `duplicate key value violates unique constraint "pg_extension_name_index"`, both replicas restarting. Fixed with a Postgres advisory lock around the DDL; `verify.sh` now drops the extension before each deploy so the check is exercised every run. |
| Per-replica memory looked unequal and was not | 1394 MiB against 1655 MiB — page cache for the 470 MB model file, charged to whichever cgroup faulted it in first and varying from 18 to 379 MiB over a pod's life. `anon` is 951 MiB in both, every time. |
| A capacity check that was wrong twice, in two different ways | **First**, it grepped `requests:` with three lines of context and a comment block sits between the key and the value — so it printed "2 replicas x  = 0 MiB" and reported PASS. A check measuring nothing, written into the harness whose purpose is catching exactly that. It reads the rendered spec through `kubectl --dry-run` now and fails loudly when it cannot parse. **Second**, once it parsed correctly it compared against the node's *allocatable* memory rather than what was *free*, and passed on a node already at 81% of its memory requests. It now subtracts what other namespaces have reserved. Two forced-red runs, one per version. |

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
