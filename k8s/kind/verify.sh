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
# Compare captured output instead of piping into `grep -q`.
#
# Under `set -o pipefail`, `producer | grep -q pattern` fails *because it matched*: grep
# exits at the first hit, the producer takes SIGPIPE and exits 141, and pipefail reports
# the pipeline as failed. Whether it loses the race depends on how fast the producer
# finishes, so it is flaky rather than broken -- these checks passed on the first run of
# this script and failed on the second against an identical pod.
#
# The Java implementation's harness documents this trap for its `grep -c` detector. This
# script copied the harness, kept the comment, and fell into the same trap somewhere else.
contains(){ local d=$1 pattern=$2; shift 2
  local out; out=$("$@" 2>&1) || true
  case "$out" in (*"$pattern"*) ok "$d";; (*) bad "$d -- got: ${out:0:80}";; esac; }

if [[ ${1:-} == --down ]]; then kind delete cluster --name "$CLUSTER"; exit 0; fi

for t in kind kubectl docker; do
  command -v "$t" >/dev/null || { echo "missing: $t" >&2; exit 1; }
done

say "cluster"
kind get clusters 2>/dev/null | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
kubectl config use-context "kind-$CLUSTER" >/dev/null

say "image  $IMAGE"
if [[ ${1:-} == --keep ]] && docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "  reusing the local image"
else
  docker build -t "$IMAGE" "$ROOT"
fi
# 1.1 GB, of which 470 MB is the embedding model. This takes a minute and is the honest
# cost of baking the model in rather than downloading it at startup.
kind load docker-image "$IMAGE" --name "$CLUSTER"

say "deploy"
kubectl apply -f "$ROOT/k8s/namespace.yaml"
kubectl apply -f "$ROOT/k8s/kind/postgres.yaml"
kubectl -n "$NS" rollout status deploy/postgres --timeout=180s

kubectl -n "$NS" create secret generic ai-customer-service-go-secrets \
  --from-literal=ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-placeholder-no-model-call-is-made-during-startup}" \
  --from-literal=POSTGRES_USER=csagent \
  --from-literal=POSTGRES_PASSWORD=csagent \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# The directory form on purpose: it has to be safe, which is why the Secret template
# lives in k8s/examples/.
kubectl apply -f "$ROOT/k8s/"
# Not fatal. A failed rollout is a result: the assertions below say *why*, and
# "OOMKilled -- the memory limit is too low" is a better last line than a rollout timeout.
kubectl -n "$NS" rollout status deploy/ai-customer-service-go --timeout=300s || true

say "assertions"
POD=$(kubectl -n "$NS" get pods -l app.kubernetes.io/component=app \
        -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' | awk '{print $1}')

replicas=$(kubectl -n "$NS" get deploy ai-customer-service-go -o jsonpath='{.status.readyReplicas}')
[[ ${replicas:-0} == 2 ]] && ok "both replicas ready" || bad "readyReplicas=${replicas:-0}, want 2"

if kubectl -n "$NS" get pods -l app.kubernetes.io/component=app -o json | grep -q OOMKilled; then
  bad "a container was OOMKilled -- the memory limit is too low"
else
  ok "no container was OOMKilled"
fi

if kubectl -n "$NS" get secret ai-customer-service-go-secrets \
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
for p in $(kubectl -n "$NS" get pods -l app.kubernetes.io/component=app -o name); do
  hits=$(kubectl -n "$NS" logs "$p" --previous 2>/dev/null | grep -c pg_extension_name_index || true)
  [[ ${hits:-0} -gt 0 ]] && raced=$((raced + 1))
done
if [[ $raced -gt 0 ]]; then
  bad "$raced replica(s) lost the CREATE EXTENSION race on a cold database and restarted"
else
  ok "no replica lost the CREATE EXTENSION race"
fi

contains "runs as uid 10001" "10001" kubectl -n "$NS" exec "$POD" -- id -u
contains "root filesystem is read-only" "Read-only" \
         kubectl -n "$NS" exec "$POD" -- sh -c 'touch /nope'
# The Java implementation needs a writable /tmp because ONNX Runtime unpacks its native
# library there. This one does not, and that claim is worth checking rather than asserting
# in a comment: the pod has no volumes at all and the process is serving.
if kubectl -n "$NS" get pod "$POD" -o jsonpath='{.spec.volumes[*].name}' |
     tr ' ' '\n' | grep -qv '^kube-api-access'; then
  note "the pod mounts a volume other than the service-account token"
else
  ok "no writable volume is needed at all"
fi

kubectl -n "$NS" port-forward svc/ai-customer-service-go 18081:8081 >/dev/null 2>&1 &
PF=$!; trap 'kill $PF 2>/dev/null || true' EXIT
sleep 4

contains "health is UP through the Service" "UP" curl -sf localhost:18081/healthz
contains "readiness reaches Postgres"       "UP" curl -sf localhost:18081/readyz
contains "the metrics endpoint serves Go metrics" "go_goroutines" curl -sf localhost:18081/metrics
contains "the demo page is served" "AI Customer Service" curl -sf localhost:18081/

# GOMAXPROCS comes from the cgroup CPU limit on Go 1.25+, and it is what the embedding
# concurrency bound defaults to. Reported, because the number is the point.
gomax=$(curl -s localhost:18081/metrics | awk '/^go_sched_gomaxprocs_threads/{print $2}')
node_cpus=$(kubectl get nodes -o jsonpath='{.items[0].status.capacity.cpu}')
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
kubectl top pods -n "$NS" -l app.kubernetes.io/component=app --no-headers 2>/dev/null \
  | sed 's/^/  /' || echo "  (metrics-server not installed; see k8s/README.md for the measured numbers)"

say "result"
printf '  %d passed, %d failed\n' "$PASS" "$FAIL"
printf '  cluster left running; %s --down to remove it\n' "$0"
[[ $FAIL -eq 0 ]]
