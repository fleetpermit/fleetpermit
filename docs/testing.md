# Testing

Every layer runs from a clean clone with `make`. CI calls the same targets.

| Layer | Command | Needs | What it proves |
|---|---|---|---|
| Static checks | `make verify` | Go, Helm, Python 3 | gofmt, `go vet`, generated code up to date, YAML parses, Helm lint and render, shell syntax, secret and personal-data scan, license headers |
| Unit | `make test-unit` | Go | lease evaluation (subset, duration, subject, placement narrowing, expiry boundary, determinism), digest determinism and known-answer vector, renderer output and CEL-injection rejection, capacity ordering, OCM work-state and drift logic, metric names and cardinality. Race detector on. |
| Integration | `make test-integration` | Go (envtest downloads kube-apiserver and etcd) | the real API server enforces the CRD schema and CEL rules (Scenario 14); every rendered shape is accepted by the **pinned upstream XAccessPolicy CRD**; the reconciler against real OCM CRDs with a simulated work agent: lease lifecycle, escalations, placement moves and deletion, ManifestWork tampering and deletion, drift re-apply request, concurrent leases, restart without disturbance, policy deletion, standing grants |
| End to end | `make demo-up && make test-e2e` | podman (or docker), kind, kubectl, clusteradm, helm, jq, Go | the scenarios in [results.md](results.md) on 1 hub + 3 managed clusters, using real SPIFFE mTLS and real MCP calls through Envoy |
| Benchmark | `make benchmark` | as above | simulated controller scale (10–100 logical clusters, envtest) and repeated real-cluster latencies |
| Results | `make results` | Go | merges everything into `test-results/results.json`, `docs/results.md`, the README block and the website data |

## End-to-end scenarios

| ID | Scenario | Expected |
|---|---|---|
| S1 | correct identity, cluster, tool; active lease | ALLOW on east and west |
| S2 | prohibited tool (`read_secret`), and a policy tool not in the lease | DENY |
| S3 | wrong SPIFFE identity | DENY |
| S4 | policy exists, no lease | DENY |
| S5 | lease expiry | ALLOW before, DENY after |
| S6 | cluster outside the placement | DENY, nothing rendered |
| S7 | placement change (relabel clusters) | authorization moves |
| S8 | lease asks for an unpermitted tool | lease Denied |
| S9 | lease asks for 1h, policy max 10m | lease Denied |
| S10 | rendered policy deleted on a managed cluster | restored; the anchor denies meanwhile |
| S11 | hub paused before expiry | DENY at expiry on every cluster, hub unreachable |
| S12 | hub resumed | lease Expired, stale grants withdrawn |
| S13 | two leases, different tools and durations | independent expiry |
| S14 | malformed policy | rejected at admission |
| S15 | FleetPermit and agentic-networking controllers restarted | no change in decisions; new leases still work |
| S16 | allowed tool, prohibited argument | recorded as **unsupported** by upstream v0.2.0 |
| MATRIX | `sre-agent` and `security-agent` × 3 clusters × 4 tools, lease active and then expired (48 real calls) | ALLOW only for `sre-agent` on east/west for the two leased tools while the lease is active; every other call DENY |
| R1 | lease deleted | DENY everywhere |
| A1 | backend without any XAccessPolicy | open (upstream behaviour), closed again with the anchor |
| RBAC | controller ServiceAccount | cannot touch pods, secrets, RBAC or namespaces |
| METRICS | metrics endpoint | every `fleetpermit_*` metric present |

Run a subset with `test/e2e/run.sh S1 S5 S11`. Every probe response, including the test agent that
made the call, is kept as evidence in `test-results/e2e-results.json`.

**Test agents.** Two workload identities make every call: `sre-agent`
(`spiffe://cluster.local/ns/agents/sa/sre-agent`), the policy's only subject, and `security-agent`
(`spiffe://cluster.local/ns/agents/sa/security-agent`), which no policy lists and which must always be
denied. Both get X.509 SVIDs from Kubernetes Pod Certificates. They are ordinary pods running the
deterministic probe (`demo/tools/probe`), not AI models, so every run is reproducible without API keys. The lab and the runner only ever use the kubeconfig in
`.work/lab/kubeconfig` and contexts named `kind-fleetpermit-*`. They never touch your current
kubectl context.

## Upstream conformance

`make conformance` runs the kube-agentic-networking v0.2.0 conformance suite, unmodified, from the
upstream repository at the pinned tag, against the enforcement path on a lab cluster. It records the
per-test results and a summary in `test-results/conformance/`. It declares
`SupportAccessPolicySPIFFESource`, because FleetPermit relies on SPIFFE source matching. External
authorization tests are skipped because FleetPermit does not use that feature.

The v0.2.0 suite deploys `quickstart-everything-mcp:main` from the upstream staging registry, which no
longer publishes that image (the repository has no tags). The script builds it from the upstream
Dockerfile at the same tag and loads it into the cluster, so the test code itself is untouched.

## Coverage

Coverage is measured, never hand-written. `make test-unit` and `make test-integration` both record
profiles for `internal/...`, and `make results` merges them into the combined figure shown in the
README.

## Clean-up guarantees

`make demo-down` deletes only the clusters recorded in `.work/lab/created-clusters` that also carry the
`fleetpermit-` prefix. The e2e runner restores cluster labels, the default-deny anchor and a paused hub
on exit, even when a scenario fails.
