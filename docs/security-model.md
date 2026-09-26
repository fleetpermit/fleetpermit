# Security model

<p align="center"><img src="assets/security-model.svg" alt="FleetPermit security model: trust boundaries between the hub, the OCM delivery path and managed-cluster enforcement, with fail-closed points" width="820"></p>

## Principles

1. **Ceiling and activation are separate objects.** A `FleetAccessPolicy` defines the maximum. A
   `ToolAccessLease` activates part of it for a bounded time. Kubernetes RBAC decides who may write each,
   so a team can let on-call engineers create leases without letting them change the ceiling.
2. **Enforce where the call happens.** Identity, tool and time are all checked by the gateway in
   front of the tool server. The hub decides; it does not sit in the request path.
3. **Fail closed.** Every error path results in less authority:
   - no placement → nothing delivered;
   - rendering error → that cluster's grants withdrawn;
   - lease violates policy → `Denied`, terminal;
   - lease expired → the gateway denies, and the controller withdraws;
   - backend without any FleetPermit grant → the default-deny anchor denies;
   - `failMode` accepts only `Closed`.
4. **Subset only.** A lease can narrow tools, duration and clusters, never widen them. Its spec
   cannot be edited after creation.
5. **Traceable.** Every delivered object names its policy UID, policy generation, lease UIDs, cluster,
   expiry and SHA-256 content digest, so a rule found on a cluster leads back to the request that
   created it.

## Who can do what

| Principal | Needs | Can |
|---|---|---|
| Platform team | write `fleetaccesspolicies`, `placements` | define ceilings and cluster selection |
| On-call / automation | create `toolaccessleases` | activate a subset of a ceiling for a bounded time |
| FleetPermit controller | see [RBAC](operations.md#rbac) | read policies, leases, placements and clusters; write ManifestWork and status |
| Agent workload | a SPIFFE identity | call tools that an active lease grants it |

Grant `create` on `toolaccessleases`, not `update` (the spec is immutable anyway) and not `delete`,
unless the principal should also be able to revoke early.

## Identity

FleetPermit subjects are SPIFFE IDs. FleetPermit never issues, stores or sees credentials. The gateway
authenticates the caller from its mTLS peer certificate. In the lab those certificates are issued per
pod by Kubernetes Pod Certificates through the kube-agentic-networking signer
(`spiffe://cluster.local/ns/<namespace>/sa/<serviceaccount>`). The managed clusters share one CA, so an
identity issued on one cluster is trusted by the gateways of the others. In production, use a SPIFFE
trust domain whose trust bundle is distributed to every gateway, for example through SPIRE federation.

## Data plane limits (inherited, stated plainly)

- Tool matching is by exact tool name. Argument values are **not** matched in kube-agentic-networking
  v0.2.0.
- CEL rules apply to `tools/call`. Session methods are granted by a separate inline rule that is not
  time-bounded in the data plane.
- At most 5 `XAccessPolicy` objects per target and 10 rules per object upstream.
- The upstream APIs (`XAccessPolicy` v1alpha1, `XBackend` v0alpha0) are experimental.

See the [threat model](threat-model.md) for the full analysis and residual risks.
