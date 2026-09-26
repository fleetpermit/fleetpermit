#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Multi-cluster end-to-end scenarios. Every decision below is a real MCP call
# from a real SPIFFE identity through a real Envoy gateway on a managed
# cluster. Raw probe output is kept as evidence in test-results/e2e-results.json.
#
# Usage: test/e2e/run.sh [scenario-id ...]   (default: all)

set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

need jq kubectl python3
detect_engine
load_gateways

RUN_DIR="${FP_WORK_DIR}/e2e-run"
rm -rf "${RUN_DIR}" && mkdir -p "${RUN_DIR}" "${FP_RESULTS_DIR}"
JSONL="${RUN_DIR}/scenarios.jsonl"
: >"${JSONL}"
STARTED="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# The commit whose build is under test, captured before any scenario runs.
FP_COMMIT="$(git -C "${FP_ROOT}" describe --always --dirty --exclude '*' 2>/dev/null || echo unknown)"
HUB_PAUSED=0

cleanup() {
  if [[ "${HUB_PAUSED}" == 1 ]]; then
    "${CONTAINER_ENGINE}" unpause "$(hub_node)" >/dev/null 2>&1 || true
  fi
  for c in ${FP_MANAGED_CLUSTERS}; do
    local env=staging
    for p in ${FP_PRODUCTION_CLUSTERS}; do [[ "$p" == "$c" ]] && env=production; done
    set_env_label "$c" "$env" 2>/dev/null || true
    FP_CLUSTER_NAME="$c" bash "${FP_ROOT}/demo/scripts/render-workloads.sh" 2>/dev/null \
      | kc "$c" apply -f - >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

# record <id> <name> <pass|fail|unsupported> <expected> <observed> <evidence-json-array> [metrics-json]
record() {
  local id="$1" name="$2" status="$3" expected="$4" observed="$5" evidence="$6" metrics="${7:-}"
  [[ -n "${metrics}" ]] || metrics='{}'
  jq -cn --arg id "$id" --arg name "$name" --arg status "$status" --arg expected "$expected" \
    --arg observed "$observed" --argjson evidence "$evidence" --argjson metrics "$metrics" \
    '{id:$id, name:$name, status:$status, expected:$expected, observed:$observed, evidence:$evidence, metrics:$metrics}' >>"${JSONL}"
  local color="${C_G}"; [[ "$status" == pass ]] || color="${C_R}"; [[ "$status" == unsupported ]] && color="${C_Y}"
  printf '  %s%-11s%s %-7s %s\n' "${color}" "$(tr '[:lower:]' '[:upper:]' <<<"$status")" "${C_0}" "$id" "$name"
}

# expect_decision <agent> <cluster> <tool> <want> — one call; prints the probe JSON.
expect() {
  local out; out="$(probe "$1" "$2" "$3" "${5:-}")"
  printf '%s' "$(jq -c . <<<"$out")"
  [[ "$(decision "$out")" == "$4" ]]
}

arr() { local IFS=,; printf '[%s]' "$*"; }

setup_policy() {
  say "Resetting FleetPermit state"
  hub -n "${FP_FLEET_NAMESPACE}" delete toolaccesslease --all --wait=true >/dev/null 2>&1 || true
  cleanup
  apply_placement
  apply_policy 10m 2m
  local start; start=$(date +%s)
  until [[ "$(hub -n "${FP_FLEET_NAMESPACE}" get fap "${FP_POLICY}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == True ]]; do
    (( $(date +%s) - start > 120 )) && die "policy never became Ready"
    sleep 1
  done
  wait_no_grants 180
}

s14() {
  local err ok=pass
  err="$(hub -n "${FP_FLEET_NAMESPACE}" create -f - 2>&1 <<EOF || true
apiVersion: fleetpermit.github.io/v1alpha1
kind: FleetAccessPolicy
metadata: {name: malformed}
spec:
  subjects: [{spiffeID: "https://not-spiffe.example/agent"}]
  placement: {placementRef: {name: ""}}
  target: {namespace: mcp-tools, ref: {name: fleet-tools}}
  permissions: [{tool: "x') || true || ('"}]
  enforcement: {failMode: Open}
EOF
)"
  hub -n "${FP_FLEET_NAMESPACE}" get fap malformed >/dev/null 2>&1 && ok=fail
  for want in spiffeID tool failMode placementRef; do grep -q "$want" <<<"$err" || ok=fail; done
  record S14 "Malformed policy is rejected at admission" "$ok" "rejected with field errors" "$(tr '\n' ' ' <<<"$err" | cut -c1-400)" "$(jq -cn --arg e "$err" '[$e]')"
}

