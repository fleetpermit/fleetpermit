#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Helpers shared by the end-to-end scenarios, the benchmark and the demo.

# shellcheck source=../../demo/scripts/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/demo/scripts/lib.sh"

FP_POLICY="${FP_POLICY:-sre-remediation}"
FP_PLACEMENT="${FP_PLACEMENT:-production-clusters}"
FP_RESULTS_DIR="${FP_RESULTS_DIR:-${FP_ROOT}/test-results}"

spiffe_of() { printf 'spiffe://%s/ns/%s/sa/%s' "${FP_TRUST_DOMAIN}" "${FP_AGENTS_NAMESPACE}" "$1"; }

now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }

# iso_ms <RFC3339 timestamp, any fractional precision> -> epoch milliseconds
iso_ms() {
  python3 - "$1" <<'PY'
import re, sys, datetime
m = re.match(r'^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?Z$', sys.argv[1])
if not m:
    sys.exit("bad timestamp: " + sys.argv[1])
base = datetime.datetime.strptime(m.group(1), "%Y-%m-%dT%H:%M:%S").replace(tzinfo=datetime.timezone.utc)
frac = int((m.group(2) or "0")[:3].ljust(3, "0"))
print(int(base.timestamp()) * 1000 + frac)
PY
}

# Deterministic arguments for each demo tool.
default_args() {
  case "$1" in
    restart_workload) printf '%s' '{"namespace":"shop","name":"checkout"}' ;;
    scale_workload) printf '%s' '{"namespace":"shop","name":"checkout","replicas":3}' ;;
    read_secret) printf '%s' '{"namespace":"shop","name":"db-password"}' ;;
    *) printf '%s' '{}' ;;
  esac
}

gateway_ip() {
  kc "$1" -n "${FP_TOOLS_NAMESPACE}" get gateway "${FP_GATEWAY_NAME}" -o jsonpath='{.status.addresses[0].value}'
}

# Gateway addresses, one "cluster address" pair per line (bash 3.2 friendly).
GW_TABLE=""
load_gateways() {
  GW_TABLE=""
  local c ip
  for c in ${FP_MANAGED_CLUSTERS}; do
    ip="$(gateway_ip "$c")"
    [[ -n "${ip}" ]] || die "no gateway address on $c; is the lab up? (make demo-up)"
    GW_TABLE+="${c} ${ip}"$'\n'
  done
}
gw() { awk -v c="$1" '$1 == c {print $2}' <<<"${GW_TABLE}"; }

# probe <agent> <cluster> <tool> [json-args] — one real MCP call; prints JSON.
probe() {
  local agent="$1" cluster="$2" tool="$3" args="${4:-}"
  [[ -n "${args}" ]] || args="$(default_args "$tool")"
  kc "${FP_AGENT_CLUSTER}" -n "${FP_AGENTS_NAMESPACE}" exec "deploy/${agent}" -c agent -- \
    /demo-probe -url "https://$(gw "$cluster"):${FP_GATEWAY_PORT}/mcp" -tool "${tool}" -args "${args}" \
    -cluster "${cluster}" -trust-domain "${FP_TRUST_DOMAIN}" -timeout 5s 2>/dev/null \
    || printf '{"cluster":"%s","tool":"%s","decision":"ERROR","detail":"exec failed"}\n' "${cluster}" "${tool}"
}

decision() { jq -r '.decision' <<<"$1"; }

# wait_decision <agent> <cluster> <tool> <ALLOW|DENY> <timeout-s>
# Polls with real calls until the decision matches. Prints the elapsed
# milliseconds and the last probe output; returns non-zero on timeout.
wait_decision() {
  local agent="$1" cluster="$2" tool="$3" want="$4" timeout="${5:-60}"
  local start out
  start=$(now_ms)
  while :; do
    out="$(probe "$agent" "$cluster" "$tool")"
    if [[ "$(decision "$out")" == "$want" ]]; then
      printf '%s %s\n' "$(( $(now_ms) - start ))" "$(jq -c . <<<"$out")"
      return 0
    fi
    if (( $(now_ms) - start > timeout * 1000 )); then
      printf '%s %s\n' "-1" "$(jq -c . <<<"$out")"
      return 1
    fi
    sleep 0.25
  done
}

