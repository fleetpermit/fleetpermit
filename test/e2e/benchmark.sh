#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Real multi-cluster latency benchmark against the lab. Repeats lease
# activation, revocation, expiry and drift recovery and records every sample
# in test-results/benchmark.json. Latencies are observed from the host with
# real MCP calls, so each sample includes one probe round trip; the measured
# round-trip baseline is recorded alongside.
#
#   BENCH_ITERATIONS  activation/revocation repetitions (default 8)
#   BENCH_EXPIRY      expiry repetitions (default 3)

set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

need jq kubectl python3
detect_engine
if ! hub get ns >/dev/null 2>&1; then
  warn "lab is not running; skipping the real-cluster benchmark (make demo-up first)"
  exit 0
fi
load_gateways
ITER="${BENCH_ITERATIONS:-8}"
EXP="${BENCH_EXPIRY:-3}"
mkdir -p "${FP_RESULTS_DIR}"
S="${FP_WORK_DIR}/bench-samples.jsonl"
: >"$S"
sample() { jq -cn --arg m "$1" --argjson v "$2" '{metric:$m, value:$v}' >>"$S"; }

say "Resetting state"
hub -n "${FP_FLEET_NAMESPACE}" delete toolaccesslease --all --wait=true >/dev/null 2>&1 || true
apply_placement
apply_policy 10m 2m
wait_no_grants 180

say "Probe round-trip baseline"
for i in $(seq 1 10); do
  t0=$(now_ms); probe sre-agent cluster-east get_cluster_health >/dev/null; sample probeRoundTripMs $(( $(now_ms) - t0 ))
done

say "Activation and revocation (${ITER} iterations)"
for i in $(seq 1 "$ITER"); do
  name="bench-${i}"
  t0=$(now_ms)
  create_lease "$name" 5m restart_workload
  out="$(wait_decisions "$t0" sre-agent restart_workload 60 cluster-east=ALLOW cluster-west=ALLOW)" || die "activation timed out: $out"
  while read -r _ ms _; do sample activationToAllowMs "$ms"; done <<<"$out"
  until [[ "$(lease_field "$name" '{.status.conditions[?(@.type=="Ready")].status}')" == True ]]; do
    (( $(now_ms) - t0 > 180000 )) && die "lease never reported Ready"
    sleep 0.2
  done
  sample activationToReadyStatusMs $(( $(now_ms) - t0 ))

  if (( i % 2 == 0 )); then
    obj="$(rendered_policy cluster-west)"
    d0=$(now_ms)
    kc cluster-west -n "${FP_TOOLS_NAMESPACE}" delete "$obj" --wait=true >/dev/null
    until kc cluster-west -n "${FP_TOOLS_NAMESPACE}" get "$obj" >/dev/null 2>&1; do
      (( $(now_ms) - d0 > 400000 )) && die "drift was not repaired within 400s"
      sleep 0.1
    done
    sample driftRecoveryMs $(( $(now_ms) - d0 ))
  fi

  t1=$(now_ms)
  hub -n "${FP_FLEET_NAMESPACE}" delete toolaccesslease "$name" --wait=false >/dev/null
  out="$(wait_decisions "$t1" sre-agent restart_workload 60 cluster-east=DENY cluster-west=DENY)" || die "revocation timed out: $out"
  while read -r _ ms _; do sample revocationToDenyMs "$ms"; done <<<"$out"
  printf '    iteration %d/%d done\n' "$i" "$ITER"
done

say "Expiry enforced by the gateway (${EXP} iterations, 15s leases)"
for i in $(seq 1 "$EXP"); do
  name="bench-exp-${i}"
  create_lease "$name" 15s get_cluster_health
  wait_decision sre-agent cluster-east get_cluster_health ALLOW 60 >/dev/null || die "expiry lease never activated"
  exp_ms="$(iso_ms "$(lease_field "$name" '{.status.expiresAt}')")"
  while (( $(now_ms) < exp_ms - 1000 )); do sleep 0.2; done
  out="$(wait_decision sre-agent cluster-east get_cluster_health DENY 30)" || die "expiry never denied"
  sample expiryToDenyMs $(( $(iso_ms "$(jq -r .timestamp <<<"$(cut -d' ' -f2- <<<"$out")")") - exp_ms ))
  delete_lease "$name"
done

jq -s --arg gen "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg k8s "$(kc cluster-east version -o json | jq -r .serverVersion.gitVersion)" \
  --arg ocm "${OCM_BUNDLE_VERSION}" --arg kan "${KAN_VERSION}" --arg envoy "${ENVOY_IMAGE##*:}" \
  --arg arch "$(uname -m)" --arg os "$(uname -s)" --arg sync "${OCM_STATUS_SYNC_INTERVAL}" '
  (group_by(.metric) | map({key: .[0].metric, value: (map(.value))}) | from_entries) as $s | {
    kind: "real-multicluster-benchmark",
    generatedAt: $gen,
    environment: {
      description: "local kind clusters (1 hub + 3 managed) on one development host; not a production benchmark",
      kubernetes: $k8s, openClusterManagement: $ocm, kubeAgenticNetworking: $kan, envoy: $envoy, os: $os, arch: $arch,
      ocmStatusSyncInterval: $sync,
      method: "host-side real MCP calls via kubectl exec; east and west are polled concurrently from the same start time; each latency includes one probe round trip (see probeRoundTripMs)"
    },
    samples: $s
  }' "$S" >"${FP_RESULTS_DIR}/benchmark.json"
say "Wrote test-results/benchmark.json"
jq -r '.samples | to_entries[] | "    \(.key): n=\(.value|length) median=\(.value|sort|.[length/2|floor])ms"' "${FP_RESULTS_DIR}/benchmark.json"
