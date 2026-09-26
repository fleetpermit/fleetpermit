#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Early warning for upstream changes. Compares pinned versions with the
# latest upstream releases, then runs the integration suite against the
# LATEST upstream XAccessPolicy CRD. Version drift is reported; the job only
# fails when FleetPermit no longer works with the latest upstream API.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../demo/scripts/lib.sh"
need curl jq go

# latest <owner/repo> — newest non-prerelease tag that looks like a version
# (vX.Y.Z), skipping other release streams such as Helm chart tags.
latest() {
  local auth=()
  [[ -n "${GITHUB_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
  curl -fsSL ${auth[@]+"${auth[@]}"} "https://api.github.com/repos/$1/releases?per_page=30" \
    | jq -r '[.[] | select(.prerelease | not) | .tag_name | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))][0]'
}

say "Pinned versus latest upstream releases"
drift=0
while read -r repo pinned; do
  l="$(latest "$repo" || echo unknown)"
  mark="="; [[ "$l" == "$pinned" ]] || { mark="!"; drift=1; }
  printf '    %s %-45s pinned %-10s latest %s\n' "$mark" "$repo" "$pinned" "$l"
done <<EOF2
open-cluster-management-io/ocm ${OCM_BUNDLE_VERSION}
kubernetes-sigs/kube-agentic-networking ${KAN_VERSION}
kubernetes-sigs/gateway-api ${GATEWAY_API_VERSION}
envoyproxy/envoy ${ENVOY_IMAGE##*:}
metallb/metallb ${METALLB_VERSION}
EOF2

kan_latest="$(latest kubernetes-sigs/kube-agentic-networking)"
dir="${FP_WORK_DIR}/canary/crd"
mkdir -p "${dir}"
curl -fsSL "https://raw.githubusercontent.com/kubernetes-sigs/kube-agentic-networking/${kan_latest}/k8s/crds/agentic.networking.x-k8s.io_xaccesspolicies.yaml" \
  -o "${dir}/xaccesspolicies.yaml"
say "Integration suite against the kube-agentic-networking ${kan_latest} XAccessPolicy CRD"
FP_UPSTREAM_CRD_DIR="${dir}" make -C "${FP_ROOT}" --no-print-directory test-integration
[[ "${drift}" == 0 ]] || warn "newer upstream releases exist; update the pins after running the lab e2e suite (docs/upstream-compatibility.md)"
