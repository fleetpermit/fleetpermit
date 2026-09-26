# Upstream compatibility

FleetPermit composes upstream projects and pins the versions it tests. This page records what is
used, from where, and how stable it is. It was checked against the upstream sources on 2026-09-26.

| Component | Foundation / governance | Version | API used | Purpose | Maturity | Source |
|---|---|---|---|---|---|---|
| Kubernetes | CNCF | v1.35.0 (kind node `kindest/node:v1.35.0`) | core, CRDs, CEL validation, Pod Certificates (`certificates.k8s.io/v1beta1` PodCertificateRequest, ClusterTrustBundle, feature-gated) | platform; lab identity issuance | stable (Pod Certificates: beta, feature-gated) | https://kubernetes.io |
| Open Cluster Management | CNCF Sandbox | v1.3.1 (API module v1.3.0) | `cluster.open-cluster-management.io/v1beta1` Placement, PlacementDecision; `v1` ManagedCluster; `work.open-cluster-management.io/v1` ManifestWork (JSONPaths status feedback, server-side apply) | cluster selection and delivery | stable APIs | https://github.com/open-cluster-management-io/ocm |
| kube-agentic-networking | Kubernetes SIG Network subproject | v0.2.0 | `agentic.networking.x-k8s.io/v1alpha1` XAccessPolicy (`Allow`, SPIFFE sources, Inline MCP and CEL rules); `v0alpha0` XBackend | tool-level authorization | experimental: `X` kinds, alpha versions, reference implementation marked "not production-ready" | https://github.com/kubernetes-sigs/kube-agentic-networking |
| kube-agentic-networking controller image | Kubernetes project staging registry | `agentic-networking-controller:v0.2.0` | reference implementation | enforcement in the lab | experimental | `us-central1-docker.pkg.dev/k8s-staging-images/agentic-net` |
| Gateway API | Kubernetes SIG Network | v1.5.1 standard channel | `gateway.networking.k8s.io/v1` Gateway, HTTPRoute | routing to the tool server | GA | https://gateway-api.sigs.k8s.io |
| Envoy | CNCF Graduated | v1.36.6 | RBAC filter with CEL conditions, MCP filter (through the reference implementation) | data plane | stable (MCP filter is new) | https://www.envoyproxy.io |
| SPIFFE | CNCF Graduated | SPIFFE ID and X.509 SVID formats | `spiffe://` identities in mTLS certificates | workload identity | stable | https://spiffe.io |
| Model Context Protocol | Linux Foundation (Agentic AI Foundation) | protocol revision 2025-06-18 (probe); Go SDK v1.8.0 (demo server) | Streamable HTTP, `initialize`, `tools/call` | agent-to-tool protocol | stable spec | https://modelcontextprotocol.io |
| controller-runtime / controller-tools | Kubernetes SIG API Machinery | v0.25.1 / v0.22.0 | manager, envtest, CRD generation | controller framework | stable | https://github.com/kubernetes-sigs |
| Prometheus client_golang | CNCF Graduated | v1.24.1 | metrics | observability | stable | https://prometheus.io |
| OpenTelemetry Go | CNCF Incubating | v1.46.0 | traces over OTLP/HTTP | observability | stable (tracing) | https://opentelemetry.io |
| Helm | CNCF Graduated | chart apiVersion v2; `hack/install-helm.sh` installs Helm v3.19.0 only when no Helm is installed | packaging | installation | stable | https://helm.sh |
| MetalLB | CNCF Sandbox | v0.15.3 | IPAddressPool, L2Advertisement | lab only: gateway addresses on kind | stable | https://metallb.io |
| kind | Kubernetes SIG Testing | v0.32.0 | none | lab only | stable | https://kind.sigs.k8s.io |

## Upstream facts FleetPermit depends on

