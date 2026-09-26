# Design and architecture decisions

This document records the decisions that shape FleetPermit v0.1, the alternatives considered, and
the evidence behind each choice. Each decision says what would make us revisit it. "v0.1" means the
v0.1.0 release plus the fixes listed under Unreleased in the [changelog](../CHANGELOG.md), which ship
in v0.1.1. Behaviour marked "(v0.1.1)" is not in v0.1.0.

## ADR-1: Compose existing projects; implement exactly one provider per seam

**Decision.** FleetPermit is one controller and two CRDs. It composes existing open projects:

| Concern | Project | Governance |
|---|---|---|
| Cluster selection and delivery | Open Cluster Management (`Placement`, `PlacementDecision`, `ManifestWork`) | CNCF Sandbox |
| Tool-level authorization | kube-agentic-networking (`XAccessPolicy`) | Kubernetes SIG Network subproject |
| Data plane | Envoy (through the kube-agentic-networking reference implementation) | CNCF Graduated |
| Workload identity | SPIFFE IDs, issued in the lab by Kubernetes Pod Certificates | CNCF Graduated (SPIFFE), Kubernetes |
| Tool protocol | Model Context Protocol | Linux Foundation (Agentic AI Foundation) |

The code has two seams, each a Go interface with one implementation: `placement.Provider` (OCM) and
`enforcement.Renderer` (kube-agentic-networking). The controller wires these two implementations at
startup ([`cmd/fleetpermit-controller/main.go`](../cmd/fleetpermit-controller/main.go)). The protocol
is a third seam at the API level. The CRD fields `placement.provider`, `enforcement.provider` and
`target.protocol` are enums with one value each (`ocm`, `kubernetes-agentic-networking`, `MCP`), so
a second implementation can be added later without changing the API shape. With one value per field,
the controller does not need to read them yet.

**Why.** What FleetPermit adds is the combination of fleet placement, tool-level authorization and
time bounds. Re-implementing either side would add risk and remove the reason to trust the result.

**Revisit when** users ask for a second neutral multicluster project or enforcement layer.

## ADR-2: No managed-cluster agent; the expiry lives in the data plane

**Question.** A lease must stop working on time even if the hub, OCM or FleetPermit is unavailable.
Does that need a FleetPermit agent on every managed cluster to delete expired grants locally?

