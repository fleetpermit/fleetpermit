# API reference

API group `fleetpermit.github.io`, version `v1alpha1`. Both kinds are namespaced. A lease references a
policy in its own namespace, and a policy references an OCM `Placement` in its own namespace. The hub
namespace scopes which policies, leases and Placements refer to each other. It does not restrict which
managed-cluster namespace or backend a policy targets; control that with RBAC on
`fleetaccesspolicies`.

Source of truth: [`api/v1alpha1`](../api/v1alpha1). CRDs: [`config/crd`](../config/crd).

Validation happens in two places:

- The API server enforces the OpenAPI schema and the CEL rules generated from `api/v1alpha1`: field
  types, patterns, lengths, list sizes and uniqueness, enums, the duration limits marked below, and
  lease immutability. A request that fails these checks is rejected before it is stored.
- The controller checks everything that depends on another object when it evaluates a lease: the
  policy exists, the subject is listed in the policy, the tools are a subset of the policy's
  permissions, the duration is at most the policy's `maxDuration`, and the requested clusters are in
  the placement. A lease is always stored first. It is then marked `Denied` if its subject, tools or
  duration break the policy, and `Pending` if no requested cluster is placed; the reasons are listed
  under [Condition reasons](#condition-reasons), which also covers a missing policy. The renderer
  re-checks tool names and SPIFFE IDs before it builds a CEL expression.

In the tables, "API server" and "controller" say where each rule is enforced.

## FleetAccessPolicy

Short name `fap`. The maximum authority that leases may activate.

| Field | Type | Default | Validation / meaning |
|---|---|---|---|
| `spec.subjects[].spiffeID` | string | required | API server: 1–16 unique entries; 10–512 characters; `^spiffe://[a-z0-9._-]+(/[A-Za-z0-9._-]+)*$` (same as upstream). A policy with `lease.required: false` may list at most 5 subjects, because each standing subject needs two of the upstream limit of 10 rules per policy |
| `spec.placement.provider` | enum | `ocm` | API server: only `ocm` |
| `spec.placement.placementRef.name` | string | required | an OCM `Placement` in the same namespace; the controller reports `PlacementNotFound` if it is missing |
| `spec.target.protocol` | enum | `MCP` | API server: only `MCP` |
| `spec.target.namespace` | string | required | API server: a DNS label. The namespace of the target on each managed cluster |
| `spec.target.ref.group` | enum | none: follows the kind | `agentic.networking.x-k8s.io` or `gateway.networking.k8s.io`. When unset, it is the group that serves the kind: `agentic.networking.x-k8s.io` for `XBackend`, `gateway.networking.k8s.io` for `Gateway` |
| `spec.target.ref.kind` | enum | `XBackend` | API server: or `Gateway`. If `group` is set, the pair must be `XBackend` in `agentic.networking.x-k8s.io` or `Gateway` in `gateway.networking.k8s.io` |
| `spec.target.ref.name` | string | required | the backend or gateway name. The default-deny anchor on each managed cluster must target the same resource; see [operations.md](operations.md#install) |
| `spec.permissions[].tool` | string | required | API server: 1–16 unique tools; `^[A-Za-z0-9][A-Za-z0-9_.-]*$`, at most 20 characters (upstream params limit). Re-checked by the renderer |
| `spec.lease.required` | bool | `true` | `false` grants every permission to every subject without a lease (standing access) |
| `spec.lease.defaultDuration` | duration | none: `15m`, or `maxDuration` if that is shorter | used when a lease omits `duration`. API server: at least `10s` and at most `maxDuration` |
| `spec.lease.maxDuration` | duration | `1h` | API server: at least `10s` and at most `24h` |
| `spec.enforcement.provider` | enum | `kubernetes-agentic-networking` | API server: only value |
| `spec.enforcement.failMode` | enum | `Closed` | API server: only value; an open mode is intentionally not offered |

If the whole `spec.lease` block is omitted, the API server fills in `required: true` and
`maxDuration: 1h`; the default duration is then `15m`. A later change of `maxDuration` below `15m`
lowers the default duration with it.

The standing-subject limit and the target group and kind rule are new in v0.1.1. A policy created
earlier that breaks one of them stays in place; the API server applies the rules when its spec is
next changed. FleetPermit adds and removes its finalizer with a merge patch that leaves the spec
alone, so such a policy can still be deleted.

Status:

| Field | Meaning |
|---|---|
| `conditions` | `Ready`, `Progressing`, `Degraded`, each with a reason |
| `selectedClusters`, `readyClusters`, `clusterSummary` | for example `2/2` |
| `activeLeases` | leases currently granting on at least one cluster |
| `clusters[]` | `name`, `ready`, `reason`, `message`, `grants` (lease and standing grants rendered for the cluster), `contentDigest` for each selected cluster, and for each cluster that left the placement until its delivery is gone. At most 512 entries: beyond that, clusters that are not ready are listed first, the counts above stay exact, and the `Ready` message says how many clusters are listed |
| `observedGeneration` | last processed generation |

```console
$ kubectl get fap -n fleet
NAME              CLUSTERS   READY   ACTIVE-LEASES   AGE
sre-remediation   2/2        True    1               10m
```

Reasons in `status.clusters[]`:

| Reason | Ready | Meaning |
|---|---|---|
| `Enforced` | yes | the work agent applied the current content, the enforcement controller accepted it, and OCM's status feedback reports the current content digest |
| `Delivering` | no | the ManifestWork does not exist yet; or the cluster was placed again while its previous ManifestWork is still being deleted; or an earlier policy with the same namespace and name, deleted without its finalizer, left a ManifestWork that is being deleted. Delivery resumes once that deletion completes |
| `Updating` | no | the cluster still holds a previous revision |
| `Applying` | no | the work agent has not applied the current ManifestWork generation |
| `AwaitingAcceptance` | no | the enforcement controller has not accepted the object yet, or OCM's status feedback has not reported the current content digest |
| `Drifted` | no | the delivered object was deleted or changed on the cluster; FleetPermit has asked OCM to re-apply it |
| `Revoking` | no | the cluster left the placement, or the placement was deleted, and its ManifestWork still exists or is being deleted. If the cluster is unavailable, it is reported as `ClusterUnavailable` instead |
| `DeliveryFailed` | no | rendering failed, the hub rejected the ManifestWork, or another policy owns a ManifestWork with the same name |
| `ApplyFailed` | no | the work agent could not apply the ManifestWork |
| `RejectedByEnforcement` | no | the enforcement controller rejected the object |
| `ClusterUnavailable` | no | OCM reports the ManagedCluster unavailable, so the status may be stale. A withdrawal from the cluster, or a delivery that waits for its previous ManifestWork to be removed, also waits for it to reconnect. The controller does not retry it quickly (only its regular check at least every two minutes); the ManagedCluster watch resumes it when the cluster becomes available. Counts as failed: the policy is `Degraded` with `ClustersFailed` |

## ToolAccessLease

Short name `tal`. Activates a subset of a policy for a bounded time. The spec is immutable: the API
server rejects any change to it after creation. An unchanged duration written in another form (for
example `30m0s` for `30m`) is not a change.

| Field | Type | Default | Validation / meaning |
|---|---|---|---|
| `spec.policyRef.name` | string | required | a `FleetAccessPolicy` in the same namespace. Controller: a lease created before its policy waits in `Pending` with reason `PolicyNotFound` for up to 5 minutes after its creation and activates if the policy appears in that time; see `PolicyNotFound` below |
| `spec.subject.spiffeID` | string | required | API server: SPIFFE ID pattern. Controller: must be one of the policy's subjects (`SubjectNotAllowed`) |
| `spec.permissions[].tool` | string | required | API server: 1–16 unique tools, same pattern as the policy. Controller: must be a subset of the policy's permissions (`PermissionNotAllowed`) |
| `spec.duration` | duration | the policy's `defaultDuration` (`15m`, or its `maxDuration` if shorter, when unset) | API server: at least `10s`. Controller: at most the policy's `maxDuration` (`DurationExceedsMaximum`). When omitted, see below |
| `spec.clusters[]` | []string | all placed clusters | API server: at most 64 unique names. Controller: narrows the placement; clusters outside it receive nothing |
| `spec.reason` | string | none | free text for audit. API server: at most 256 characters |

When a lease omits `spec.duration`, the controller uses the policy's `defaultDuration` at the lease's
first evaluation and records the result in `status.expiresAt`. From then on the expiry is pinned
through that status field, so a later change to the policy default cannot extend the lease. This means
the status subresource carries part of the lease's authority. Only the controller should be able to
write `toolaccessleases/status`; see [RBAC](operations.md#rbac). A principal that could write lease
status could extend such a lease up to the policy's `maxDuration`. A recorded expiry beyond that
maximum denies the lease.

Status:

| Field | Meaning |
|---|---|
| `phase` | `Pending`: no requested cluster is placed yet, the enforcement rule limit is reached on every cluster, or the policy does not exist yet (for at most 5 minutes after the lease's creation). `Active`: rendered on at least one cluster. `Expired`, `Denied`: terminal. A summary of the conditions |
| `expiresAt` | `creationTimestamp` + effective duration. For a lease without `spec.duration`, that is the policy default in effect at first evaluation, pinned here. Policy changes never extend it |
| `clusters`, `clusterCount` | clusters the grant is rendered for, plus clusters that left the placement and are still being withdrawn from (these do not affect `Ready`); delivery may still be in progress until the lease is `Ready`. After expiry or denial, the clusters the grant is still being withdrawn from |
| `conditions` | `Ready`, `Progressing`, `Degraded`, `Expired`, `Denied` |

```console
$ kubectl get tal -n fleet
NAME          POLICY            CLUSTERS   EXPIRES-AT             STATUS   AGE
incident-42   sre-remediation   2          2026-09-26T10:15:00Z   Active   1m
$ kubectl get tal -n fleet -o wide      # adds the SUBJECT column
```

### Condition reasons

Reasons on `ToolAccessLease` conditions. Rows marked "policy" are `FleetAccessPolicy` reasons.

| Reason | Condition | Meaning |
|---|---|---|
| `LeaseActive` | Ready=True | granted and enforced on every target cluster |
| `RollingOut` | Ready=False, Progressing=True | delivery or acceptance pending on some clusters |
| `NoEligibleClusters` | Ready=False (phase Pending) | none of the requested clusters is currently placed |
| `CapacityExceeded` | Degraded=True (policy: also Ready=False) | the upstream rule limit is reached on the listed clusters. For a lease that fits on no cluster, the lease is in phase `Pending` (Ready=False with the same reason), is not counted as active, and activates when capacity frees up. On a policy, it means standing grants did not fit on the listed clusters |
| `ClustersFailed` | Degraded=True (policy: also Ready=False) | at least one cluster is `DeliveryFailed` (rendering failed, the hub rejected the ManifestWork, or another policy owns a ManifestWork with the same name), `ApplyFailed` (the work agent could not apply it), `RejectedByEnforcement` or `ClusterUnavailable`; the message lists the clusters |
| `PolicyNotFound` | Ready=False (phase Pending), or Denied=True | the referenced policy does not exist. A lease that has never been evaluated against its policy (for example one a GitOps tool applied before the policy) waits in `Pending` for up to 5 minutes after its creation; the message names the deadline, and the lease activates if the policy appears in time. After that the lease is `Denied`, which is terminal; if its `spec.duration` ends first, it is `Expired`. A lease that was evaluated and whose policy was then deleted is `Denied`, or `Expired` if it was already past its recorded expiry |
| `SubjectNotAllowed` | Denied=True | the subject is not in the policy |
| `PermissionNotAllowed` | Denied=True | a requested tool is not in the policy (the message lists them) |
| `DurationExceedsMaximum` | Denied=True | the duration exceeds `maxDuration` |
| `LeaseExpired` | Expired=True | past `expiresAt` |
| `Revoking` | Progressing=True | grants of an expired or denied lease are still being withdrawn from the listed clusters, including clusters whose ManifestWork is still being deleted. The lease keeps listing them in `status.clusters` until the withdrawal is complete; while a cluster is offline, this can last indefinitely |
| `Allowed` | Denied=False | the lease satisfies the policy |
| `NotExpired` | Expired=False | the lease has not reached `expiresAt`, or it was denied before it could |
| `Reconciled` | Progressing=False, Degraded=False (policy: also Ready=True) | nothing is in progress or failing; on a policy, every selected cluster is in the desired state |
| `NoClustersSelected` | policy: Ready=True | the placement selects no clusters, so nothing is granted |
| `PlacementNotFound` | policy: Ready=False, Degraded=True | the referenced Placement does not exist; every grant is withdrawn and the policy's leases wait in `Pending` (`NoEligibleClusters`) |

`Denied` and `Expired` are terminal, provided only the controller can write
`toolaccessleases/status`. A policy change that removes a lease's tool or subject, or lowers
`maxDuration` below the lease's duration, denies it.
A policy change that would allow a previously denied lease does not revive it. `Pending` is not
terminal: a pending lease activates when its policy is created (within the 5-minute grace period), a
requested cluster is placed or capacity frees up, as long as it has not expired.

## Labels and annotations written by FleetPermit

On `ManifestWork` (hub) and the rendered `XAccessPolicy` (managed clusters):

| Key | On | Value |
|---|---|---|
| `app.kubernetes.io/managed-by` | both (label) | `fleetpermit` |
| `fleetpermit.github.io/policy-uid` | both (label); also an annotation on the XAccessPolicy | source policy UID |
| `fleetpermit.github.io/policy` | both | `namespace/name` of the source policy |
| `fleetpermit.github.io/content-digest` | both | `sha256:…` of the rendered content |
| `fleetpermit.github.io/work-digest` | ManifestWork | `sha256:…` of the generated ManifestWork spec |
| `fleetpermit.github.io/resync` | ManifestWork (label) | set when FleetPermit asks OCM to re-apply after drift |
| `fleetpermit.github.io/cluster` | XAccessPolicy | target cluster |
| `fleetpermit.github.io/lease-uids` | XAccessPolicy | comma-separated lease UIDs |
| `fleetpermit.github.io/expires-at` | XAccessPolicy | latest expiry among the rendered grants |

Both objects are named `fleetpermit-<policy name>-<hash>`, where the hash is the first 16 hex
characters of the SHA-256 of the policy's `namespace/name`. If the result would be longer than 63
characters, the name part is shortened to fit. The rendered object does not carry the
policy's generation, so a policy edit that does not change a cluster's grants changes neither its
content digest nor its ManifestWork, and causes no rollout. If a policy disappears without its
finalizer running (for example because the finalizer was removed by hand), the controller finds its
ManifestWorks by the `fleetpermit.github.io/policy` annotation and deletes them, each on condition
that its UID has not changed.

## Samples

[`config/samples`](../config/samples) contains a Placement, a policy, a lease and a standing
read-only policy. The integration tests validate them against the CRDs.

`standing-readonly-policy.yaml` grants `get_cluster_health` to `sre-agent` without a lease
(`lease.required: false`). Applying the whole directory therefore changes the result of a call made
without a lease: `get_cluster_health` is then allowed on the placed clusters. Apply the files one by
one if you want to reproduce the no-lease denial.