| Fact | Upstream location (v0.2.0) | What happens if it changes |
|---|---|---|
| CEL rules may use `request.time` and `request.mcp.tool_name` | `pkg/translator/cel.go` (`isValidVariable`) | The upstream controller rejects the rendered policy (`InvalidCEL`), FleetPermit reports `RejectedByEnforcement`, and the grant does not take effect while the anchor keeps the backend closed. The CRD schema does not check CEL variables, so the canary cannot see this; the lab end-to-end suite (S1, S5) does. |
| Every CEL rule is paired with a `tools/call` method match | `pkg/translator/accesspolicy.go` (`buildToolsCallMethodPermission`) | Lease rules could allow other MCP methods; the renderer would need an explicit method condition. |
| `MATCH_BASE_PROTOCOL_METHODS` allows `initialize`, `tools/list`, `ping`, methods under `completion/`, `logging/` and `notifications/`, HTTP `GET`, and HTTP `DELETE` with an `mcp-session-id` header | `pkg/translator/accesspolicy.go` (`buildBaseProtocolMethodsRules`) | The per-subject session rule would allow more or less base protocol traffic. |
| Allow policies on a target are OR-merged into one Envoy RBAC filter | `pkg/translator/accesspolicy.go` (`mergeAllowPoliciesToRBAC`) | See [How Allow policies are combined](#how-allow-policies-are-combined). |
| A target without any accepted policy is not enforced | `pkg/translator/accesspolicy.go` (`buildBackendLevelRBACFilters`) | The default-deny anchor becomes unnecessary (harmless). |
| At most 5 policies per target, 10 rules per policy | `pkg/constants/controller.go`, CRD `MaxItems` | Capacity constants in `internal/enforcement/agenticnetworking/types.go`. |
| `params` match `params.name` only | `api/v1alpha1/accesspolicy_types.go` | Argument-level constraints stay unsupported (S16). |
| Denied calls get HTTP 200 with a JSON-RPC error (code 403, "Access to this tool is forbidden.") | quickstart documentation, upstream issue #169 | The probe also treats HTTP 401/403 as a denial. |
| The v0.2.0 conformance suite's MCP backend image (`quickstart-everything-mcp:main`) is not published | `conformance/resources/base.yaml.tmpl` | `hack/conformance.sh` builds it from the upstream Dockerfile at the pinned tag. |
| An empty `ManifestWork` is rejected by OCM's admission webhook (the CRD schema alone allows it) | OCM work webhook | FleetPermit delivers an inert policy instead of an empty work (ADR-4). |
| Status feedback JSONPaths are polled every `statusSyncInterval` | OCM `pkg/work/spoke/statusfeedback` | Affects status latency only. |
| The work agent re-applies on ManifestWork spec or label changes, and otherwise every 4 to 6 minutes (a 4-minute base with up to 50% added jitter) | OCM `pkg/work/spoke/controllers/manifestcontroller` | Drift repair falls back to the periodic resync. |

## How Allow policies are combined

The upstream API documentation for `XAccessPolicy.spec.action` in v0.2.0 says that a request is
allowed only if all Allow policies attached to a target allow it
([`api/v1alpha1/accesspolicy_types.go`](https://github.com/kubernetes-sigs/kube-agentic-networking/blob/v0.2.0/api/v1alpha1/accesspolicy_types.go)).
The v0.2.0 implementation does something different. `mergeAllowPoliciesToRBAC` puts every rule of
every accepted Allow policy on a target into a single Envoy RBAC filter with action `ALLOW`, so a
request is allowed when any one rule matches
([`pkg/translator/accesspolicy.go`](https://github.com/kubernetes-sigs/kube-agentic-networking/blob/v0.2.0/pkg/translator/accesspolicy.go)).
The lab observes the same behaviour: with the anchor and a FleetPermit policy on one backend, leased
calls are allowed (S1) and the matrix calls match the OR semantics.

FleetPermit's design depends on the implemented OR behaviour. The default-deny anchor's rule never
matches; under the documented AND semantics, the anchor would deny every call, including leased ones.
Several FleetPermit policies on one backend would also stop combining. This is a risk to track: if a
future upstream release aligns the implementation with the documentation, the anchor design has to
change. The lab end-to-end suite would show it immediately, because every ALLOW scenario would fail.

## Why some pins are not the newest release

Gateway API (v1.5.1) and Envoy (v1.36.x) are pinned to the versions that kube-agentic-networking
v0.2.0 itself is tested with (its CI and quickstart), rather than to the newest releases of those
projects. FleetPermit's data plane is whatever the agentic-networking reference implementation
programs, so its tested combination is the one that matters. The upstream canary reports newer
releases so they can be adopted deliberately, together with a new agentic-networking release.

## Keeping up with upstream

Upstream versions are pinned in these places:

| Where | What it pins |
|---|---|
| [`demo/scripts/lib.sh`](../demo/scripts/lib.sh) | the lab: kind node image (Kubernetes v1.35.0, by digest), OCM bundle v1.3.1 and its status sync interval, Gateway API v1.5.1, kube-agentic-networking v0.2.0 and its controller image, Envoy v1.36.6, MetalLB v0.15.3. `lab-up.sh`, `hack/conformance.sh` and `hack/upstream-canary.sh` read these values. |
| [`hack/install-lab-tools.sh`](../hack/install-lab-tools.sh) | kind v0.32.0 and clusteradm v1.3.1 on Linux CI runners |
| [`hack/install-helm.sh`](../hack/install-helm.sh) | Helm v3.19.0, installed only when no Helm is on the PATH; an existing Helm (for example the one on GitHub-hosted runners) is used as is |
| [`Makefile`](../Makefile) | `KAN_VERSION` v0.2.0, controller-gen v0.22.0, the envtest Kubernetes version 1.35.0 and setup-envtest `release-0.25`, govulncheck v1.1.4, bom v0.8.0 |
| [`go.mod`](../go.mod) | Go 1.26.0 and every Go library, including the OCM API module v1.3.0 |
| [`Dockerfile`](../Dockerfile) | the Go toolchain build image, by digest |
| [`test/fixtures/upstream`](../test/fixtures/upstream) | the kube-agentic-networking v0.2.0 `XAccessPolicy` CRD, vendored for schema tests |
| [`internal/enforcement/agenticnetworking/types.go`](../internal/enforcement/agenticnetworking/types.go) | the mirrored v0.2.0 `XAccessPolicy` fields and the rule limit |
| [`.github/workflows`](../.github/workflows) | every GitHub Action by commit SHA; the Go version comes from `go.mod` |
| [`charts/fleetpermit/Chart.yaml`](../charts/fleetpermit/Chart.yaml) | the supported Kubernetes range (`>=1.30.0-0`) |

[DEPENDENCIES.md](../DEPENDENCIES.md), this page and the README repeat these versions for readers.
Update them together with the pins.

The scheduled upstream canary ([`.github/workflows/upstream-canary.yaml`](../.github/workflows/upstream-canary.yaml))
runs `hack/upstream-canary.sh` every Monday and on demand. It does two things:

1. It looks up the latest release of OCM, kube-agentic-networking, Gateway API, Envoy and MetalLB on
   GitHub and prints any that differ from the pins in `demo/scripts/lib.sh`. Version drift is reported
   as a warning and does not fail the job.
2. It downloads the `XAccessPolicy` CRD from the latest kube-agentic-networking release and runs the
   integration suite (`make test-integration`) against it. The suite creates every rendered policy
   shape and the default-deny anchor on a real kube-apiserver (envtest), so a schema or CEL-validation
   change in the upstream CRD fails the job.

The canary does not run the upstream controller or Envoy, and it does not change any pin.
Translation behaviour, such as CEL variables or how policies are combined, is covered only by the lab
end-to-end suite against the pinned versions.

To upgrade a pin: change it, run `make verify test`, then `make demo-down demo-up test-e2e results`,
and update this page, DEPENDENCIES.md and the README.
