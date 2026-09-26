# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/). APIs are `v1alpha1` and may change before v1.

## [Unreleased]

### Fixed

- **Security:** a lease that omitted `duration` took the policy's *current* `defaultDuration` on every
  reconcile, so raising the default extended already-issued leases. The first recorded expiry is now
  pinned; later policy changes can only shorten or deny a lease (found in an independent code audit).
- **Security:** FleetPermit could overwrite a ManifestWork owned by another policy if names collided.
  Names now carry a 64-bit hash, foreign-owned works are never modified, and stale works from older
  naming are pruned.
- Lease durations below 10 s are rejected at admission.
- ManifestWork changes are now merge patches without an optimistic lock, so they no longer conflict
  with the OCM work agent's continuous status writes. That conflict occasionally delayed activation by
  5 to 8 seconds. Delivery failures are retried after 1 second.

### Added

- Decision-matrix end-to-end scenario: 2 test agents × 3 clusters × 4 tools, with the lease active and
  then expired (48 real calls, expected and observed).
- README recordings of real demo runs, the "five answers" overview, and an independent reproduction
  on a GitHub-hosted Linux runner.

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
