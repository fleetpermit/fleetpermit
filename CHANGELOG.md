# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/). APIs are `v1alpha1` and may change before v1.

## [v0.1.0] - 2026-09-26

First pre-release.

### Added

- `FleetAccessPolicy` (ceiling: subjects, OCM placement, MCP target, permitted tools, lease limits,
  fail-closed enforcement) and `ToolAccessLease` (immutable, time-bound subset) APIs, with
  schema and CEL validation and kubectl printer columns.
- Controller that resolves Open Cluster Management placements and renders one kube-agentic-networking
  `XAccessPolicy` per policy and cluster, with one CEL rule per lease that bounds `request.time`. It
  delivers them with `ManifestWork`, reads acceptance back through status feedback, and requests an
  immediate re-apply when delivered content drifts.
- Deterministic SHA-256 content digests and traceability annotations on every delivered object.
- Default-deny anchor manifest and OCM work-agent RBAC for managed clusters.
- Prometheus metrics and optional OpenTelemetry tracing.
- Helm chart with configurable image, resources, replicas, metrics, RBAC and leader election.
- Reproducible lab (1 OCM hub and 3 managed kind clusters, podman or docker), a narrated demo, 20
  end-to-end scenarios, a real-cluster benchmark, a controller scale simulation, and upstream
  conformance and canary tooling.