s4() {
  local e
  if e="$(expect sre-agent cluster-east restart_workload DENY)"; then
    record S4 "Valid identity and tool, policy exists, no lease" pass DENY DENY "[$e]"
  else record S4 "Valid identity and tool, policy exists, no lease" fail DENY "$(jq -r .decision <<<"$e")" "[$e]"; fi
}

s1() {
  local t0 east west ready ev st=pass
  t0=$(now_ms)
  create_lease e2e-s1 5m restart_workload,get_cluster_health
  east="$(wait_decision sre-agent cluster-east restart_workload ALLOW 60)" || st=fail
  west="$(wait_decision sre-agent cluster-west restart_workload ALLOW 60)" || st=fail
  local te=$(( $(cut -d' ' -f1 <<<"$east") )) tw=$(( $(cut -d' ' -f1 <<<"$west") ))
  # Slow activations are kept diagnosable: capture hub and work-agent logs.
  if (( te > 2000 || tw > 2000 )); then
    local d="${FP_RESULTS_DIR}/diagnostics/S1-$(date -u +%Y%m%dT%H%M%SZ)"
    mkdir -p "$d"
    hub -n "${FP_SYSTEM_NAMESPACE}" logs deploy/fleetpermit-controller --since=2m >"$d/fleetpermit-controller.log" 2>&1 || true
    for c in cluster-east cluster-west; do
      kc "$c" -n open-cluster-management-agent logs deploy/klusterlet-work-agent --timestamps --since=2m >"$d/work-agent-$c.log" 2>&1 || true
    done
    hub get manifestwork -A -l app.kubernetes.io/managed-by=fleetpermit -o yaml >"$d/manifestworks.yaml" 2>&1 || true
  fi
  wait_lease_phase e2e-s1 Active 60 || st=fail
  local start; start=$(date +%s)
  until [[ "$(lease_field e2e-s1 '{.status.conditions[?(@.type=="Ready")].status}')" == True ]]; do
    (( $(date +%s) - start > 120 )) && { st=fail; break; }
    sleep 0.5
  done
  ready=$(( $(now_ms) - t0 ))
  ev="$(arr "$(cut -d' ' -f2- <<<"$east")" "$(cut -d' ' -f2- <<<"$west")")"
  record S1 "Permitted tool with an active lease (east, west)" "$st" "ALLOW on selected clusters" "ALLOW east+west" "$ev" \
    "$(jq -cn --argjson e "$te" --argjson w "$tw" --argjson r "$ready" '{activationToAllowMsEast:$e, activationToAllowMsWest:$w, activationToReadyStatusMs:$r}')"
}

s6() {
  local e st=pass rendered
  e="$(expect sre-agent cluster-edge restart_workload DENY)" || st=fail
  rendered="$(rendered_policy cluster-edge)"
  [[ -z "$rendered" ]] || st=fail
  record S6 "Cluster outside the placement receives no grant" "$st" "DENY, nothing rendered on cluster-edge" "$(jq -r .decision <<<"$e"), rendered='${rendered}'" "[$e]"
}

s2() {
  local a b st=pass
  a="$(expect sre-agent cluster-east read_secret DENY)" || st=fail
  b="$(expect sre-agent cluster-east scale_workload DENY)" || st=fail
  record S2 "Prohibited tool (read_secret) and tool outside the lease" "$st" "DENY both" "$(jq -r .decision <<<"$a")/$(jq -r .decision <<<"$b")" "$(arr "$a" "$b")"
}

