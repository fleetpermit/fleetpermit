# Security self-assessment

This self-assessment follows the structure of the CNCF TAG Security self-assessment template. It
describes FleetPermit v0.1.1 (see the [changelog](../CHANGELOG.md) for what changed since v0.1.0). It
has not been reviewed by TAG Security or audited by a third party.

## Metadata

| | |
|---|---|
| Software | https://github.com/fleetpermit/fleetpermit |
| Security provider | Yes. FleetPermit makes authorization decisions and delivers them to an enforcement layer. |
| Languages | Go (controller); Bash (lab and test tooling) |
| SBOM | SPDX 2.3, generated with the Kubernetes SIG Release `bom` tool (`make sbom`) and attached to each release |
| Security docs | [security model](security-model.md), [threat model](threat-model.md), [SECURITY.md](../SECURITY.md) |

## Overview

FleetPermit gives workload identities (for example AI agents) time-bound permission to call specific
MCP tools on specific clusters of a Kubernetes fleet. A `FleetAccessPolicy` defines the maximum
authority. A `ToolAccessLease` activates a subset of it for a bounded time. The controller runs on an
Open Cluster Management hub, renders one kube-agentic-networking `XAccessPolicy` per policy and
selected cluster, and delivers it with `ManifestWork`. Each cluster's Envoy gateway enforces identity,
tool and expiry on every request.

### Actors

| Actor | Trust | Isolation |
|---|---|---|
| Hub API server and etcd | trusted | Kubernetes control plane |
| Policy authors (`fleetaccesspolicies` write) | trusted to define ceilings | Kubernetes RBAC |
| Lease requesters (`toolaccessleases` create) | trusted only within a ceiling | Kubernetes RBAC; the API rejects edits after creation |
| fleetpermit-controller | trusted; can read every ManifestWork on the hub (which can include other tools' Secrets) and create and update ManifestWork in every managed-cluster namespace | ServiceAccount with generated RBAC: no access to the Secret, workload or RBAC APIs; optional `--work-executor` makes the work agent check FleetPermit's content against a restricted ServiceAccount before applying it, but a stolen credential can omit it unless the hub enables OCM's `NilExecutorValidating` gate |
| OCM work agent (managed cluster) | trusted to apply delivered content | OCM RBAC; FleetPermit's `work-agent-rbac.yaml` adds `xaccesspolicies` permissions to it |
| kube-agentic-networking controller and Envoy gateway | trusted enforcement point | upstream project |
| Agent workloads (callers) | untrusted | mTLS with SPIFFE X.509 identities; authorized per call |
| MCP tool servers | protected assets | behind the gateway; closed by default when the default-deny anchor is installed |

### Actions

1. A policy author creates a `FleetAccessPolicy`. The API server validates it (schema and CEL).
2. A requester creates a `ToolAccessLease`. The API server validates its schema. The controller then
   evaluates subject membership, tool subset and maximum duration; a violation is recorded as a
   terminal `Denied` condition. It also intersects the requested clusters with the placement. The
   placement never denies a lease: requested clusters outside it are ignored, and a lease with no
   placed cluster waits in `Pending`.
3. The controller renders rules from validated inputs only and delivers one ManifestWork per selected
   cluster. The ManifestWork carries SHA-256 content digests.
4. The work agent applies the `XAccessPolicy`. The enforcement controller accepts it and programs Envoy.
5. A caller connects to the gateway with mTLS. Envoy authenticates the SPIFFE ID and matches the tool
   and `request.time` against the rules. The call is allowed or denied.
6. At expiry the gateway denies by itself, and the controller removes the grant. If no grants remain
   on that cluster, an inert policy that allows nothing stays in place.

### Goals

- A lease never grants more than its policy: identities, tools, clusters and duration are subsets.
- Grants stop at expiry even when the hub or the controller is unavailable.
- With the default-deny anchor installed, errors never add authority: a failure withdraws grants or
  leaves them to expire on time. Without the anchor, upstream enforces nothing on a backend that no
  `XAccessPolicy` targets.
- Every delivered rule is traceable to its source policy and lease.

### Non-goals

- Issuing or storing identities or credentials.
- Argument-level authorization of tool calls (not supported by the upstream API in v0.2.0).
- Protecting against a compromised hub control plane or a malicious managed-cluster administrator.
- Authorization inside tool servers.

## Self-assessment use

This document is for users evaluating FleetPermit and for a future TAG Security review. It is not an
audit.

## Security functions and features

| Critical | Function |
|---|---|
| Lease evaluation (`internal/lease`) | pure function; subset, duration and placement intersection; unit tested at the boundaries |
| Rendering (`internal/enforcement/agenticnetworking`) | strict re-validation of tool names and SPIFFE IDs before building CEL; tested against injection attempts |
| Expiry in the data plane | CEL `request.time` bound evaluated by Envoy per request; tested with the hub paused (S11) |
| API validation | CRD schema and CEL rules; lease spec immutability; `failMode: Closed` only |
| Default-deny anchor | closes backends that would otherwise be open (upstream behaviour, scenario A1) |

| Security relevant | Function |
|---|---|
| RBAC | generated from code markers; no access to the Secret, workload or RBAC APIs on the hub. Cluster-wide ManifestWork access (read every ManifestWork, write in every managed-cluster namespace) is the controller's most powerful permission |
| Drift handling | OCM server-side apply with force, plus immediate re-apply requests when OCM reports drift |
| Observability | Prometheus metrics without identity labels; optional OpenTelemetry traces |

## Project compliance

No compliance standards are claimed.

## Secure development practices

- All commits and release tags are signed and verified on GitHub.
- Release images and assets are signed keylessly with Sigstore cosign; see
  [operations.md](operations.md#verifying-releases).
- CI runs `make verify` (gofmt, vet, generated-code and manifest checks, Helm lint, shell syntax, a
  secret and personal-data scan, license headers), `make vulncheck` (`govulncheck`) and `make test`
  (unit tests with the race detector and envtest integration tests).
- GitHub Actions are pinned to commit SHAs. OpenSSF Scorecard runs on every push to `main`.
- Dependencies are limited to Kubernetes, CNCF and Linux Foundation projects
  ([DEPENDENCIES.md](../DEPENDENCIES.md)). Images are static binaries on `scratch`, running as a
  non-root user.
- A weekly upstream canary validates rendered policies against the latest upstream `XAccessPolicy`
  CRD schema and reports newer upstream releases. It does not run the upstream controller; upgrades
  are validated with the lab end-to-end suite
  ([upstream-compatibility.md](upstream-compatibility.md#keeping-up-with-upstream)).

## Security issue resolution

Reports are received privately through GitHub private vulnerability reporting
([SECURITY.md](../SECURITY.md)). Acknowledgement is within 5 business days, and fixes are released
with a GitHub Security Advisory.

## Appendix: known limitations

See the residual-risk column of the [threat model](threat-model.md): no argument-level matching,
reliance on managed-cluster clocks, early revocation requiring hub reachability, upstream rule
capacity, MCP base protocol methods that are not time-bounded in the data plane, the controller's
ManifestWork permission, lease status that only the controller should be able to write, and the
dependency on how upstream combines Allow policies.
