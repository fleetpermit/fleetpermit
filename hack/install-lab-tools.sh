#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
# Installs pinned kind, clusteradm and Helm on a Linux CI runner.
set -euo pipefail
KIND_VERSION="${KIND_VERSION:-v0.32.0}"
CLUSTERADM_VERSION="${CLUSTERADM_VERSION:-v1.3.1}"
arch="$(uname -m)"; [[ "$arch" == x86_64 ]] && arch=amd64; [[ "$arch" == aarch64 ]] && arch=arm64
# Four kind clusters on one Linux host need more inotify instances than the default.
sudo sysctl -q -w fs.inotify.max_user_watches=524288 fs.inotify.max_user_instances=8192
if [[ "${GITHUB_ACTIONS:-}" == true ]]; then
  # Hosted runners ship large preinstalled SDKs the lab does not need.
  sudo rm -rf /usr/share/dotnet /usr/local/lib/android /opt/ghc /opt/hostedtoolcache/CodeQL
  df -h / | tail -1
fi
curl -fsSLo kind "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${arch}"
sudo install kind /usr/local/bin/kind && rm kind
curl -fsSL "https://github.com/open-cluster-management-io/clusteradm/releases/download/${CLUSTERADM_VERSION}/clusteradm_linux_${arch}.tar.gz" | tar -xz clusteradm
sudo install clusteradm /usr/local/bin/clusteradm && rm clusteradm
"$(dirname "$0")/install-helm.sh"
kind version; clusteradm version 2>/dev/null | head -1 || true
