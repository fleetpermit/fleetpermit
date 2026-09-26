# Architecture

<p align="center"><img src="assets/architecture.svg" alt="FleetPermit architecture" width="880"></p>

## Components

On the hub (a Kubernetes cluster running the Open Cluster Management hub):

| Component | Role |
|---|---|
| `fleetpermit-controller` | Reconciles `FleetAccessPolicy` and `ToolAccessLease`; resolves placements; renders and delivers grants; aggregates status; exposes Prometheus metrics and optional OpenTelemetry traces. One Deployment, installed with Helm. |
| OCM `Placement` / `PlacementDecision` | Selects managed clusters. FleetPermit only reads them. |
| OCM `ManifestWork` | Carries one rendered `XAccessPolicy` per (policy, cluster) into the cluster's namespace on the hub. |

On each managed cluster:

| Component | Role |
|---|---|
| OCM klusterlet (registration and work agents) | Applies `ManifestWork` content, re-applies it on drift and reports status feedback. |
| kube-agentic-networking controller | Accepts `XAccessPolicy`, programs the Envoy gateway over xDS, and issues SPIFFE X.509 identities through Kubernetes Pod Certificates. |
| Envoy gateway (`Gateway` + `HTTPRoute` + `XBackend`) | Terminates mTLS, authenticates the caller's SPIFFE ID, parses the MCP request and evaluates the RBAC rules, including the CEL time bound. |
| MCP tool server | The protected backend. |
| Default-deny anchor | An `XAccessPolicy` that makes the backend deny by default. |

