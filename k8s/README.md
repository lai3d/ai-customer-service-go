# Kubernetes manifests

Namespace, ConfigMap, Service, Deployment, NetworkPolicy, PodDisruptionBudget,
HorizontalPodAutoscaler, Ingress. No Postgres and no Secret — see
[Before you apply](#before-you-apply). [docs/deployment.md](../docs/deployment.md) is the
part that happens off the cluster: publishing an image, pinning a digest, and the three
things about secrets that none of this fixes.

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
├── admin-ui.yaml            the operations UI: its own Deployment, Service and ConfigMap
├── networkpolicy.yaml       default-deny, plus the edges that actually exist
├── poddisruptionbudget.yaml what a node drain may take away at once
├── hpa.yaml                 scaling the API on CPU, and why CPU is the wrong signal
├── ingress.yaml             an example: needs a real controller and a real certificate
├── examples/secret.yaml     a template, deliberately not in the directory apply path
└── kind/
    ├── postgres.yaml        a Postgres for the throwaway cluster only, and its policy
    └── verify.sh            create a cluster, deploy, assert forty-five things
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
  # --from-literal=ORDER_SERVICE_TOKEN="$ORDER_SERVICE_TOKEN"   # if ORDER_SERVICE_URL is set

# ingress.yaml names a TLS Secret. Create it, or the controller quietly serves its own
# self-signed certificate and the site works with a warning nobody investigates.
kubectl -n ai-customer-service-go create secret tls ai-customer-service-go-tls \
  --cert=fullchain.pem --key=privkey.pem   # or let cert-manager create it

kubectl apply -f k8s/
kubectl -n ai-customer-service-go rollout status deploy/ai-customer-service-go
```

`kubectl apply -f k8s/` now applies a NetworkPolicy set whose first rule is default-deny.
**On a cluster whose CNI enforces policy, applying these without reading them will cut the
app off from a Postgres that is not in this namespace** — see
[NetworkPolicy](#networkpolicy-default-deny-and-five-exceptions) below, where the one line
you have to edit is marked. That failure is loud: `/readyz` never passes.

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

## The operations UI is a second deployment

`k8s/admin-ui.yaml` is a static bundle on nginx: its own Deployment, Service and
ConfigMap, two replicas, `10m`/`24Mi` requested. It holds no secret and reaches no
database — everything it displays arrives in the operator's own browser, authenticated by
the operator's own token — so a compromised UI pod has nothing to steal that the operator
did not already have.

Two values have to agree, and nothing but the harness checks that they do:

```
k8s/admin-ui.yaml   ADMIN_API_BASE      the URL a BROWSER will call
k8s/configmap.yaml  ADMIN_CORS_ORIGINS  must contain that URL's origin, verbatim
```

`ADMIN_API_BASE` is a browser's view, not the cluster's: a Service DNS name resolves
nowhere on the operator's laptop. If the two disagree the page loads, looks correct, and
fails every request with an opaque network error and nothing in the server log — which is
why `verify.sh` asserts the pair rather than either half.

Unlike the API pod, this one needs writable volumes. `config.js` is written at start-up
from the ConfigMap so one built image serves any environment, and nginx wants its own
temporary directories; all three are `emptyDir`, and the root filesystem stays read-only.

## NetworkPolicy: default-deny and five exceptions

The edges were read out of the code, not guessed: the app reaches Postgres, the model
provider over 443, and an OTLP collector when tracing is on; the operations UI reaches
**nothing** — it is nginx serving a bundle, and the browser calls the API from the
operator's laptop, which is what `ADMIN_CORS_ORIGINS` is about.

```
default-deny          everything, both directions, every pod        <- delete this and the rest is decoration
app-egress-dns        the app -> kube-dns
app-egress-postgres   the app -> Postgres:5432          <- THE LINE YOU MUST EDIT
app-egress-https      the app -> 0.0.0.0/0:443, except RFC1918 and 169.254.0.0/16
app-egress-otlp       the app -> the observability namespace:4318
app-ingress           ingress-nginx + monitoring -> the app:8081
admin-ui-ingress      ingress-nginx -> the UI:8080, and no egress rule anywhere
```

The line to edit is the Postgres one. It selects a pod in this namespace, which is what
the kind harness runs; **a managed instance is an address, not a pod**, so uncomment the
`ipBlock` and put your instance in it or the app will not start. That failure is loud —
`/readyz` never passes — which is the only reason it is safe to ship this way.

"Postgres accepts only the app" lives in `kind/postgres.yaml` rather than here, because
these manifests ship no Postgres and a policy selecting a pod that does not exist is a
policy that protects nothing while looking installed.

**A NetworkPolicy is only a policy if something enforces it**, and nothing tells you when
nothing does: the objects apply, `kubectl get netpol` lists them, and every connection
they forbid still works. Flannel ignores them entirely. So the harness opens sockets that
must not open — from a pod no rule names, and from the UI — rather than reading the
objects back. kindnet on kind v0.31 / Kubernetes 1.35 does enforce them, measured rather
than assumed, and two things it does *not* police are written down under
[what running them found](#what-running-them-found).

## PodDisruptionBudget: one at a time

`maxUnavailable: 1` on both deployments. Not `minAvailable: 1`: they are the same number
at two replicas and they diverge the moment the count moves, and the API's count does move
— the HPA scales it to six, where `minAvailable: 1` would permit evicting five at once.

One at a time matters here more than for a typical stateless service because a replacement
is not free: 470 MB of model into memory, 8.0 s of CPU, 4.4 s to Ready at best. Two
replicas drained together is a full outage for tens of seconds.

It constrains **voluntary** disruption only — `kubectl drain`, a node-pool upgrade, a
cluster autoscaler compacting nodes. Not a node that dies, and not the rolling update
(that is `strategy.rollingUpdate`, already `maxUnavailable: 0`).

The failure worth knowing: a PDB whose selector matches nothing is accepted, listed, and
protects nothing. The harness asserts `status.expectedPods` and then **evicts a pod
through the eviction API** and requires the second eviction to be refused — both have been
seen red, from a one-word selector typo.

## HorizontalPodAutoscaler: a bound, not a load-follower

`minReplicas: 2`, `maxReplicas: 6`, CPU at 200% of requests, with a five-minute
stabilisation window on the way up.

**CPU is the wrong signal and is used anyway.** A turn spends most of its life blocked on
the provider's socket consuming no CPU, so a replica holding a hundred streaming turns can
read close to idle. What CPU *does* measure is the embedding model, which runs in-process
through cgo and is the one thing in the request path with a hard per-pod ceiling. The
useful signal is in-flight model calls; `/metrics` publishes it, and reaching it means
KEDA or the Prometheus adapter — a second operator nobody has picked up. Until then this
HPA is the honest half-measure, and saying so is what stops it being read as more.

**The Go-specific part.** `limits.cpu: "2"` sets `GOMAXPROCS`, and `GOMAXPROCS` is what
the embedding concurrency bound defaults to. So the CPU limit already decided how many
goroutines may sit inside the native embedding call, and three things follow: a pod cannot
exceed 2 cores however large the node, so utilisation against a 500m request saturates at
400% (the 200% target is half of what one pod can use); scaling *out* is the only way to
add embedding throughput, because scaling *up* buys OS threads blocked in cgo rather than
parallelism; and anyone who edits `limits.cpu` has changed the embedding concurrency and
the meaning of the target in the same edit.

Startup costs 8.0 s of CPU, so a new replica *raises* the average CPU while it boots —
positive feedback on a CPU-driven HPA. The 300 s scale-up window and one-pod-per-two-
minutes policy exist to outlast the boot rather than to be cautious in general. Memory is
deliberately not a metric: RSS is ~960 MiB of model whether idle or saturated, so a memory
target would sit at a constant 62% and never signal anything.

An HPA fails silently in two ways that look identical from outside — a `scaleTargetRef`
naming something that is not there, and no metrics pipeline to read. The harness asserts
both conditions, and both have been seen red.

## Ingress and TLS: an example, and what it needs

`ingress.yaml` is committed knowing every value in it is wrong for you: `.test` hostnames,
`ingressClassName: nginx`, and a TLS Secret you have to create. It is committed anyway
because "put an Ingress in front of it" as a README sentence is exactly how the sibling
Java repository ended up with two manifests that were wrong — so this one is applied and
driven on every harness run, against a real ingress-nginx, over TLS, on both hosts.

Three things in it are not boilerplate:

- **`proxy-buffering: "off"`.** A turn is an SSE stream and nginx buffers proxied
  responses by default. Buffered, the page still works and every test that reads the
  completed response still passes — while the measured property (retrieval on screen 3.5
  seconds before the first word) is silently gone.
- **Two hosts, not two paths.** The operations UI on its own origin is what makes the CORS
  allowlist a control rather than dead configuration.
- **The TLS Secret.** Get the name wrong and ingress-nginx does not fail: it serves its own
  "Kubernetes Ingress Controller Fake Certificate" and the site works. The harness
  therefore asserts *which* certificate was served, and that assertion has been seen red.

And the thing to read before copying it: `AUTH_MODE` is `off`, so this Ingress publishes
an API where conversation ids are client-supplied and unowned. Turn identity on, or put
something that authenticates in front of the host, before it exists in DNS.

## Alerts and scraping live in `observability/`, not in here

```sh
# Both need the monitoring.coreos.com CRDs -- kube-prometheus-stack, or the operator alone.
kubectl apply -f observability/servicemonitor.yaml
kubectl apply -f observability/prometheus-rule.yaml
```

A `ServiceMonitor` (because the operator ignores the `prometheus.io/*` annotations on the
Deployment and reads one of these instead) and a `PrometheusRule` with eleven alerts and
an SLO on the turn. [docs/observability.md](../docs/observability.md#an-slo-on-the-turn)
has the reasoning and says which numbers are measured, which is none of the thresholds.

**They are outside this directory on purpose.** `kubectl apply -f k8s/` is the documented
way to deploy this and it would fail on both files on any cluster without those CRDs —
including the throwaway cluster `kind/verify.sh` builds, where no operator is installed.
Rather than being verified by proximity, they are verified by a test:
`internal/deployment` checks their namespace against `namespace.yaml`, their selector
against `service.yaml`'s labels, the scraped port name against the Service's ports, the
scraped path against the route `cmd/server/main.go` registers, and the `job` label the
rules match on against the `jobLabel` the monitor sets. None of those five produces an
error anywhere when it is wrong: the rules simply evaluate against nothing.

## Before you apply

1. `deployment.yaml` → `image` — currently `ghcr.io/lai3d/ai-customer-service-go:0.1.0`.
   Point it at your registry and, in anything you care about, a digest:
   [docs/deployment.md](../docs/deployment.md#3-pinning-the-manifests).
2. `configmap.yaml` → `POSTGRES_HOST` / `POSTGRES_PORT` / `POSTGRES_DB`. The database
   needs the `vector` extension available.
3. `networkpolicy.yaml` → `app-egress-postgres`. If the database is not a pod in this
   namespace, the commented `ipBlock` is the rule that has to name it.
4. `ingress.yaml` → both hostnames, `ingressClassName`, and a TLS Secret that exists.

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

It applies `k8s/` **unmodified** and adds only what the manifests deliberately do not ship.
Forty-five assertions: both replicas ready, nothing OOMKilled, the Secret untouched by the
directory apply, no replica losing the `CREATE EXTENSION` race, uid 10001, a read-only root
filesystem, no writable volume needed at all, health and readiness through the Service, Go
metrics, the demo page, the operations API off and then on, the CORS allowlist answering
one origin and refusing another, the operations UI rolling out and serving with its
`config.js` and its security headers as a non-root user on a read-only filesystem, a bad
key surfacing as `502` rather than as a healthy pod returning errors, no image reference
that floats, four connections that must not open and three that must, an Ingress adopted
and serving both hosts over TLS with the certificate from *its* Secret, HTTP redirected,
an HPA reading a real metric off a real target, two disruption budgets that cover their
pods, and an eviction allowed once and refused twice. No API key needed; export
`ANTHROPIC_API_KEY` to check the model call too.

**Three things it installs into the cluster**, alongside the Postgres and the Secret,
because a real cluster has them and these manifests do not ship them: a self-signed TLS
certificate for the Ingress, **ingress-nginx** (pinned), and **metrics-server** (pinned).
Without the second, `ingress.yaml` is a document rather than a route; without the third the
HPA reports `ScalingActive=False` for ever — which is indistinguishable from a *wrong* HPA,
so the assertions could not have told them apart. All three are cluster infrastructure,
not manifests: `k8s/` is still applied exactly as committed. It also means the harness
needs network access beyond the image pulls.

Three sections change the cluster while they run, and each undoes itself:

- The **operations** assertions patch `ADMIN_TOKENS` into the Secret and
  `ADMIN_CORS_ORIGINS` into the ConfigMap the harness created itself, then restart, so
  that the documented way to turn the surface on is the way it was tested. The token is
  generated per run and never printed.
- The **network policy** and **egress** sections run two throwaway pods. The second wears
  the app's labels — that is how it gets the app's policy — which also makes it an
  endpoint of the Service, so it runs last, is deleted immediately, and any leftover from
  an interrupted run is deleted before the next deploy.
- The **disruption budget** section evicts a replica, and waits for the rollout before
  moving on.

One assertion was **deleted** rather than kept: that `/admin/` is a 404 when
`ADMIN_TOKENS` is unset. The API serves no page at all now, so it would have gone on
passing while checking nothing. An API path replaced it.

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
| the admin API refuses a tokenless request (401), accepts an operator token (200) | **yes, both** — run with the enabling patch removed: both got `404` where they wanted `401` and `200`. That is the specific way they could have been vacuous: passing because the surface was already on from a previous run rather than because this run turned it on. |
| `config.js` carries the API base from the ConfigMap | **yes** — run with the harness's own ConfigMap patch removed, and the failure printed the value it got, which was the manifest's default rather than the port-forward. |
| the UI sends a Content-Security-Policy on the document itself | **yes** — forced with an image whose nginx declared the header only at server level. That is not a hypothetical mistake: it is what the config did before the include, and `curl -I /` came back with no policy at all while the config file read correctly. |
| the UI's root filesystem is read-only | **yes, but spuriously first** — the assertion was red against a pod that was demonstrably read-only, because `kubectl exec ... \| grep -q` fails under `pipefail` when the exec's own exit code is non-zero, which is exactly what a successful "this must fail" check produces. It uses the retrying helper now. |
| the CORS preflight is answered for one origin and refused for another | no, not in the harness — the six CORS rules were each forced red in `internal/admin`, but nothing has made these two go red on a cluster |
| the UI rolls out, is served, runs as uid 101, can write /tmp | no |
| no image reference in `k8s/` floats | **yes** — `admin-ui.yaml` pointed at `:latest`: *a manifest points at a floating image* |
| a pod no policy names cannot reach Postgres | **yes** — with `default-deny` *and* `postgres-ingress` deleted: `PROBE_OPEN`. Deleting either one alone left it green, which is the layering working and is why the red needed both |
| a pod no policy names cannot reach the API | **yes** — `app-ingress` widened to `podSelector: {}` (the classic over-broad rule) plus an egress rule for the probe |
| the operations UI cannot open a connection to the API | **yes** — same perturbation |
| the operations UI cannot resolve a name | **yes**, and the red-test found the check was wrong: it asked for a short name, and busybox's `nslookup` does not walk the search path, so it exited non-zero on an NXDOMAIN *from a resolver it had reached*. The check reported "cannot resolve" while the pod was resolving. It asks for the FQDN now |
| a controller adopted the Ingress | **yes** — `ingressClassName: does-not-exist`; the two TLS checks and the redirect went red with it |
| the API / the UI answer through the Ingress over TLS | **yes** — same perturbation (404 from the default backend) |
| the certificate served is the one in the Secret | **yes** — Secret deleted: *the controller served its own fake certificate*. The other four ingress assertions stayed green through it, which is exactly the point |
| plain HTTP is redirected (308) | **yes** — `ssl-redirect: "false"`: got 200, in plaintext |
| the HPA's target exists (`AbleToScale`) | **yes** — `scaleTargetRef` pointed at `ai-customer-service-gone`: `AbleToScale=False/FailedGetScale`, while `ScalingActive` stayed True |
| the HPA reads a real metric (`ScalingActive`) | **yes** — metrics-server scaled to zero: `ScalingActive=False/FailedGetResourceMetric`, while `AbleToScale` stayed True |
| the API budget covers its pods | **yes**, twice — a selector typo (`expectedPods=0`) and `maxUnavailable: 0` (`disruptionsAllowed=0`) |
| the UI budget covers its pods | no — same loop body as the row above, not forced separately |
| one eviction is allowed | **yes** — `maxUnavailable: 0`: *the first eviction was refused, which means the budget is too strict to drain a node* |
| the second eviction is refused | **yes** — with the selector typo both replicas were evicted at once |
| the app may reach the provider on 443 | **yes** — `app-egress-https` deleted |
| the app may not reach that host on 80 | **yes** — an allow-all egress rule added |
| the app may not reach a private 443 that answers | **yes**, twice — allow-all egress, and (the sharper one) removing *only* the `except:` list, which opened it |

Unproven: the directory apply leaving the Secret alone, no writable volume, the four
service-level checks, the 502, the two cluster-level CORS checks, the four that only say
the UI came up, and the operations UI's disruption budget. They are worth keeping — a
check that has never fired is not the same as a check that cannot — but they should not be
read as evidence until something has made each of them red.

**Two things the harness cannot make red at all, and does not pretend to.** The egress rule
excludes `169.254.169.254`, the cloud metadata endpoint, which is the reason the exception
list exists; nothing answers on that address in kind, so a probe of it is "blocked" whether
or not any policy is enforced — an assertion that cannot fail, which is the thing this
harness exists to avoid shipping. And kindnet exempts traffic addressed to the node itself,
so the Kubernetes API server (whose ClusterIP is DNATed to the node) stays reachable from
every pod here whatever the policy says. Both are printed as NOTEs on every run rather than
counted as passes.

## What running them found

| | |
| --- | --- |
| A placeholder `requests: 4Gi` left the second replica `Pending` forever | 2 × 4Gi against a 7.75 GiB node. `Insufficient memory`, not a crash — a rollout that simply never finishes. |
| The first memory measurement was self-inconsistent and I nearly published it | A row read `peak 1380 MiB` under a `1280Mi` limit, which is impossible. `kubectl rollout status` returns while the previous pod is still `Running`, and the sampler picked it. Every row now reads `memory.max` from inside the same pod it measures, so a row cannot lie about which pod it describes. |
| Two assertions failed roughly one run in three, against pods that were correct | **Three wrong fixes before the right one, and the change that found the cause was not a fix for it.** First blamed on `set -o pipefail` turning `producer \| grep -q` into a failure when it matched — a real trap, but not this one: pipefail does not propagate into `sh -c`, which every one of those checks used. Then on API-server pressure, also unproven. The actual cause: the pod was selected with `phase == "Running"`, which is true of a pod that is shutting down, and `kubectl exec` then fails with *"cannot exec into a container in a completed pod; current phase is Succeeded"*. It became visible only because the helper had been changed to print what it got instead of discarding it — a change made for the wrong reason that paid for itself immediately. Selecting a `Ready` pod was still not enough: Ready and terminating are both true of an old pod for a few seconds, and it can begin terminating between being chosen and being exec'd into. The check now excludes anything with a `deletionTimestamp` and re-resolves the pod on a stale-name error. Four consecutive clean runs, against roughly one failure in three before. |
| A cold-database reset that invented a bug | `DROP EXTENSION vector CASCADE` takes the `embedding` column with it and leaves the table, so `CREATE TABLE IF NOT EXISTS` does nothing and the app serves 500s from a table with no vector column. No deployment reaches that state on its own. The reset drops and recreates the schema instead. |
| The `CREATE EXTENSION` check had been passing without ever running | It only fires on a *cold* database, and no run had ever had two replicas start against one — the first run had a replica stuck `Pending`, and every later run reused an extension that already existed. Forced cold, it fails: `duplicate key value violates unique constraint "pg_extension_name_index"`, both replicas restarting. Fixed with a Postgres advisory lock around the DDL; `verify.sh` now drops the extension before each deploy so the check is exercised every run. |
| Per-replica memory looked unequal and was not | 1394 MiB against 1655 MiB — page cache for the 470 MB model file, charged to whichever cgroup faulted it in first and varying from 18 to 379 MiB over a pod's life. `anon` is 951 MiB in both, every time. |
| A "cannot resolve" check that was green while the pod was resolving | The UI's DNS assertion asked busybox `nslookup` for a short name. busybox does not walk the search path, so with DNS egress **deliberately opened** it still exited non-zero — NXDOMAIN from a resolver it had reached perfectly well — and the check reported the property it was supposed to be testing the absence of. Found only because the perturbation that should have turned it red did not. It asks for the FQDN now. |
| A probe pod that poisoned the next run's Service | `app-egress-probe` wears the app's labels on purpose — that is how it gets the app's NetworkPolicy — which also makes it an endpoint of the Service. An interrupted run left one behind, and `kubectl port-forward svc/...` then picked it: four service-level assertions failed with an empty body and nothing said why. The section runs last, deletes the pod, and the deploy step now deletes leftovers before anything looks at the namespace. |
| The dataplane is eventually consistent and the first version of the egress probes did not know it | A denial assertion run immediately after the probe pod became Ready watched a forbidden connection *open*: kindnet programs a new pod's rules a moment after the pod is Ready. Measured at up to four attempts — around ten seconds. The probes retry until they see what they expect, and say how many attempts it took; a wrong rule still fails all ten. |
| Neither half of a layered denial can be red-tested alone | Deleting `default-deny` left every "must not connect" assertion green, because the destination-side rule still refused; deleting the destination-side rule alone left them green because default-deny still refused the source. That is defence in depth doing its job, and it means a red test has to remove *both* — which is also the only way to know the probe could have reached the target at all. |
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

The first four entries here used to be *Ingress, HPA, PodDisruptionBudget, NetworkPolicy*,
each with a paragraph explaining why not. All four are in now, with the objections kept
rather than deleted — the HPA still says CPU is the wrong signal, the Ingress still says it
needs a controller and a certificate you own, and the NetworkPolicy still says it depends
on the CNI. What changed is that each is applied and driven on a real cluster on every run
instead of being a paragraph.

What is still not here:

- **A Postgres.** Conversation memory and the pgvector embeddings share one database, so
  it wants a real managed instance with backups, not a StatefulSet nobody owns.
- **Secrets that are more than base64.** A Kubernetes Secret is an encoding.
  [docs/deployment.md](../docs/deployment.md#5-secrets-which-are-still-not-solved) has the
  three ways out and the reason none of them changes a manifest here.
- **A digest in the image reference.** The manifests carry an explicit tag, and the harness
  asserts it does not float; pinning the digest is documented and is not verified by
  anything, because `kind load` moves an image by tag.
- **A Gateway API version of the Ingress.** One edge object, verified, beats two written
  from the same understanding.
- **KEDA or the Prometheus adapter**, which is what scaling on in-flight model calls needs.

---

[← Back to the README](../README.md)
