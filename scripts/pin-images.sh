#!/usr/bin/env bash
#
# Rewrite the manifests' image references from a tag to a digest.
#
# `k8s/` carries a tag, and that is not the recommendation — it is what the verification
# harness needs. `kind load docker-image` moves an image into the cluster's node by tag and
# does not give it a registry digest, so a digest-pinned `k8s/` would make every harness run
# pull from GHCR: slower, dependent on the network, and no longer a test of the manifests in
# front of you.
#
# So the tag stays where it is checked, and this puts the digest in on the way out. A tag is
# a pointer somebody can move; a digest is the image. For a deployment anybody has to answer
# for, that difference is the whole point of pinning.
#
#   scripts/pin-images.sh out/ \
#     ghcr.io/lai3d/ai-customer-service-go=sha256:abc... \
#     ghcr.io/lai3d/ai-customer-service-go-admin-ui=sha256:def...
#
# The digests are printed by `.github/workflows/publish.yml` on the tagged build, and can be
# read back at any time with `docker buildx imagetools inspect <ref>`.
#
# It refuses rather than half-doing the job: an image in `k8s/` with no digest given is an
# error, not a line left on its tag. A manifest set where some images are pinned and some
# are not is the outcome that looks done.
set -euo pipefail

usage() {
  echo "usage: $0 <output-dir> <repository>=<sha256:...> [<repository>=<sha256:...> ...]" >&2
  exit 2
}

[ $# -ge 2 ] || usage
OUT="$1"
shift

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

declare -a REPOS=()
declare -a DIGESTS=()
for pair in "$@"; do
  case "$pair" in
    *=sha256:*) ;;
    *) echo "not <repository>=<sha256:...>: $pair" >&2; exit 2 ;;
  esac
  REPOS+=("${pair%%=*}")
  DIGESTS+=("${pair#*=}")
done

digest_for() {
  local repo="$1" i
  for i in "${!REPOS[@]}"; do
    if [ "${REPOS[$i]}" = "$repo" ]; then
      printf '%s' "${DIGESTS[$i]}"
      return 0
    fi
  done
  return 1
}

mkdir -p "$OUT"
# Only the manifests `kubectl apply -f k8s/` would apply. `k8s/kind/` is the throwaway
# cluster's own Postgres and `k8s/examples/` is a template that deliberately is not applied.
shopt -s nullglob
pinned=0
for src in "$ROOT"/k8s/*.yaml; do
  name="$(basename "$src")"
  dst="$OUT/$name"
  cp "$src" "$dst"

  while read -r ref; do
    [ -n "$ref" ] || continue
    repo="${ref%%:*}"
    if ! digest="$(digest_for "$repo")"; then
      echo "no digest given for $repo (in $name)" >&2
      echo "every image in k8s/ must be pinned, or the output is half a job" >&2
      exit 1
    fi
    # The tag is replaced rather than appended to. `repo:tag@sha256:...` is legal and is
    # worse than either half: the tag is then decoration that a reader will trust and that
    # nothing checks.
    awk -v old="$ref" -v new="$repo@$digest" \
      '{ gsub("image: " old "$", "image: " new); print }' "$dst" > "$dst.tmp"
    mv "$dst.tmp" "$dst"
    pinned=$((pinned + 1))
  done < <(awk '/^ *image: /{print $NF}' "$src" | sort -u)
done

if [ "$pinned" -eq 0 ]; then
  echo "no image references found in $ROOT/k8s/*.yaml; this script is no longer reading them" >&2
  exit 1
fi

# Read back rather than trusted. The rewrite is a text substitution, and a text substitution
# that silently matched nothing is the failure this whole script exists to avoid.
if remaining="$(awk '/^ *image: /{print $NF}' "$OUT"/*.yaml | grep -v '@sha256:' || true)"; [ -n "$remaining" ]; then
  echo "these are still on a tag after pinning: $remaining" >&2
  exit 1
fi

echo "pinned $pinned image reference(s) into $OUT"
