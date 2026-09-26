# Threat model

This document describes what FleetPermit defends against, what it relies on, and where its
guarantees end. It covers v0.1 with the kube-agentic-networking v0.2.0 reference implementation and
Open Cluster Management v1.3. Claims marked *(tested)* are exercised by the end-to-end scenarios in
[results.md](results.md) or by the integration tests in `test/integration`.

## Assets

1. **Tool invocation authority**: the ability of a workload identity to invoke a given MCP tool on
   a given cluster.
2. **The policy ceiling**: `FleetAccessPolicy` objects that bound what leases may activate.
3. **Delivered enforcement state**: `XAccessPolicy` objects on managed clusters and the Envoy
   configuration derived from them.

## Trust assumptions

- **Hub API server and etcd.** Anyone who can write `FleetAccessPolicy` objects defines the
  ceiling. Anyone who can create `ToolAccessLease` objects can activate authority within it. Both are
  ordinary Kubernetes RBAC decisions that FleetPermit does not replace.
- **OCM.** The hub can create `ManifestWork` in cluster namespaces, and the work agent applies it.
  Any principal that can create `ManifestWork` for a cluster can change that cluster, with or
  without FleetPermit. The FleetPermit controller holds that permission, so its ServiceAccount is a
  high-value credential.
- **Managed cluster administrators.** A principal with write access to `XAccessPolicy`, the gateway
  or the tool server on a managed cluster can change local enforcement. FleetPermit detects and
  repairs some of these changes (below) but cannot prevent them.
- **Identity.** SPIFFE identities are issued to workloads by an issuer the gateway trusts. In the lab
  this is the kube-agentic-networking signer through Kubernetes Pod Certificates. FleetPermit trusts
  the identity that the gateway authenticates from the mTLS peer certificate.
- **Clocks.** Expiry is evaluated with each managed cluster's clock. Clocks are assumed to be
  synchronised (for example with NTP) to well within the shortest lease duration in use.
- **Upstream correctness.** Enforcement is performed by Envoy as configured by the
  kube-agentic-networking controller, whose API is **experimental**. FleetPermit validates everything
  it renders against the pinned upstream CRD and runs real calls through it end to end, but it inherits
  that implementation's behaviour and limits.

## Threats and mitigations

