# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/). APIs are `v1alpha1` and may change before v1.

## [v0.1.1] - 2026-09-26

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
- Helm chart: validates `workExecutor` (it must be `namespace/name`, checked whether or not the chart
  creates RBAC) and `watchNamespace` (a namespace name), refuses more than one replica without leader
  election, and prints installation notes (`NOTES.txt`) with the managed-cluster steps
  and the CRD upgrade step.
- Plain-text transcripts of the demo recordings (`demo/recordings/*.txt`) as text alternatives to the
  videos. `hack/cast-to-text.py` renders each from its cast without escape sequences, and
  `make demo-videos` regenerates them with the recordings.
- The annotation `fleetpermit.github.io/skip-withdrawal-wait: "true"` on a `FleetAccessPolicy` lets
  its deletion finish once the deletion of every ManifestWork has been requested, for managed clusters
  that will not reconnect. Removal on those clusters is then not confirmed, and the controller logs
  them.
- `status.policyUID` on `ToolAccessLease`: the UID of the policy that first evaluated the lease (see
  Fixed).

### Changed

- Upgrade note: ManifestWork and XAccessPolicy names now end in 16 hex characters of a SHA-256 hash of
  the policy's namespace and name. v0.1.0 used 8. A `helm upgrade` from v0.1.0 therefore renames the
  delivered ManifestWork and XAccessPolicy objects. The controller removes the objects with the old
  names, including on clusters that have left the placement and when a policy is deleted.
- End-to-end scenario S10 now records its own metric, `driftRecoveryToAllowMs` (rendered policy
  deleted → calls allowed again), separate from the benchmark's `driftRecoveryMs` (rendered policy
  deleted → object restored).
- Helm chart: cluster-scoped objects (ClusterRoles and bindings) carry the release name unless the
  release is called `fleetpermit`, so several releases can share a hub, each installed in its own
  namespace with a different `watchNamespace`.
- The narrated demo's default lease is 40 s instead of 45 s.
- `make demo-videos` copies the casts into `demo/recordings/` and, when `../fleetpermit.github.io` is
  a checkout of the website, copies the MP4 videos and poster images there too.
- The release workflow waits for the GitHub release to exist before it pushes images, instead of
  failing only at the final upload.
- The release workflow stops before building anything unless `Chart.yaml`'s `version` and
  `appVersion` match the tag (`0.1.1` and `v0.1.1` for tag `v0.1.1`).
- Security: the lab's Envoy is v1.36.10, pinned by tag and digest, instead of v1.36.6, which predates
  the security fixes released in v1.36.7, v1.36.9 and v1.36.10.
- `hack/install-helm.sh` and `hack/install-lab-tools.sh` check every download's SHA-256: Helm and kind
  against the values those projects publish, and clusteradm, which publishes none, against hashes
  pinned from its v1.3.1 release assets. On CI, a preinstalled Helm of another version is replaced by
  v3.19.0; elsewhere an existing Helm is left alone and its version reported. setup-envtest is pinned
  to a module version instead of the moving `release-0.25` branch.
- A lease created before its policy waits in `Pending` with reason `PolicyNotFound` for up to
  5 minutes after its creation, and its message names the deadline, so GitOps tools can apply objects
  in any order. It activates if the policy appears in that time; after that it is `Denied`, and it is
  `Expired` if its `spec.duration` ends first. A lease that was evaluated before its policy was
  deleted is still `Denied`, and one already past its recorded expiry is `Expired`. The controller
  confirms that a policy is absent with an uncached read.
- The rendered `XAccessPolicy` no longer carries the `fleetpermit.github.io/policy-generation`
  annotation, so a policy edit that does not change a cluster's grants causes no rollout.
- New CRD rules: a policy with `lease.required: false` may list at most 5 subjects, and `target.ref`
  must be `XBackend` in `agentic.networking.x-k8s.io` or `Gateway` in `gateway.networking.k8s.io`.
  Existing objects that break a rule are rejected only when their spec is next changed.
- `target.ref.group` has no default any more. When it is unset, it follows the kind:
  `agentic.networking.x-k8s.io` for `XBackend` and `gateway.networking.k8s.io` for `Gateway`, so a
  Gateway target no longer needs the group spelled out.
- `lease.defaultDuration` has no field default any more. When it is unset, a lease without a duration
  gets `15m`, or the policy's `maxDuration` if that is shorter, so a policy with a short maximum no
  longer needs to set `defaultDuration`. An omitted `lease` block defaults to
  `{required: true, maxDuration: 1h}`, so a later lower `maxDuration` is accepted too.
  `maxDuration` must be at least 10 s.
