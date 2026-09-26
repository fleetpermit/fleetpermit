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
  the placement. A lease that fails one of these checks is stored, then marked `Denied` (or `Pending`
  for the placement check) with a reason. The renderer also re-checks tool names and SPIFFE IDs
  before it builds a CEL expression.

In the tables, "API server" and "controller" say where each rule is enforced.

## FleetAccessPolicy

Short name `fap`. The maximum authority that leases may activate.

| Field | Type | Default | Validation / meaning |
|---|---|---|---|
| `spec.subjects[].spiffeID` | string | required | API server: 1–16 unique entries; 10–512 characters; `^spiffe://[a-z0-9._-]+(/[A-Za-z0-9._-]+)*$` (same as upstream) |
| `spec.placement.provider` | enum | `ocm` | API server: only `ocm` |
| `spec.placement.placementRef.name` | string | required | an OCM `Placement` in the same namespace; the controller reports `PlacementNotFound` if it is missing |
| `spec.target.protocol` | enum | `MCP` | API server: only `MCP` |
| `spec.target.namespace` | string | required | API server: a DNS label. The namespace of the target on each managed cluster |
| `spec.target.ref.group` | enum | `agentic.networking.x-k8s.io` | API server: or `gateway.networking.k8s.io` |
| `spec.target.ref.kind` | enum | `XBackend` | API server: or `Gateway` |
| `spec.target.ref.name` | string | required | the backend or gateway name |
| `spec.permissions[].tool` | string | required | API server: 1–16 unique tools; `^[A-Za-z0-9][A-Za-z0-9_.-]*$`, at most 20 characters (upstream params limit). Re-checked by the renderer |
| `spec.lease.required` | bool | `true` | `false` grants every permission to every subject without a lease (standing access) |
| `spec.lease.defaultDuration` | duration | `15m` | used when a lease omits `duration`. API server: at least `10s` and at most `maxDuration` |
| `spec.lease.maxDuration` | duration | `1h` | API server: at most `24h` |
| `spec.enforcement.provider` | enum | `kubernetes-agentic-networking` | API server: only value |
| `spec.enforcement.failMode` | enum | `Closed` | API server: only value; an open mode is intentionally not offered |

Status:

| Field | Meaning |
|---|---|
| `conditions` | `Ready`, `Progressing`, `Degraded`, each with a reason |
| `selectedClusters`, `readyClusters`, `clusterSummary` | for example `2/2` |
| `activeLeases` | leases currently granting on at least one cluster |
| `clusters[]` | `name`, `ready`, `reason`, `message`, `grants`, `contentDigest` |
| `observedGeneration` | last processed generation |

```console
$ kubectl get fap
NAME              CLUSTERS   READY   ACTIVE-LEASES   AGE
sre-remediation   2/2        True    1               10m
```

## ToolAccessLease

Short name `tal`. Activates a subset of a policy for a bounded time. The spec is immutable: the API
server rejects any change to it after creation.

| Field | Type | Default | Validation / meaning |
|---|---|---|---|
| `spec.policyRef.name` | string | required | a `FleetAccessPolicy` in the same namespace. Controller: `Denied/PolicyNotFound` if it does not exist |
| `spec.subject.spiffeID` | string | required | API server: SPIFFE ID pattern. Controller: must be one of the policy's subjects (`SubjectNotAllowed`) |
| `spec.permissions[].tool` | string | required | API server: 1–16 unique tools, same pattern as the policy. Controller: must be a subset of the policy's permissions (`PermissionNotAllowed`) |
| `spec.duration` | duration | policy `defaultDuration` | API server: at least `10s`. Controller: at most the policy's `maxDuration` (`DurationExceedsMaximum`). When omitted, see below |
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
| `phase` | `Pending`: no requested cluster is placed yet, or the enforcement rule limit is reached on every cluster. `Active`: rendered on at least one cluster. `Expired`, `Denied`: terminal. A summary of the conditions |
| `expiresAt` | `creationTimestamp` + effective duration. For a lease without `spec.duration`, that is the policy default in effect at first evaluation, pinned here. Policy changes never extend it |
| `clusters`, `clusterCount` | clusters that hold the grant; after expiry or denial, clusters still being revoked |
| `conditions` | `Ready`, `Progressing`, `Degraded`, `Expired`, `Denied` |

```console
$ kubectl get tal
NAME          POLICY            CLUSTERS   EXPIRES-AT             STATUS   AGE
incident-42   sre-remediation   2          2026-09-26T10:15:00Z   Active   1m
$ kubectl get tal -o wide      # adds the SUBJECT column
```

### Condition reasons

| Reason | Condition | Meaning |
|---|---|---|
| `LeaseActive` | Ready=True | granted and enforced on every target cluster |
| `RollingOut` | Ready=False, Progressing=True | delivery or acceptance pending on some clusters |
| `NoEligibleClusters` | Ready=False (phase Pending) | none of the requested clusters is currently placed |
| `CapacityExceeded` | Degraded=True | the upstream rule limit is reached on the listed clusters. If that is every cluster, the lease is in phase `Pending` (Ready=False with the same reason), is not counted as active, and activates when capacity frees up |
| `ClustersFailed` | Degraded=True | apply failed, rejected by enforcement, or cluster unavailable |
| `PolicyNotFound` | Denied=True | the referenced policy does not exist |
| `SubjectNotAllowed` | Denied=True | the subject is not in the policy |
| `PermissionNotAllowed` | Denied=True | a requested tool is not in the policy (the message lists them) |
| `DurationExceedsMaximum` | Denied=True | the duration exceeds `maxDuration` |
| `LeaseExpired` | Expired=True | past `expiresAt` |

`Denied` and `Expired` are terminal, provided only the controller can write
`toolaccessleases/status`. A policy change that removes a lease's tool or subject denies it.
A policy change that would allow a previously denied lease does not revive it. `Pending` is not
terminal: a pending lease activates when a requested cluster is placed or capacity frees up, as long
as it has not expired.

## Labels and annotations written by FleetPermit

On `ManifestWork` (hub) and the rendered `XAccessPolicy` (managed clusters):

| Key | On | Value |
|---|---|---|
| `app.kubernetes.io/managed-by` | both | `fleetpermit` |
| `fleetpermit.github.io/policy-uid` | both (label) | source policy UID |
| `fleetpermit.github.io/policy` | both | `namespace/name` of the source policy |
| `fleetpermit.github.io/content-digest` | both | `sha256:…` of the rendered content |
| `fleetpermit.github.io/work-digest` | ManifestWork | `sha256:…` of the generated ManifestWork spec |
| `fleetpermit.github.io/resync` | ManifestWork (label) | set when FleetPermit asks OCM to re-apply after drift |
| `fleetpermit.github.io/policy-generation` | XAccessPolicy | generation rendered |
| `fleetpermit.github.io/cluster` | XAccessPolicy | target cluster |
| `fleetpermit.github.io/lease-uids` | XAccessPolicy | comma-separated lease UIDs |
| `fleetpermit.github.io/expires-at` | XAccessPolicy | latest expiry among the rendered grants |

Both objects are named `fleetpermit-<policy name>-<hash>`, where the hash is the first 16 hex
characters of the SHA-256 of the policy's `namespace/name`.

## Samples

[`config/samples`](../config/samples) contains a Placement, a policy, a lease and a standing
read-only policy. The integration tests validate them against the CRDs.