| # | Threat | Mitigation | Residual risk |
|---|---|---|---|
| T1 | **Compromised agent workload** uses its identity for unintended calls | Authority is limited to tools listed in an active lease, on placed clusters, until expiry. Tools outside the lease are denied (S2) and grants end at expiry (S5). | Within an active lease the compromised workload holds that lease's authority. Keep leases short and narrow. |
| T2 | **Wrong or spoofed SPIFFE identity** | The gateway authenticates the mTLS peer certificate against the trust bundle, and rules match the exact SPIFFE ID. An unlisted identity is denied (S3). | Depends on the identity issuer's integrity. |
| T3 | **Stolen or replayed credentials** | X.509 SVIDs are short-lived and bound to a private key held by the pod. mTLS prevents replay of captured traffic. | A stolen key is usable until its certificate expires, within the same lease limits as T1. |
| T4 | **Request for an unapproved tool** | Exact tool-name matching in the rendered rule; `read_secret` is denied (S2). | None beyond upstream correctness. |
| T5 | **Disallowed tool arguments** (for example `replicas=1000`) | **Not mitigated in v0.1.** kube-agentic-networking v0.2.0 matches only `params.name`; S16 records that both argument values are allowed. | Use separate tools for dangerous operations, or validate arguments in the tool server. |
| T6 | **Expired lease still used** | The expiry is part of the CEL rule Envoy evaluates on every request. Calls are denied at expiry (S5), including with the hub disconnected (S11). | Clock skew (T17). |
| T7 | **Lease permission escalation** (asks for more tools) | `Denied/PermissionNotAllowed`; nothing is rendered (S8, integration tests). Denials are terminal, so later widening the policy does not activate a denied lease. | None known. |
| T8 | **Lease placement expansion** (names clusters outside the placement) | Requested clusters are intersected with the placement; outside clusters get nothing and are reported (integration tests). | None known. |
| T9 | **Duration escalation** | `Denied/DurationExceedsMaximum` (S9). The expiry is derived from the API-server creation time plus an immutable duration. | None known. |
| T10 | **Malicious CRD mutation** (editing a lease after approval, injecting CEL) | Lease spec is immutable (API rejects edits). Tool names and SPIFFE IDs are validated by schema and validated again by the renderer. The character set cannot terminate a CEL string literal (S14, unit tests). | A principal who can edit the *policy* can change the ceiling; protect it with RBAC. |
| T11 | **Delivered XAccessPolicy deleted or drifted** on a managed cluster | Deletion: the anchor keeps the backend closed. OCM reports the object missing, and FleetPermit requests an immediate re-apply (S10). Edits that change the digest annotation are handled the same way. Other in-place edits are overwritten by OCM's periodic server-side apply. | An in-place edit that preserves the annotation and *broadens* access is effective until OCM's next periodic re-apply (4–6 min in OCM v1.3). Restrict write access to `xaccesspolicies` on managed clusters. |
| T12 | **Tampered ManifestWork on the hub** | FleetPermit restores the ManifestWork content from the policy on the next reconcile (integration test). | A principal with ManifestWork write access is already trusted (see assumptions). |
| T13 | **OCM hub disconnect** | Managed clusters keep enforcing the last delivered grants and expire them on time (S11). Stale grants are withdrawn about a second after reconnection (S12). | Early revocation (deleting a lease before expiry) cannot reach a disconnected cluster. |
| T14 | **Managed cluster disconnect** | Same as T13 for that cluster. The policy reports `ClusterUnavailable` once OCM marks it unavailable. | As T13. |
| T15 | **Hub or controller restart** | State is recomputed from the API. A restart neither withdraws nor re-creates grants; 40/40 calls stayed allowed across controller restarts (S15). | Leases created during the outage activate after recovery. |
| T16 | **Partial rollout** | A lease is `Ready` only when every target cluster runs the current digest and the enforcement layer accepted it. Otherwise it is `Progressing` or `Degraded`, naming the affected clusters. | Clients must not assume a lease is usable everywhere before `Ready`. |
| T17 | **Clock skew** between hub and managed clusters | Expiry is enforced by the managed cluster's clock, so a cluster whose clock is behind honours the grant longer by the skew. Status timestamps come from the hub. | Keep clocks synchronised; prefer lease durations much larger than possible skew. |
| T18 | **Cluster joins the placement after the lease was created** | A lease without explicit clusters follows the placement, so the new cluster receives the grant with the *same* expiry. A lease with explicit clusters does not grow (integration test). | Intended behaviour; name clusters in the lease to pin it. |
| T19 | **Cluster removed from the placement** | Its grants are withdrawn (S7). The anchor keeps it closed. | Withdrawal needs the hub to reach that cluster (as T13). |
| T20 | **Concurrent overlapping leases** | Each lease is an independent rule with its own expiry; the union applies (S13). Beyond the upstream rule limit, excess leases are reported `CapacityExceeded`, never silently dropped. | Capacity is about nine concurrent grants per policy per cluster (upstream limits). |
| T21 | **Stale status** | Readiness requires the work's `Applied` condition to match the current generation, acceptance feedback, and cluster availability. Otherwise the cluster is reported not ready with a reason. | OCM status feedback is periodic (its status sync interval), so status can lag enforcement. |
| T22 | **Deletion or finalizer failure** | A finalizer withdraws every ManifestWork before a policy disappears. Leases of a missing policy are denied (integration test). | If the controller is down, deletion waits for it. Grants still expire on time. |
| T23 | **Malformed upstream resources** (missing CRDs, rejected rules) | Apply failures and enforcement rejections are reported per cluster (`ApplyFailed`, `RejectedByEnforcement`) and never counted as ready. Rendering errors withdraw that cluster's content. | None known. |
| T24 | **Backend with no policy at all** (upstream fails open) | The default-deny anchor. Scenario A1 demonstrates the open state without it. | Operators must install the anchor with every governed backend. |
| T25 | **MCP session methods** (initialize, tools/list) | Granted per subject only while the subject holds a grant on that cluster; they cannot invoke tools. | They are not time-bounded in the data plane. They are removed by the controller after expiry, so they depend on the hub path. |
| T26 | **Controller ServiceAccount misuse** | Least-privilege RBAC with no secrets, no workloads and no RBAC writes (RBAC scenario). Optional OCM executor (`--work-executor`) makes the work agent apply content as a restricted managed-cluster identity. | ManifestWork write permission is inherently powerful; protect the controller's namespace. |

## Out of scope

Compromise of the Kubernetes control plane, etcd or the node OS; supply-chain compromise of
upstream images; confidentiality of tool responses (use TLS to the backend); denial of service
against gateways or the hub; and authorization inside the tool server itself.

## Security claims we do not make

FleetPermit does not make tool calls "safe", does not guarantee the absence of vulnerabilities, and
does not replace RBAC, network policy or tool-level authorization. It narrows *who can call which
tool, where, and for how long*, and it reports what it enforces.