- The controller adds and removes its finalizer with a merge patch that leaves the spec alone, so its
  role has `patch` instead of `update` on `fleetaccesspolicies`.
- An active lease's `status.clusters` also lists clusters that left the placement and are still being
  withdrawn from; they do not affect `Ready`.
- A withdrawal from, or a re-delivery to, a cluster that OCM reports unavailable is reported as
  `ClusterUnavailable` and counts as failed (policy `Degraded` with `ClustersFailed`, not
  `Progressing`). It waits for the cluster to reconnect instead of being retried quickly.
- The integration suite can run several times against one API server (`go test -count=N`).
- A cluster is `Ready` only when OCM's status feedback reports the content digest the hub delivered;
  until then it is `AwaitingAcceptance`.
- Delivery failures are retried after 1 s, then 2, 4, 8 s and so on up to 2 minutes while they
  persist; any reconcile without a failure resets the delay.
- Only `PlacementDecision` objects whose controller owner is the policy's `Placement` are used.
- `ManagedCluster` updates reconcile policies only when the cluster's availability changes.
- Lease status writes are conditional on the resource version that was read; a conflict is retried
  and shows as a reconcile error.
- `fleetpermit_placement_changes_total` counts the changes this controller process observes.
- `status.clusters` on a policy holds at most 512 entries. Only when more clusters would be listed
  are the clusters that are not ready listed first, and the `Ready` message says how many are listed.
