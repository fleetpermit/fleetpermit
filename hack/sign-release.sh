#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Keyless signing with Sigstore cosign (runs in GitHub Actions with an OIDC
# token). Usage: hack/sign-release.sh <images.txt> <blob>...
#   images.txt lists one image reference by digest (repo@sha256:...) per line.
set -euo pipefail
images="${1:?images.txt}"; shift
while read -r ref; do
  [[ -n "${ref}" ]] || continue
  echo "signing ${ref}"
  cosign sign --yes "${ref}"
done <"${images}"
for blob in "$@"; do
  echo "signing ${blob}"
  cosign sign-blob --yes --bundle "${blob}.sigstore.json" "${blob}" >/dev/null
done
