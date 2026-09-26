#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Runs the upstream kube-agentic-networking conformance suite (pinned tag)
# against the enforcement path on one lab cluster and records the result in
# test-results/conformance/. The suite is used unmodified.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../demo/scripts/lib.sh"

need git go jq kubectl
target="${FP_CONFORMANCE_CLUSTER:-cluster-edge}"
src="${FP_WORK_DIR}/upstream/kube-agentic-networking-${KAN_VERSION}"
out="${FP_ROOT}/test-results/conformance"
mkdir -p "${out}" "$(dirname "${src}")"
if [[ ! -d "${src}" ]]; then
  git clone -q --depth 1 --branch "${KAN_VERSION}" https://github.com/kubernetes-sigs/kube-agentic-networking.git "${src}"
fi
# The v0.2.0 suite deploys quickstart-everything-mcp:main from the upstream
# staging registry, which no longer publishes that image. Build it from the
# upstream Dockerfile at the same tag and load it into the target cluster, so
# the suite itself still runs unmodified.
detect_engine
mcp_image="us-central1-docker.pkg.dev/k8s-staging-images/agentic-net/quickstart-everything-mcp:main"
if ! "${CONTAINER_ENGINE}" manifest inspect "${mcp_image}" >/dev/null 2>&1; then
  say "Building ${mcp_image##*/} from upstream ${KAN_VERSION} (not published upstream)"
  "${CONTAINER_ENGINE}" build -q -t "${mcp_image}" "${src}/site-src/guides/quickstart/mcpserver" >/dev/null
  load_image "${target}" "${mcp_image}"
fi

kcfg="${FP_WORK_DIR}/conformance-kubeconfig"
kubectl --kubeconfig "${FP_KUBECONFIG}" config view --minify --flatten --context "$(ctx "${target}")" >"${kcfg}"
chmod 600 "${kcfg}"

# Leftovers of an interrupted run must finish terminating first.
for ns in agentic-conformance-infra gateway-conformance-infra gateway-conformance-web-backend gateway-conformance-app-backend; do
  wait_for 300 "namespace ${ns} to terminate" bash -c "! kubectl --kubeconfig '${FP_KUBECONFIG}' --context '$(ctx "${target}")' get ns '${ns}'"
done

say "Running kube-agentic-networking ${KAN_VERSION} conformance against ${target}"
started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# FleetPermit relies on SPIFFE source matching, which the reference
# implementation supports; external authorization is not used by FleetPermit.
FP_CONFORMANCE_FEATURES="${FP_CONFORMANCE_FEATURES:-SupportAccessPolicySPIFFESource}"
cmd="go test -count=1 -v ./conformance -run TestConformance -timeout 30m -args --gateway-class=kube-agentic-networking --cleanup-base-resources=true --supported-features=${FP_CONFORMANCE_FEATURES} --report-output=<out>/kube-agentic-networking-${KAN_VERSION}-report.yaml"
set +e
(cd "${src}" && KUBECONFIG="${kcfg}" go test -count=1 -v ./conformance -run TestConformance -timeout 30m -args \
  --gateway-class=kube-agentic-networking --cleanup-base-resources=true \
  --supported-features="${FP_CONFORMANCE_FEATURES}" \
  --report-output="${out}/kube-agentic-networking-${KAN_VERSION}-report.yaml") >"${FP_WORK_DIR}/conformance.log" 2>&1
rc=$?
set -e
rm -f "${kcfg}"
passed="$(grep -c -E '^\s+--- PASS: TestConformance/' "${FP_WORK_DIR}/conformance.log" || true)"
failed="$(grep -c -E '^\s+--- FAIL: TestConformance/' "${FP_WORK_DIR}/conformance.log" || true)"
skipped="$(grep -c -E '^\s+--- SKIP: TestConformance/' "${FP_WORK_DIR}/conformance.log" || true)"
result="PASS"; [[ "${rc}" == 0 ]] || result="FAIL (exit ${rc})"
grep -E '^\s*--- (PASS|FAIL|SKIP)' "${FP_WORK_DIR}/conformance.log" | sed 's/ (.*//' >"${out}/kube-agentic-networking-${KAN_VERSION}-tests.txt" || true
note="The upstream MCP backend image referenced by the suite is not published; it was built from the upstream Dockerfile at ${KAN_VERSION}."
jq -n --arg note "${note}" --arg v "${KAN_VERSION}" --arg t "${target}" --arg r "${result} — ${passed} passed, ${failed} failed, ${skipped} skipped" \
  --arg d "${started}" --arg c "${cmd}" --argjson p "${passed}" --argjson f "${failed}" --argjson s "${skipped}" \
  --arg k8s "$(kc "${target}" version -o json | jq -r .serverVersion.gitVersion)" '{
  suites: [{suite: "kube-agentic-networking conformance (upstream, unmodified)", version: $v,
            target: ($t + " (Kubernetes " + $k8s + ", kind)"), result: $r, passed: $p, failed: $f, skipped: $s,
            date: $d, command: $c, note: $note}]
}' >"${out}/summary.json"
say "Conformance: ${result} (${passed} passed, ${failed} failed, ${skipped} skipped)"
[[ "${rc}" == 0 ]]
