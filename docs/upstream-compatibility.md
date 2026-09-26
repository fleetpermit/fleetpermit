# Upstream compatibility

FleetPermit composes upstream projects and pins the versions it tests. This page records what is
used, from where, and how stable it is. It was verified against the upstream sources on 2026-09-26.

| Component | Foundation / governance | Version | API used | Purpose | Maturity | Source |
|---|---|---|---|---|---|---|
| Kubernetes | CNCF | v1.35.0 (kind node `kindest/node:v1.35.0`) | core, CRDs, CEL validation, Pod Certificates (`certificates.k8s.io/v1beta1` PodCertificateRequest, ClusterTrustBundle, feature-gated) | platform; lab identity issuance | stable (Pod Certificates: beta, feature-gated) | https://kubernetes.io |
| Open Cluster Management | CNCF Sandbox | v1.3.1 (API module v1.3.0) | `cluster.open-cluster-management.io/v1beta1` Placement, PlacementDecision; `v1` ManagedCluster; `work.open-cluster-management.io/v1` ManifestWork (JSONPaths status feedback, server-side apply) | cluster selection and delivery | stable APIs | https://github.com/open-cluster-management-io/ocm |
| kube-agentic-networking | Kubernetes SIG Network subproject | v0.2.0 | `agentic.networking.x-k8s.io/v1alpha1` XAccessPolicy (`Allow`, SPIFFE sources, Inline MCP and CEL rules); `v0alpha0` XBackend | tool-level authorization | **experimental**: `X` kinds, alpha versions, reference implementation marked "not production-ready" | https://github.com/kubernetes-sigs/kube-agentic-networking |
| kube-agentic-networking controller image | Kubernetes project staging registry | `agentic-networking-controller:v0.2.0` | reference implementation | enforcement in the lab | experimental | `us-central1-docker.pkg.dev/k8s-staging-images/agentic-net` |
| Gateway API | Kubernetes SIG Network | v1.5.1 standard channel | `gateway.networking.k8s.io/v1` Gateway, HTTPRoute | routing to the tool server | GA | https://gateway-api.sigs.k8s.io |
| Envoy | CNCF Graduated | v1.36.6 | RBAC filter with CEL conditions, MCP filter (through the reference implementation) | data plane | stable (MCP filter is new) | https://www.envoyproxy.io |
| SPIFFE | CNCF Graduated | SPIFFE ID and X.509 SVID formats | `spiffe://` identities in mTLS certificates | workload identity | stable | https://spiffe.io |
| Model Context Protocol | Linux Foundation (Agentic AI Foundation) | protocol revision 2025-06-18 (probe); Go SDK v1.8.0 (demo server) | Streamable HTTP, `initialize`, `tools/call` | agent-to-tool protocol | stable spec | https://modelcontextprotocol.io |
| controller-runtime / controller-tools | Kubernetes SIG API Machinery | v0.25.1 / v0.22.0 | manager, envtest, CRD generation | controller framework | stable | https://github.com/kubernetes-sigs |
| Prometheus client_golang | CNCF Graduated | v1.24.1 | metrics | observability | stable | https://prometheus.io |
| OpenTelemetry Go | CNCF Incubating | v1.46.0 | traces over OTLP/HTTP | observability | stable (tracing) | https://opentelemetry.io |
| Helm | CNCF Graduated | chart apiVersion v2 (tested with Helm v4.1) | packaging | installation | stable | https://helm.sh |
| MetalLB | CNCF Sandbox | v0.15.3 | IPAddressPool, L2Advertisement | lab only: gateway addresses on kind | stable | https://metallb.io |
| kind | Kubernetes SIG Testing | v0.32.0 | — | lab only | stable | https://kind.sigs.k8s.io |

## Upstream facts FleetPermit depends on (and where they come from)

| Fact | Upstream location (v0.2.0) | What happens if it changes |
|---|---|---|
| CEL rules may use `request.time` and `request.mcp.tool_name` | `pkg/translator/cel.go` (`isValidVariable`) | Rendering is rejected upstream (`InvalidCEL`) → FleetPermit reports `RejectedByEnforcement` and the backend stays closed. Caught by the upstream canary job. |
| Allow policies on a target are OR-merged into one Envoy RBAC filter | `pkg/translator/accesspolicy.go` (`mergeAllowPoliciesToRBAC`) | Lease semantics would change; e2e scenarios S1/S13 detect it. |
| A target without any accepted policy is not enforced | `buildBackendLevelRBACFilters` | The default-deny anchor becomes unnecessary (harmless). |
| At most 5 policies per target, 10 rules per policy | `pkg/constants/controller.go`, CRD `MaxItems` | Capacity constants in `internal/enforcement/agenticnetworking/types.go`. |
| `params` match `params.name` only | `api/v1alpha1/accesspolicy_types.go` | Argument-level constraints stay unsupported (S16). |
| Denied calls get a JSON-RPC error with HTTP 200 (upstream issue #169) | quickstart documentation | The probe also treats HTTP 401/403 as a denial. |
| The v0.2.0 conformance suite's MCP backend image (`quickstart-everything-mcp:main`) is not published | `conformance/resources/base.yaml.tmpl` | `hack/conformance.sh` builds it from the upstream Dockerfile at the pinned tag. |
| An empty `ManifestWork` is rejected by OCM's admission webhook (the CRD schema alone allows it) | OCM work webhook | FleetPermit delivers an inert policy instead of an empty work (ADR-4). |
| Status feedback JSONPaths are polled every `statusSyncInterval` | OCM `pkg/work/spoke/statusfeedback` | Affects status latency only. |
| The work agent re-applies on ManifestWork spec or label changes, otherwise every 4 min ± 50% | OCM `pkg/work/spoke/controllers/manifestcontroller` | Drift repair falls back to the periodic resync. |

## Keeping up with upstream

- Versions are pinned in exactly two places: [`demo/scripts/lib.sh`](../demo/scripts/lib.sh) for the
  lab and `go.mod` for libraries. The upstream CRD used for schema tests is vendored in
  [`test/fixtures/upstream`](../test/fixtures/upstream).
- The scheduled **upstream canary** workflow ([`.github/workflows/upstream-canary.yaml`](../.github/workflows/upstream-canary.yaml))
  runs `hack/upstream-canary.sh` weekly. It resolves the latest releases of OCM, kube-agentic-networking,
  Gateway API and Envoy, reports any that differ from the pins, and runs the integration suite against the
  latest upstream `XAccessPolicy` CRD. A failure opens a visible red check instead of breaking users.
- Upgrading a pin: change it, run `make verify test`, then `make demo-down demo-up test-e2e results`,
  and update this table.
