#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Builds the FleetPermit lab: one Open Cluster Management hub and three
# managed clusters running the Kubernetes SIG Agentic Networking reference
# implementation, a deterministic MCP tool server and two agent identities.
#
# Usage: lab-up.sh [all|clusters|ocm|agentic|images|workloads|fleetpermit|status]

set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

step_clusters() {
  say "Creating kind clusters (${FP_HUB_NAME} + ${FP_MANAGED_CLUSTERS})"
  mkdir -p "${FP_WORK_DIR}"
  touch "$(lab_marker)"
  local existing; existing="$(kind_clusters)"
  for c in $(all_clusters); do
    local name; name="$(kind_name "$c")"
    if grep -qx "${name}" <<<"${existing}"; then
      info "${name} exists"
      kind export kubeconfig --name "${name}" --kubeconfig "${FP_KUBECONFIG}" >/dev/null 2>&1
      continue
    fi
    local cfg="${FP_ROOT}/demo/kind/managed-cluster.yaml"
    [[ "$c" == "${FP_HUB_NAME}" ]] && cfg="${FP_ROOT}/demo/kind/hub-cluster.yaml"
    info "creating ${name}"
    kind create cluster --name "${name}" --image "${KIND_NODE_IMAGE}" --config "${cfg}" \
      --kubeconfig "${FP_KUBECONFIG}" --wait 180s >/dev/null
    grep -qx "${name}" "$(lab_marker)" || echo "${name}" >>"$(lab_marker)"
  done
}

step_ocm() {
  say "Installing Open Cluster Management ${OCM_BUNDLE_VERSION}"
  if ! hub get crd managedclusters.cluster.open-cluster-management.io >/dev/null 2>&1; then
    clusteradm init --wait --bundle-version "${OCM_BUNDLE_VERSION}" \
      --kubeconfig "${FP_KUBECONFIG}" --context "$(ctx "${FP_HUB_NAME}")" >/dev/null
  fi
  wait_for 300 "OCM hub controllers" hub -n open-cluster-management-hub wait --for=condition=Available deploy --all --timeout=10s

  local token apiserver
  token="$(clusteradm get token --kubeconfig "${FP_KUBECONFIG}" --context "$(ctx "${FP_HUB_NAME}")" -o json 2>/dev/null | jq -r '."hub-token"')"
  apiserver="$(clusteradm get token --kubeconfig "${FP_KUBECONFIG}" --context "$(ctx "${FP_HUB_NAME}")" -o json 2>/dev/null | jq -r '."hub-apiserver"')"
  [[ -n "${token}" && "${token}" != null ]] || die "could not read the OCM bootstrap token"
  for c in ${FP_MANAGED_CLUSTERS}; do
    if hub get managedcluster "$c" >/dev/null 2>&1; then
      info "$c already registered"
      continue
    fi
    info "joining $c"
    clusteradm join --hub-token "${token}" --hub-apiserver "${apiserver}" --cluster-name "$c" \
      --bundle-version "${OCM_BUNDLE_VERSION}" --force-internal-endpoint-lookup --wait \
      --kubeconfig "${FP_KUBECONFIG}" --context "$(ctx "$c")" >/dev/null
  done
  local list; list="$(tr ' ' ',' <<<"${FP_MANAGED_CLUSTERS}")"
  wait_for 300 "cluster registrations" bash -c "for c in ${FP_MANAGED_CLUSTERS}; do kubectl --kubeconfig '${FP_KUBECONFIG}' --context '$(ctx "${FP_HUB_NAME}")' get csr -l open-cluster-management.io/cluster-name=\$c -o name | grep -q . || exit 1; done"
  clusteradm accept --clusters "${list}" --wait --kubeconfig "${FP_KUBECONFIG}" --context "$(ctx "${FP_HUB_NAME}")" >/dev/null 2>&1 || true
  for c in ${FP_MANAGED_CLUSTERS}; do
    wait_for 300 "$c to become available" hub wait --for=condition=ManagedClusterConditionAvailable "managedcluster/$c" --timeout=10s
  done

  # How often the OCM work agent reports status feedback to the hub. It affects
  # how quickly FleetPermit reports Ready and how quickly a deleted delivered
  # object is noticed and re-applied; enforcement of new content is immediate.
  for c in ${FP_MANAGED_CLUSTERS}; do
    kc "$c" patch klusterlet klusterlet --type merge \
      -p "{\"spec\":{\"workConfiguration\":{\"statusSyncInterval\":\"${OCM_STATUS_SYNC_INTERVAL}\"}}}" >/dev/null
  done

  say "Labelling clusters and binding the '${FP_CLUSTERSET}' ManagedClusterSet"
  for c in ${FP_MANAGED_CLUSTERS}; do
    local env=staging
    for p in ${FP_PRODUCTION_CLUSTERS}; do [[ "$p" == "$c" ]] && env=production; done
    hub label managedcluster "$c" "env=${env}" "cluster.open-cluster-management.io/clusterset=${FP_CLUSTERSET}" --overwrite >/dev/null
  done
  hub apply -f - >/dev/null <<EOF
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSet
metadata:
  name: ${FP_CLUSTERSET}
spec:
  clusterSelector:
    selectorType: ExclusiveClusterSetLabel
---
apiVersion: v1
kind: Namespace
metadata:
  name: ${FP_FLEET_NAMESPACE}
---
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSetBinding
metadata:
  name: ${FP_CLUSTERSET}
  namespace: ${FP_FLEET_NAMESPACE}
spec:
  clusterSet: ${FP_CLUSTERSET}
EOF
}

