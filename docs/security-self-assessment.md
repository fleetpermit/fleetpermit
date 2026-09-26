# Security self-assessment

This self-assessment follows the structure of the CNCF TAG Security self-assessment template. It
describes FleetPermit v0.1 as implemented. It has **not** been reviewed by TAG Security or audited by a
third party.

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
Open Cluster Management hub, renders one Kubernetes Agentic Networking `XAccessPolicy` per policy and
selected cluster, and delivers it with `ManifestWork`. Each cluster's Envoy gateway enforces identity,
tool and expiry on every request.

### Actors

| Actor | Trust | Isolation |
|---|---|---|
| Hub API server and etcd | trusted | Kubernetes control plane |
| Policy authors (`fleetaccesspolicies` write) | trusted to define ceilings | Kubernetes RBAC |
| Lease requesters (`toolaccessleases` create) | trusted only within a ceiling | Kubernetes RBAC; the API rejects edits after creation |
| fleetpermit-controller | trusted; can create ManifestWork | ServiceAccount with generated least-privilege RBAC |
| OCM work agent (managed cluster) | trusted to apply delivered content | OCM RBAC; FleetPermit adds `xaccesspolicies` only |
| kube-agentic-networking controller and Envoy gateway | trusted enforcement point | upstream project |
| Agent workloads (callers) | untrusted | mTLS with SPIFFE X.509 identities; authorized per call |
| MCP tool servers | protected assets | behind the gateway; the default-deny anchor closes them |

### Actions

1. A policy author creates a `FleetAccessPolicy`. The API server validates it (schema and CEL).
2. A requester creates a `ToolAccessLease`. The controller evaluates subject membership, tool subset,
   maximum duration and the placement intersection. Violations are recorded as terminal `Denied`
   conditions.
3. The controller renders rules with validated inputs only and delivers one ManifestWork per selected
   cluster. The ManifestWork carries SHA-256 content digests.
4. The work agent applies the `XAccessPolicy`. The enforcement controller accepts it and programs Envoy.
5. A caller connects to the gateway with mTLS. Envoy authenticates the SPIFFE ID and matches the tool
   and `request.time` against the rules. The call is allowed or denied.
6. On expiry the gateway denies by itself, and the controller replaces the grant with an inert policy.

### Goals

- A lease never grants more than its policy: identities, tools, clusters and duration are subsets.
- Grants stop at expiry even when the hub or the controller is unavailable.
- Every error path results in less authority (fail closed).
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
| Rendering (`internal/enforcement/agenticnetworking`) | strict re-validation of tool names and SPIFFE IDs before building CEL; tested for injection attempts |
| Expiry in the data plane | CEL `request.time` bound evaluated by Envoy per request; tested with the hub paused (S11) |
| API validation | CRD schema and CEL rules; lease spec immutability; `failMode: Closed` only |
| Default-deny anchor | closes backends that would otherwise be open (upstream behaviour, scenario A1) |

| Security relevant | Function |
|---|---|
| RBAC | generated from code markers; no secrets, workloads or RBAC permissions |
| Drift handling | OCM server-side apply with force, plus immediate re-apply requests when OCM reports drift |
| Observability | Prometheus metrics without identity labels; optional OpenTelemetry traces |

## Project compliance

No compliance standards are claimed.

## Secure development practices

- All commits and release tags are signed and verified on GitHub.
- CI (`make verify`, `make test`) runs gofmt, vet, `govulncheck`, a secret and personal-data scan,
  generated-code checks, unit tests with the race detector and envtest integration tests.
- GitHub Actions are pinned to commit SHAs. OpenSSF Scorecard runs on every push to `main`.
- Dependencies are limited to Kubernetes, CNCF and Linux Foundation projects
  ([DEPENDENCIES.md](../DEPENDENCIES.md)). Images are static binaries on `scratch`, running as a non-root user.
- A scheduled canary tests against the latest upstream releases.

## Security issue resolution

Reports are received privately through GitHub private vulnerability reporting
([SECURITY.md](../SECURITY.md)). Acknowledgement is within 5 business days, and fixes are released
with a GitHub Security Advisory.

## Appendix: known limitations

See the residual-risk column of the [threat model](threat-model.md): no argument-level matching,
reliance on managed-cluster clocks, early revocation requiring hub reachability, upstream rule
capacity, and MCP session methods that are not time-bounded in the data plane.
