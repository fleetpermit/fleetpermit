# Design and architecture decisions

This document records the decisions that shape FleetPermit v0.1, the alternatives considered, and
the evidence behind each choice. Each decision lists what would make us revisit it.

## ADR-1: Compose existing projects; implement exactly one provider per seam

**Decision.** FleetPermit is one controller and two CRDs. It *composes* existing open projects:

| Concern | Project | Governance |
|---|---|---|
| Cluster selection and delivery | Open Cluster Management (`Placement`, `PlacementDecision`, `ManifestWork`) | CNCF Sandbox |
| Tool-level authorization | kube-agentic-networking (`XAccessPolicy`) | Kubernetes SIG Network subproject |
| Data plane | Envoy (through the kube-agentic-networking reference implementation) | CNCF Graduated |
| Workload identity | SPIFFE IDs, issued in the lab by Kubernetes Pod Certificates | CNCF Graduated (SPIFFE), Kubernetes |
| Tool protocol | Model Context Protocol | Linux Foundation (Agentic AI Foundation) |

The code has three seams, each an interface with one production implementation:
`placement.Provider` (OCM), `enforcement.Renderer` (kube-agentic-networking) and the `target.protocol`
field (MCP). The CRD fields `placement.provider`, `enforcement.provider` and `target.protocol` are
enums with one value, so a second implementation can be added without changing the API shape.

**Why.** The novel part is the *composition*: fleet placement plus tool-level authorization plus
time bounds. Re-implementing either side would add risk and remove the reason to trust the result.

**Revisit when** a second neutral multicluster project or enforcement layer is requested by users.

## ADR-2: No managed-cluster agent; the expiry lives in the data plane

**Question.** A lease must stop working on time even if the hub, OCM or FleetPermit is unavailable.
Does that need a FleetPermit agent on every managed cluster to delete expired grants locally?

