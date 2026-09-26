#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Narrated FleetPermit demo against the lab (make demo-up first).
# The "$ command" lines it prints are simplified for readability. They show
# the equivalent kubectl or container command, while the script runs helper
# functions with the lab's kubeconfig, contexts and inline manifests. Every
# ALLOWED/DENIED line and every status output is real output from the run;
# each ALLOWED/DENIED line is one MCP call through a real gateway.
#
# Usage: demo/run.sh [overview|security|disconnect]
#   DEMO_LEASE_SECONDS  lease length for the overview (default 40)
#   DEMO_PAUSE          seconds to pause between steps (default 1)

set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../test/e2e/lib.sh"

DEMO_LEASE_SECONDS="${DEMO_LEASE_SECONDS:-40}"
DEMO_PAUSE="${DEMO_PAUSE:-1}"
STEP=0
TOTAL=0
HUB_PAUSED=0

trap 'if [[ ${HUB_PAUSED} == 1 ]]; then "${CONTAINER_ENGINE}" unpause "$(hub_node)" >/dev/null 2>&1 || true; fi' EXIT

step() { STEP=$((STEP + 1)); printf '\n%s[%d/%d]%s %s%s%s\n' "${C_C}${C_B}" "$STEP" "$TOTAL" "${C_0}" "${C_B}" "$*" "${C_0}"; sleep "${DEMO_PAUSE}"; }
line() { printf '      %s\n' "$*"; }
cmd()  { printf '      %s$ %s%s\n' "${C_DIM}" "$*" "${C_0}"; }

# call <agent> <cluster> <tool> — one real MCP call, printed as one line.
call() {
  local out d detail
  out="$(probe "$1" "$2" "$3")"
  d="$(decision "$out")"
  detail="$(jq -r '.detail' <<<"$out" | sed -e 's/^JSON-RPC error 403: //' -e 's/^\[[a-z-]*\] [a-z_]*: //' | cut -c1-44)"
  case "$d" in
    ALLOW) printf '      %-15s %-13s %-19s %sALLOWED%s  %s%s%s\n' "$1" "$2" "$3" "${C_G}${C_B}" "${C_0}" "${C_DIM}" "$detail" "${C_0}" ;;
    DENY)  printf '      %-15s %-13s %-19s %sDENIED%s   %s%s%s\n' "$1" "$2" "$3" "${C_R}${C_B}" "${C_0}" "${C_DIM}" "$detail" "${C_0}" ;;
    *)     printf '      %-15s %-13s %-19s %sERROR%s    %s\n' "$1" "$2" "$3" "${C_Y}" "${C_0}" "$detail" ;;
  esac
}

fleet_status() {
  local c env sel
  for c in ${FP_MANAGED_CLUSTERS}; do
    env="$(hub get managedcluster "$c" -o jsonpath='{.metadata.labels.env}')"
    sel="${C_DIM}not selected${C_0}"
    if hub -n "${FP_FLEET_NAMESPACE}" get placementdecision -l "cluster.open-cluster-management.io/placement=${FP_PLACEMENT}" \
        -o jsonpath='{.items[*].status.decisions[*].clusterName}' | tr ' ' '\n' | grep -qx "$c"; then
      sel="${C_G}selected${C_0}"
    fi
    printf '      %-15s env=%-11s %s\n' "$c" "$env" "$sel"
  done
}

reset() {
  hub -n "${FP_FLEET_NAMESPACE}" delete toolaccesslease --all --wait=true >/dev/null 2>&1 || true
  apply_placement
  apply_policy 10m 2m
  wait_no_grants 180
}

countdown_to() {
  local target_ms="$1" label="$2" left
  while :; do
    left=$(( (target_ms - $(now_ms)) / 1000 ))
    (( left <= 0 )) && break
    printf '\r      %s %s%3ds%s ' "$label" "${C_Y}" "$left" "${C_0}"
    sleep 1
  done
  printf '\r      %s %s0s — expired%s\n' "$label" "${C_Y}" "${C_0}"
}

show_lease() {
  cmd "kubectl get toolaccesslease $1"
  hub -n "${FP_FLEET_NAMESPACE}" get toolaccesslease "$1" | sed 's/^/      /'
}

show_rule() {
  local name; name="$(rendered_policy "$1" | head -1)"
  [[ -n "$name" ]] || { line "(no FleetPermit XAccessPolicy on $1)"; return; }
  cmd "kubectl --context $1 -n ${FP_TOOLS_NAMESPACE} get ${name} -o jsonpath='{..cel.expression}'"
  kc "$1" -n "${FP_TOOLS_NAMESPACE}" get "$name" -o jsonpath='{range .spec.rules[*]}{.authorization.cel.expression}{"\n"}{end}' \
    | grep -v '^$' | sed "s/^/      ${C_C}/;s/$/${C_0}/"
}

