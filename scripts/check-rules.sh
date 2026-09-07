#!/usr/bin/env bash
#
# Syntax-check the alert rules with Prometheus's own promtool, and run the rule unit tests
# next to them.
#
# The Go test (internal/deployment/observability_test.go) checks the rules against the
# application: that every metric, label, label value and bucket boundary they name is one
# the code emits. It does not parse PromQL -- doing that properly means pulling in the
# whole github.com/prometheus/prometheus module -- so a rule can pass it and still be a
# syntax error. This covers that half, and the unit tests cover the third question the
# other two do not answer: whether the alert actually fires on data that should trip it.
#
# promtool comes out of the Prometheus image rather than being installed, because this is
# the only thing in the repository that needs it. Not in CI for the same reason `make
# bench` is not: it needs a container image CI would pull on every run. Run it when you
# change a rule.
#
#   scripts/check-rules.sh
#
set -euo pipefail

IMAGE="${PROMETHEUS_IMAGE:-prom/prometheus:v3.6.0}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# promtool reads a plain rules file: `groups:` at the top level. A PrometheusRule wraps
# exactly that under `spec:`, so the file it checks is the manifest's own spec with two
# spaces taken off every line -- not a second copy of the rules that could drift from it.
awk '/^spec:/{found=1; next} found' "$root/observability/prometheus-rule.yaml" \
  | sed 's/^  //' > "$work/rules.yaml"

if ! grep -q '^groups:' "$work/rules.yaml"; then
  echo "could not lift a rules file out of observability/prometheus-rule.yaml." >&2
  echo "The manifest's shape has changed; this script reads everything under spec:." >&2
  exit 1
fi
cp "$root/observability/rules_test.yaml" "$work/rules_test.yaml"

echo "==> promtool check rules  ($IMAGE)"
docker run --rm --entrypoint promtool -v "$work:/work" "$IMAGE" check rules /work/rules.yaml

echo "==> promtool test rules"
docker run --rm --entrypoint promtool -v "$work:/work" "$IMAGE" test rules /work/rules_test.yaml
