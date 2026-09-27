#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Installs a pinned Helm release from get.helm.sh and verifies its SHA-256
# against the value published by the Helm project. On GitHub Actions a
# preinstalled Helm of another version is replaced; on other machines an
# existing Helm is left alone and only reported.
set -euo pipefail
version="${HELM_VERSION:-v3.19.0}"
os="$(uname -s | tr '[:upper:]' '[:lower:]')"; arch="$(uname -m)"
[[ "$arch" == x86_64 ]] && arch=amd64; [[ "$arch" == aarch64 ]] && arch=arm64

if command -v helm >/dev/null 2>&1; then
  have="$(helm version --template '{{.Version}}' 2>/dev/null || true)"
  if [[ "$have" == "$version" ]]; then helm version --short; exit 0; fi
  if [[ "${GITHUB_ACTIONS:-}" != true ]]; then
    echo "helm ${have} is installed; the pinned version is ${version} (left unchanged outside CI)"
    exit 0
  fi
fi

# Published by the Helm project next to each archive (<archive>.sha256sum).
case "${version}-${os}-${arch}" in
  v3.19.0-linux-amd64)  sum=a7f81ce08007091b86d8bd696eb4d86b8d0f2e1b9f6c714be62f82f96a594496 ;;
  v3.19.0-linux-arm64)  sum=440cf7add0aee27ebc93fada965523c1dc2e0ab340d4348da2215737fc0d76ad ;;
  v3.19.0-darwin-amd64) sum=09a108c0abda42e45af172be65c49125354bf7cd178dbe10435e94540e49c7b9 ;;
  v3.19.0-darwin-arm64) sum=31513e1193da4eb4ae042eb5f98ef9aca7890cfa136f4707c8d4f70e2115bef6 ;;
  *) echo "no pinned checksum for helm ${version} ${os}/${arch}" >&2; exit 1 ;;
esac

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
archive="$tmp/helm.tar.gz"
curl -fsSLo "$archive" "https://get.helm.sh/helm-${version}-${os}-${arch}.tar.gz"
got="$( (command -v sha256sum >/dev/null && sha256sum "$archive" || shasum -a 256 "$archive") | awk '{print $1}')"
[[ "$got" == "$sum" ]] || { echo "helm archive checksum mismatch: got ${got}, want ${sum}" >&2; exit 1; }
tar -xzf "$archive" -C "$tmp"
sudo install "$tmp/${os}-${arch}/helm" /usr/local/bin/helm
helm version --short
