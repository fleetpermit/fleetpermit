# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/). APIs are `v1alpha1` and may change before v1.

## [Unreleased]

### Changed

- Upgrade note: ManifestWork and XAccessPolicy names now end in 16 hex characters of a SHA-256 hash of
  the policy's namespace and name. v0.1.0 used 8. A `helm upgrade` from v0.1.0 therefore renames the
  delivered ManifestWork and XAccessPolicy objects. The controller removes the objects with the old
  names, including on clusters that have left the placement and when a policy is deleted.
- End-to-end scenario S10 now records its own metric, `driftRecoveryToAllowMs` (rendered policy
  deleted → calls allowed again), separate from the benchmark's `driftRecoveryMs` (rendered policy
  deleted → object restored).

### Fixed

- Security: a lease that omitted `duration` took the policy's current `defaultDuration` on every
  reconcile, so raising the default extended leases that had already been issued. The first recorded
  expiry is now pinned; later policy changes can only shorten or deny a lease. Found in code review.
- Security: FleetPermit could overwrite a ManifestWork owned by another policy if their names
  collided. Names now carry a 64-bit hash, and FleetPermit never modifies a ManifestWork that another
  policy owns.
- Withdrawing grants deleted the ManifestWork with the policy's current name only. It now deletes
  every ManifestWork labelled with the policy's UID in that cluster namespace, whatever its name, so
  deliveries under an earlier name are removed too. Each delete is conditional on the object's UID,
  so a ManifestWork that belongs to another policy is never deleted, even if it has the same name.
- A lease that fits on no cluster because the enforcement rule limit is reached everywhere is now in
  phase `Pending` with `Degraded` reason `CapacityExceeded`, instead of `Active`. It is not counted
  in `status.activeLeases` or the `fleetpermit_active_leases` gauge, and it activates when capacity
  frees up, for example when another lease expires.
- Lease durations below 10 s are rejected at admission.
- ManifestWork changes are now merge patches without an optimistic lock, so they no longer conflict
  with the OCM work agent's continuous status writes. Delivery failures are retried after 1 second.
  This removed the `Update` conflicts that delayed some activations.
- After a restart or rolling update, the new controller pod waited for the old pod's leader-election
  lease to expire before reconciling anything, which delayed the first lease after a restart by
  several seconds. The leader now releases the lease when it shuts down. This was the cause of the
  slow first activations seen in earlier lab runs, which had been attributed to OCM delivery.
- The end-to-end suite and benchmark polled east and then west, so the second cluster's latency was
  measured only after the first had finished. All clusters are now polled at the same time.
- A lease that had already expired became `Denied` (with both `Expired` and `Denied` true) when its
  policy was deleted. The first terminal state recorded now stays: an expired lease stays `Expired`.
- `fleetpermit_lease_revocation_seconds` was never recorded. It is now observed when the last cluster
  withdraws an expired or denied lease's grant.
- `fleetpermit_policy_propagation_seconds` was observed again, with the lease's full age, whenever a
  lease went back to Ready. It is now observed once per lease.
- The controller's role no longer has `update` or `patch` on `toolaccessleases`, `update` on
  `manifestworks` or on the status subresources, or `patch` on `fleetaccesspolicies`, which it did
  not need.
- The YAML check now also covers `.yaml` workflow files and the demo manifests.
- The inert policy is now validated against the upstream `XAccessPolicy` schema in the integration
  tests, like every other rendered shape.

### Added

- Helm chart: when `workExecutor` is set, the chart grants the controller OCM's `execute-as`
  permission for that one ServiceAccount, which OCM requires before it admits such a ManifestWork.
- `make images` removes the Dockerfile's leftover builder-stage images after building, so repeated
  lab builds with podman do not fill the container engine's disk.
- Decision-matrix end-to-end scenario: 2 test agents × 3 clusters × 4 tools, with the lease active and
  then expired (48 real calls, expected and observed).
- README recordings of real demo runs, the "five answers" overview, and a reproduction of the
  end-to-end scenarios on a GitHub-hosted runner (Linux amd64).
- Keyless Sigstore cosign signing of release images and assets in the release workflow, and a manual
  `sign-release` workflow, which signed the v0.1.0 images and assets after publication.
- OpenSSF Scorecard, CodeQL, fuzz tests, Dependabot updates, a security self-assessment and a
  project maturity page.

## [v0.1.0] - 2026-09-26

First pre-release.

### Added

- `FleetAccessPolicy` (ceiling: subjects, OCM placement, MCP target, permitted tools, lease limits,
  `failMode: Closed`) and `ToolAccessLease` (immutable, time-bound subset) APIs, with schema and CEL
  validation and kubectl printer columns.
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
