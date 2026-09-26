#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Deletes the lab clusters. Only clusters that this lab recorded as created
# (in its marker file) AND that carry the lab prefix are ever deleted.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

need kind
detect_engine
marker="$(lab_marker)"
if [[ ! -f "${marker}" ]]; then
  say "No lab marker at ${marker}; nothing to delete"
  exit 0
fi
existing="$(kind_clusters)"
while read -r name; do
  [[ -n "${name}" ]] || continue
  if [[ "${name}" != "${FP_LAB_NAME}-"* ]]; then
    warn "refusing to delete '${name}': it does not carry the '${FP_LAB_NAME}-' prefix"
    continue
  fi
  if grep -qx "${name}" <<<"${existing}"; then
    say "Deleting kind cluster ${name}"
    kind delete cluster --name "${name}" --kubeconfig "${FP_KUBECONFIG}" >/dev/null 2>&1
  fi
done <"${marker}"
rm -f "${marker}" "${FP_KUBECONFIG}"
rm -rf "${FP_WORK_DIR}/images"
say "Lab removed"
