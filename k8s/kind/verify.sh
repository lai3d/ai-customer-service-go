#!/usr/bin/env bash
# Verify the Kubernetes manifests on a throwaway kind cluster.
#
#   k8s/kind/verify.sh            create the cluster, deploy, assert, leave it running
#   k8s/kind/verify.sh --down     delete the cluster and exit
#   k8s/kind/verify.sh --keep     skip the image build if the tag is already present
#
# It applies the manifests in k8s/ *unmodified*. That is the point: a harness that patches
# the resources or the image before applying verifies the patch, not the file anyone else
# will use. The only things it adds are the ones the manifests deliberately do not ship --
# a Postgres (k8s/kind/postgres.yaml), a Secret and a TLS certificate created imperatively,
# and the two cluster add-ons a real cluster already has: an ingress controller for
# k8s/ingress.yaml to be served by, and metrics-server for the HorizontalPodAutoscaler to
# read. Those are infrastructure, not manifests; both are pinned to a version here.
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
# Pinned, because "whatever is latest today" is not a thing a harness can be re-run
# against next month and get the same answer from.
INGRESS_NGINX=controller-v1.13.3
METRICS_SERVER=v0.8.0
# The hostnames in k8s/ingress.yaml. .test is reserved for exactly this and resolves
# nowhere, which is why every curl below carries --resolve.
CHAT_HOST=chat.example.test
OPS_HOST=ops.example.test
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

# Encryption at rest for etcd, so "a Kubernetes Secret is base64" can be measured rather
# than asserted -- in both directions.
#
# Without this, the value is in etcd in the clear: `etcdctl get
# /registry/secrets/<ns>/<name>` on the control-plane node returns it, and an etcd backup is
# a copy of every key you hold. That was measured on this cluster before the config existed
# and the command is in docs/deployment.md.
#
# The key is generated per run and gitignored. A committed key that is "only for tests" is
# the shape a real one eventually takes, and this cluster is deleted anyway.
#
# aescbc rather than a KMS provider: a real deployment wants KMS, and a KMS provider needs a
# KMS. What is being demonstrated here is the mechanism and the property, not the key
# custody -- and the property is the part a manifest cannot show you.
ENCRYPTION_CONF="$(dirname "$0")/encryption.yaml"
if [ ! -f "$ENCRYPTION_CONF" ]; then
  cat > "$ENCRYPTION_CONF" <<ENC
apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources: ["secrets"]
    providers:
      - aescbc:
          keys:
            - name: harness
              secret: $(head -c 32 /dev/urandom | base64)
      # identity last: it decrypts what was written before this file existed. First, it
      # would mean "write everything in the clear" while looking configured.
      - identity: {}
ENC
fi

say "cluster"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  # The cluster exists but this kubeconfig may not describe it yet.
  kind export kubeconfig --name "$CLUSTER" >/dev/null
else
  kind create cluster --name "$CLUSTER" --wait 120s --config - <<KIND
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: $(cd "$(dirname "$ENCRYPTION_CONF")" && pwd)/$(basename "$ENCRYPTION_CONF")
        containerPath: /etc/kubernetes/encryption.yaml
        readOnly: true
    kubeadmConfigPatches:
      - |
        kind: ClusterConfiguration
        apiServer:
          extraArgs:
            encryption-provider-config: /etc/kubernetes/encryption.yaml
          extraVolumes:
            - name: encryption
              hostPath: /etc/kubernetes/encryption.yaml
              mountPath: /etc/kubernetes/encryption.yaml
              readOnly: true
              pathType: File
KIND
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

