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
failed=0
while read -r name; do
  [[ -n "${name}" ]] || continue
  if [[ "${name}" != "${FP_LAB_NAME}-"* ]]; then
    warn "refusing to delete '${name}': it does not carry the '${FP_LAB_NAME}-' prefix"
    continue
  fi
  if grep -qx "${name}" <<<"${existing}"; then
    say "Deleting kind cluster ${name}"
    if ! out="$(kind delete cluster --name "${name}" --kubeconfig "${FP_KUBECONFIG}" 2>&1)"; then
      warn "could not delete ${name}: ${out}"
      failed=1
    fi
  fi
done <"${marker}"
# Keep the marker and kubeconfig unless every recorded cluster is gone, so a
# failed teardown can be retried instead of losing track of the clusters.
left="$(kind_clusters | grep -Fx -f "${marker}" || true)"
if (( failed )) || [[ -n "${left}" ]]; then
  die "lab clusters still present: ${left:-see the warnings above}; the marker and kubeconfig are kept, re-run to retry"
fi
rm -f "${marker}" "${FP_KUBECONFIG}"
rm -rf "${FP_WORK_DIR}/images"
say "Lab removed"
