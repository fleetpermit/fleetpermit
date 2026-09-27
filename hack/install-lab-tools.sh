#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Installs pinned kind, clusteradm and Helm on a Linux CI runner, verifying
# each download's SHA-256. The kind values are published by the kind project;
# clusteradm publishes no checksums, so its values were computed from the
# v1.3.1 release assets and pinned here to detect any later change.
set -euo pipefail
KIND_VERSION="${KIND_VERSION:-v0.32.0}"
CLUSTERADM_VERSION="${CLUSTERADM_VERSION:-v1.3.1}"
arch="$(uname -m)"; [[ "$arch" == x86_64 ]] && arch=amd64; [[ "$arch" == aarch64 ]] && arch=arm64

case "${KIND_VERSION}-${arch}" in
  v0.32.0-amd64) kind_sum=50030de23cf40a18505f20426f6a8506bedf13c6e509244bd1fa9463721b0f54 ;;
  v0.32.0-arm64) kind_sum=b92cd615e97585de8ddade28ed5cd7feb4248d717c233eea5b03c37298900f5d ;;
  *) echo "no pinned checksum for kind ${KIND_VERSION} ${arch}" >&2; exit 1 ;;
esac
case "${CLUSTERADM_VERSION}-${arch}" in
  v1.3.1-amd64) clusteradm_sum=20f5b0f57af27619d8337ee7255d95a76b6ce101f782b30284abebdab0255aef ;;
  v1.3.1-arm64) clusteradm_sum=973af6a36363026942aaca10ac2f04cb02778baaff9e4f1d70b700ba24c68d56 ;;
  *) echo "no pinned checksum for clusteradm ${CLUSTERADM_VERSION} ${arch}" >&2; exit 1 ;;
esac

# verify <file> <sha256>
verify() {
  local got
  got="$(sha256sum "$1" | awk '{print $1}')"
  [[ "$got" == "$2" ]] || { echo "checksum mismatch for $1: got ${got}, want $2" >&2; exit 1; }
}

# Four kind clusters on one Linux host need more inotify instances than the default.
sudo sysctl -q -w fs.inotify.max_user_watches=524288 fs.inotify.max_user_instances=8192
if [[ "${GITHUB_ACTIONS:-}" == true ]]; then
  # Hosted runners ship large preinstalled SDKs the lab does not need.
  sudo rm -rf /usr/share/dotnet /usr/local/lib/android /opt/ghc /opt/hostedtoolcache/CodeQL
  df -h / | tail -1
fi
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
curl -fsSLo "$tmp/kind" "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${arch}"
verify "$tmp/kind" "$kind_sum"
sudo install "$tmp/kind" /usr/local/bin/kind
curl -fsSLo "$tmp/clusteradm.tar.gz" "https://github.com/open-cluster-management-io/clusteradm/releases/download/${CLUSTERADM_VERSION}/clusteradm_linux_${arch}.tar.gz"
verify "$tmp/clusteradm.tar.gz" "$clusteradm_sum"
tar -xzf "$tmp/clusteradm.tar.gz" -C "$tmp" clusteradm
sudo install "$tmp/clusteradm" /usr/local/bin/clusteradm
"$(dirname "$0")/install-helm.sh"
kind version; clusteradm version 2>/dev/null | head -1 || true