step_agentic() {
  say "Installing Gateway API ${GATEWAY_API_VERSION}, kube-agentic-networking ${KAN_VERSION} and MetalLB ${METALLB_VERSION}"
  local kan_raw="https://raw.githubusercontent.com/kubernetes-sigs/kube-agentic-networking/${KAN_VERSION}"
  local ca_file="${FP_WORK_DIR}/agentic-identity-ca-pool.yaml"
  local idx=0 subnet prefix
  if [[ "${CONTAINER_ENGINE}" == podman ]]; then
    subnet="$(podman network inspect kind | jq -r '.[].subnets[].subnet | select(contains(":") | not)' | head -1)"
  else
    subnet="$(docker network inspect kind | jq -r '.[].IPAM.Config[].Subnet | select(contains(":") | not)' | head -1)"
  fi
  [[ -n "${subnet}" ]] || die "could not determine the kind network subnet"
  prefix="$(awk -F. '{printf "%s.%s.%s", $1, $2, $3}' <<<"${subnet}")"

  for c in ${FP_MANAGED_CLUSTERS}; do
    info "$c"
    kc "$c" apply --server-side -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" >/dev/null
    kc "$c" apply --server-side -f "${kan_raw}/k8s/crds/agentic.networking.x-k8s.io_xbackends.yaml" >/dev/null
    kc "$c" apply --server-side -f "${kan_raw}/k8s/crds/agentic.networking.x-k8s.io_xaccesspolicies.yaml" >/dev/null

    # MetalLB gives each Gateway a routable address on the kind network.
    # Each cluster gets its own slice of the subnet so addresses never overlap.
    kc "$c" apply -f "https://raw.githubusercontent.com/metallb/metallb/${METALLB_VERSION}/config/manifests/metallb-native.yaml" >/dev/null
    local first=$((200 + idx * 10)) last=$((209 + idx * 10))
    idx=$((idx + 1))
    wait_for 300 "MetalLB on $c" kc "$c" -n metallb-system rollout status deploy/controller --timeout=10s
    wait_for 120 "MetalLB webhook on $c" kc "$c" apply -f - <<EOF
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata: {name: lab, namespace: metallb-system}
spec: {addresses: ["${prefix}.${first}-${prefix}.${last}"]}
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata: {name: lab, namespace: metallb-system}
spec: {ipAddressPools: [lab]}
EOF

    # The agentic identity CA is shared by every managed cluster, so a
    # workload identity issued on one cluster is trusted by all gateways.
    kc "$c" create namespace agentic-net-system --dry-run=client -o yaml | kc "$c" apply -f - >/dev/null
    if [[ ! -f "${ca_file}" ]]; then
      local tmp="${FP_WORK_DIR}/ca-kubeconfig"
      kubectl --kubeconfig "${FP_KUBECONFIG}" config view --minify --flatten --context "$(ctx "$c")" >"${tmp}"
      (cd "${FP_WORK_DIR}" && go run "sigs.k8s.io/kube-agentic-networking/cmd/agentic-net-tool@${KAN_VERSION}" \
        make-ca-pool-secret --kubeconfig "${tmp}" --ca-id=v1 --namespace=agentic-net-system --name=agentic-identity-ca-pool) >/dev/null
      kc "$c" -n agentic-net-system get secret agentic-identity-ca-pool -o json \
        | jq 'del(.metadata.uid,.metadata.resourceVersion,.metadata.creationTimestamp,.metadata.managedFields)' >"${ca_file}"
      chmod 600 "${ca_file}"
      rm -f "${tmp}"
    else
      kc "$c" apply -f "${ca_file}" >/dev/null
    fi

    curl -fsSL "${kan_raw}/k8s/deploy/deployment.yaml" \
      | sed -e "s|image: .*/agentic-networking-controller:.*|image: ${KAN_CONTROLLER_IMAGE}|" \
            -e "s|--proxy-image=.*|--proxy-image=${ENVOY_IMAGE}|" \
            -e "s|--agentic-identity-trust-domain=.*|--agentic-identity-trust-domain=${FP_TRUST_DOMAIN}|" \
            -e "s|--v=5.*|--v=2|" \
      | kc "$c" apply -f - >/dev/null
  done
  for c in ${FP_MANAGED_CLUSTERS}; do
    wait_for 300 "agentic-net-controller on $c" kc "$c" -n agentic-net-system rollout status deploy/agentic-net-controller --timeout=10s
  done
}