- The help text of `fleetpermit_active_leases` and `fleetpermit_authorized_clusters` now says what
  they count: grants rendered for a cluster, whose delivery may still be in progress (the lease's
  `Ready` condition, or the cluster's entry in the policy status, confirms it).

### Fixed

- Security: with `serviceAccount.create=false` and no `serviceAccount.name`, the Helm chart bound the
  controller's ClusterRole to the release namespace's `default` ServiceAccount, so every pod running
  as it got the controller's ManifestWork permissions. When it creates RBAC, the chart now refuses to
  render unless `serviceAccount.name` names a dedicated ServiceAccount (not empty, not `default`).
- Security: a policy deleted and created again under the same name could grant the leases issued
  under the earlier one, if it appeared before the controller had denied them, for example while the
  controller was down and the finalizer had been removed by hand. A lease now records the UID of the
  policy that first evaluates it (`status.policyUID`) and is granted only under that policy; under
  another policy with the same name it is `Denied` with reason `PolicyNotFound`, and the message names
  the earlier UID. Deleting a policy also ends its leases before the policy is gone. A lease that no
  policy has evaluated yet belongs to the first policy that evaluates it, and so does a lease from
  before this change.
- Security: a lease that omitted `duration` took the policy's current `defaultDuration` on every
  reconcile, so raising the default extended leases that had already been issued. The first recorded
  expiry is now pinned: later policy changes can never extend it, and lowering `maxDuration` below it
  denies the lease. Found in code review.
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
  with the OCM work agent's continuous status writes. This removed the `Update` conflicts that delayed
  some activations.
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
  lease went back to Ready. It is now observed once per lease per controller process.
- The controller's role no longer has `update` or `patch` on `toolaccessleases`, or `update` on
  `manifestworks`, on the status subresources, on `fleetaccesspolicies/finalizers` or on
  `fleetaccesspolicies`, which it did not need.
- The YAML check now also covers `.yaml` workflow files and the demo manifests.
- The inert policy is now validated against the upstream `XAccessPolicy` schema in the integration
  tests, like every other rendered shape.
- `make demo-down` passes the lab's kubeconfig to kind, so it never touches the default kubeconfig.
- Tracing could never start: the trace resource used a schema URL that conflicts with the SDK
  default. It now starts when `OTEL_EXPORTER_OTLP_ENDPOINT` is set, and spans are flushed on exit.
- A cluster that left the placement counted as withdrawn as soon as the deletion of its ManifestWork
  was requested. It is now reported as `Revoking` until the ManifestWork is gone, expired or denied
  leases keep listing it, and the revocation metric waits until then.
- A cluster placed again while its previous ManifestWork was still being deleted was reported
  `DeliveryFailed`. It is now `Delivering`, and delivery resumes once the deletion completes.
- A policy with `lease.required: false` and more than 5 subjects dropped the extra standing grants
  without saying so. The CRD now rejects such a policy, and standing grants that do not fit make the
  policy `Degraded` and not `Ready`, with `CapacityExceeded` listing the clusters.
- With more than 512 selected clusters, the policy status could not be written. The list is now
  capped.
- With `--watch-namespace`, ManifestWorks of policies in other namespaces caused endless reconcile
  errors. Policies and events outside the namespace are now ignored.
- `fleetpermit_lease_revocation_seconds` could record a sample of about 292 years for an expired lease
  without an evaluated expiry, and `fleetpermit_placement_changes_total` over-counted.
- The ManifestWorks of a policy deleted without its finalizer running were left behind. They are now
  found by the `fleetpermit.github.io/policy` annotation and deleted, each on condition that its UID is
  unchanged.
- Tampered ManifestWork fields other than the manifests (delete option, update strategy, executor)
  were not restored. They now are.
- Security: a policy deleted without its finalizer and re-created under the same name left the
  earlier policy's ManifestWorks in place, and the new policy could never deliver. The controller now
  deletes ManifestWorks that carry the policy's name but another UID, on placed and unplaced
  clusters, each on condition that its UID is unchanged; a placed cluster shows `Delivering` until
  they are gone.
- Restoring a tampered ManifestWork now also removes fields FleetPermit never sets: ignored fields,
  condition rules, a deletion TTL, selective orphaning and extra manifest configurations.
- A policy created with v0.1.0 that a newer CRD rule rejects could not be deleted, because adding or
  removing the finalizer rewrote the whole object. The finalizer change is now a merge patch; an
  upgrade test starts from the v0.1.0 CRDs.
- The lease immutability rule rejected an unchanged duration written in another form (`30m0s` for
  `30m`). Durations are now compared by value.
- The CRD description of a cluster's `grants` in the policy status said it counted lease grants only.
  It counts lease and standing grants, as it always did, and the description now says so.
- Lease metrics could be counted twice when a status write failed and the reconcile was retried. They
  are now recorded once, after the write succeeds.
- End-to-end scenario A1 passed even when it did not see the open state it exists to show. It now
  fails unless it observes the ALLOW without the anchor. S10's expected outcome claimed a denial while
  the rendered policy was missing, but the scenario also accepted the policy being back first. It now
  watches for the anchor's DENY and reports when it saw it, or that it was not observed because the
  policy came back first.
- A deleted policy lost its finalizer as soon as the deletion of its ManifestWorks was requested, so it
  could disappear while its grants were still on a cluster, for example an offline one. The finalizer
  is now held until every ManifestWork of the policy is gone, which the OCM work agent allows only
  after removing the delivered objects. Meanwhile the policy is `Ready=False` with reason `Deleting`,
  naming the clusters; they are listed as `Revoking`, or as `ClusterUnavailable` (policy `Degraded`)
  while offline.
- A `Placement` that was being deleted still selected its clusters, and `PlacementDecision` objects
  that were being deleted still counted. A Placement being deleted now selects no clusters
  (`PlacementNotFound`, grants withdrawn), and decisions being deleted are ignored.
- A ManifestWork that lost its `app.kubernetes.io/managed-by` label was invisible to the controller's
  cache, so it was neither restored nor removed with its policy. Deleting a policy, cleaning up after
  one that disappeared, and creating a work that already exists now read ManifestWorks from the API
  server: such a work is restored by the next reconcile and removed on deletion. On these paths only
  ManifestWorks named `fleetpermit-*` in the namespace of a ManagedCluster are deleted. A change to any
  label or annotation FleetPermit sets on a ManifestWork is now restored too.
- A ManagedCluster that did not exist or could not be read counted as available. A missing one is now
  unavailable ("does not exist") and an unreadable one is not ready ("cluster availability unknown:
  ..."); both are reported as `ClusterUnavailable`.
- The results generator did not count a package that failed without a failing test (a build failure,
  a panic outside a test, a run that never finished), and it showed test counts and coverage reused
  from an earlier run as if they were current. Such packages now count as failures and are listed
  (`failedPackages`), malformed `go test -json` input stops generation, and reused data is marked
  carried forward with the date it was produced (`carriedForward` in `results.json`, and a note in
  `docs/results.md` and the README).

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

[v0.1.1]: https://github.com/fleetpermit/fleetpermit/compare/v0.1.0...v0.1.1
[v0.1.0]: https://github.com/fleetpermit/fleetpermit/releases/tag/v0.1.0
