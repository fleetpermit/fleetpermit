# Roadmap

FleetPermit v0.1 is deliberately small. Items below are **plans, not commitments**, and are driven
by user feedback and upstream progress.

## v0.1 (this release)

- `FleetAccessPolicy` and `ToolAccessLease` APIs (`v1alpha1`) with strict validation
- Open Cluster Management placement and delivery (Placement, PlacementDecision, ManifestWork)
- kube-agentic-networking `XAccessPolicy` enforcement, with the lease expiry enforced in the data plane
- Default-deny anchor for tool backends
- Drift detection and immediate re-apply requests
- Status with per-cluster readiness, Prometheus metrics, OpenTelemetry traces
- Helm chart, reproducible 1 hub + 3 cluster lab, end-to-end scenarios, benchmarks

## Next

- Track kube-agentic-networking releases, and adopt native expiry or argument-level MCP matching if
  upstream adds them.
- Faster detection of in-place edits to delivered objects, using OCM raw status feedback.
- Guidance and tests for federated SPIFFE trust domains across clusters (for example SPIRE federation).
- A published OCI Helm chart and signed images with provenance.

## Later (under consideration)

- Additional placement providers built on other neutral multicluster projects, through the existing
  `placement.Provider` seam, if users need them.
- Additional enforcement providers through the `enforcement.Renderer` seam.
- Protocols beyond MCP, as Kubernetes agentic networking APIs support them.
- A separate, optional approval workflow for leases.

## Out of scope

- Issuing identities or credentials.
- Replacing Kubernetes RBAC or in-cluster authorization of tool servers.
- Features that depend on a specific cloud, vendor or AI model.