s3() {
  local e st=pass
  e="$(expect security-agent cluster-east restart_workload DENY)" || st=fail
  record S3 "Wrong SPIFFE identity, allowed tool" "$st" DENY "$(jq -r .decision <<<"$e")" "[$e]"
}

s8() {
  local st=pass reason e
  create_lease e2e-s8 2m restart_workload,read_secret
  wait_lease_phase e2e-s8 Denied 30 || st=fail
  reason="$(lease_field e2e-s8 '{.status.conditions[?(@.type=="Denied")].reason}')"
  [[ "$reason" == PermissionNotAllowed ]] || st=fail
  e="$(expect sre-agent cluster-east read_secret DENY)" || st=fail
  record S8 "Lease asks for a tool the policy does not permit" "$st" "lease Denied (PermissionNotAllowed), call DENY" "lease ${reason}, call $(jq -r .decision <<<"$e")" "[$e]"
}

s9() {
  local st=pass reason msg
  create_lease e2e-s9 1h restart_workload
  wait_lease_phase e2e-s9 Denied 30 || st=fail
  reason="$(lease_field e2e-s9 '{.status.conditions[?(@.type=="Denied")].reason}')"
  msg="$(lease_field e2e-s9 '{.status.conditions[?(@.type=="Denied")].message}')"
  [[ "$reason" == DurationExceedsMaximum ]] || st=fail
  record S9 "Lease asks for 1h against a 10m maximum" "$st" "lease Denied (DurationExceedsMaximum)" "$reason: $msg" "$(jq -cn --arg m "$msg" '[$m]')"
}

s16() {
  local a b
  create_lease e2e-s16 2m scale_workload
  wait_lease_phase e2e-s16 Active 30 || true
  wait_decision sre-agent cluster-east scale_workload ALLOW 30 >/dev/null || true
  a="$(probe sre-agent cluster-east scale_workload '{"namespace":"shop","name":"checkout","replicas":3}' | jq -c .)"
  b="$(probe sre-agent cluster-east scale_workload '{"namespace":"shop","name":"checkout","replicas":1000}' | jq -c .)"
  delete_lease e2e-s16
  record S16 "Allowed tool, prohibited argument (replicas=1000)" unsupported \
    "DENY replicas=1000 if upstream supports argument matching" \
    "replicas=3 $(jq -r .decision <<<"$a"), replicas=1000 $(jq -r .decision <<<"$b"): kube-agentic-networking ${KAN_VERSION} matches params.name only, so argument limits are not enforceable" "$(arr "$a" "$b")"
}

s_revoke() {
  local t0 east west st=pass
  t0=$(now_ms)
  delete_lease e2e-s1
  east="$(wait_decision sre-agent cluster-east restart_workload DENY 60)" || st=fail
  west="$(wait_decision sre-agent cluster-west restart_workload DENY 60)" || st=fail
  record R1 "Deleting a lease revokes it on every cluster" "$st" "DENY east+west" "DENY" \
    "$(arr "$(cut -d' ' -f2- <<<"$east")" "$(cut -d' ' -f2- <<<"$west")")" \
    "$(jq -cn --argjson e "$(cut -d' ' -f1 <<<"$east")" --argjson w "$(cut -d' ' -f1 <<<"$west")" '{revocationToDenyMsEast:$e, revocationToDenyMsWest:$w}')"
}

