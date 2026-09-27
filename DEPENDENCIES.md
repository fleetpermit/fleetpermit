# Dependencies

FleetPermit's policy: architectural and direct dependencies come from the Kubernetes project, the
Cloud Native Computing Foundation or another Linux Foundation-hosted foundation. The only exceptions
are the Go toolchain, operating-system tooling, Git, GitHub hosting and CI, and unavoidable transitive
dependencies of approved projects. Nothing in the runtime architecture depends on a cloud provider, an
AI model vendor or a GitHub API.

Libraries are pinned in [`go.mod`](go.mod) and lab components in
[`demo/scripts/lib.sh`](demo/scripts/lib.sh). CI tools and build images have their own pins; the full
list of places is in
[docs/upstream-compatibility.md](docs/upstream-compatibility.md#keeping-up-with-upstream), together
with the APIs used.

## Go modules (direct)

| Module | Version | Project | Governance | License | Used for |
|---|---|---|---|---|---|
| `k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go` | v0.37.0 | Kubernetes | CNCF (Kubernetes) | Apache-2.0 | Kubernetes types and clients |
| `sigs.k8s.io/controller-runtime` | v0.25.1 | controller-runtime | Kubernetes SIG API Machinery | Apache-2.0 | controller framework, envtest |
| `sigs.k8s.io/yaml` | v1.6.0 | yaml | Kubernetes SIG | MIT, BSD-3-Clause and Apache-2.0 (per file) | tests |
| `open-cluster-management.io/api` | v1.3.0 | Open Cluster Management | CNCF Sandbox | Apache-2.0 | Placement, PlacementDecision, ManagedCluster, ManifestWork types |
| `github.com/prometheus/client_golang` | v1.24.1 | Prometheus | CNCF Graduated | Apache-2.0 | metrics |
| `github.com/prometheus/client_model` | v0.6.2 | Prometheus | CNCF Graduated | Apache-2.0 | tests (reading metric values) |
| `go.opentelemetry.io/otel`, `/sdk`, `/exporters/otlp/otlptrace/otlptracehttp` | v1.46.0 | OpenTelemetry | CNCF Graduated | Apache-2.0 | tracing |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | Model Context Protocol Go SDK | Linux Foundation (Agentic AI Foundation) | Apache-2.0 and MIT: the project is moving from MIT to Apache-2.0, new contributions are Apache-2.0, and code whose authors have not agreed to relicense stays MIT; documentation is CC-BY-4.0 (see its `LICENSE` file) | demo tool server only (`demo/tools/mcp-server`) |

Transitive dependencies are those of the modules above (for example `go.uber.org/zap` through
controller-runtime's logger, and `google.golang.org/protobuf` through OpenTelemetry). `make sbom`
produces a complete SPDX SBOM with the Kubernetes SIG Release `bom` tool.

## Upstream components FleetPermit integrates with (not linked)

| Component | Version | Governance | License | Role |
|---|---|---|---|---|
| Open Cluster Management (hub and klusterlet) | v1.3.1 | CNCF Sandbox | Apache-2.0 | cluster selection and delivery |
| kube-agentic-networking | v0.2.0 | Kubernetes SIG Network | Apache-2.0 | `XAccessPolicy` enforcement (experimental API) |
| Gateway API | v1.5.1 | Kubernetes SIG Network | Apache-2.0 | `Gateway`, `HTTPRoute` |
| Envoy | v1.36.10 (lab image pinned by tag and digest) | CNCF Graduated | Apache-2.0 | data plane (through kube-agentic-networking) |
| SPIFFE | spec | CNCF Graduated | Apache-2.0 | identity format |

The kube-agentic-networking `XAccessPolicy` CRD v0.2.0 is vendored unmodified, for schema tests, in
[`test/fixtures/upstream`](test/fixtures/upstream) (Apache-2.0, copyright The Kubernetes Authors).

## Lab and development tooling (not part of FleetPermit)

| Tool | Version tested | Governance | Role |
|---|---|---|---|
| kind | v0.32.0 | Kubernetes SIG Testing | local clusters |
| clusteradm | v1.3.1 | Open Cluster Management (CNCF) | OCM bootstrap |
| Helm | any Helm 3 or later; `hack/install-helm.sh` installs v3.19.0, checked against the Helm project's published SHA-256, when none is installed, or on CI when another version is | CNCF Graduated | installing the chart |
| MetalLB | v0.15.3 | CNCF Sandbox | gateway addresses on kind |
| controller-gen, setup-envtest | v0.22.0, v0.25.2-0.20260923145615-d837464d41be (`release-0.25` branch) | Kubernetes SIGs | code generation, integration tests |
| bom | v0.8.0 | Kubernetes SIG Release | SPDX SBOM |
| govulncheck | v1.1.4 | Go project (toolchain exception) | vulnerability audit |
| podman (or docker) | any | development infrastructure | runs kind nodes and builds images; FleetPermit code never calls it |
| asciinema, agg, ffmpeg | any | OS media tooling | recording demo videos only |

## Images

| Image | Base | Notes |
|---|---|---|
| `fleetpermit-controller` | `scratch` (built with the Go toolchain image) | static binary, UID 65532, CA certificates only |
| `demo-mcp-tools`, `demo-probe` | `scratch` | lab only |
