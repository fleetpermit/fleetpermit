#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Shared configuration and helpers for the FleetPermit lab.
# Every value can be overridden from the environment. The lab only ever talks
# to clusters through its own kubeconfig file, never the user's current context.

set -euo pipefail

FP_ROOT="${FP_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

# --- lab topology -----------------------------------------------------------
FP_LAB_NAME="${FP_LAB_NAME:-fleetpermit}"
FP_HUB_NAME="${FP_HUB_NAME:-hub}"
FP_MANAGED_CLUSTERS="${FP_MANAGED_CLUSTERS:-cluster-east cluster-west cluster-edge}"
# Clusters labelled env=production (selected by the demo Placement).
FP_PRODUCTION_CLUSTERS="${FP_PRODUCTION_CLUSTERS:-cluster-east cluster-west}"
# Cluster that runs the demo agent workloads.
FP_AGENT_CLUSTER="${FP_AGENT_CLUSTER:-cluster-east}"
FP_WORK_DIR="${FP_WORK_DIR:-${FP_ROOT}/.work/lab}"
FP_KUBECONFIG="${FP_KUBECONFIG:-${FP_WORK_DIR}/kubeconfig}"

# --- namespaces and names used by the demo ---------------------------------
FP_FLEET_NAMESPACE="${FP_FLEET_NAMESPACE:-fleet}"
FP_CLUSTERSET="${FP_CLUSTERSET:-fleet}"
FP_SYSTEM_NAMESPACE="${FP_SYSTEM_NAMESPACE:-fleetpermit-system}"
FP_TOOLS_NAMESPACE="${FP_TOOLS_NAMESPACE:-mcp-tools}"
FP_AGENTS_NAMESPACE="${FP_AGENTS_NAMESPACE:-agents}"
FP_BACKEND_NAME="${FP_BACKEND_NAME:-fleet-tools}"
FP_GATEWAY_NAME="${FP_GATEWAY_NAME:-agentic-gateway}"
FP_GATEWAY_PORT="${FP_GATEWAY_PORT:-10001}"
FP_TRUST_DOMAIN="${FP_TRUST_DOMAIN:-cluster.local}"

# --- pinned upstream versions (see docs/upstream-compatibility.md) ----------
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.35.0@sha256:452d707d4862f52530247495d180205e029056831160e22870e37e3f6c1ac31f}"
OCM_BUNDLE_VERSION="${OCM_BUNDLE_VERSION:-v1.3.1}"
OCM_STATUS_SYNC_INTERVAL="${OCM_STATUS_SYNC_INTERVAL:-10s}"
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.5.1}"
KAN_VERSION="${KAN_VERSION:-v0.2.0}"
KAN_CONTROLLER_IMAGE="${KAN_CONTROLLER_IMAGE:-us-central1-docker.pkg.dev/k8s-staging-images/agentic-net/agentic-networking-controller:${KAN_VERSION}}"
# Envoy is pinned by tag and digest (v1.36.10 carries the 1.36 security fixes).
ENVOY_VERSION="${ENVOY_VERSION:-v1.36.10}"
ENVOY_IMAGE="${ENVOY_IMAGE:-docker.io/envoyproxy/envoy:${ENVOY_VERSION}@sha256:3a76238cdf52c7a3e33951f44c02d694108e6481feffe6663bf9319f55ec1394}"
METALLB_VERSION="${METALLB_VERSION:-v0.15.3}"

# --- images built from this repository --------------------------------------
FP_IMAGE_REGISTRY="${FP_IMAGE_REGISTRY:-localhost/fleetpermit}"
FP_IMAGE_TAG="${FP_IMAGE_TAG:-dev}"
FP_CONTROLLER_IMAGE="${FP_CONTROLLER_IMAGE:-${FP_IMAGE_REGISTRY}/fleetpermit-controller:${FP_IMAGE_TAG}}"
FP_TOOLS_IMAGE="${FP_TOOLS_IMAGE:-${FP_IMAGE_REGISTRY}/demo-mcp-tools:${FP_IMAGE_TAG}}"
FP_PROBE_IMAGE="${FP_PROBE_IMAGE:-${FP_IMAGE_REGISTRY}/demo-probe:${FP_IMAGE_TAG}}"