s5_s13() {
  local st5=pass st13=pass a b exp_a after_a keep lat
  create_lease e2e-s13-health 40s get_cluster_health
  create_lease e2e-s13-restart 100s restart_workload
  a="$(wait_decision sre-agent cluster-west get_cluster_health ALLOW 60)" || st13=fail
  b="$(wait_decision sre-agent cluster-west restart_workload ALLOW 60)" || st13=fail
  exp_a="$(lease_field e2e-s13-health '{.status.expiresAt}')"
  local exp_ms; exp_ms="$(iso_ms "$exp_a")"
  # Poll right up to and past the expiry instant.
  local before; before="$(probe sre-agent cluster-west get_cluster_health | jq -c .)"
  while (( $(now_ms) < exp_ms - 1500 )); do sleep 0.5; done
  after_a="$(wait_decision sre-agent cluster-west get_cluster_health DENY 30)" || st5=fail
  local deny_at; deny_at="$(jq -r .timestamp <<<"$(cut -d' ' -f2- <<<"$after_a")")"
  lat=$(( $(iso_ms "$deny_at") - exp_ms ))
  keep="$(expect sre-agent cluster-west restart_workload ALLOW)" || st13=fail
  [[ "$(jq -r .decision <<<"$before")" == ALLOW ]] || st5=fail
  record S5 "Lease expiry: ALLOW before, DENY after" "$st5" "ALLOW then DENY at expiry" \
    "before=$(jq -r .decision <<<"$before"), after=DENY ${lat}ms after expiresAt" "$(arr "$before" "$(cut -d' ' -f2- <<<"$after_a")")" \
    "$(jq -cn --argjson l "$lat" --arg e "$exp_a" '{expiresAt:$e, expiryToDenyMs:$l}')"
  record S13 "Concurrent leases for one subject are independent" "$st13" "health expires at 40s, restart still ALLOW" \
    "health DENY after its expiry, restart $(jq -r .decision <<<"$keep")" "$(arr "$(cut -d' ' -f2- <<<"$a")" "$(cut -d' ' -f2- <<<"$b")" "$keep")"
  delete_lease e2e-s13-health; delete_lease e2e-s13-restart
}

s7() {
  local st=pass w e t0 west_deny edge_allow
  create_lease e2e-s7 10m restart_workload
  wait_decision sre-agent cluster-west restart_workload ALLOW 60 >/dev/null || st=fail
  e="$(expect sre-agent cluster-edge restart_workload DENY)" || st=fail
  t0=$(now_ms)
  set_env_label cluster-west staging
  set_env_label cluster-edge production
  west_deny="$(wait_decision sre-agent cluster-west restart_workload DENY 90)" || st=fail
  edge_allow="$(wait_decision sre-agent cluster-edge restart_workload ALLOW 90)" || st=fail
  record S7 "Placement change moves authorization (west out, edge in)" "$st" "west DENY, edge ALLOW" "west DENY, edge ALLOW" \
    "$(arr "$e" "$(cut -d' ' -f2- <<<"$west_deny")" "$(cut -d' ' -f2- <<<"$edge_allow")")" \
    "$(jq -cn --argjson a "$(cut -d' ' -f1 <<<"$west_deny")" --argjson b "$(cut -d' ' -f1 <<<"$edge_allow")" '{labelChangeToDenyMs:$a, labelChangeToAllowMs:$b}')"
  set_env_label cluster-west production
  set_env_label cluster-edge staging
  wait_decision sre-agent cluster-west restart_workload ALLOW 90 >/dev/null || true
  wait_decision sre-agent cluster-edge restart_workload DENY 90 >/dev/null || true
}

s10() {
  local st=pass name during restored t0
  name="$(rendered_policy cluster-west)"
  [[ -n "$name" ]] || { record S10 "Drift: rendered XAccessPolicy deleted on a managed cluster" fail "recreated" "no rendered policy found" "[]"; return; }
  t0=$(now_ms)
  kc cluster-west -n "${FP_TOOLS_NAMESPACE}" delete "$name" --wait=true >/dev/null
  local immediate missing
  immediate="$(probe sre-agent cluster-west restart_workload | jq -c .)"
  sleep 2
  missing="$(probe sre-agent cluster-west restart_workload | jq -c .)"
  [[ "$(jq -r .decision <<<"$missing")" == DENY || -n "$(rendered_policy cluster-west)" ]] || st=fail
  restored="$(wait_decision sre-agent cluster-west restart_workload ALLOW 180)" || st=fail
  local recovered=$(( $(now_ms) - t0 ))
  record S10 "Drift: rendered XAccessPolicy deleted on a managed cluster" "$st" \
    "denied while the grant is missing (anchor), then restored from the hub" \
    "right after deletion: $(jq -r .decision <<<"$immediate") (gateway not yet updated); 2s later: $(jq -r .decision <<<"$missing"); ALLOW restored ${recovered}ms after deletion" \
    "$(arr "$immediate" "$missing" "$(cut -d' ' -f2- <<<"$restored")")" "$(jq -cn --argjson r "$recovered" '{driftRecoveryMs:$r}')"
}

