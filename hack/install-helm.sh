#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
# Installs a pinned Helm release from get.helm.sh (used by CI).
set -euo pipefail
version="${HELM_VERSION:-v3.19.0}"
command -v helm >/dev/null 2>&1 && { helm version --short; exit 0; }
os="$(uname -s | tr '[:upper:]' '[:lower:]')"; arch="$(uname -m)"; [[ "$arch" == x86_64 ]] && arch=amd64; [[ "$arch" == aarch64 ]] && arch=arm64
tmp="$(mktemp -d)"
curl -fsSL "https://get.helm.sh/helm-${version}-${os}-${arch}.tar.gz" | tar -xz -C "$tmp"
sudo install "$tmp/${os}-${arch}/helm" /usr/local/bin/helm
helm version --short