**Finding.** The upstream `XAccessPolicy` v1alpha1 API has no expiry field. Its reference
implementation does accept `type: CEL` authorization rules, and `request.time` is an allowed CEL
variable ([`pkg/translator/cel.go`](https://github.com/kubernetes-sigs/kube-agentic-networking/blob/v0.2.0/pkg/translator/cel.go)).
The rule compiles into an Envoy RBAC policy condition, which Envoy evaluates on every request.

**Decision.** Each lease is rendered as one CEL rule that contains its expiry:

```
request.mcp.tool_name in ['restart_workload'] && request.time < timestamp('2026-09-26T10:15:00Z')
```

The gateway stops honouring the grant at the expiry instant, using its own clock. It does not depend
on the hub, the network to the hub, FleetPermit or the OCM work agent. The controller removes expired
grants afterwards as cleanup; enforcement has already happened in the gateway.

**Evidence.** Scenario S11 pauses the entire hub node before a lease expires. Both managed clusters
started denying within a few hundred milliseconds after expiry while the rendered policy was still
present on the cluster ([results](results.md)).

**Consequences.**

- There is one component fewer to install, upgrade, secure and grant RBAC to on every cluster.
- Expiry depends on the managed cluster's clock (see the threat model on clock skew).
- Upstream pairs every CEL rule with a `tools/call` method match, so lease rules only ever allow
  `tools/call`. MCP session traffic is allowed by a separate inline rule per subject with
  `mcpBaseProtocolMethodsOption: MATCH_BASE_PROTOCOL_METHODS`. In kube-agentic-networking v0.2.0
  that option allows the methods `initialize`, `tools/list` and `ping`, every method that starts
  with `completion/`, `logging/` or `notifications/`, HTTP `GET` (the event stream) and HTTP
  `DELETE` with an `mcp-session-id` header (closing a session)
  ([`buildBaseProtocolMethodsRules`](https://github.com/kubernetes-sigs/kube-agentic-networking/blob/v0.2.0/pkg/translator/accesspolicy.go)).
  None of these can call a tool. The controller removes the session rule when the subject has no
  grant left on that cluster, but the rule itself has no time bound in the data plane.
- If a future upstream release removed `request.time` from the allowed CEL variables, the upstream
  controller would reject the rendered policy (`InvalidCEL`). FleetPermit would report
  `RejectedByEnforcement`, the grant would not take effect, and the anchor would keep the backend
  closed. The upstream CRD schema does not check CEL variables, so the integration tests and the
  upstream canary would not catch this. The lab end-to-end suite (S1, S5) would, and it must pass
  before any upstream pin is raised.

**Revisit when** upstream adds a native expiry field (prefer it), or removes CEL time access (then a
small managed-cluster component becomes necessary).

## ADR-3: Default-deny anchor

**Finding.** The reference implementation installs no RBAC rules for a target that has no accepted
`XAccessPolicy`, so such a backend is open. Scenario A1 shows it: removing every policy from a
backend makes a prohibited tool reachable.

**Decision.** FleetPermit ships [`default-deny-anchor.yaml`](../config/managed-cluster/default-deny-anchor.yaml),
an `Allow` policy whose only rule can never match (a CEL rule of `false` for an unused identity). In
kube-agentic-networking v0.2.0, once any Allow policy exists on a target, Envoy denies every request
that no rule allows. Install the anchor with the backend on every cluster, whether or not a
FleetPermit placement currently selects it. A cluster that is not selected must still be closed.

This design relies on how v0.2.0 combines several Allow policies on one target. See
[upstream-compatibility.md](upstream-compatibility.md#how-allow-policies-are-combined).

**Why not deliver the anchor with ManifestWork?** Delivery follows placement, and the anchor must also
protect clusters that are not placed. The anchor belongs to the backend and to whoever deploys it.

## ADR-4: One XAccessPolicy per (policy, cluster), one rule per lease

The reference implementation accepts at most 5 `XAccessPolicy` objects per target
(`MaxAccessPoliciesPerTarget`), and each policy holds at most 10 rules. One object per lease would cap
a backend at four concurrent leases, because the anchor takes one slot. FleetPermit renders one object
per (FleetAccessPolicy, cluster), with one rule per lease plus one session rule per subject. A policy
can therefore carry up to nine concurrent grants per cluster for one subject.

A placed cluster without grants receives an inert policy: the same object with a single rule that can
never match (source `spiffe://fleetpermit.invalid/no-active-grants`, CEL `false`). Three findings from
the real lab led to this:

1. OCM's admission webhook rejects a `ManifestWork` with no manifests. The CRD schema alone allows it,
   so this does not show up in envtest.
2. Deleting a `ManifestWork` and immediately re-creating one with the same name waits for OCM's
   foreground deletion finalizer, which delayed activation noticeably in the lab.
3. The OCM work agent applies spec changes to an existing work immediately.

So activation, revocation and expiry cleanup are always in-place updates. A side effect is that a
placed cluster reports `Ready` only after the full path (delivery, the enforcement controller's
acceptance, status feedback) has been verified for that policy, before an incident needs it. The works
of clusters that leave the placement, and of deleted policies, are deleted.

When capacity is exceeded, grants are ordered deterministically: standing grants first, then leases by
creation time and name. A lease that does not fit on some clusters is reported `Degraded` with reason
`CapacityExceeded` on those clusters. A lease that fits on no cluster stays in phase `Pending` with the
same reason, and activates when capacity frees up (v0.1.1). Leases are never dropped silently.

## ADR-5: API group `fleetpermit.github.io`

The project does not control `fleetpermit.io` or `fleetpermit.dev`; `fleetpermit.io` was unregistered
when checked on 2026-09-26. The project does control the GitHub Pages namespace `fleetpermit.github.io`, so the API
group, labels and annotations use it. If the project later controls a dedicated domain, a new API
version can move to it.

## ADR-6: Leases are immutable and terminal states are sticky

- `ToolAccessLease.spec` is immutable (a CEL `self == oldSelf` rule), so every grant corresponds to
  exactly one reviewed request.
- Expiry is `metadata.creationTimestamp + duration`. The API server assigns the creation timestamp,
  so the requester cannot choose it. A lease that omits `duration` gets the policy's
  `defaultDuration` in effect when it is first evaluated. That expiry is then pinned through
  `status.expiresAt`, so raising the policy default later cannot extend an issued lease, and lowering
  the policy maximum below it denies the lease (v0.1.1; in v0.1.0 a lease without `duration` followed
  the policy's current default). This was found in code review; see
  `TestPolicyDefaultChangeCannotExtendLease`.
- Because that pinned value lives in the status subresource, only the controller should be able to
  write `toolaccessleases/status`. A principal that can write it could extend such a lease up to the
  policy's `maxDuration`. A recorded expiry beyond the maximum denies the lease
  (`TestEvaluateForgedStatusCannotExceedMaximum`).
- ManifestWork and XAccessPolicy names carry a 64-bit hash (16 hex characters) of the policy's
  namespace and name, and FleetPermit refuses to modify a ManifestWork owned by another policy
  (`TestForeignManifestWorkIsNotOverwritten`). Both are v0.1.1; v0.1.0 used 8 hex characters.
- `Denied` and `Expired` are terminal, provided only the controller can write
  `toolaccessleases/status`: the controller reads the recorded conditions back to keep them sticky. A
  lease denied for asking for `read_secret` does not become active later if someone adds
  `read_secret` to the policy. A policy that narrows denies the leases it no longer covers at once.
  Deleting a policy denies its leases that have not expired; expired leases stay `Expired`
  (v0.1.1, `TestExpiredLeaseStaysExpiredWhenPolicyIsDeleted`).
- A lease without `clusters` follows the placement as it changes, within its expiry. A lease with
  `clusters` only ever intersects them with the placement.

## ADR-7: Status that is safe to rely on

A cluster is `Ready` only when the ManifestWork's `Applied` condition matches the current generation,
the enforcement controller reported `Accepted` for the delivered object (read back through
ManifestWork status feedback), and the managed cluster is reported available. The content digest on
the hub must match what was delivered. Anything else is reported as `Progressing` or `Degraded`, with
a reason such as `Delivering`, `Updating`, `AwaitingAcceptance`, `RejectedByEnforcement`,
`ClusterUnavailable` or `Revoking`.

## ADR-8: Mirror, don't import, the upstream types

The upstream Go module pulls in Envoy's control plane and its dependencies. FleetPermit mirrors the
subset of `XAccessPolicy` fields it renders
([`internal/enforcement/agenticnetworking/types.go`](../internal/enforcement/agenticnetworking/types.go))
and validates every rendered shape against the pinned upstream CRD in the integration tests. The OCM
API module is small and is imported directly.

## ADR-9: Patch ManifestWorks; never read-modify-write against the OCM work agent

This decision and ADR-10 are v0.1.1.

**Finding.** In the lab, the first lease after a controller restart sometimes reached one cluster
seconds later than the other. The OCM work agent writes ManifestWork status continuously. A
read-modify-write `Update` from FleetPermit's informer cache then failed with a `resourceVersion`
conflict, and delivery waited for the next progress requeue.

**Decision.** FleetPermit changes ManifestWork spec, labels and annotations with a JSON merge patch and
no optimistic lock: it owns those fields, and the work agent owns status. Any delivery failure is
retried after 1 second. The regression test `TestDeliveryIsNotBlockedByConcurrentStatusWrites`
rewrites status every 10 ms while a lease is created, and requires delivery within 3 seconds.

**Result.** The switch to merge patches removed the `Update` conflicts, but slow first activations
continued after controller restarts. The logs the suite captures for slow activations showed the
cause: a new controller pod started while the old one still held the leader-election lease, and it
reconciled nothing until that lease expired (15 s by default in controller-runtime). Both work agents
then applied the change at the same moment. The suite used to poll east and then west, so the second
cluster looked fast and the delay looked like it belonged to one work agent inside OCM. That reading
was wrong. See ADR-10 for the fix.

## ADR-10: Release leadership on shutdown

**Decision.** The controller sets controller-runtime's `LeaderElectionReleaseOnCancel`, so a stopping
leader releases the lease and its replacement takes over without waiting for the lease to expire.
This is safe because the process exits as soon as the manager stops. The end-to-end suite and the
benchmark now poll all clusters at the same time from one start time.

## Outside the v0.1 scope

- Argument-level constraints such as `replicas <= 10`. kube-agentic-networking v0.2.0 matches
  `params.name` only (scenario S16 records this), and FleetPermit does not claim enforcement that the
  data plane cannot provide.
- Approval workflows. Who may create leases is ordinary Kubernetes RBAC on `toolaccessleases`.
  Approval fits better in a separate controller that composes with FleetPermit.
- A CLI. `kubectl` with the printer columns covers status, and `kubectl describe` shows the
  conditions.
