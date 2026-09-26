# Architecture

<p align="center"><img src="assets/architecture.svg" alt="FleetPermit architecture" width="880"></p>

## Components

**On the hub** (a Kubernetes cluster running the Open Cluster Management hub):

| Component | Role |
|---|---|
| `fleetpermit-controller` | Reconciles `FleetAccessPolicy` and `ToolAccessLease`; resolves placements; renders and delivers grants; aggregates status; exposes Prometheus metrics and optional OpenTelemetry traces. One Deployment, installed with Helm. |
| OCM `Placement` / `PlacementDecision` | Selects managed clusters. FleetPermit only reads them. |
| OCM `ManifestWork` | Carries one rendered `XAccessPolicy` per (policy, cluster) into the cluster's namespace on the hub. |

**On each managed cluster:**

| Component | Role |
|---|---|
| OCM klusterlet (registration and work agents) | Applies `ManifestWork` content, re-applies it on drift and reports status feedback. |
| kube-agentic-networking controller | Accepts `XAccessPolicy`, programs the Envoy gateway over xDS, and issues SPIFFE X.509 identities through Kubernetes Pod Certificates. |
| Envoy gateway (`Gateway` + `HTTPRoute` + `XBackend`) | Terminates mTLS, authenticates the caller's SPIFFE ID, parses the MCP request and evaluates the RBAC rules, including the CEL time bound. |
| MCP tool server | The protected backend. |
| Default-deny anchor | An `XAccessPolicy` that makes the backend deny by default. |

There is no FleetPermit component on managed clusters ([ADR-2](design.md#adr-2-no-managed-cluster-agent-the-expiry-lives-in-the-data-plane)).

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

A denied request receives a JSON-RPC error (`-32603`/`403`-style, "Access to this tool is forbidden")
from the gateway. The call never reaches the tool server.

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

The reconciler is keyed by `FleetAccessPolicy`. Lease, `PlacementDecision`, `Placement`,
`ManifestWork` and `ManagedCluster` events all map back to the policies they affect, so every decision
about a policy is made from one consistent snapshot. The reconciler requeues at the next lease expiry
and at least every two minutes.

## What gets rendered

For the `sre-remediation` policy with lease `incident-42` on `cluster-east`:

```yaml
apiVersion: agentic.networking.x-k8s.io/v1alpha1
kind: XAccessPolicy
metadata:
  name: fleetpermit-sre-remediation-a46c9779
  namespace: mcp-tools
  labels:
    app.kubernetes.io/managed-by: fleetpermit
    fleetpermit.github.io/policy-uid: 5b0c...
  annotations:
    fleetpermit.github.io/policy: fleet/sre-remediation
    fleetpermit.github.io/policy-uid: 5b0c...
    fleetpermit.github.io/policy-generation: "1"
    fleetpermit.github.io/cluster: cluster-east
    fleetpermit.github.io/lease-uids: 9f1e...
    fleetpermit.github.io/expires-at: "2026-09-26T10:15:00Z"
    fleetpermit.github.io/content-digest: sha256:8a5e41c4...
spec:
  targetRefs:
    - {group: agentic.networking.x-k8s.io, kind: XBackend, name: fleet-tools}
  action: Allow
  rules:
    - name: session-3f2a9c1b0d                # MCP session methods for this subject
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

With no active grant on a cluster, the same object carries a single rule named `no-active-grants`
that can never match, so the backend stays closed and later grants are in-place updates
([ADR-4](design.md#adr-4-one-xaccesspolicy-per-policy-cluster-one-rule-per-lease)).

The content digest is SHA-256 over the canonical JSON of the policy UID, the cluster, the object's
name and namespace, and its spec. It is deterministic: identical inputs give identical digests, and
any change to tools, subject, expiry or target changes it.

## Status model

`FleetAccessPolicy.status`: `Ready`, `Progressing` and `Degraded` conditions; `selectedClusters`,
`readyClusters`, `clusterSummary` (`2/2`) and `activeLeases`; one entry per selected cluster with a
reason and the desired content digest.

`ToolAccessLease.status`: `Ready`, `Progressing`, `Degraded`, `Expired` and `Denied` conditions; a
`phase` summary (`Pending`, `Active`, `Expired`, `Denied`); `expiresAt`; and the clusters that
currently hold its grant. After expiry or denial, `clusters` lists only clusters still being revoked
and becomes empty when revocation is complete.

## Failure behaviour

| If this disappears… | Effect on authorization | Recovery |
|---|---|---|
| fleetpermit-controller | Existing grants keep working **until their expiry**, which Envoy enforces. New leases are not activated. | Restart; state is recomputed from the API. No grant is withdrawn or re-created by a restart (S15). |
| OCM hub (whole hub cluster) | Same as above; managed clusters keep enforcing their last delivered grants and expire them on time (S11). | On reconnect, expired grants are withdrawn in about a second (S12). |
| OCM work agent on a cluster | Delivered grants stay enforced and expire on time; changes (new leases, early revocation) are not applied on that cluster, and FleetPermit reports it `ClusterUnavailable` once OCM marks the cluster unavailable. | Work agent restart re-syncs. |
| Envoy gateway / agentic-networking controller | No path to the tool server; calls fail. A controller restart leaves Envoy's last configuration in place (S15). | Standard Deployment recovery. |
| Identity issuance (Pod Certificates signer) | Callers without a valid certificate cannot complete mTLS and are rejected. | Signer recovery; certificates rotate automatically. |
| Placement (deleted) | Every grant for the policy is withdrawn; the policy reports `Degraded/PlacementNotFound`. | Recreate the placement. |
| Rendering fails for a cluster | That cluster's grants are withdrawn (fail closed) and the cluster is reported `DeliveryFailed`. | Fix the input. |
| A delivered XAccessPolicy is deleted on a cluster | While missing, the anchor denies (loss of availability, not of safety). OCM reports the object missing at its next status sync; FleetPermit then asks the work agent to re-apply immediately (S10). | Automatic, within about one OCM status-sync interval. |
| A delivered XAccessPolicy is edited in place on a cluster | If the content-digest annotation changes, handled like deletion. An edit that keeps the annotation is overwritten by the OCM work agent's periodic server-side re-apply (4–6 minutes in OCM v1.3). | Automatic. Write access to XAccessPolicy on a managed cluster is outside FleetPermit's trust boundary; see the threat model. |

Early revocation (deleting a lease before it expires) needs the hub path to be working. If the hub is
unreachable, the lease remains usable on disconnected clusters until its expiry. Keep lease durations
short for this reason.

## Observability

Metrics (Prometheus, `:8080/metrics`) are listed in [operations.md](operations.md#metrics). Traces
(OpenTelemetry over OTLP/HTTP) are exported when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. The controller
creates a `fleetpermit.reconcile.policy` span per reconciliation and a `fleetpermit.render.cluster`
span per cluster, carrying the content digest and grant count. Identities are never used as metric
labels.