wait_active() {
  wait_lease_phase "$1" Active 60 || die "lease $1 did not become Active"
  local c
  for c in $2; do wait_decision sre-agent "$c" "$3" ALLOW 60 >/dev/null || die "grant never reached $c"; done
}

overview() {
  TOTAL=8
  printf '%sFleetPermit%s — least privilege for agents, across every cluster\n' "${C_B}" "${C_0}"
  reset
  step "Fleet status (Open Cluster Management placement: env=production)"
  fleet_status
  step "Policy exists, but no lease: nothing is granted"
  cmd "kubectl get fleetaccesspolicy"
  hub -n "${FP_FLEET_NAMESPACE}" get fleetaccesspolicy | sed 's/^/      /'
  call sre-agent cluster-east restart_workload
  step "Create a ${DEMO_LEASE_SECONDS}s lease for restart_workload"
  cmd "kubectl create -f incident-42.yaml   # restart_workload, ${DEMO_LEASE_SECONDS}s"
  create_lease incident-42 "${DEMO_LEASE_SECONDS}s" restart_workload
  wait_active incident-42 "cluster-east cluster-west" restart_workload
  show_lease incident-42
  step "Allowed call on every selected cluster"
  call sre-agent cluster-east restart_workload
  call sre-agent cluster-west restart_workload
  step "Wrong cluster: cluster-edge is not in the placement"
  call sre-agent cluster-edge restart_workload
  step "Forbidden tool and wrong identity"
  call sre-agent cluster-east read_secret
  call security-agent cluster-east restart_workload
  step "The expiry is part of the rule the gateway evaluates"
  show_rule cluster-east
  countdown_to "$(iso_ms "$(lease_field incident-42 '{.status.expiresAt}')")" "lease incident-42 expires in"
  step "Call after expiry"
  call sre-agent cluster-east restart_workload
  call sre-agent cluster-west restart_workload
  wait_lease_phase incident-42 Expired 60 || true
  show_lease incident-42
}

security() {
  TOTAL=6
  printf '%sFleetPermit%s — security boundaries\n' "${C_B}" "${C_0}"
  reset
  step "A lease cannot ask for more tools than the policy allows"
  cmd "kubectl create -f wants-read-secret.yaml   # restart_workload + read_secret"
  create_lease wants-read-secret 5m restart_workload,read_secret
  wait_lease_phase wants-read-secret Denied 30 || true
  line "status: $(lease_field wants-read-secret '{.status.conditions[?(@.type=="Denied")].reason}') — $(lease_field wants-read-secret '{.status.conditions[?(@.type=="Denied")].message}')"
  call sre-agent cluster-east read_secret
  step "A lease cannot outlive the policy's maximum"
  cmd "kubectl create -f one-hour.yaml   # maxDuration is 10m"
  create_lease one-hour 1h restart_workload
  wait_lease_phase one-hour Denied 30 || true
  line "status: $(lease_field one-hour '{.status.conditions[?(@.type=="Denied")].reason}') — $(lease_field one-hour '{.status.conditions[?(@.type=="Denied")].message}')"
  step "A lease cannot be widened after approval"
  create_lease incident-7 5m restart_workload
  cmd "kubectl patch toolaccesslease incident-7 --type=json -p '[{\"op\":\"add\",\"path\":\"/spec/permissions/-\",\"value\":{\"tool\":\"read_secret\"}}]'"
  hub -n "${FP_FLEET_NAMESPACE}" patch toolaccesslease incident-7 --type=json \
    -p '[{"op":"add","path":"/spec/permissions/-","value":{"tool":"read_secret"}}]' 2>&1 | sed 's/^/      /' || true
  step "Malformed policies are rejected at admission"
  cmd "kubectl apply -f bad-policy.yaml   # tool: \"x') || true || ('\", failMode: Open"
  hub -n "${FP_FLEET_NAMESPACE}" create -f - 2>&1 <<EOF | fold -w 110 | sed 's/^/      /' | head -6 || true
apiVersion: fleetpermit.github.io/v1alpha1
kind: FleetAccessPolicy
metadata: {name: bad-policy}
spec:
  subjects: [{spiffeID: "$(spiffe_of sre-agent)"}]
  placement: {placementRef: {name: ${FP_PLACEMENT}}}
  target: {namespace: ${FP_TOOLS_NAMESPACE}, ref: {name: ${FP_BACKEND_NAME}}}
  permissions: [{tool: "x') || true || ('"}]
  enforcement: {failMode: Open}
EOF
  step "A grant deleted on a managed cluster is restored from the hub"
  wait_active incident-7 "cluster-west" restart_workload
  call sre-agent cluster-west restart_workload
  local name; name="$(rendered_policy cluster-west)"
  cmd "kubectl --context cluster-west -n ${FP_TOOLS_NAMESPACE} delete ${name}"
  local t0; t0=$(now_ms)
  kc cluster-west -n "${FP_TOOLS_NAMESPACE}" delete "$name" --wait=true >/dev/null
  wait_for 180 "the grant to be restored" bash -c "kubectl --kubeconfig '${FP_KUBECONFIG}' --context '$(ctx cluster-west)' -n '${FP_TOOLS_NAMESPACE}' get '$name'"
  line "restored by the OCM work agent $(( $(now_ms) - t0 ))ms after deletion (content digest: $(kc cluster-west -n "${FP_TOOLS_NAMESPACE}" get "$name" -o jsonpath='{.metadata.annotations.fleetpermit\.github\.io/content-digest}' | cut -c1-23)...)"
  call sre-agent cluster-west restart_workload
  step "Summary"
  cmd "kubectl get toolaccesslease"
  hub -n "${FP_FLEET_NAMESPACE}" get toolaccesslease | sed 's/^/      /'
  hub -n "${FP_FLEET_NAMESPACE}" delete toolaccesslease --all --wait=false >/dev/null
}

