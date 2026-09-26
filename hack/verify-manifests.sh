#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Parses every YAML manifest in the repository and checks that generated
# workload manifests render. Server-side schema validation of the samples and
# rendered objects happens in the integration tests (make test-integration).
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - <<'PY'
import glob, sys
try:
    import yaml  # optional
except ImportError:
    yaml = None
files = [f for pattern in ("config/**/*.yaml", "charts/**/*.yaml", ".github/**/*.yml", "test/fixtures/**/*.yaml")
         for f in glob.glob(pattern, recursive=True) if "/templates/" not in f]
bad = 0
for f in files:
    text = open(f).read()
    if "\t" in text:
        print(f"{f}: contains a tab character"); bad += 1
    if yaml:
        try:
            list(yaml.safe_load_all(text))
        except Exception as e:
            print(f"{f}: {e}"); bad += 1
print(f"checked {len(files)} YAML files" + ("" if yaml else " (PyYAML not installed: tab check only)"))
sys.exit(1 if bad else 0)
PY
for c in cluster-east cluster-edge; do
  FP_CLUSTER_NAME=$c bash demo/scripts/render-workloads.sh >/dev/null
done
bash demo/scripts/render-agents.sh >/dev/null
echo "workload renderers OK"