say "image references"
# Static, and the only assertion here that needs no cluster: no manifest may float.
#
# `:latest` with imagePullPolicy: Always is how a cluster ends up running something nobody
# can name -- two pods of the same Deployment on the same tag, started a week apart, are
# two different builds and nothing anywhere says so. docs/deployment.md documents the
# path from this Dockerfile to a registry and to a digest; this checks the outcome.
#
# The count is asserted as well as the contents. A check that reads no images at all and
# reports PASS is the capacity check's first version all over again -- it measured nothing
# and said so in green.
images=$(awk '/^ *image: /{print $NF}' "$ROOT/k8s"/*.yaml)
n_images=$(printf '%s\n' "$images" | grep -c . || true)
floating=$(printf '%s\n' "$images" | grep -E ':latest$|^[^:]*$' || true)
if [ "${n_images:-0}" -lt 2 ]; then
  bad "read $n_images image references out of k8s/*.yaml; this check has stopped looking"
elif [ -n "$floating" ]; then
  bad "a manifest points at a floating image: $(printf '%s' "$floating" | tr '\n' ' ')"
else
  ok "all $n_images images in k8s/ carry an explicit tag, none is :latest"
fi

say "cluster add-ons"
# Infrastructure a real cluster already has and these manifests do not ship, in the same
# spirit as the Postgres: without an ingress controller k8s/ingress.yaml is a document
# rather than a route, and without metrics-server the HorizontalPodAutoscaler has nothing
# to read and reports ScalingActive=False for ever -- which is exactly the state a wrong
# HPA is in, so the assertions could not tell them apart.
if "${KUBECTL[@]}" -n ingress-nginx get deploy ingress-nginx-controller >/dev/null 2>&1; then
  echo "  ingress-nginx is already installed"
else
  # The kind flavour of the manifest schedules on a node labelled ingress-ready. That
  # label normally comes from a cluster config file; this harness creates its cluster with
  # no config so that `--down` and a fresh `kind create` stay symmetrical, so the label
  # goes on afterwards.
  "${KUBECTL[@]}" label node "${CLUSTER}-control-plane" ingress-ready=true --overwrite >/dev/null
  "${KUBECTL[@]}" apply -f \
    "https://raw.githubusercontent.com/kubernetes/ingress-nginx/${INGRESS_NGINX}/deploy/static/provider/kind/deploy.yaml" >/dev/null
fi
if ! "${KUBECTL[@]}" -n kube-system get deploy metrics-server >/dev/null 2>&1; then
  "${KUBECTL[@]}" apply -f \
    "https://github.com/kubernetes-sigs/metrics-server/releases/download/${METRICS_SERVER}/components.yaml" >/dev/null
fi
# kind's kubelet serves a self-signed certificate, so metrics-server refuses to scrape it
# until told not to check. Read the args and match on the text rather than piping into
# `grep -q`: a matching grep exits early, kubectl takes SIGPIPE, and under `pipefail` the
# pipeline reports failure *because the flag was found* -- which here would append the
# flag a second time on every run.
ms_args=$("${KUBECTL[@]}" -n kube-system get deploy metrics-server \
            -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || true)
case "$ms_args" in
  (*kubelet-insecure-tls*) ;;
  (*) "${KUBECTL[@]}" -n kube-system patch deploy metrics-server --type=json \
        -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]' >/dev/null;;
esac
"${KUBECTL[@]}" -n kube-system rollout status deploy/metrics-server --timeout=180s >/dev/null
"${KUBECTL[@]}" -n ingress-nginx rollout status deploy/ingress-nginx-controller --timeout=300s >/dev/null
echo "  ingress-nginx ${INGRESS_NGINX} and metrics-server ${METRICS_SERVER} ready"

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

# Remove the probe pods a previous run may have left behind, before anything else looks
# at this namespace.
#
# `app-egress-probe` wears the app's labels on purpose -- that is how it gets the app's
# NetworkPolicy -- which also makes it an endpoint of the Service. A run interrupted
# between creating it and deleting it leaves a pod in the Service that listens on nothing,
# and `kubectl port-forward svc/...` then picks it about half the time: four service-level
# assertions fail with an empty body and nothing says why. Observed, on the run after the
# probe was first written.
"${KUBECTL[@]}" -n "$NS" delete pod app-egress-probe netpolicy-probe \
  --ignore-not-found --now >/dev/null 2>&1 || true
"${KUBECTL[@]}" apply -f "$ROOT/k8s/kind/postgres.yaml"
"${KUBECTL[@]}" -n "$NS" rollout status deploy/postgres --timeout=180s

"${KUBECTL[@]}" -n "$NS" create secret generic ai-customer-service-go-secrets \
  --from-literal=ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-placeholder-no-model-call-is-made-during-startup}" \
  --from-literal=POSTGRES_USER=csagent \
  --from-literal=POSTGRES_PASSWORD=csagent \
  --dry-run=client -o yaml | "${KUBECTL[@]}" apply -f - >/dev/null

# The certificate k8s/ingress.yaml names. Self-signed, generated per run, thrown away.
#
# It is created here for the same reason the API key is: a private key is not a thing a
# repository can hold, and an Ingress manifest that shipped one would be a manifest whose
# TLS was decorative. What this buys is the assertion below -- that the certificate the
# controller actually serves is the one in this Secret. Get the name wrong and
# ingress-nginx does not fail; it serves its own "Kubernetes Ingress Controller Fake
# Certificate" and the site keeps working, which is the quietest failure in this file.
TLSDIR=$(mktemp -d)
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -keyout "$TLSDIR/tls.key" -out "$TLSDIR/tls.crt" \
  -subj "/CN=ai-customer-service-go kind harness" \
  -addext "subjectAltName=DNS:${CHAT_HOST},DNS:${OPS_HOST}" 2>/dev/null
"${KUBECTL[@]}" -n "$NS" create secret tls ai-customer-service-go-tls \
  --cert="$TLSDIR/tls.crt" --key="$TLSDIR/tls.key" \
  --dry-run=client -o yaml | "${KUBECTL[@]}" apply -f - >/dev/null
rm -rf "$TLSDIR"

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

say "network policy"
# Every assertion above this line ran with k8s/networkpolicy.yaml in force, which is
# already evidence: default-deny plus five allow rules, and the app still reaches Postgres
# (/readyz), the kubelet still reaches both probes, and the ingress controller still
# reaches both Services. Those are the "allowed" half.
#
# This section is the other half, and it is the half worth having. A NetworkPolicy on a
# cluster whose CNI ignores policy applies cleanly, lists cleanly, and permits everything;
# nothing anywhere reports an error. So the first assertion here is really "does this
# cluster enforce policy at all", asked in the only way that can answer it -- by opening a
# socket that must not open.
"${KUBECTL[@]}" -n "$NS" delete pod netpolicy-probe --ignore-not-found --now >/dev/null 2>&1 || true
cat <<YAML | "${KUBECTL[@]}" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: netpolicy-probe
  namespace: $NS
  labels:
    # Deliberately not the app's labels: this is the unlabelled sidecar, the debug pod
    # somebody ran with kubectl run, the next component that has not been thought about.
    # Nothing in k8s/networkpolicy.yaml selects it, so default-deny is all it gets.
    app.kubernetes.io/component: netpolicy-probe
spec:
  terminationGracePeriodSeconds: 1
  containers:
    - name: probe
      # The Postgres fixture's image, already on the node, and it carries bash -- whose
      # /dev/tcp is the smallest thing that can open a TCP connection and say so.
      image: pgvector/pgvector:pg17
      imagePullPolicy: IfNotPresent
      command: ["bash", "-c", "sleep 1800"]
YAML
"${KUBECTL[@]}" -n "$NS" wait --for=condition=Ready pod/netpolicy-probe --timeout=180s >/dev/null

# By ClusterIP, never by name. The probe pod has no DNS egress either, so a name-based
# probe would fail at resolution and report "blocked" without a packet ever being sent --
# an assertion that passes whether or not the policy exists.
APP_IP=$("${KUBECTL[@]}" -n "$NS" get svc ai-customer-service-go -o jsonpath='{.spec.clusterIP}')
PG_IP=$("${KUBECTL[@]}" -n "$NS" get svc postgres -o jsonpath='{.spec.clusterIP}')

# tcp_probe DESCRIPTION PROBE_OPEN|PROBE_BLOCKED POD HOST PORT
#
# By pod name rather than through `exec_in_pod`, whose job is to re-resolve a pod that a
# rollout is replacing underneath it. These two are single pods this script created and
# nothing is replacing them, and selecting by component would be actively wrong for the
# egress probe: it wears the app's labels, so a component lookup would find a real app
# replica -- an image with no bash in it.
#
# The inner command always exits 0 and prints which happened, so the exec's exit code
# carries no meaning and cannot be confused with the assertion's -- the trap that cost a
# red assertion against a demonstrably read-only filesystem earlier in this file.
#
# It retries until it sees what it expects, for up to half a minute, and says how long it
# took. That is not a way of turning a red assertion green: a rule that is wrong stays
# wrong for all ten attempts and fails with what it got. It is there because the dataplane
# is eventually consistent -- the policy agent programs a *new* pod's rules a moment after
# the pod is Ready, and the first version of this section probed instantly and watched a
# denied connection open. Measured at up to four attempts, around ten seconds, on kind; a
# run reporting more than one attempt is reporting something real about the CNI.
tcp_probe() {
  local d=$1 want=$2 pod=$3 host=$4 port=$5 out="" tries=0
  for _ in $(seq 1 10); do
    tries=$((tries+1))
    out=$("${KUBECTL[@]}" -n "$NS" exec "$pod" -- bash -c \
      "timeout 5 bash -c 'exec 3<>/dev/tcp/$host/$port' && echo PROBE_OPEN || echo PROBE_BLOCKED" 2>&1) || true
    case "$out" in (*"$want"*) break;; esac
    sleep 3
  done
  case "$out" in
    (*"$want"*) ok "$d$([ $tries -gt 1 ] && echo " (settled after $tries attempts)")";;
    (*)         bad "$d -- got: ${out:0:90}";;
  esac
}

tcp_probe "a pod that no policy names cannot reach Postgres (so this CNI enforces policy)" \
  PROBE_BLOCKED netpolicy-probe "$PG_IP" 5432
tcp_probe "a pod that no policy names cannot reach the API" \
  PROBE_BLOCKED netpolicy-probe "$APP_IP" 8081

# The operations UI reaches nothing. It is nginx and a bundle; the browser calls the API,
# which is what ADMIN_CORS_ORIGINS is about. If this pod could open the API, the CORS
# design would be describing a boundary that does not exist.
COMPONENT=admin-ui exec_in_pod "the operations UI cannot open a connection to the API" \
  PROBE_BLOCKED sh -c \
  "wget -T 4 -q -O- http://$APP_IP:8081/healthz >/dev/null 2>&1 && echo PROBE_OPEN || echo PROBE_BLOCKED"
# The fully qualified name, and that is the whole difference between this check and a
# useless one. The first version asked for the short name `ai-customer-service-go`, and
# busybox's nslookup does not walk the search path: with DNS egress deliberately opened it
# still exited non-zero, on an NXDOMAIN from a resolver it had reached perfectly well. The
# check reported "cannot resolve" while the pod was resolving. It was found by red-testing
# it -- the perturbation that should have turned it red did not.
COMPONENT=admin-ui exec_in_pod "the operations UI cannot even resolve a name" \
  PROBE_BLOCKED sh -c \
  "timeout 8 nslookup ai-customer-service-go.$NS.svc.cluster.local >/dev/null 2>&1 && echo PROBE_OPEN || echo PROBE_BLOCKED"

say "ingress and TLS"
# k8s/ingress.yaml is an example that needs a real controller and a real certificate, and
# "needs" is the reason it is applied here rather than trusted: the two wrong manifests in
# the sibling Java repository were wrong in ways reading them would not have shown.
#
# Reached through a port-forward to the controller rather than through the node's ports,
# so the harness still needs no cluster configuration file and no host ports.
ing_addr=$("${KUBECTL[@]}" -n "$NS" get ingress ai-customer-service-go \
             -o jsonpath='{.status.loadBalancer.ingress[0]}' 2>/dev/null || true)
if [ -n "$ing_addr" ]; then
  ok "a controller adopted the Ingress and published an address"
else
  bad "the Ingress has no address: ingressClassName names a controller that is not here"
fi

"${KUBECTL[@]}" -n ingress-nginx port-forward svc/ingress-nginx-controller 18443:443 >/dev/null 2>&1 &
PFTLS=$!
"${KUBECTL[@]}" -n ingress-nginx port-forward svc/ingress-nginx-controller 18080:80 >/dev/null 2>&1 &
PFHTTP=$!
trap 'kill $PF $PFUI $PFTLS $PFHTTP 2>/dev/null || true' EXIT
sleep 4

contains "the API answers through the Ingress over TLS" "UP" \
  curl -sk --max-time 10 --resolve "$CHAT_HOST:18443:127.0.0.1" "https://$CHAT_HOST:18443/healthz"
contains "the operations UI answers through the Ingress over TLS" "<title>Operations</title>" \
  curl -sk --max-time 10 --resolve "$OPS_HOST:18443:127.0.0.1" "https://$OPS_HOST:18443/"

# Which certificate, not whether TLS worked. ingress-nginx answers on 443 whatever
# happens: a missing or misnamed Secret gets you its built-in self-signed one and a
# working site, so "https:// returned 200" is not evidence that this Ingress's TLS block
# is right.
tls_seen=$(curl -skv --max-time 10 --resolve "$CHAT_HOST:18443:127.0.0.1" \
             "https://$CHAT_HOST:18443/healthz" 2>&1 || true)
case "$tls_seen" in
  (*"kind harness"*)      ok "the certificate served is the one in the Secret the Ingress names";;
  (*"Fake Certificate"*)  bad "the controller served its own fake certificate -- tls.secretName does not resolve";;
  (*)                     bad "could not read the served certificate -- got: $(printf '%s' "$tls_seen" | tr -d '\r' | grep -i -m1 'subject:' || echo none)";;
esac

# TLS is not optional on this host. ingress-nginx redirects by default when a host has a
# tls block; the annotation says so out loud, and this checks the annotation is doing what
# it says rather than being decoration.
status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
           --resolve "$CHAT_HOST:18080:127.0.0.1" "http://$CHAT_HOST:18080/healthz" || echo 000)
[[ $status == 308 ]] && ok "plain HTTP is redirected to HTTPS (308)" \
                     || bad "http:// through the Ingress returned $status, want 308"

say "autoscaling"
# An HPA fails silently in two different ways and both look like an HPA that has decided
# not to scale: a scaleTargetRef naming something that is not there (AbleToScale=False),
# and no metrics pipeline to read from (ScalingActive=False). Neither produces an event
# anybody sees, and `kubectl get hpa` prints <unknown>/200% for both.
hpa_conds=""
for _ in $(seq 1 20); do
  hpa_conds=$("${KUBECTL[@]}" -n "$NS" get hpa ai-customer-service-go \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason} {end}' 2>/dev/null || true)
  case "$hpa_conds" in (*"ScalingActive=True"*) break;; esac
  sleep 3
done
case "$hpa_conds" in
  (*"AbleToScale=True"*) ok "the HPA's scaleTargetRef resolves to a Deployment that exists";;
  (*) bad "AbleToScale is not True: $hpa_conds";;
esac
case "$hpa_conds" in
  (*"ScalingActive=True"*) ok "the HPA is reading a real CPU metric from the pods";;
  (*) bad "ScalingActive is not True: $hpa_conds";;
esac
util=$("${KUBECTL[@]}" -n "$NS" get hpa ai-customer-service-go \
        -o jsonpath='{.status.currentMetrics[0].resource.current.averageUtilization}' 2>/dev/null || true)
note "the HPA reads ${util:-?}% of requests.cpu against a 200% target; idle, so it holds at minReplicas"

say "disruption budget"
# Two ways a PodDisruptionBudget is not a PodDisruptionBudget: its selector matches no
# pods (accepted, listed, protecting nothing), or the numbers permit every replica to go
# at once. status.expectedPods answers the first. The eviction below answers the second,
# by doing what a node drain does rather than by reading the object back.
for p in ai-customer-service-go ai-customer-service-go-admin-ui; do
  read -r expected allowed <<<"$("${KUBECTL[@]}" -n "$NS" get pdb "$p" \
    -o jsonpath='{.status.expectedPods} {.status.disruptionsAllowed}' 2>/dev/null || echo '0 0')"
  if [[ ${expected:-0} == 2 && ${allowed:-0} == 1 ]]; then
    ok "the $p budget covers its 2 pods and allows exactly 1 disruption"
  else
    bad "the $p budget reports expectedPods=${expected:-none} disruptionsAllowed=${allowed:-none}, want 2 and 1"
  fi
done

# The eviction API, which is the API `kubectl drain` and every autoscaler use. The second
# call has to be refused; if it is not, a node drain takes both replicas and this service
# is gone for as long as a 470 MB model takes to load twice.
evict() {
  printf '{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"%s","namespace":"%s"}}' "$1" "$NS" \
    | "${KUBECTL[@]}" create --raw "/api/v1/namespaces/$NS/pods/$1/eviction" -f - 2>&1 || true
}
app_pods=$("${KUBECTL[@]}" -n "$NS" get pods -l app.kubernetes.io/component=app \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.conditions[?(@.type=="Ready")].status}{" "}{.metadata.deletionTimestamp}{"\n"}{end}' \
  | awk '$2=="True" && $3==""{print $1}')
first_pod=$(printf '%s\n' "$app_pods" | sed -n 1p)
second_pod=$(printf '%s\n' "$app_pods" | sed -n 2p)
if [ -z "$first_pod" ] || [ -z "$second_pod" ]; then
  bad "needed two Ready replicas to test eviction and found: $(printf '%s' "$app_pods" | tr '\n' ' ')"
else
  case "$(evict "$first_pod")" in
    (*Success*) ok "a drain may evict one replica";;
    (*)         bad "the first eviction was refused, which means the budget is too strict to drain a node";;
  esac
  # Immediately, while the replacement is still loading the model. There is no race to
  # lose here: Ready takes 4.4 s at the very best and this call is one round trip away.
  case "$(evict "$second_pod")" in
    (*"disruption budget"*) ok "and the second is refused by the budget, so a drain cannot take both";;
    (*Success*)             bad "both replicas were evicted at once; the budget is not protecting anything";;
    (*)                     bad "the second eviction failed for some other reason";;
  esac
  "${KUBECTL[@]}" -n "$NS" rollout status deploy/ai-customer-service-go --timeout=300s >/dev/null || true
fi

say "the app's own egress"
# The rules in k8s/networkpolicy.yaml select on labels, so a pod wearing the app's labels
# is given exactly the app's policy. That is what makes this a test of the rules rather
# than of the app image -- which contains no curl, no nc and no bash, on purpose.
#
# It goes last because those labels also make it an endpoint of the Service and a member
# of the PodDisruptionBudget while it lives. It is deleted at the end of the section.
cat <<YAML | "${KUBECTL[@]}" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: app-egress-probe
  namespace: $NS
  labels:
    app.kubernetes.io/name: ai-customer-service-go
    app.kubernetes.io/component: app
spec:
  terminationGracePeriodSeconds: 1
  containers:
    - name: probe
      image: pgvector/pgvector:pg17
      imagePullPolicy: IfNotPresent
      command: ["bash", "-c", "sleep 600"]
YAML
"${KUBECTL[@]}" -n "$NS" wait --for=condition=Ready pod/app-egress-probe --timeout=180s >/dev/null

# By name, this time: DNS is one of the rules being checked, and a provider's address is
# not a fixed thing to hardcode.
tcp_probe "the app may reach the model provider on 443" \
  PROBE_OPEN app-egress-probe api.anthropic.com 443
tcp_probe "the app may not reach the same host on 80" \
  PROBE_BLOCKED app-egress-probe api.anthropic.com 80
# 443 is allowed to 0.0.0.0/0 *except* the private ranges, and the target has to be an
# address something is actually listening on -- otherwise "blocked" is just nothing being
# there and the assertion is vacuous. The ingress controller's own pod is the one private
# 443 in this cluster that answers, so it is the one that can tell the exception apart
# from an empty socket.
CTRL_IP=$("${KUBECTL[@]}" -n ingress-nginx get pod -l app.kubernetes.io/component=controller \
            -o jsonpath='{.items[0].status.podIP}' 2>/dev/null || true)
if [ -n "$CTRL_IP" ]; then
  tcp_probe "the app may not reach a private 443 that answers (the RFC1918 exception)" \
    PROBE_BLOCKED app-egress-probe "$CTRL_IP" 443
else
  bad "could not find the ingress controller's pod IP to probe the private-range exception"
fi

# What this cluster cannot answer, said out loud rather than left as a passing check.
#
# The same rule excludes 169.254.169.254, the cloud metadata endpoint, which is the reason
# the exception list exists at all. Nothing answers on that address in kind, so a probe of
# it is "blocked" whether or not any policy is enforced -- an assertion that cannot fail,
# which is the thing this whole harness exists to avoid shipping.
#
# Measured separately, and worth knowing before trusting an egress rule on this CNI: a pod
# under default-deny with no egress rule at all still reaches the node's own addresses.
# The Kubernetes API server's ClusterIP is DNATed to the node, so it stays reachable from
# every pod here whatever this file says. That is the same exemption that lets the kubelet
# reach the probes; on Calico or Cilium it is a policy decision rather than a given.
note "not verifiable on kind: the 169.254.169.254 exception (nothing answers there) and"
note "anything addressed to the node itself (kindnet exempts host traffic from policy)"

"${KUBECTL[@]}" -n "$NS" delete pod app-egress-probe --now >/dev/null 2>&1 || true

# --- secrets at rest ------------------------------------------------------------------
#
# "A Kubernetes Secret is base64, not encryption" is a true sentence anybody can write. This
# reads the bytes out of etcd and makes it a measurement -- and, with the encryption
# provider configured above, makes the opposite one.
#
# Two assertions, and the second is what stops the first passing vacuously. A cluster with
# no encryption fails both: the plaintext is there, and the value does not carry the
# `k8s:enc:` prefix the API server writes in front of an encrypted one. A check that only
# looked for the absence of a string would also pass against an etcd this script could not
# read at all -- which is exactly how the first version of this probe returned "0
# occurrences" while its `sh -c` was failing, because the etcd image is distroless and has
# no shell.
say "secrets at rest"
ETCD_POD="etcd-${CLUSTER}-control-plane"
MARKER="harness-secret-probe-$$"
"${KUBECTL[@]}" -n "$NS" delete secret etcd-probe --now >/dev/null 2>&1 || true
"${KUBECTL[@]}" -n "$NS" create secret generic etcd-probe --from-literal=PROBE="$MARKER" >/dev/null

# No `sh -c`: the etcd image is distroless.
etcd_raw() {
  "${KUBECTL[@]}" -n kube-system exec "$ETCD_POD" -- etcdctl \
    --cacert /etc/kubernetes/pki/etcd/ca.crt \
    --cert /etc/kubernetes/pki/etcd/server.crt \
    --key /etc/kubernetes/pki/etcd/server.key \
    get "/registry/secrets/$NS/etcd-probe" 2>/dev/null
}
# LC_ALL=C: this is a protobuf blob with an encrypted payload in it, and `tr` on a UTF-8
# locale calls that an illegal byte sequence and says so on every run. The bytes are the
# point; the locale is not.
RAW="$(etcd_raw | LC_ALL=C tr -d '\0' || true)"

if [ -z "$RAW" ]; then
  bad "could not read the probe secret out of etcd; this section measured nothing"
else
  case "$RAW" in
    (*"k8s:enc:aescbc:"*) ok "the value in etcd is encrypted (k8s:enc:aescbc:)";;
    (*) bad "the value in etcd carries no encryption prefix -- is this cluster older than \
the encryption config? \`$0 --down\` and run again";;
  esac
  case "$RAW" in
    (*"$MARKER"*) bad "the secret's value is in etcd in the clear";;
    (*) ok "the secret's value is not in etcd in the clear";;
  esac
fi
"${KUBECTL[@]}" -n "$NS" delete secret etcd-probe --now >/dev/null 2>&1 || true

say "footprint"
"${KUBECTL[@]}" top pods -n "$NS" -l app.kubernetes.io/component=app --no-headers 2>/dev/null \
  | sed 's/^/  /' || echo "  (kubectl top returned nothing -- metrics-server may still be warming up after a restart)"

say "result"
printf '  %d passed, %d failed\n' "$PASS" "$FAIL"
printf '  cluster left running; %s --down to remove it\n' "$0"
[[ $FAIL -eq 0 ]]