disconnect() {
  TOTAL=6
  printf '%sFleetPermit%s — the lease still expires when the hub is gone\n' "${C_B}" "${C_0}"
  reset
  step "Create a 40s lease and confirm it works on both production clusters"
  create_lease maintenance-window 40s restart_workload
  wait_active maintenance-window "cluster-east cluster-west" restart_workload
  show_lease maintenance-window
  call sre-agent cluster-east restart_workload
  call sre-agent cluster-west restart_workload
  step "Disconnect the hub (pause the hub node: OCM hub and FleetPermit stop)"
  cmd "${CONTAINER_ENGINE} pause $(hub_node)"
  local expires; expires="$(iso_ms "$(lease_field maintenance-window '{.status.expiresAt}')")"
  "${CONTAINER_ENGINE}" pause "$(hub_node)" >/dev/null
  HUB_PAUSED=1
  cmd "kubectl --context hub get fleetaccesspolicy"
  kubectl --kubeconfig "${FP_KUBECONFIG}" --context "$(ctx "${FP_HUB_NAME}")" --request-timeout=3s get fap -A 2>&1 | head -1 | cut -c1-100 | sed 's/^/      /' || true
  step "Managed clusters keep enforcing the time bound on their own"
  show_rule cluster-east
  countdown_to "${expires}" "lease expires in"
  step "Hub still unreachable, lease expired"
  call sre-agent cluster-east restart_workload
  call sre-agent cluster-west restart_workload
  cmd "kubectl --context cluster-east -n ${FP_TOOLS_NAMESPACE} get xaccesspolicy"
  kc cluster-east -n "${FP_TOOLS_NAMESPACE}" get xaccesspolicy | sed 's/^/      /'
  line "the grant object is still present; the gateway denies because request.time passed the bound"
  step "Reconnect the hub"
  cmd "${CONTAINER_ENGINE} unpause $(hub_node)"
  "${CONTAINER_ENGINE}" unpause "$(hub_node)" >/dev/null
  HUB_PAUSED=0
  wait_for 180 "hub API" hub get ns
  wait_lease_phase maintenance-window Expired 180 || true
  local start; start=$(date +%s)
  while [[ -n "$(rendered_policy cluster-east)" ]] && (( $(date +%s) - start < 180 )); do sleep 1; done
  step "Converged: expired grant withdrawn from the fleet"
  show_lease maintenance-window
  local obj; obj="$(kc cluster-east -n "${FP_TOOLS_NAMESPACE}" get xaccesspolicy -l app.kubernetes.io/managed-by=fleetpermit -o name | head -1)"
  cmd "kubectl --context cluster-east -n ${FP_TOOLS_NAMESPACE} get ${obj} -o jsonpath='{.spec.rules[*].name}'"
  line "$(kc cluster-east -n "${FP_TOOLS_NAMESPACE}" get "${obj}" -o jsonpath='{.spec.rules[*].name}')   ${C_DIM}(the policy stays in place, with no grants)${C_0}"
  call sre-agent cluster-east restart_workload
}

need jq kubectl python3
detect_engine
load_gateways
case "${1:-overview}" in
  overview) overview ;;
  security) security ;;
  disconnect) disconnect ;;
  *) die "usage: $0 [overview|security|disconnect]" ;;
esac
printf '\n'
