#!/usr/bin/env bash
# Verify the Kubernetes manifests on a throwaway kind cluster.
#
#   k8s/kind/verify.sh            create the cluster, deploy, assert, leave it running
#   k8s/kind/verify.sh --down     delete the cluster and exit
#   k8s/kind/verify.sh --keep     skip the image build if the tag is already present
#
# It applies the manifests in k8s/ *unmodified*. That is the point: a harness that patches
# the resources or the image before applying verifies the patch, not the file anyone else
# will use. The only things it adds are the two the manifests deliberately do not ship --
# a Postgres (k8s/kind/postgres.yaml) and a Secret, created imperatively.
#
# The Secret gets a placeholder ANTHROPIC_API_KEY unless one is exported. Nothing in
# startup or in either probe calls the model, so a fake key verifies everything except the
# model call -- and it verifies that a bad key surfaces as 502 rather than as a healthy
# pod serving errors. Export a real key to check the model path too.
#
# This exists because the Java implementation of this system committed its manifests
# without ever applying them and two were wrong. Copying the manifests without copying the
# harness would have been copying the half that did not work.
set -euo pipefail

CLUSTER=${CLUSTER:-ai-cs-go}
NS=ai-customer-service-go
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
IMAGE=$(grep -m1 'image: ghcr.io' "$ROOT/k8s/deployment.yaml" | awk '{print $2}')
PASS=0; FAIL=0

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$*"; PASS=$((PASS+1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAIL=$((FAIL+1)); }
note() { printf '  \033[33mNOTE\033[0m %s\n' "$*"; }
check(){ local d=$1; shift; if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d"; fi; }
# Compare captured output instead of discarding it.
#
# The `check` helper above sends stdout and stderr to /dev/null, so an assertion failure
# and a transient infrastructure failure look identical -- a bare FAIL with nothing to
# read. Two checks here failed exactly once, against a pod that was demonstrably correct,
# while the cluster's API server was intermittently timing out under memory pressure, and
# the helper gave no way to tell those apart.
#
# `contains` prints what it actually got, so the next such failure says whether the
# assertion was wrong or the cluster was.
#
# (An earlier version of this comment blamed `set -o pipefail` turning `producer | grep -q`
# into a failure when it matched -- a real trap, documented in the Java implementation's
# harness. It is not what happened here: pipefail is a shell option and does not propagate
# into `sh -c`, which every one of these checks used. Verified rather than assumed.)
contains(){ local d=$1 pattern=$2; shift 2
  local out; out=$("$@" 2>&1) || true
  case "$out" in (*"$pattern"*) ok "$d";; (*) bad "$d -- got: ${out:0:80}";; esac; }

# A kubeconfig of the harness's own, so the user's is never opened for writing.
#
# Pinning --context was the previous fix and it was incomplete: `kind create cluster`
# writes the new context into $KUBECONFIG and switches to it, so a *fresh* run modified
# the user's file even though every later command was context-pinned. The claim "this
# harness never touches your kubeconfig" was true only for runs that reused a cluster.
#
# Saving and restoring would work and is not worth it. What this guards is which cluster
# somebody's next `kubectl delete` reaches, and this kubeconfig has production-shaped
# contexts in it -- a mechanism that has to be right is worse than one that cannot be
# wrong. `trap ... EXIT` replacing rather than adding is how the Java implementation's
# harness lost a restore it had just added and tested.
export KUBECONFIG="$(dirname "$0")/.kubeconfig"

if [[ ${1:-} == --down ]]; then
  kind delete cluster --name "$CLUSTER"
  rm -f "$KUBECONFIG"
  exit 0
fi

for t in kind kubectl docker; do
  command -v "$t" >/dev/null || { echo "missing: $t" >&2; exit 1; }
done

say "cluster"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  # The cluster exists but this kubeconfig may not describe it yet.
  kind export kubeconfig --name "$CLUSTER" >/dev/null
else
  kind create cluster --name "$CLUSTER" --wait 120s
fi

# Never `kubectl config use-context`. It is global state in the caller's kubeconfig, and
# this machine runs two of these harnesses -- the sibling Java implementation's session had
# its context switched out from under it by a run of this script, and found its namespace
# apparently empty.
#
# The obvious fix is to save the context and restore it in a trap. That fix is worse than
# it looks: `trap ... EXIT` *replaces* the previous handler rather than adding to it, so a
# second trap further down silently disables the restore and nothing errors. That happened
# to the Java harness, to a restore that had just been added, tested, and announced.
#
# Belt and braces: the kubeconfig above already contains only this cluster, and pinning
# the context means a stray KUBECONFIG in the environment cannot redirect a command.
KUBECTL=(kubectl --context "kind-$CLUSTER")

say "image  $IMAGE"
if [[ ${1:-} == --keep ]] && docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "  reusing the local image"
else
  docker build -t "$IMAGE" "$ROOT"
fi
# 1.1 GB, of which 470 MB is the embedding model. This takes a minute and is the honest
# cost of baking the model in rather than downloading it at startup.
kind load docker-image "$IMAGE" --name "$CLUSTER"

say "capacity"
# Check the node can hold what the manifests ask for, before deploying rather than after.
#
# A too-large request does not fail: the pod sits Pending and the rollout times out, which
# reads as the manifests being broken when it is the laptop being small. This cost a
# confusing first run here, and the Java implementation's session hit the same thing from
# the other side -- fixing a crash removed an accidental stagger between two replicas and
# they then collided on a node at 108% of its memory.
node_mem_ki=$("${KUBECTL[@]}" get nodes -o jsonpath='{.items[0].status.allocatable.memory}' | tr -d 'Ki')
# Read the rendered spec, not the file. The first version grepped `requests:` with three
# lines of context and found nothing, because a comment block sits between the key and the
# value -- and then reported "2 replicas x  = 0 MiB" and PASSED. A check that measures
# nothing and passes is the failure this harness exists to avoid, written into the harness.
req=$("${KUBECTL[@]}" apply --dry-run=client -o jsonpath='{.spec.template.spec.containers[0].resources.requests.memory}' \
        -f "$ROOT/k8s/deployment.yaml" 2>/dev/null)
reps=$("${KUBECTL[@]}" apply --dry-run=client -o jsonpath='{.spec.replicas}' -f "$ROOT/k8s/deployment.yaml" 2>/dev/null)
case "$req" in
  (*Gi) req_mi=$(( ${req%Gi} * 1024 ));;
  (*Mi) req_mi=${req%Mi};;
  (*)   req_mi="";;
esac
if [ -z "$req_mi" ] || [ -z "$reps" ]; then
  bad "could not read the memory request or replica count from deployment.yaml (got req='$req' replicas='$reps')"
else
  want_mi=$(( req_mi * reps ))
  total_mi=$(( node_mem_ki / 1024 ))

  # Against what is *available*, not what the node has.
  #
  # Comparing to allocatable was the second version of this check and it was still blind:
  # it passed on a node already at 81% of its memory requests, because nothing subtracted
  # what everything else had reserved. A capacity check that ignores the other tenants
  # answers a question nobody asked.
  #
  # Our own namespace is excluded: those pods are about to be replaced by this deploy.
  used_mi=$("${KUBECTL[@]}" get pods --all-namespaces \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{range .spec.containers[*]}{.resources.requests.memory}{" "}{end}{"\n"}{end}' 2>/dev/null \
    | awk -v skip="$NS" '$1!=skip{for(i=2;i<=NF;i++){v=$i;
        if (v ~ /Gi$/) {sub(/Gi$/,"",v); m+=v*1024}
        else if (v ~ /Mi$/) {sub(/Mi$/,"",v); m+=v}
        else if (v ~ /Ki$/) {sub(/Ki$/,"",v); m+=v/1024}}} END{printf "%d", m}')
  free_mi=$(( total_mi - used_mi ))

  printf '  node %d MiB allocatable, %d MiB reserved by other namespaces, %d MiB free; this deploy wants %s x %s = %d MiB\n' \
    "$total_mi" "$used_mi" "$free_mi" "$reps" "$req" "$want_mi"
  if [ "$want_mi" -gt "$free_mi" ]; then
    bad "only $free_mi MiB is free -- a replica will sit Pending and the rollout will just time out"
  else
    ok "the node has room for $reps replicas at $req ($want_mi of $free_mi MiB free)"
  fi
fi

say "deploy"
"${KUBECTL[@]}" apply -f "$ROOT/k8s/namespace.yaml"
"${KUBECTL[@]}" apply -f "$ROOT/k8s/kind/postgres.yaml"
"${KUBECTL[@]}" -n "$NS" rollout status deploy/postgres --timeout=180s

"${KUBECTL[@]}" -n "$NS" create secret generic ai-customer-service-go-secrets \
  --from-literal=ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-placeholder-no-model-call-is-made-during-startup}" \
  --from-literal=POSTGRES_USER=csagent \
  --from-literal=POSTGRES_PASSWORD=csagent \
  --dry-run=client -o yaml | "${KUBECTL[@]}" apply -f - >/dev/null

# Make the cold-database path real on every run, not just the first.
#
# CREATE EXTENSION IF NOT EXISTS is not concurrency-safe, and the check below is only
# meaningful if both replicas actually start against a database without the extension.
# On a --keep run the extension already exists from last time, so the race cannot happen
# and the check passes without testing anything. It reported PASS for two days that way.
#
# The whole schema, not just the extension. `DROP EXTENSION vector CASCADE` takes the
# `embedding` column with it and leaves the table behind, so `CREATE TABLE IF NOT EXISTS`
# then does nothing and the app comes up serving 500s from a table with no vector column.
# That is a state no deployment reaches on its own -- it was the harness inventing a bug.
"${KUBECTL[@]}" -n "$NS" exec deploy/postgres -- psql -U csagent -d csagent \
  -c 'DROP SCHEMA public CASCADE' -c 'CREATE SCHEMA public' >/dev/null 2>&1 || true

# The directory form on purpose: it has to be safe, which is why the Secret template
# lives in k8s/examples/.
"${KUBECTL[@]}" apply -f "$ROOT/k8s/"
# Force both replicas to start together against that cold database.
"${KUBECTL[@]}" -n "$NS" rollout restart deploy/ai-customer-service-go >/dev/null 2>&1 || true
# Not fatal. A failed rollout is a result: the assertions below say *why*, and
# "OOMKilled -- the memory limit is too low" is a better last line than a rollout timeout.
"${KUBECTL[@]}" -n "$NS" rollout status deploy/ai-customer-service-go --timeout=300s || true

say "assertions"
# Resolving a pod is not a one-shot operation while a rollout is settling.
#
# `phase == "Running"` is true of a pod that is shutting down, so the first version of
# this failed intermittently with "cannot exec into a container in a completed pod;
# current phase is Succeeded". Selecting a *Ready* pod instead was not enough either:
# Ready and terminating are both true of an old pod for a few seconds, and the pod can
# begin terminating between being chosen and being exec'd into.
#
# So: exclude anything with a deletionTimestamp, and re-resolve on failure rather than
# trusting a name to stay valid. Two wrong fixes preceded this one, and both looked
# right because the next run happened to pass.
# ready_pod [COMPONENT] -- defaults to the API, since most assertions are about it.
ready_pod() {
  "${KUBECTL[@]}" -n "$NS" get pods -l "app.kubernetes.io/component=${1:-app}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.conditions[?(@.type=="Ready")].status}{" "}{.metadata.deletionTimestamp}{"\n"}{end}' \
    | awk '$2=="True" && $3==""{print $1; exit}'
}

# exec_in_pod DESCRIPTION EXPECTED-SUBSTRING -- COMMAND...
#
# COMPONENT selects which deployment's pod to enter; it is a variable rather than an
# argument so the existing calls read unchanged.
#
# Use this rather than `kubectl exec ... | grep -q`. Under `set -o pipefail` the exec's
# own non-zero exit -- which is exactly what a successful "this should fail" assertion
# produces -- fails the pipeline even when grep matched. That cost a red assertion against
# a pod whose filesystem was demonstrably read-only.
exec_in_pod() {
  local d=$1 pattern=$2; shift 2
  local out="" pod=""
  for _ in $(seq 1 10); do
    pod=$(ready_pod "${COMPONENT:-app}")
    if [ -n "$pod" ]; then
      out=$("${KUBECTL[@]}" -n "$NS" exec "$pod" -- "$@" 2>&1) || true
      case "$out" in
        (*"completed pod"*|*"not found"*|*"is terminating"*) ;;   # stale name, re-resolve
        (*) case "$out" in
              (*"$pattern"*) ok "$d"; return;;
              (*) bad "$d -- got: ${out:0:90}"; return;;
            esac;;
      esac
    fi
    sleep 2
  done
  bad "$d -- no Ready, non-terminating pod after 20s"
}

POD=$(ready_pod)

replicas=$("${KUBECTL[@]}" -n "$NS" get deploy ai-customer-service-go -o jsonpath='{.status.readyReplicas}')
[[ ${replicas:-0} == 2 ]] && ok "both replicas ready" || bad "readyReplicas=${replicas:-0}, want 2"

if "${KUBECTL[@]}" -n "$NS" get pods -l app.kubernetes.io/component=app -o json | grep -q OOMKilled; then
  bad "a container was OOMKilled -- the memory limit is too low"
else
  ok "no container was OOMKilled"
fi

if "${KUBECTL[@]}" -n "$NS" get secret ai-customer-service-go-secrets \
     -o jsonpath='{.data.ANTHROPIC_API_KEY}' | base64 -d | grep -q REPLACE_ME; then
  bad "the directory apply overwrote the Secret with placeholders"
else
  ok "the directory apply left the Secret alone"
fi

# CREATE EXTENSION IF NOT EXISTS is not concurrency-safe in Postgres: two replicas
# starting against a cold database can collide on pg_extension_name_index. Reported per
# replica rather than asserted, because whether it happens is a race.
#
# `grep -c` with `|| true`, not `grep -q`: under `set -o pipefail`, `kubectl logs | grep -q`
# fails *because it matched* -- grep exits at the first hit, kubectl takes SIGPIPE, and
# pipefail reports the pipeline as failed. A detector that breaks exactly when it fires.
raced=0
for p in $("${KUBECTL[@]}" -n "$NS" get pods -l app.kubernetes.io/component=app -o name); do
  hits=$("${KUBECTL[@]}" -n "$NS" logs "$p" --previous 2>/dev/null | grep -c pg_extension_name_index || true)
  [[ ${hits:-0} -gt 0 ]] && raced=$((raced + 1))
done
if [[ $raced -gt 0 ]]; then
  bad "$raced replica(s) lost the CREATE EXTENSION race on a cold database and restarted"
else
  ok "no replica lost the CREATE EXTENSION race"
fi

exec_in_pod "runs as uid 10001" "10001" id -u
exec_in_pod "root filesystem is read-only" "Read-only" sh -c 'touch /nope'
# The Java implementation needs a writable /tmp because ONNX Runtime unpacks its native
# library there. This one does not, and that claim is worth checking rather than asserting
# in a comment: the pod has no volumes at all and the process is serving.
if "${KUBECTL[@]}" -n "$NS" get pod "$POD" -o jsonpath='{.spec.volumes[*].name}' |
     tr ' ' '\n' | grep -qv '^kube-api-access'; then
  note "the pod mounts a volume other than the service-account token"
else
  ok "no writable volume is needed at all"
fi

"${KUBECTL[@]}" -n "$NS" port-forward svc/ai-customer-service-go 18081:8081 >/dev/null 2>&1 &
PF=$!; trap 'kill $PF 2>/dev/null || true' EXIT
sleep 4

contains "health is UP through the Service" "UP" curl -sf localhost:18081/healthz
contains "readiness reaches Postgres"       "UP" curl -sf localhost:18081/readyz
contains "the metrics endpoint serves Go metrics" "go_goroutines" curl -sf localhost:18081/metrics
contains "the demo page is served" "AI Customer Service" curl -sf localhost:18081/

# The operations surface, both ways round.
#
# Note what is *not* asserted here any more: that /admin/ is a 404 when ADMIN_TOKENS is
# unset. It is a 404 now whatever the configuration says, because the API serves no page
# at all -- so the assertion would pass for a reason unrelated to what it claimed to
# check. An API path is used instead, which is absent only when no operator is configured.
#
# First the unconfigured case, and it has to be *made* unconfigured rather than assumed:
# a --keep run reuses the Secret that the enabling half of this section patched last time,
# so assuming would give an assertion that passes once and then fails forever after.
if "${KUBECTL[@]}" -n "$NS" get secret ai-customer-service-go-secrets \
     -o jsonpath='{.data.ADMIN_TOKENS}' 2>/dev/null | grep -q .; then
  note "clearing ADMIN_TOKENS left by an earlier run, so the unconfigured case is real"
  "${KUBECTL[@]}" -n "$NS" patch secret ai-customer-service-go-secrets --type=json \
    -p '[{"op":"remove","path":"/data/ADMIN_TOKENS"}]' >/dev/null
  "${KUBECTL[@]}" -n "$NS" rollout restart deploy/ai-customer-service-go >/dev/null
  "${KUBECTL[@]}" -n "$NS" rollout status deploy/ai-customer-service-go --timeout=180s >/dev/null
  kill $PF 2>/dev/null || true
  "${KUBECTL[@]}" -n "$NS" port-forward svc/ai-customer-service-go 18081:8081 >/dev/null 2>&1 &
  PF=$!; trap 'kill $PF 2>/dev/null || true' EXIT
  sleep 4
fi

# Unconfigured has to mean the routes were never registered. A 404 says that. A 401 would
# say the routes exist and something is deciding -- and a decision can be misconfigured,
# while an absent route cannot.
status=$(curl -s -o /dev/null -w '%{http_code}' localhost:18081/api/admin/v1/whoami || echo 000)
[[ $status == 404 ]] && ok "with no ADMIN_TOKENS the admin API does not exist (404, not 401)" \
                     || bad "whoami returned $status with no ADMIN_TOKENS, want 404"

# The API serves no UI at any configuration. It used to embed one.
status=$(curl -s -o /dev/null -w '%{http_code}' localhost:18081/admin/ || echo 000)
[[ $status == 404 ]] && ok "the API serves no page at /admin" \
                     || bad "/admin/ on the API returned $status, want 404"

# And then turn it on, because "documented but never deployed" is how the manifests in the
# sibling Java repository were wrong twice. This patches the Secret and the ConfigMap the
# harness created itself -- k8s/ is still applied unmodified -- and restarts, which is
# exactly what a real operator would do.
say "operations surface"
PROBE_TOKEN=$(openssl rand -hex 24)
UI_ORIGIN="http://localhost:18090"
"${KUBECTL[@]}" -n "$NS" patch secret ai-customer-service-go-secrets --type=merge \
  -p "{\"stringData\":{\"ADMIN_TOKENS\":\"probe:${PROBE_TOKEN}:operator\"}}" >/dev/null
"${KUBECTL[@]}" -n "$NS" patch configmap ai-customer-service-go-config --type=merge \
  -p "{\"data\":{\"ADMIN_CORS_ORIGINS\":\"${UI_ORIGIN}\"}}" >/dev/null
"${KUBECTL[@]}" -n "$NS" rollout restart deploy/ai-customer-service-go >/dev/null
"${KUBECTL[@]}" -n "$NS" rollout status deploy/ai-customer-service-go --timeout=180s >/dev/null

# The old port-forward pointed at pods that no longer exist.
kill $PF 2>/dev/null || true
"${KUBECTL[@]}" -n "$NS" port-forward svc/ai-customer-service-go 18081:8081 >/dev/null 2>&1 &
PF=$!; trap 'kill $PF 2>/dev/null || true' EXIT
sleep 4

status=$(curl -s -o /dev/null -w '%{http_code}' localhost:18081/api/admin/v1/whoami || echo 000)
[[ $status == 401 ]] && ok "the admin API refuses a request with no token (401)" \
                     || bad "whoami with no token returned $status, want 401"

# The token is passed through a variable and never printed: a PASS line is the only thing
# this assertion is allowed to leave behind.
status=$(curl -s -o /dev/null -w '%{http_code}' localhost:18081/api/admin/v1/whoami \
           -H "Authorization: Bearer ${PROBE_TOKEN}" || echo 000)
[[ $status == 200 ]] && ok "an operator token is accepted through the Service (200)" \
                     || bad "whoami with a valid token returned $status, want 200"

# The UI is a separate origin now, so the browser decides whether it may read these
# responses. These two are that decision, and they are the assertions that would catch a
# ConfigMap whose ADMIN_CORS_ORIGINS does not match where the UI is actually served from.
status=$(curl -s -o /dev/null -w '%{http_code}' -X OPTIONS localhost:18081/api/admin/v1/whoami \
           -H "Origin: ${UI_ORIGIN}" -H 'Access-Control-Request-Method: GET' || echo 000)
[[ $status == 204 ]] && ok "a preflight from the configured UI origin is answered (204)" \
                     || bad "preflight from ${UI_ORIGIN} returned $status, want 204"

status=$(curl -s -o /dev/null -w '%{http_code}' -X OPTIONS localhost:18081/api/admin/v1/whoami \
           -H 'Origin: http://not-the-ui.test' -H 'Access-Control-Request-Method: GET' || echo 000)
[[ $status == 403 ]] && ok "a preflight from any other origin is refused (403)" \
                     || bad "preflight from an unlisted origin returned $status, want 403"

say "operations UI"
UI_IMAGE=$(grep -m1 'image: ghcr.io/lai3d/ai-customer-service-go-admin-ui' "$ROOT/k8s/admin-ui.yaml" | awk '{print $2}')
if [[ ${1:-} != --keep ]] || ! docker image inspect "$UI_IMAGE" >/dev/null 2>&1; then
  docker build -q -t "$UI_IMAGE" "$ROOT/admin-ui" >/dev/null
fi
kind load docker-image "$UI_IMAGE" --name "$CLUSTER" >/dev/null

# The API base the browser will use is the port-forward, because that is where a browser
# on this machine would reach the cluster from.
"${KUBECTL[@]}" apply -f "$ROOT/k8s/admin-ui.yaml" >/dev/null
"${KUBECTL[@]}" -n "$NS" patch configmap ai-customer-service-go-admin-ui --type=merge \
  -p '{"data":{"ADMIN_API_BASE":"http://localhost:18081"}}' >/dev/null
"${KUBECTL[@]}" -n "$NS" rollout restart deploy/ai-customer-service-go-admin-ui >/dev/null
if "${KUBECTL[@]}" -n "$NS" rollout status deploy/ai-customer-service-go-admin-ui --timeout=180s >/dev/null; then
  ok "the operations UI rolled out"
else
  bad "the operations UI did not become ready"
fi

"${KUBECTL[@]}" -n "$NS" port-forward svc/ai-customer-service-go-admin-ui 18090:8080 >/dev/null 2>&1 &
PFUI=$!; trap 'kill $PF $PFUI 2>/dev/null || true' EXIT
sleep 4

contains "the operations UI is served" "<title>Operations</title>" curl -sf localhost:18090/

# config.js is written at start-up from the ConfigMap. If this said the wrong origin the
# page would load, look correct, and fail every request with an opaque network error.
contains "config.js carries the API base from the ConfigMap" "localhost:18081" \
  curl -sf localhost:18090/config.js

# nginx does not inherit add_header into a location that sets one of its own, which is how
# this header went missing from / while remaining in the config file.
if curl -sfI localhost:18090/ | grep -qi content-security-policy; then
  ok "the UI sends a Content-Security-Policy on the document itself"
else
  bad "no Content-Security-Policy on GET / from the UI"
fi

COMPONENT=admin-ui exec_in_pod "the UI runs as uid 101" "101" id -u
COMPONENT=admin-ui exec_in_pod "the UI's root filesystem is read-only" "Read-only" \
  sh -c 'touch /nope'
# ...and /tmp is writable, because config.js is written there at start-up and a read-only
# root with nowhere to write is a pod that starts and serves the wrong API base.
COMPONENT=admin-ui exec_in_pod "the UI can still write /tmp, where config.js goes" "ok" \
  sh -c 'touch /tmp/probe && echo ok' 

# GOMAXPROCS comes from the cgroup CPU limit on Go 1.25+, and it is what the embedding
# concurrency bound defaults to. Reported, because the number is the point.
gomax=$(curl -s localhost:18081/metrics | awk '/^go_sched_gomaxprocs_threads/{print $2}')
node_cpus=$("${KUBECTL[@]}" get nodes -o jsonpath='{.items[0].status.capacity.cpu}')
note "GOMAXPROCS=${gomax:-?} inside the pod; the node has ${node_cpus} CPUs"

# Retrieval runs before the model call, so this exercises the embedding path and then
# fails at the provider -- which must be a 502, not a 500 and not a healthy 200.
status=$(curl -s -o /dev/null -w '%{http_code}' localhost:18081/api/v1/chat \
           -H 'Content-Type: application/json' \
           -d '{"message":"How long do I have to return an item?"}' || echo 000)
if [[ -n ${ANTHROPIC_API_KEY:-} ]]; then
  [[ $status == 200 ]] && ok "a real turn answered (200)" || bad "a real turn returned $status, want 200"
else
  [[ $status == 502 ]] && ok "a bad key surfaces as 502, not a healthy error" \
                       || bad "a bad key returned $status, want 502"
fi

say "footprint"
"${KUBECTL[@]}" top pods -n "$NS" -l app.kubernetes.io/component=app --no-headers 2>/dev/null \
  | sed 's/^/  /' || echo "  (metrics-server not installed; see k8s/README.md for the measured numbers)"

say "result"
printf '  %d passed, %d failed\n' "$PASS" "$FAIL"
printf '  cluster left running; %s --down to remove it\n' "$0"
[[ $FAIL -eq 0 ]]