FleetPermit runs no component on managed clusters ([ADR-2](design.md#adr-2-no-managed-cluster-agent-the-expiry-lives-in-the-data-plane)).

## Request path

```
agent pod (SPIFFE X.509 SVID from Pod Certificates)
   │  mTLS, MCP over Streamable HTTP: initialize → tools/call {name: restart_workload}
   ▼
Envoy gateway ── RBAC ──► ALLOW if (peer SPIFFE ID == rule source)
   │                           AND (tool ∈ rule tools)
   │                           AND (request.time < expiresAt)
   ▼
MCP tool server
```

The gateway answers a denied call itself, and the call never reaches the tool server. In
kube-agentic-networking v0.2.0 the answer is HTTP 200 with a JSON-RPC error whose code is 403 and
whose message is "Access to this tool is forbidden." (upstream issue #169). The lab's probe records
it as `JSON-RPC error 403: Access to this tool is forbidden.` and also treats HTTP 401 and 403 as
denials ([`demo/tools/probe/main.go`](../demo/tools/probe/main.go)).

## Control path

```
ToolAccessLease ─┐
FleetAccessPolicy├─► fleetpermit-controller ──► lease.Evaluate (pure: subject, subset, duration, placement ∩ request)
Placement(Decision)┘            │
                                ├─► enforcement.Renderer ──► XAccessPolicy (+ SHA-256 digest annotations)
                                ├─► placement.Provider.Apply ──► ManifestWork per selected cluster (inert policy if no grants)
                                ├─► placement.Provider.Remove ──► clusters that left the placement
                                └─► status: policy + every lease (conditions, clusters, expiresAt)
```

The reconciler is keyed by `FleetAccessPolicy`. Lease, `PlacementDecision`, `Placement` and
`ManifestWork` events map back to the policies they affect. `ManagedCluster` events do so only when a
cluster is created or deleted, or its `Available` condition changes. Every decision about a policy is made from one consistent snapshot.
The reconciler requeues at the next lease expiry and at least every two minutes. With
`--watch-namespace`, policies and events outside that namespace are ignored.

Only `PlacementDecision` objects whose controller owner is the policy's `Placement` count. OCM sets that
owner on the decisions it writes; the placement label alone could be set by anyone allowed to create
decisions in the namespace.

## What gets rendered

For the `sre-remediation` policy in namespace `fleet`, with lease `incident-42`, on `cluster-east`:

```yaml
apiVersion: agentic.networking.x-k8s.io/v1alpha1
kind: XAccessPolicy
metadata:
  name: fleetpermit-sre-remediation-a46c9779b9dca599   # 16 hex characters of SHA-256("fleet/sre-remediation")
  namespace: mcp-tools
  labels:
    app.kubernetes.io/managed-by: fleetpermit
    fleetpermit.github.io/policy-uid: 5b0c...
  annotations:
    fleetpermit.github.io/policy: fleet/sre-remediation
    fleetpermit.github.io/policy-uid: 5b0c...
    fleetpermit.github.io/cluster: cluster-east
    fleetpermit.github.io/lease-uids: 9f1e...
    fleetpermit.github.io/expires-at: "2026-09-26T10:15:00Z"
    fleetpermit.github.io/content-digest: sha256:8a5e41c4...
spec:
  targetRefs:
    - {group: agentic.networking.x-k8s.io, kind: XBackend, name: fleet-tools}
  action: Allow
  rules:
    - name: session-c8735226c1                # MCP base protocol methods for this subject
      source: {type: SPIFFE, spiffe: spiffe://cluster.local/ns/agents/sa/sre-agent}
      authorization:
        type: Inline
        mcp: {mcpBaseProtocolMethodsOption: MATCH_BASE_PROTOCOL_METHODS}
    - name: lease-7c41d2e9aa                  # the lease: tools/call, bounded in time
      source: {type: SPIFFE, spiffe: spiffe://cluster.local/ns/agents/sa/sre-agent}
      authorization:
        type: CEL
        cel:
          expression: "request.mcp.tool_name in ['restart_workload'] && request.time < timestamp('2026-09-26T10:15:00Z')"
```

The object name is `fleetpermit-<policy name>-<hash>`, where the hash is the first 16 hex characters
(64 bits) of the SHA-256 of the policy's `namespace/name`. If the result would be longer than 63
characters, the name part is shortened to fit. The ManifestWork on the hub uses the same name. Rule names carry a 10-character hash of the subject or the lease UID.

The session rule uses the upstream option `MATCH_BASE_PROTOCOL_METHODS`. In kube-agentic-networking
v0.2.0 it allows `initialize`, `tools/list`, `ping`, every method under `completion/`, `logging/` and
`notifications/`, HTTP `GET` for the event stream, and HTTP `DELETE` with an `mcp-session-id` header to
close a session. It does not allow `tools/call`. Only the lease rules do, and upstream pairs every CEL
rule with a `tools/call` match.

With no active grant on a cluster, the same object carries a single rule named `no-active-grants`
that can never match. The backend stays closed, and later grants are in-place updates
([ADR-4](design.md#adr-4-one-xaccesspolicy-per-policy-cluster-one-rule-per-lease)).

The content digest is SHA-256 over the canonical JSON of the policy UID, the cluster, the object's
name and namespace, and its spec. Identical inputs give identical digests, and any change to tools,
subject, expiry or target changes it.

## Status model

`FleetAccessPolicy.status`: `Ready`, `Progressing` and `Degraded` conditions; `selectedClusters`,
`readyClusters`, `clusterSummary` (`2/2`) and `activeLeases`; one entry per selected cluster with a
reason and the desired content digest, plus one entry, reason `Revoking`, for each cluster that left
the placement until its ManifestWork is gone. A cluster is `Ready` only when OCM's status feedback
reports the content digest the hub delivered. The list is sorted by name and holds at most 512
entries; only when more clusters would be listed are the clusters that are not ready listed first, and
the `Ready` message then says how many are listed. The per-cluster reasons are listed in
[api.md](api.md#fleetaccesspolicy).

`ToolAccessLease.status`: `Ready`, `Progressing`, `Degraded`, `Expired` and `Denied` conditions; a
`phase` summary (`Pending`, `Active`, `Expired`, `Denied`); `expiresAt`; and `clusters`, the clusters
the grant is rendered for, plus clusters that left the placement and are still being withdrawn from
(these do not count towards `Ready`). Delivery to the rendered clusters may still be in progress
until the lease is `Ready`. After expiry or denial, `clusters` lists the clusters the grant is still being withdrawn from,
including clusters whose ManifestWork is still being deleted, and it becomes empty when withdrawal is
complete. While a cluster is offline, this can last indefinitely.

## Failure behaviour

| If this disappears | Effect on authorization | Recovery |
|---|---|---|
| fleetpermit-controller | Existing grants keep working until their expiry, which Envoy enforces. New leases are not activated. | Restart; state is recomputed from the API. A restart neither withdraws nor re-creates grants (S15). |
| OCM hub (whole hub cluster) | Same as above. Managed clusters keep enforcing their last delivered grants and expire them on time (S11). | On reconnect, the controller withdraws expired grants (S12 records how long it took). |
| OCM work agent on a cluster | Delivered grants stay enforced and expire on time. Changes (new leases, early revocation) are not applied on that cluster. A cluster with a pending change shows as not ready (`Applying`); FleetPermit reports `ClusterUnavailable` only once OCM marks the ManagedCluster unavailable. | A work agent restart re-syncs. |
| A managed cluster (offline) | Delivered grants stay enforced and expire on time. Withdrawing grants from the cluster, or delivering to it again while its previous ManifestWork must first be removed, waits for it to reconnect. The cluster is reported `ClusterUnavailable` and counts as failed, so the policy is `Degraded` with `ClustersFailed` rather than `Progressing`. | When OCM marks the cluster available again, the ManagedCluster watch resumes the policy; the controller does not retry it quickly in the meantime. |
| Envoy gateway | No path to the tool server; calls fail. | Standard Deployment recovery. |
| kube-agentic-networking controller | Envoy keeps its last configuration, so decisions do not change and grants still expire on time (S15 restarts it). New grants and early revocations are not programmed into Envoy until the controller returns. | Standard Deployment recovery. |
| Identity issuance (Pod Certificates signer) | Callers without a valid certificate cannot complete mTLS and are rejected. | Signer recovery; certificates rotate automatically. |
| The policy's `Placement` (deleted) | Every grant for the policy is withdrawn, and the policy reports `Degraded/PlacementNotFound`. Its leases go to phase `Pending` (`NoEligibleClusters`); they are not denied. | Recreate the placement. Leases that have not expired are delivered again. |
| The `FleetAccessPolicy` (deleted) | The finalizer withdraws every grant. A lease that was evaluated against the policy becomes `Denied/PolicyNotFound`, which is terminal, or `Expired` if it was already past its recorded expiry. A lease that was never evaluated (for example applied before the policy) waits in `Pending/PolicyNotFound` for up to 5 minutes after its creation, then is denied, or expires if its `spec.duration` ends first. If the finalizer was removed by hand, the controller still finds the policy's ManifestWorks by their `fleetpermit.github.io/policy` annotation and deletes them. | Recreate the policy. Pending leases activate if it appears within their grace period; denied ones need new leases. A policy re-created under the same name first deletes any ManifestWorks the earlier one left, on every cluster; a placed cluster shows `Delivering` until they are gone. |
| Rendering fails for a cluster | That cluster's grants are withdrawn and the cluster is reported `DeliveryFailed`. | Fix the input. The controller retries after 1 s, then 2, 4, 8 s and so on up to 2 minutes while the failure persists; a fix that triggers no event can take up to that delay to be noticed. |
| Delivering the ManifestWork fails (the hub rejects it, or another policy owns one with the same name) | The cluster is reported `DeliveryFailed`; what it already holds stays in place. | Fix the cause. Retries back off in the same way, and any reconcile without a failure resets the backoff. |
| A delivered XAccessPolicy is deleted on a cluster | The gateway may still allow calls briefly, until it drops the deleted rule; then the anchor denies. OCM reports the object missing at its next status sync (every 10 s in the lab), and FleetPermit then asks the work agent to re-apply it immediately (S10). | Automatic; results.md shows the measured drift recovery time. |
| A delivered XAccessPolicy is edited in place on a cluster | If the content-digest annotation changes, this is handled like deletion. An edit that keeps the annotation is overwritten by the OCM work agent's periodic server-side re-apply, every 4 to 6 minutes in OCM v1.3. | Automatic. Write access to XAccessPolicy on a managed cluster is outside FleetPermit's trust boundary; see the threat model. |

Early revocation (deleting a lease before it expires) needs a working path from the hub. If the hub is
unreachable, the lease remains usable on disconnected clusters until it expires. Keep lease durations
short for this reason.

## Observability

Metrics (Prometheus, `:8080/metrics`) are listed in [operations.md](operations.md#metrics). Traces
(OpenTelemetry over OTLP/HTTP) are exported when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. The controller
creates a `fleetpermit.reconcile.policy` span per reconciliation and a `fleetpermit.render.cluster`
span per cluster, carrying the content digest and grant count. Identities are never used as metric
labels.