s15() {
  local st=pass decisions="" out i new
  hub -n "${FP_SYSTEM_NAMESPACE}" delete pod -l app.kubernetes.io/name=fleetpermit --wait=false >/dev/null
  for i in $(seq 1 20); do
    out="$(probe sre-agent cluster-east restart_workload)"
    decisions+="$(decision "$out") "
    sleep 0.5
  done
  hub -n "${FP_SYSTEM_NAMESPACE}" rollout status deploy/fleetpermit-controller --timeout=120s >/dev/null || st=fail
  kc cluster-east -n agentic-net-system rollout restart deploy/agentic-net-controller >/dev/null
  for i in $(seq 1 20); do
    out="$(probe sre-agent cluster-east restart_workload)"
    decisions+="$(decision "$out") "
    sleep 0.5
  done
  kc cluster-east -n agentic-net-system rollout status deploy/agentic-net-controller --timeout=120s >/dev/null || st=fail
  grep -q -E 'DENY|ERROR' <<<"$decisions" && st=fail
  create_lease e2e-s15-after 2m get_cluster_health
  new="$(wait_decision sre-agent cluster-east get_cluster_health ALLOW 60)" || st=fail
  delete_lease e2e-s15-after
  record S15 "Controller restarts (FleetPermit and agentic-networking) keep an active grant stable" "$st" \
    "40/40 ALLOW during restarts; new leases work afterwards" "$(tr ' ' '\n' <<<"$decisions" | grep -v '^$' | sort | uniq -c | awk '{printf "%s %s ", $1, $2}')" \
    "[$(cut -d' ' -f2- <<<"$new")]"
  delete_lease e2e-s7
  wait_decision sre-agent cluster-west restart_workload DENY 60 >/dev/null || true
}