**Finding.** The upstream `XAccessPolicy` v1alpha1 API has no expiry field. However, its reference
implementation accepts `type: CEL` authorization rules, and `request.time` is an allowed CEL variable
([`pkg/translator/cel.go`](https://github.com/kubernetes-sigs/kube-agentic-networking/blob/v0.2.0/pkg/translator/cel.go)).
The rule compiles into an Envoy RBAC policy condition, which Envoy evaluates on every request.

**Decision.** Each lease is rendered as one CEL rule that contains its expiry:

```
request.mcp.tool_name in ['restart_workload'] && request.time < timestamp('2026-09-26T10:15:00Z')
```

The gateway therefore stops honouring the grant at the expiry instant using its own clock, with no
dependency on the hub, the network to the hub, FleetPermit or the OCM work agent. The controller
removes expired grants afterwards; that is cleanup, not enforcement.

**Evidence.** Scenario S11 pauses the entire hub node before a lease expires. Both managed clusters
started denying within a few hundred milliseconds after expiry while the rendered policy was still
present on the cluster ([results](results.md)).

**Consequences.**
- One component fewer to install, upgrade, secure and grant RBAC to on every cluster.
- Expiry depends on the managed cluster's clock (see the threat model on clock skew).
- CEL rules in the reference implementation authorize `tools/call` only. MCP session methods
  (`initialize`, `tools/list`, `ping`) are granted by a separate inline rule per subject that the
  controller removes when the subject has no grant left; these are *not* time-bounded in the data
  plane. They list tools; they cannot call them.
- If a future upstream release removes `request.time` from the allowed CEL variables, rendering
  fails closed (the rule is rejected) and the integration test against the pinned CRD, plus the
  scheduled upstream canary, flag it.

**Revisit when** upstream adds a native expiry field (prefer it), or removes CEL time access (then a
small managed-cluster component becomes necessary).

## ADR-3: Default-deny anchor

**Finding.** The reference implementation installs no RBAC rules for a target that has no accepted
`XAccessPolicy`, so such a backend is open. Scenario A1 shows it: removing every policy from a
backend makes a prohibited tool reachable.

**Decision.** FleetPermit ships [`default-deny-anchor.yaml`](../config/managed-cluster/default-deny-anchor.yaml),
an `Allow` policy whose only rule can never match (a CEL rule of `false` for an unused identity).
Once any allow policy exists, Envoy denies every request that no rule allows. Install the anchor with
the backend on **every** cluster, whether or not a FleetPermit placement currently selects it; a
cluster that is not selected must still be closed.

**Why not deliver the anchor with ManifestWork?** Delivery follows placement, and the anchor must also
protect clusters that are *not* placed. It is a property of the backend, owned by whoever deploys it.

## ADR-4: One XAccessPolicy per (policy, cluster), one rule per lease

The reference implementation accepts at most **5** `XAccessPolicy` objects per target
(`MaxAccessPoliciesPerTarget`) and each policy holds at most **10** rules. One object per lease would
cap a backend at four concurrent leases (the anchor takes one slot). FleetPermit renders one object per
(FleetAccessPolicy, cluster) with one rule per lease plus one session rule per subject, so a policy
can carry up to nine concurrent grants per cluster for one subject.

A **placed cluster without grants receives an inert policy**: the same object, with a single rule
that can never match (source `spiffe://fleetpermit.invalid/no-active-grants`, CEL `false`), instead of
nothing. Three findings from the real lab drove this:

1. OCM's admission webhook rejects a `ManifestWork` with no manifests (the CRD schema alone allows it,
   so this does not show up in envtest).
2. Deleting a `ManifestWork` and immediately re-creating one with the same name waits for OCM's
   foreground deletion finalizer. One run measured a 14.7 s activation instead of about 130 ms.
3. The OCM work agent applies spec changes to an existing work immediately.

So activation, revocation and expiry cleanup are always in-place updates. As a side effect, a placed
cluster reports `Ready` only after the full path (delivery, the enforcement controller's acceptance,
status feedback) has been verified for that policy, before an incident needs it. Clusters that leave
the placement, and deleted policies, have their works deleted.

When capacity is exceeded, grants are ordered deterministically (standing grants, then leases by
creation time and name). Leases that do not fit are reported `Degraded` with reason
`CapacityExceeded` on the affected clusters. They are never silently dropped.

## ADR-5: API group `fleetpermit.github.io`

The project does not control `fleetpermit.io` or `fleetpermit.dev`. `fleetpermit.io` was unregistered
when checked. The project does control the GitHub Pages namespace `fleetpermit.github.io`, so the
API group, labels and annotations use it. If the project later controls a dedicated domain, a new
API version can move to it.

## ADR-6: Leases are immutable and terminal states are sticky

- `ToolAccessLease.spec` is immutable (a CEL `self == oldSelf` rule), so every grant corresponds to
  exactly one reviewed request.
- Expiry is `metadata.creationTimestamp + duration`. The creation timestamp is assigned by the API
  server, so the requester cannot choose it.
- `Denied` and `Expired` are terminal. A lease denied for asking for `read_secret` does not become
  active later if someone adds `read_secret` to the policy. A policy that *narrows* immediately denies
  the leases it no longer covers.
- A lease without `clusters` follows the placement as it changes, within its expiry. A lease with
  `clusters` only ever intersects them with the placement.

## ADR-7: Status that is safe to rely on

A cluster is `Ready` only when the ManifestWork's `Applied` condition matches the current generation,
the enforcement controller reported `Accepted` for the delivered object (read back through
ManifestWork status feedback), and the managed cluster is reported available. The content digest
on the hub must match what was delivered. Anything else is reported as `Progressing` or `Degraded`
with a reason: `Delivering`, `Updating`, `AwaitingAcceptance`, `RejectedByEnforcement`,
`ClusterUnavailable` or `Revoking`.

## ADR-8: Mirror, don't import, the upstream types

The upstream Go module pulls in Envoy's control plane and its dependencies. FleetPermit mirrors the
subset of `XAccessPolicy` fields it renders
([`internal/enforcement/agenticnetworking/types.go`](../internal/enforcement/agenticnetworking/types.go))
and validates every rendered shape against the pinned upstream CRD in the integration tests. The OCM
API module is small and is imported directly.

## ADR-9: Patch ManifestWorks; never read-modify-write against the OCM work agent

**Finding.** In the lab, the first lease after a controller restart sometimes took 5 to 8 seconds to
reach one cluster, while the other cluster took about 140 ms. The OCM work agent writes ManifestWork
status continuously. A read-modify-write `Update` from FleetPermit's informer cache then failed with a
`resourceVersion` conflict, and delivery waited for the next progress requeue.

**Decision.** FleetPermit changes ManifestWork spec, labels and annotations with a JSON merge patch and
no optimistic lock: it owns those fields, and the work agent owns status. Any delivery failure is
retried after 1 second. The regression test `TestDeliveryIsNotBlockedByConcurrentStatusWrites`
rewrites status every 10 ms while a lease is created, and requires delivery within 3 seconds.

## Not in v0.1 (deliberately)

- **Argument-level constraints** such as `replicas <= 10`. kube-agentic-networking v0.2.0 matches
  `params.name` only (scenario S16 records this), and FleetPermit will not fake protocol-level
  enforcement it cannot deliver.
- **Approval workflows.** Who may create leases is ordinary Kubernetes RBAC on `toolaccessleases`.
  Approval is a good candidate for a separate, composable controller.
- **A CLI.** `kubectl` with the printer columns covers status, and `kubectl describe` covers the
  conditions.