step_images() {
  say "Building FleetPermit images with ${CONTAINER_ENGINE}"
  make -C "${FP_ROOT}" --no-print-directory images \
    CONTAINER_ENGINE="${CONTAINER_ENGINE}" FP_CONTROLLER_IMAGE="${FP_CONTROLLER_IMAGE}" \
    FP_TOOLS_IMAGE="${FP_TOOLS_IMAGE}" FP_PROBE_IMAGE="${FP_PROBE_IMAGE}" >/dev/null
  rm -rf "${FP_WORK_DIR}/images"
  load_image "${FP_HUB_NAME}" "${FP_CONTROLLER_IMAGE}"
  for c in ${FP_MANAGED_CLUSTERS}; do
    load_image "$c" "${FP_TOOLS_IMAGE}"
    load_image "$c" "${FP_PROBE_IMAGE}"
  done
}

step_workloads() {
  say "Deploying the MCP tool server, Gateway and default-deny anchor on each managed cluster"
  for c in ${FP_MANAGED_CLUSTERS}; do
    FP_CLUSTER_NAME="$c" bash "${FP_ROOT}/demo/scripts/render-workloads.sh" | kc "$c" apply -f - >/dev/null
    # Lets the OCM work agent manage XAccessPolicy objects, and nothing else extra.
    kc "$c" apply -f "${FP_ROOT}/config/managed-cluster/work-agent-rbac.yaml" >/dev/null
  done
  for c in ${FP_MANAGED_CLUSTERS}; do
    wait_for 300 "tool server on $c" kc "$c" -n "${FP_TOOLS_NAMESPACE}" rollout status deploy/"${FP_BACKEND_NAME}" --timeout=10s
    wait_for 300 "Envoy gateway on $c" bash -c "kubectl --kubeconfig '${FP_KUBECONFIG}' --context '$(ctx "$c")' -n '${FP_TOOLS_NAMESPACE}' get deploy -l gateway.networking.k8s.io/gateway-name=${FP_GATEWAY_NAME} -o name | grep -q ."
    kc "$c" -n "${FP_TOOLS_NAMESPACE}" wait deploy -l "gateway.networking.k8s.io/gateway-name=${FP_GATEWAY_NAME}" --for=condition=Available --timeout=300s >/dev/null
    wait_for 300 "Gateway address on $c" bash -c "[[ -n \"\$(kubectl --kubeconfig '${FP_KUBECONFIG}' --context '$(ctx "$c")' -n '${FP_TOOLS_NAMESPACE}' get gateway '${FP_GATEWAY_NAME}' -o jsonpath='{.status.addresses[0].value}')\" ]]"
  done

  say "Deploying agent workloads on ${FP_AGENT_CLUSTER}"
  bash "${FP_ROOT}/demo/scripts/render-agents.sh" | kc "${FP_AGENT_CLUSTER}" apply -f - >/dev/null
  for a in sre-agent security-agent; do
    wait_for 300 "${a}" kc "${FP_AGENT_CLUSTER}" -n "${FP_AGENTS_NAMESPACE}" rollout status deploy/"${a}" --timeout=10s
  done
}

step_fleetpermit() {
  say "Installing FleetPermit on the hub with Helm"
  local repo="${FP_CONTROLLER_IMAGE%:*}" tag="${FP_CONTROLLER_IMAGE##*:}"
  # Helm installs CRDs only on first install and never upgrades them.
  hub apply --server-side --force-conflicts -f "${FP_ROOT}/charts/fleetpermit/crds/" >/dev/null
  helm upgrade --install fleetpermit "${FP_ROOT}/charts/fleetpermit" \
    --kubeconfig "${FP_KUBECONFIG}" --kube-context "$(ctx "${FP_HUB_NAME}")" \
    --namespace "${FP_SYSTEM_NAMESPACE}" --create-namespace \
    --set image.registry="" --set image.repository="${repo}" --set image.tag="${tag}" --set image.pullPolicy=Never \
    --wait --timeout 5m >/dev/null
  hub -n "${FP_SYSTEM_NAMESPACE}" rollout status deploy/fleetpermit-controller --timeout=120s >/dev/null
}

step_status() {
  say "Lab status"
  hub get managedclusters -L env
  for c in ${FP_MANAGED_CLUSTERS}; do
    local addr
    addr="$(kc "$c" -n "${FP_TOOLS_NAMESPACE}" get gateway "${FP_GATEWAY_NAME}" -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)"
    printf '    %-14s gateway=%s\n' "$c" "${addr:-<none>}"
  done
  hub -n "${FP_SYSTEM_NAMESPACE}" get deploy 2>/dev/null || true
  printf '\n    export KUBECONFIG=%s\n' "${FP_KUBECONFIG}"
}

main() {
  need kind kubectl clusteradm helm jq curl go
  detect_engine
  mkdir -p "${FP_WORK_DIR}"
  case "${1:-all}" in
    all) step_clusters; step_ocm; step_agentic; step_images; step_workloads; step_fleetpermit; step_status ;;
    clusters) step_clusters ;;
    ocm) step_ocm ;;
    agentic) step_agentic ;;
    images) step_images ;;
    workloads) step_workloads ;;
    fleetpermit) step_fleetpermit ;;
    status) step_status ;;
    *) die "unknown step '$1'" ;;
  esac
}

main "$@"