s11_s12() {
  local st11=pass st12=pass exp exp_ms e w still t_unpause converged east_after west_after
  create_lease e2e-s11 50s restart_workload
  wait_decision sre-agent cluster-east restart_workload ALLOW 60 >/dev/null || st11=fail
  wait_decision sre-agent cluster-west restart_workload ALLOW 60 >/dev/null || st11=fail
  exp="$(lease_field e2e-s11 '{.status.expiresAt}')"
  exp_ms="$(iso_ms "$exp")"

  say "Pausing the hub node (OCM hub API, FleetPermit controller and hub etcd all stop)"
  "${CONTAINER_ENGINE}" pause "$(hub_node)" >/dev/null
  HUB_PAUSED=1
  if kubectl --kubeconfig "${FP_KUBECONFIG}" --context "$(ctx "${FP_HUB_NAME}")" --request-timeout=3s get ns >/dev/null 2>&1; then st11=fail; fi
  local before; before="$(probe sre-agent cluster-east restart_workload | jq -c .)"
  [[ "$(jq -r .decision <<<"$before")" == ALLOW ]] || st11=fail
  while (( $(now_ms) < exp_ms - 1500 )); do sleep 0.5; done
  e="$(wait_decision sre-agent cluster-east restart_workload DENY 30)" || st11=fail
  w="$(wait_decision sre-agent cluster-west restart_workload DENY 30)" || st11=fail
  still="$(rendered_policy cluster-east)"
  local lat_e lat_w
  lat_e=$(( $(iso_ms "$(jq -r .timestamp <<<"$(cut -d' ' -f2- <<<"$e")")") - exp_ms ))
  lat_w=$(( $(iso_ms "$(jq -r .timestamp <<<"$(cut -d' ' -f2- <<<"$w")")") - exp_ms ))
  [[ -n "$still" ]] || st11=fail
  record S11 "Hub disconnected before expiry: managed clusters stop honouring the lease on time" "$st11" \
    "DENY at expiry on east and west with the hub unreachable" \
    "DENY ${lat_e}ms (east) / ${lat_w}ms (west) after expiresAt; rendered policy still present on east (${still}), so the denial came from the data-plane time bound" \
    "$(arr "$before" "$(cut -d' ' -f2- <<<"$e")" "$(cut -d' ' -f2- <<<"$w")")" \
    "$(jq -cn --argjson a "$lat_e" --argjson b "$lat_w" '{expiryToDenyMsEast:$a, expiryToDenyMsWest:$b, hubReachable:false}')"

  say "Resuming the hub node"
  t_unpause=$(now_ms)
  "${CONTAINER_ENGINE}" unpause "$(hub_node)" >/dev/null
  HUB_PAUSED=0
  wait_for 180 "hub API" hub get ns
  wait_lease_phase e2e-s11 Expired 180 || st12=fail
  local start; start=$(date +%s)
  while [[ -n "$(rendered_policy cluster-east)$(rendered_policy cluster-west)" ]]; do
    (( $(date +%s) - start > 240 )) && { st12=fail; break; }
    sleep 1
  done
  converged=$(( $(now_ms) - t_unpause ))
  east_after="$(probe sre-agent cluster-east restart_workload | jq -c .)"
  west_after="$(probe sre-agent cluster-west restart_workload | jq -c .)"
  [[ "$(jq -r .decision <<<"$east_after")" == DENY && "$(jq -r .decision <<<"$west_after")" == DENY ]] || st12=fail
  record S12 "Reconnection: hub marks the lease Expired and withdraws stale grants" "$st12" \
    "lease Expired, rendered policies removed, calls stay DENY" "converged ${converged}ms after the hub resumed" \
    "$(arr "$east_after" "$west_after")" "$(jq -cn --argjson c "$converged" '{reconnectConvergenceMs:$c}')"
}

a1() {
  local st=pass open closed
  kc cluster-edge -n "${FP_TOOLS_NAMESPACE}" delete xaccesspolicy fleetpermit-default-deny --wait=true >/dev/null
  open="$(wait_decision sre-agent cluster-edge read_secret ALLOW 60)" || true
  FP_CLUSTER_NAME=cluster-edge bash "${FP_ROOT}/demo/scripts/render-workloads.sh" | kc cluster-edge apply -f - >/dev/null
  closed="$(wait_decision sre-agent cluster-edge read_secret DENY 60)" || st=fail
  record A1 "Why the default-deny anchor exists (upstream behaviour without any XAccessPolicy)" "$st" \
    "without anchor the backend is open; with anchor it is closed" \
    "without anchor: $(jq -r .decision <<<"$(cut -d' ' -f2- <<<"$open")"); with anchor: DENY" \
    "$(arr "$(cut -d' ' -f2- <<<"$open")" "$(cut -d' ' -f2- <<<"$closed")")"
}

# expected_for <agent> <cluster> <tool> <lease-state> — the policy model the
# lab implements: sre-agent is the only subject of sre-remediation, the
# placement selects env=production (east, west), and the lease grants
# get_cluster_health and restart_workload (the policy also permits
# scale_workload, which the lease does not request).
expected_for() {
  local agent="$1" cluster="$2" tool="$3" lease="$4"
  [[ "$agent" == sre-agent && "$lease" == active ]] || { echo DENY; return; }
  case "$cluster" in cluster-east|cluster-west) ;; *) echo DENY; return ;; esac
  case "$tool" in get_cluster_health|restart_workload) echo ALLOW ;; *) echo DENY ;; esac
}