apply_placement() {
  hub apply -f - >/dev/null <<EOF
apiVersion: cluster.open-cluster-management.io/v1beta1
kind: Placement
metadata:
  name: ${FP_PLACEMENT}
  namespace: ${FP_FLEET_NAMESPACE}
spec:
  clusterSets: [${FP_CLUSTERSET}]
  predicates:
    - requiredClusterSelector:
        labelSelector:
          matchLabels:
            env: production
EOF
}

# apply_policy [max-duration] [default-duration]
apply_policy() {
  local max="${1:-10m}" def="${2:-2m}"
  hub apply -f - >/dev/null <<EOF
apiVersion: fleetpermit.github.io/v1alpha1
kind: FleetAccessPolicy
metadata:
  name: ${FP_POLICY}
  namespace: ${FP_FLEET_NAMESPACE}
spec:
  subjects:
    - spiffeID: $(spiffe_of sre-agent)
  placement:
    provider: ocm
    placementRef:
      name: ${FP_PLACEMENT}
  target:
    protocol: MCP
    namespace: ${FP_TOOLS_NAMESPACE}
    ref:
      kind: XBackend
      name: ${FP_BACKEND_NAME}
  permissions:
    - tool: get_cluster_health
    - tool: restart_workload
    - tool: scale_workload
  lease:
    required: true
    defaultDuration: ${def}
    maxDuration: ${max}
  enforcement:
    provider: kubernetes-agentic-networking
    failMode: Closed
EOF
}

# create_lease <name> <duration> <tools-csv> [subject-agent] [clusters-csv]
create_lease() {
  local name="$1" duration="$2" tools="$3" agent="${4:-sre-agent}" clusters="${5:-}"
  {
    cat <<EOF
apiVersion: fleetpermit.github.io/v1alpha1
kind: ToolAccessLease
metadata:
  name: ${name}
  namespace: ${FP_FLEET_NAMESPACE}
spec:
  policyRef:
    name: ${FP_POLICY}
  subject:
    spiffeID: $(spiffe_of "${agent}")
  duration: ${duration}
  reason: "e2e: ${name}"
  permissions:
EOF
    for t in ${tools//,/ }; do printf '    - tool: %s\n' "$t"; done
    if [[ -n "${clusters}" ]]; then
      printf '  clusters:\n'
      for c in ${clusters//,/ }; do printf '    - %s\n' "$c"; done
    fi
  } | hub create -f - >/dev/null
}

delete_lease() { hub -n "${FP_FLEET_NAMESPACE}" delete toolaccesslease "$1" --ignore-not-found --wait=true >/dev/null; }

lease_field() { hub -n "${FP_FLEET_NAMESPACE}" get toolaccesslease "$1" -o jsonpath="$2" 2>/dev/null; }

wait_lease_phase() {
  local name="$1" phase="$2" timeout="${3:-60}" start
  start=$(date +%s)
  until [[ "$(lease_field "$name" '{.status.phase}')" == "$phase" ]]; do
    (( $(date +%s) - start > timeout )) && return 1
    sleep 0.5
  done
}

set_env_label() { hub label managedcluster "$1" "env=$2" --overwrite >/dev/null; }

xap_name() { hub -n "${FP_FLEET_NAMESPACE}" get fleetaccesspolicy "${FP_POLICY}" -o json | jq -r '"fleetpermit-" + .metadata.name' ; }

# FleetPermit XAccessPolicy on a managed cluster that carries at least one
# grant. (A placed cluster without grants holds an inert policy.)
rendered_policy() {
  kc "$1" -n "${FP_TOOLS_NAMESPACE}" get xaccesspolicy -l app.kubernetes.io/managed-by=fleetpermit -o json 2>/dev/null \
    | jq -r '.items[] | select([.spec.rules[].name] != ["no-active-grants"]) | "xaccesspolicy.agentic.networking.x-k8s.io/" + .metadata.name'
}

# wait_no_grants [timeout-s] — until no managed cluster holds a FleetPermit grant.
wait_no_grants() {
  local timeout="${1:-120}" start c
  start=$(date +%s)
  for c in ${FP_MANAGED_CLUSTERS}; do
    while [[ -n "$(rendered_policy "$c")" ]]; do
      (( $(date +%s) - start > timeout )) && die "grants still present on $c after ${timeout}s"
      sleep 1
    done
  done
}

hub_node() { printf '%s-control-plane' "$(kind_name "${FP_HUB_NAME}")"; }
