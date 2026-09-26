#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Builds multi-arch (linux/amd64, linux/arm64) images with podman and pushes
# them. Go cross-compiles inside the build stage, so no emulation is needed.
# Usage: hack/publish-images.sh <registry> <tag>
set -euo pipefail
registry="${1:?registry}"; tag="${2:?tag}"
cd "$(dirname "$0")/.."
build() {
  local name="$1" cmd="$2" bin="$3" image="${registry}/$1:${tag}"
  podman manifest rm "${image}" >/dev/null 2>&1 || true
  podman build --platform linux/amd64,linux/arm64 --manifest "${image}" \
    --build-arg VERSION="${tag}" --build-arg CMD="${cmd}" --build-arg BIN="${bin}" .
  podman manifest push --all --digestfile "dist/${name}.digest" "${image}" "docker://${image}"
  echo "${registry}/${name}@$(cat "dist/${name}.digest")" >>dist/images.txt
}
mkdir -p dist
: >dist/images.txt
build fleetpermit-controller cmd/fleetpermit-controller fleetpermit-controller
build demo-mcp-tools demo/tools/mcp-server demo-mcp-tools
build demo-probe demo/tools/probe demo-probe