run_matrix() {
  local lease="$1" out a c t exp obs
  for a in sre-agent security-agent; do
    for c in ${FP_MANAGED_CLUSTERS}; do
      for t in get_cluster_health restart_workload scale_workload read_secret; do
        exp="$(expected_for "$a" "$c" "$t" "$lease")"
        out="$(probe "$a" "$c" "$t")"
        obs="$(decision "$out")"
        jq -c --arg lease "$lease" --arg exp "$exp" --arg obs "$obs" \
          '{agent, cluster, tool, lease: $lease, expected: $exp, observed: $obs, match: ($exp == $obs),
            httpStatus, detail, latencyMs, timestamp}' <<<"$out" >>"${RUN_DIR}/matrix.jsonl"
      done
    done
  done
}

matrix() {
  local st=pass exp_ms total matched
  : >"${RUN_DIR}/matrix.jsonl"
  create_lease e2e-matrix 10m get_cluster_health,restart_workload
  wait_decision sre-agent cluster-east restart_workload ALLOW 60 >/dev/null || st=fail
  wait_decision sre-agent cluster-west get_cluster_health ALLOW 60 >/dev/null || st=fail
  run_matrix active
  delete_lease e2e-matrix
  wait_decision sre-agent cluster-east restart_workload DENY 60 >/dev/null || st=fail

  # The same 24 calls once a lease for the same tools has expired.
  create_lease e2e-matrix-expired 20s get_cluster_health,restart_workload
  wait_decision sre-agent cluster-east restart_workload ALLOW 60 >/dev/null || st=fail
  exp_ms="$(iso_ms "$(lease_field e2e-matrix-expired '{.status.expiresAt}')")"
  while (( $(now_ms) < exp_ms + 1000 )); do sleep 0.5; done
  run_matrix expired
  delete_lease e2e-matrix-expired

  total="$(wc -l <"${RUN_DIR}/matrix.jsonl" | tr -d ' ')"
  matched="$(jq -s 'map(select(.match)) | length' "${RUN_DIR}/matrix.jsonl")"
  [[ "$total" == 48 && "$matched" == "$total" ]] || st=fail
  record MATRIX "Decision matrix: 2 agents x 3 clusters x 4 tools, lease active and expired" "$st" \
    "ALLOW only for sre-agent on east/west for the leased tools while the lease is active" \
    "${matched} of ${total} real calls matched the expected outcome" \
    "$(jq -s -c . "${RUN_DIR}/matrix.jsonl")" \
    "$(jq -cn --argjson t "$total" --argjson m "$matched" '{calls:$t, matched:$m}')"
}

rbac() {
  local sa="system:serviceaccount:${FP_SYSTEM_NAMESPACE}:fleetpermit-controller" st=pass line out=""
  for check in "create pods" "get secrets" "create clusterrolebindings" "delete namespaces" "create manifestworks.work.open-cluster-management.io -n cluster-east" "list placementdecisions.cluster.open-cluster-management.io -n fleet"; do
    line="$(hub auth can-i ${check} --as "$sa" 2>/dev/null || true)"
    out+="${check}=${line}; "
    case "$check" in
      create\ manifestworks*|list\ placement*) [[ "$line" == yes ]] || st=fail ;;
      *) [[ "$line" == no ]] || st=fail ;;
    esac
  done
  record RBAC "Controller ServiceAccount is least-privilege" "$st" "no pods/secrets/RBAC/namespaces; yes ManifestWork/PlacementDecision" "$out" "$(jq -cn --arg o "$out" '[$o]')"
}

metrics() {
  local st=pass body="" start
  # After S11/S12 the hub node may briefly report NotReady, which removes the
  # controller's endpoints; scrape like Prometheus would, with retries.
  start=$(date +%s)
  while (( $(date +%s) - start < 90 )); do
    body="$(hub get --raw "/api/v1/namespaces/${FP_SYSTEM_NAMESPACE}/services/fleetpermit-controller-metrics:8080/proxy/metrics" 2>/dev/null || true)"
    grep -q '^fleetpermit_' <<<"$body" && break
    sleep 3
  done
  grep '^fleetpermit_' <<<"$body" >"${FP_RESULTS_DIR}/metrics-snapshot.txt" || true
  for m in fleetpermit_reconcile_total fleetpermit_active_leases fleetpermit_expired_leases_total fleetpermit_denied_leases_total \
           fleetpermit_authorized_clusters fleetpermit_policy_propagation_seconds fleetpermit_lease_revocation_seconds fleetpermit_placement_changes_total; do
    grep -q "^${m}" <<<"$body" || st=fail
  done
  record METRICS "Prometheus metrics are exposed" "$st" "all fleetpermit_* metrics present" "$(grep -c '^fleetpermit_' <<<"$body") series" "[]"
}

