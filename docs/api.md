# API reference

API group `fleetpermit.github.io`, version `v1alpha1`. Both kinds are namespaced. A lease references a
policy in its own namespace, and a policy references an OCM `Placement` in its own namespace.
The namespace is the tenancy boundary.

Source of truth: [`api/v1alpha1`](../api/v1alpha1). CRDs: [`config/crd`](../config/crd). All validation
below is enforced by the API server (OpenAPI schema and CEL rules), and the controller re-validates.

## FleetAccessPolicy

Short name `fap`. The maximum authority that leases may activate.

| Field | Type | Default | Validation / meaning |
|---|---|---|---|
| `spec.subjects[].spiffeID` | string | required | 1–16 unique entries; `^spiffe://[a-z0-9._-]+(/[A-Za-z0-9._-]+)*$` (same as upstream) |
| `spec.placement.provider` | enum | `ocm` | only `ocm` |
| `spec.placement.placementRef.name` | string | required | an OCM `Placement` in the same namespace |
| `spec.target.protocol` | enum | `MCP` | only `MCP` |
| `spec.target.namespace` | string | required | namespace of the target on each managed cluster (DNS label) |
| `spec.target.ref.group` | enum | `agentic.networking.x-k8s.io` | or `gateway.networking.k8s.io` |
| `spec.target.ref.kind` | enum | `XBackend` | or `Gateway` |
| `spec.target.ref.name` | string | required | the backend or gateway name |
| `spec.permissions[].tool` | string | required | 1–16 unique tools; `^[A-Za-z0-9][A-Za-z0-9_.-]*$`, at most 20 characters (upstream params limit) |
| `spec.lease.required` | bool | `true` | `false` grants every permission to every subject without a lease (standing access) |
| `spec.lease.defaultDuration` | duration | `15m` | used when a lease omits `duration`; at least `10s`; at most `maxDuration` |
| `spec.lease.maxDuration` | duration | `1h` | at most `24h` |
| `spec.enforcement.provider` | enum | `kubernetes-agentic-networking` | only value |
| `spec.enforcement.failMode` | enum | `Closed` | only value; an open mode is intentionally not offered |

Status:

| Field | Meaning |
|---|---|
| `conditions` | `Ready`, `Progressing`, `Degraded`, each with a reason |
| `selectedClusters`, `readyClusters`, `clusterSummary` | e.g. `2/2` |
| `activeLeases` | leases currently granting on at least one cluster |
| `clusters[]` | `name`, `ready`, `reason`, `message`, `grants`, `contentDigest` |
| `observedGeneration` | last processed generation |

```console
$ kubectl get fap
NAME              CLUSTERS   READY   ACTIVE-LEASES   AGE
sre-remediation   2/2        True    1               10m
```

## ToolAccessLease

Short name `tal`. Activates a subset of a policy for a bounded time. **The spec is immutable.**

| Field | Type | Default | Validation / meaning |
|---|---|---|---|
| `spec.policyRef.name` | string | required | a `FleetAccessPolicy` in the same namespace |
| `spec.subject.spiffeID` | string | required | must be one of the policy's subjects |
| `spec.permissions[].tool` | string | required | 1–16; must be a subset of the policy's permissions |
| `spec.duration` | duration | policy `defaultDuration` | must be positive and at most the policy's `maxDuration` |
| `spec.clusters[]` | []string | all placed clusters | narrows the placement; clusters outside it receive nothing |
| `spec.reason` | string | — | free text for audit, at most 256 characters |

Status:

| Field | Meaning |
|---|---|
| `phase` | `Pending` (no eligible cluster yet), `Active`, `Expired`, `Denied`; summary of the conditions |
| `expiresAt` | `creationTimestamp + duration`, computed once, never extended |
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
| `CapacityExceeded` | Degraded=True | upstream rule limit reached on the listed clusters |
| `ClustersFailed` | Degraded=True | apply failed, rejected by enforcement, or cluster unavailable |
| `PolicyNotFound` | Denied=True | the referenced policy does not exist |
| `SubjectNotAllowed` | Denied=True | the subject is not in the policy |
| `PermissionNotAllowed` | Denied=True | a requested tool is not in the policy (lists them) |
| `DurationExceedsMaximum` | Denied=True | duration exceeds `maxDuration` |
| `LeaseExpired` | Expired=True | past `expiresAt` |

`Denied` and `Expired` are terminal. A policy change that removes a lease's tool or subject denies it.
A policy change that would allow a previously denied lease does not revive it.

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

## Samples

[`config/samples`](../config/samples) contains a Placement, a policy, a lease and a standing
read-only policy. They are validated against the CRDs in the integration tests.