# --- output helpers -----------------------------------------------------------
if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  C_B=$'\033[1m'; C_DIM=$'\033[2m'; C_G=$'\033[32m'; C_R=$'\033[31m'; C_Y=$'\033[33m'; C_C=$'\033[36m'; C_0=$'\033[0m'
else
  C_B=""; C_DIM=""; C_G=""; C_R=""; C_Y=""; C_C=""; C_0=""
fi
say()  { printf '%s==>%s %s\n' "${C_C}${C_B}" "${C_0}" "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '%sWARN%s %s\n' "${C_Y}" "${C_0}" "$*" >&2; }
die()  { printf '%sERROR%s %s\n' "${C_R}" "${C_0}" "$*" >&2; exit 1; }

need() { for b in "$@"; do command -v "$b" >/dev/null 2>&1 || die "'$b' is required but not installed"; done; }

# --- container engine (development infrastructure only) --------------------
# Podman is preferred; Docker is used only when podman is absent.
detect_engine() {
  if [[ -n "${CONTAINER_ENGINE:-}" ]]; then
    :
  elif command -v podman >/dev/null 2>&1; then
    CONTAINER_ENGINE=podman
  elif command -v docker >/dev/null 2>&1; then
    CONTAINER_ENGINE=docker
  else
    die "no container engine found (install podman or docker)"
  fi
  if [[ "${CONTAINER_ENGINE}" == podman ]]; then
    if ! podman info >/dev/null 2>&1; then
      say "Starting the podman machine"
      podman machine start >/dev/null 2>&1 || true
    fi
    podman info >/dev/null 2>&1 || die "podman is installed but not running"
    export KIND_EXPERIMENTAL_PROVIDER=podman
  fi
  export CONTAINER_ENGINE
}

# --- cluster naming -----------------------------------------------------------
kind_name() { printf '%s-%s' "${FP_LAB_NAME}" "$1"; }
ctx()       { printf 'kind-%s' "$(kind_name "$1")"; }

# kc <cluster> <kubectl args...> — always pinned to the lab kubeconfig + context.
kc() { local c="$1"; shift; kubectl --kubeconfig "${FP_KUBECONFIG}" --context "$(ctx "$c")" "$@"; }
hub() { kc "${FP_HUB_NAME}" "$@"; }

all_clusters() { printf '%s\n' "${FP_HUB_NAME}" ${FP_MANAGED_CLUSTERS}; }

lab_marker() { printf '%s/created-clusters' "${FP_WORK_DIR}"; }

# kind_clusters lists existing kind clusters with the lab prefix.
kind_clusters() { kind get clusters 2>/dev/null | grep -E "^${FP_LAB_NAME}-" || true; }

wait_for() {
  # wait_for <timeout-seconds> <description> <command...>
  local timeout="$1" desc="$2"; shift 2
  local start; start=$(date +%s)
  until "$@" >/dev/null 2>&1; do
    if (( $(date +%s) - start > timeout )); then
      die "timed out after ${timeout}s waiting for ${desc}"
    fi
    sleep 3
  done
}

# load_image <cluster> <image> — loads a locally built image into a kind cluster.
load_image() {
  local c="$1" image="$2" archive
  archive="${FP_WORK_DIR}/images/$(printf '%s' "${image}" | tr '/:@' '___').tar"
  mkdir -p "${FP_WORK_DIR}/images"
  if [[ ! -f "${archive}" ]]; then
    "${CONTAINER_ENGINE}" save -o "${archive}" "${image}" >/dev/null
  fi
  kind load image-archive "${archive}" --name "$(kind_name "$c")" >/dev/null 2>&1 || die "loading ${image} into $c failed"
}