main() {
  say "FleetPermit end-to-end scenarios"
  setup_policy
  local want="${*:-S14 S4 S1 S6 S2 S3 S8 S9 S16 R1 S5 S7 S10 S15 S11 MATRIX A1 RBAC METRICS}"
  for id in $want; do
    case "$id" in
      S14) s14 ;; S4) s4 ;; S1) s1 ;; S6) s6 ;; S2) s2 ;; S3) s3 ;; S8) s8 ;; S9) s9 ;; S16) s16 ;;
      R1) s_revoke ;; S5|S13) s5_s13 ;; S7) s7 ;; S10) s10 ;; S15) s15 ;; S11|S12) s11_s12 ;;
      MATRIX) matrix ;; A1) a1 ;; RBAC) rbac ;; METRICS) metrics ;;
      *) warn "unknown scenario $id" ;;
    esac
  done
  hub -n "${FP_FLEET_NAMESPACE}" delete toolaccesslease --all --wait=false >/dev/null 2>&1 || true

  local finished; finished="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  local k8s_server; k8s_server="$(kc cluster-east version -o json | jq -r .serverVersion.gitVersion)"
  jq -s --arg started "$STARTED" --arg finished "$finished" --arg k8s "$k8s_server" \
    --arg ocm "${OCM_BUNDLE_VERSION}" --arg kan "${KAN_VERSION}" --arg gw "${GATEWAY_API_VERSION}" \
    --arg envoy "${ENVOY_IMAGE##*:}" --arg arch "$(uname -m)" --arg os "$(uname -s)" \
    --arg engine "${CONTAINER_ENGINE}" --arg kind "$(kind version | awk '{print $2}')" \
    --arg fp "${FP_COMMIT}" \
    --arg clusters "${FP_MANAGED_CLUSTERS}" --arg sre "$(spiffe_of sre-agent)" --arg sec "$(spiffe_of security-agent)" '{
      kind: "real-multicluster-e2e",
      agents: [
        {name: "sre-agent", spiffeID: $sre, role: "listed as a subject of policy sre-remediation; receives leases"},
        {name: "security-agent", spiffeID: $sec, role: "not listed in any policy; every call must be denied"}
      ],
      startedAt: $started, finishedAt: $finished,
      environment: {
        description: "local kind clusters on a single development host; not a production benchmark",
        hub: 1, managedClusters: ($clusters | split(" ") | length), clusterNames: ($clusters | split(" ")),
        kubernetes: $k8s, openClusterManagement: $ocm, kubeAgenticNetworking: $kan,
        gatewayAPI: $gw, envoy: $envoy, kind: $kind, containerEngine: $engine, os: $os, arch: $arch,
        fleetpermitCommit: $fp
      },
      summary: {
        total: length,
        passed: (map(select(.status == "pass")) | length),
        failed: (map(select(.status == "fail")) | length),
        unsupported: (map(select(.status == "unsupported")) | length)
      },
      scenarios: .
    }' "${JSONL}" >"${FP_RESULTS_DIR}/e2e-results.json"
  say "Summary"
  jq -r '.summary | "    \(.passed) passed, \(.failed) failed, \(.unsupported) unsupported upstream (of \(.total))"' "${FP_RESULTS_DIR}/e2e-results.json"
  info "results: test-results/e2e-results.json"
  [[ "$(jq -r .summary.failed "${FP_RESULTS_DIR}/e2e-results.json")" == 0 ]]
}

main "$@"
