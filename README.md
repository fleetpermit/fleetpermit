<p align="center">
  <img src="docs/assets/logo.svg" alt="FleetPermit" width="360">
</p>

<h3 align="center">Least privilege for agents, across every cluster.</h3>

<p align="center">Define who can call which tool, on which clusters, and for how long.</p>

<p align="center">
  <a href="https://github.com/fleetpermit/fleetpermit/actions/workflows/ci.yaml"><img alt="CI" src="https://github.com/fleetpermit/fleetpermit/actions/workflows/ci.yaml/badge.svg"></a>
  <a href="https://github.com/fleetpermit/fleetpermit/actions/workflows/e2e.yaml"><img alt="E2E: 1 hub + 3 clusters" src="https://github.com/fleetpermit/fleetpermit/actions/workflows/e2e.yaml/badge.svg"></a>
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue"></a>
  <a href="docs/api.md"><img alt="API: v1alpha1" src="https://img.shields.io/badge/API-v1alpha1-orange"></a>
  <a href="https://fleetpermit.github.io/"><img alt="Website" src="https://img.shields.io/badge/docs-fleetpermit.github.io-0e7490"></a>
</p>

<p align="center">
  <strong>Star us&nbsp;→</strong>&nbsp;<a href="https://github.com/fleetpermit/fleetpermit" title="Star FleetPermit on GitHub"><picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/star-dark.svg">
    <img src="docs/assets/star-light.svg" alt="Star FleetPermit on GitHub" width="160" height="36" align="middle">
  </picture></a>
</p>

<p align="center">
  <img src="docs/assets/readme-hero.svg" alt="Animated overview: a permit travels from FleetPermit to cluster-east and cluster-west, where sre-agent's calls are allowed; a call from security-agent and any call to cluster-edge are denied; when the lease countdown runs out the permits dissolve and the same call is denied, until a new lease is issued." width="880">
</p>

FleetPermit gives AI agents time-bound, fleet-wide permission to call tools. A platform team writes
one policy that names the workload identities, the MCP tools they may call and the clusters where
they may call them. A short-lived lease activates part of that policy. FleetPermit delivers the lease
to the clusters an Open Cluster Management placement selects, and the gateway on each cluster
enforces it. The lease expires on time even when the fleet hub is unreachable.

An agent here is a workload with a [SPIFFE](https://spiffe.io/) identity that makes a standard MCP
tool call. FleetPermit does not depend on any model, agent framework, cloud or vendor.

## FleetPermit in five answers

### What is FleetPermit?

*Least privilege for agents, across every cluster.* One policy defines which workload identities may
call which MCP tools, on which clusters, and for how long at most. Short-lived leases activate part
of it. See the [architecture](docs/architecture.md).

### What is new?

Authorization for agent-to-tool calls that is both fleet-wide and time-bound. Open Cluster Management
places one policy across a changing fleet, and kube-agentic-networking enforces it on every
call. Each lease's expiry is written into the gateway's own rule, so it holds even when the hub is
unreachable. FleetPermit does not serve tools itself; it governs calls to the MCP servers you already
run. See the [design decisions](docs/design.md).

### What works today?

The whole path runs on real clusters: an OCM `Placement`, then a FleetPermit policy and lease, then a
kube-agentic-networking `XAccessPolicy` on Envoy, then an MCP ALLOW or DENY. The lab has one hub and
three managed clusters, and `make demo-up` builds it on one machine
([prerequisites](#prerequisites), [see it run](#see-it-run)).

### What evidence exists?

End-to-end scenarios on real clusters cover lease activation and expiry, denial of the wrong identity
or the wrong tool, placement changes, drift repair, and hub disconnect and reconnect. The evidence also
includes a 48-call decision matrix (agent × cluster × tool), measured propagation and revocation
latency, the upstream conformance suite, and the same scenarios reproduced on a GitHub-hosted runner
(Linux amd64). See the [measured results](#measured-results).

### Why is it reusable?

It has no vendor, cloud, distribution or model lock-in. It builds only on Kubernetes, CNCF and Linux
Foundation projects and is licensed Apache-2.0. Placement and enforcement sit behind provider seams.
Every upstream component version is pinned, and a weekly canary validates rendered policies against the latest
upstream `XAccessPolicy` schema and reports newer releases. See the [dependencies](DEPENDENCIES.md).

## See it run

<p align="center">
  <img src="docs/assets/demo-overview.gif" alt="Recording of a real run of make demo-run: the fleet status shows cluster-east and cluster-west selected and cluster-edge not selected; without a lease restart_workload is DENIED; a 40 second lease is created and becomes Active on 2 clusters; restart_workload is ALLOWED on east and west; the same call on cluster-edge, read_secret, and a call from security-agent are DENIED; the CEL rule with the expiry time is shown; after the countdown the call is DENIED and the lease is Expired." width="880">
</p>

A recording of `make demo-run` against the lab, sped up 1.3×. Every ALLOWED and DENIED line is a real
MCP call through a real gateway. The `$` command lines are shortened for readability
([how the recordings are made](demo/recordings)). Full-length MP4s at normal speed:
[overview](https://fleetpermit.github.io/assets/video/demo-overview.mp4) ·
[security checks](https://fleetpermit.github.io/assets/video/demo-security.mp4) ·
[hub disconnected](https://fleetpermit.github.io/assets/video/demo-disconnected-expiry.mp4).
Plain-text transcripts of the same runs:
[overview](demo/recordings/demo-overview.txt) ·
[security checks](demo/recordings/demo-security.txt) ·
[hub disconnected](demo/recordings/demo-disconnected-expiry.txt).

## Why FleetPermit

Authorization for agent tool calls is usually configured one cluster at a time. Fleets change:
clusters join and leave, and their labels change. The tool access an incident needs should last
minutes. Kubernetes SIG Network's [kube-agentic-networking](https://github.com/kubernetes-sigs/kube-agentic-networking)
project defines authorization for agent-to-tool traffic on one cluster, down to individual MCP tools.
[Open Cluster Management](https://open-cluster-management.io/) decides which clusters something
belongs on and delivers it there. FleetPermit combines the two:

| Question | Answered by | FleetPermit field |
|---|---|---|
| **Who?** | a SPIFFE ID, authenticated with mTLS at the gateway | `spec.subjects` (policy), `spec.subject` (lease) |
| **Where?** | an OCM `Placement` and its `PlacementDecision`s | `spec.placement.placementRef` (policy), `spec.clusters` (lease, optional) |
| **What?** | MCP tool names, matched by the gateway | `spec.permissions` (policy and lease) |
| **How long?** | a lease whose expiry is part of the enforced rule | `spec.lease.maxDuration` (policy), `spec.duration` (lease) |

## How it works

One lease, followed from request to expiry (animated, about 35 seconds per loop):

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/architecture-flow-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="docs/assets/architecture-flow-light.svg">
    <img src="docs/assets/architecture-flow-light.svg" width="100%" alt="Animated walkthrough of one lease, in ten steps: 1. sre-agent has a SPIFFE identity from Kubernetes Pod Certificates. 2. An on-call engineer creates ToolAccessLease incident-42 for restart_workload, 10 minutes. 3. FleetPermit on the Open Cluster Management hub checks WHO, WHAT and HOW LONG against FleetAccessPolicy sre-remediation; all three pass. 4. Then WHERE: the Placement selects env=production, which is cluster-east and cluster-west, not cluster-edge. 5. FleetPermit renders an XAccessPolicy with a CEL time bound and delivers it with one ManifestWork per cluster. 6. sre-agent calls restart_workload on cluster-east through the Envoy gateway: ALLOW. 7. cluster-edge has no grant: DENY; the policy does not permit read_secret: DENY. 8. Ten minutes pass and the lease reaches its expiry, 10:15:00Z. 9. Envoy itself denies the same call because request.time is past the expiry, even with the hub down. 10. FleetPermit marks the lease Expired and withdraws the grant.">
  </picture>
</p>

The components involved, and every flow animated step by step, are on the
[website's architecture page](https://fleetpermit.github.io/architecture.html#flows).

<p align="center"><img src="docs/assets/architecture.svg" alt="Architecture: the fleetpermit-controller on the Open Cluster Management hub renders XAccessPolicy grants and delivers them with ManifestWork to the clusters selected by a Placement; on each managed cluster the kube-agentic-networking Envoy gateway authenticates the agent's SPIFFE identity and enforces the tool and time bound." width="880"></p>

1. A platform team writes a `FleetAccessPolicy`. It is the ceiling: the most that may ever be granted.
2. A person or an automation creates a `ToolAccessLease` for a subset of that authority, for example
   `restart_workload` for 10 minutes.
3. The fleetpermit-controller on the OCM hub checks the lease. The subject must be in the policy, the
   tools must be a subset of its permissions, and the duration must not exceed its maximum. The
   controller then resolves the placement to a set of clusters. A lease can narrow the placement but
   never widen it.
4. For every selected cluster, the controller renders one upstream `XAccessPolicy` in which each lease
   is a CEL rule, stamps it with a SHA-256 content digest, and delivers it with an OCM `ManifestWork`:
   ```
   request.mcp.tool_name in ['restart_workload'] && request.time < timestamp('2026-09-26T10:15:00Z')
   ```
5. The OCM work agent applies it, and the kube-agentic-networking controller programs Envoy. A tool call is
   allowed only if the caller's mTLS identity, the tool and the current time all match. At the expiry
   instant the gateway starts denying by itself. It does not need the hub, FleetPermit or the network
   between them. FleetPermit then withdraws the expired grant.

FleetPermit is one controller and two CRDs. It runs no component on managed clusters, because the
time bound travels inside the policy the gateway already enforces. The
[design decisions](docs/design.md) explain why that is enough.

## 30-second example

```yaml
apiVersion: fleetpermit.github.io/v1alpha1
kind: FleetAccessPolicy
metadata: {name: sre-remediation, namespace: fleet}
spec:
  subjects:
    - spiffeID: spiffe://cluster.local/ns/agents/sa/sre-agent
  placement:
    placementRef: {name: production-clusters}     # an OCM Placement
  target:
    namespace: mcp-tools
    ref: {kind: XBackend, name: fleet-tools}      # the MCP server behind the gateway
  permissions:
    - tool: get_cluster_health
    - tool: restart_workload
    - tool: scale_workload
  lease: {required: true, defaultDuration: 15m, maxDuration: 30m}
---
apiVersion: fleetpermit.github.io/v1alpha1
kind: ToolAccessLease
metadata: {name: incident-42, namespace: fleet}
spec:
  policyRef: {name: sre-remediation}
  subject: {spiffeID: spiffe://cluster.local/ns/agents/sa/sre-agent}
  permissions: [{tool: restart_workload}]
  duration: 10m
  reason: "INC-42: checkout pods crash-looping"
```

```console
$ kubectl get fleetaccesspolicy -n fleet
NAME              CLUSTERS   READY   ACTIVE-LEASES   AGE
sre-remediation   2/2        True    1               10m

$ kubectl get toolaccesslease -n fleet
NAME          POLICY            CLUSTERS   EXPIRES-AT             STATUS   AGE
incident-42   sre-remediation   2          2026-09-26T10:15:00Z   Active   1m
```

A lease is `Denied`, with the reason in its conditions, if it asks for a tool the policy does not
list, for longer than `maxDuration`, or for a subject the policy does not name. A lease created before its
policy waits in `Pending` for up to 5 minutes, so the apply order does not matter within that time;
if the policy does not appear by then, the lease is denied, and it expires instead if its own
duration ends first. A lease whose policy is deleted after it was evaluated is denied, or `Expired` if its expiry
had already passed. A lease belongs to the policy that first evaluated it, so a policy deleted and
created again under the same name does not grant it. Provided only the controller can write
`toolaccessleases/status`, denied and expired leases never become active again. A lease's spec cannot
be edited after creation.

## Security properties

- With the default-deny anchor installed, errors never add authority: a missing placement, a
  rendering error or a denied lease withdraws grants, and every lease expires on time. `failMode`
  accepts only `Closed`.
- On a cluster that FleetPermit does not currently select, or has withdrawn from, the backend is
  closed only when the shipped [default-deny anchor](config/managed-cluster/default-deny-anchor.yaml)
  is installed; on selected clusters FleetPermit's inert policy also denies. Upstream enforces nothing
  on a target that has no `XAccessPolicy` ([scenario A1](docs/results.md)).
- Expiry is enforced where the call happens. Envoy evaluates the time bound on every request, so a
  lease stops working on time even with the hub disconnected ([scenario S11](docs/results.md)).
- Leases can narrow tools, duration and clusters, never widen them. Tool names are restricted to a
  character set that cannot alter a CEL expression.
- Every delivered object carries the source policy and its UID, the cluster and a deterministic
  SHA-256 content digest. Objects with lease grants also list the lease UIDs and the
  latest expiry, so a rule on a cluster can be traced back to its request.
- On the hub, the controller reads OCM placement and cluster APIs and ManifestWorks, and writes only
  `ManifestWork` and its own objects. It has no access to the Secret, workload or RBAC APIs. Its
  `ManifestWork` permissions cover every managed-cluster namespace, though: it can read content other
  tools deliver through `ManifestWork` (which can include Secrets) and can create and update
  `ManifestWork` that the OCM work agent applies, so treat its ServiceAccount as a privileged
  credential. The `--work-executor` flag (Helm value `workExecutor`) makes the work agent check
  FleetPermit's content against a restricted managed-cluster ServiceAccount before applying it. It
  does not stop a stolen controller credential on its own, and the lab tests do not exercise it; see
  [RBAC](docs/operations.md#rbac).

<p align="center">
  <img src="docs/assets/demo-disconnected-expiry.gif" alt="Recording of the hub-disconnect demo: a 40 second lease works on cluster-east and cluster-west; the hub node is paused and kubectl to the hub fails; the rule on cluster-east still shows the CEL time bound; after expiry both clusters DENY while the grant object is still present; the hub is reconnected, the lease shows Expired and the policy's only rule is no-active-grants." width="880">
</p>

The recording above pauses the whole hub node before the lease expires. Both managed clusters stop
honouring the lease on time, and the fleet converges when the hub returns.

These properties rest on stated trust assumptions. Read the [security model](docs/security-model.md)
and the [threat model](docs/threat-model.md), including the limitations they list.

## Demo

The lab is reproducible on one machine: one OCM hub and three managed [kind](https://kind.sigs.k8s.io/)
clusters. Each managed cluster runs the kube-agentic-networking reference gateway and a deterministic
MCP tool server. The two agent identities run as pods on cluster-east and call all three gateways. No
API key for any AI model is needed. Every decision in the demo is a real MCP call through a real
gateway.

### Prerequisites

podman (or docker), kind, kubectl, clusteradm, Helm, jq, curl, Go and Python 3. Give the container
engine about 8 CPUs and 16 GiB of memory; with podman, set this on the podman machine.

### Run the lab

```sh
git clone https://github.com/fleetpermit/fleetpermit && cd fleetpermit
make demo-up     # ~10 minutes: clusters, OCM, Gateway API, kube-agentic-networking, FleetPermit
make demo-run    # narrated walkthrough (also: demo/run.sh security | disconnect)
make test-e2e    # every end-to-end scenario (listed in docs/testing.md); rewrites the committed evidence in test-results/
make demo-down   # removes only the clusters this lab created
```

### Create your own lease in the lab

After `make demo-run`, the lab has the demo policy `sre-remediation` (maximum 10 minutes) and the
Placement `production-clusters`, which selects cluster-east and cluster-west. The demo leaves its
lease `incident-42` in phase `Expired`, and a lease's spec cannot change, so delete it before you
create the sample lease with the same name. The lab has its own kubeconfig; your current kubectl
context is not used.

```sh
export KUBECONFIG=$PWD/.work/lab/kubeconfig
kubectl --context kind-fleetpermit-hub -n fleet delete toolaccesslease incident-42 --ignore-not-found
kubectl --context kind-fleetpermit-hub create -f config/samples/toolaccesslease.yaml
kubectl --context kind-fleetpermit-hub -n fleet get fap,tal

# call restart_workload as sre-agent (a pod on cluster-east) through cluster-west's gateway
GW=$(kubectl --context kind-fleetpermit-cluster-west -n mcp-tools get gateway agentic-gateway \
  -o jsonpath='{.status.addresses[0].value}')
kubectl --context kind-fleetpermit-cluster-east -n agents exec deploy/sre-agent -c agent -- \
  /demo-probe -url "https://$GW:10001/mcp" -tool restart_workload -args '{"namespace":"shop","name":"checkout"}'
```

The probe prints one JSON line with the decision. Use `deploy/security-agent` to see the denial for
an unlisted identity, or `-tool read_secret` for a tool the policy does not permit.

### The two test agents

The demo and the tests use two agents, `sre-agent` and `security-agent`. Both are test clients from
this repository. They are not third-party agents or AI models, and they have no repositories of their
own. Both are ordinary pods that the lab creates in the `agents` namespace on `cluster-east`, and both
run the same small test client ([`demo/tools/probe`](demo/tools/probe/main.go)). Each run of the
client makes one MCP call (`initialize`, then `tools/call`) over mTLS to one cluster's gateway and
records the answer. The two agents differ only in their identity.

| Agent | Identity (SPIFFE ID from Kubernetes Pod Certificates) | Role in the demo policy | Expected result |
|---|---|---|---|
| `sre-agent` | `spiffe://cluster.local/ns/agents/sa/sre-agent` | listed as a subject of `sre-remediation`, so it can hold leases | ALLOW only on cluster-east and cluster-west, only for the leased tools, and only while the lease is active; DENY otherwise |
| `security-agent` | `spiffe://cluster.local/ns/agents/sa/security-agent` | trusted by the gateways but listed in no policy | DENY for every call, on every cluster |

Where to look:
- [`demo/tools/probe/main.go`](demo/tools/probe/main.go): the test client both agents run
- [`demo/scripts/render-agents.sh`](demo/scripts/render-agents.sh): how the two pods and their identities are created
- [`demo/tools/mcp-server/main.go`](demo/tools/mcp-server/main.go): the MCP tool server they call
- [`test/e2e/run.sh`](test/e2e/run.sh): the scenarios that drive them, including the 48-call decision matrix
- [`config/samples/fleetaccesspolicy.yaml`](config/samples/fleetaccesspolicy.yaml): a policy that lists `sre-agent`

#### Using your own agent

Any agent works the same way, whatever its framework or language. FleetPermit never sees the agent's
code. The agent needs three things:
1. a SPIFFE X.509 identity that the gateways trust. In the lab the cluster issues it. Across
   organisations, use a federated trust domain, for example with SPIRE.
2. network access to a cluster's gateway
3. a listing as a subject in a `FleetAccessPolicy`, and an active lease

An agent without a trusted certificate is rejected during the TLS handshake, before any MCP message
is read. That is upstream behaviour (the gateway listener requires a client certificate) and is not
covered by a FleetPermit test. An agent with a trusted but unlisted identity is denied, in the same way as `security-agent`.
The upstream [quickstart](https://github.com/kubernetes-sigs/kube-agentic-networking/tree/v0.2.0/site-src/guides/quickstart)
("Bring your own agent") shows how to give an existing agent an identity and route it through the gateway.

Recordings of real runs: [demo/recordings](demo/recordings) (asciinema casts and plain-text transcripts) and MP4s on the [website](https://fleetpermit.github.io/demo.html).

## Measured results

<p align="center">
  <img src="docs/assets/decision-matrix.svg" alt="Expected decisions for 2 agents, 3 clusters and 4 tools: with the lease active, only sre-agent calling get_cluster_health or restart_workload on cluster-east or cluster-west is allowed; with the lease expired every call is denied." width="880">
</p>

The diagram shows the *expected* decisions under the demo policy. The table below shows what the 24
calls made while the lease was active returned through the lab's gateways; the same 24 calls after
expiry are summarized below it (48 in all).

<!-- results:start -->
Real multi-cluster run (1 hub + 3 managed kind clusters, Kubernetes v1.35.0, OCM v1.3.1, kube-agentic-networking v0.2.0, Darwin/arm64, 2026-09-27):

- **20 of 21 scenarios passed**, 0 failed, 1 not supported by the upstream API (argument-level matching).
- **Reproduced** at commit 52d8bf7 on GitHub Actions ubuntu-latest (Linux/x86_64, docker): 20 passed, 0 failed, 1 unsupported ([run logs](https://github.com/fleetpermit/fleetpermit/actions/runs/36252665892)).
- **Decision matrix: 48 of 48 real MCP calls matched the expected outcome.**

#### Test agents and expected outcomes

Two workload identities make every call. Both are test clients from this repository, not third-party or AI agents ([what they are](#the-two-test-agents)). Their SPIFFE X.509 certificates come from Kubernetes Pod Certificates:

- **`sre-agent`** (`spiffe://cluster.local/ns/agents/sa/sre-agent`): listed as a subject of policy sre-remediation; receives leases
- **`security-agent`** (`spiffe://cluster.local/ns/agents/sa/security-agent`): not listed in any policy; every call must be denied

The lease grants `get_cluster_health` and `restart_workload` to `sre-agent`, on the clusters the placement selects (cluster-east and cluster-west). Each cell below is a real call through that cluster's gateway, showing the observed decision (✅/⛔ = matched the expectation, ❌ = did not):

**Lease active**

| Agent | Cluster | get_cluster_health | restart_workload | scale_workload | read_secret |
|---|---|---|---|---|---|
| `sre-agent` | cluster-east | ✅ ALLOW | ✅ ALLOW | ⛔ DENY | ⛔ DENY |
| `sre-agent` | cluster-west | ✅ ALLOW | ✅ ALLOW | ⛔ DENY | ⛔ DENY |
| `sre-agent` | cluster-edge | ⛔ DENY | ⛔ DENY | ⛔ DENY | ⛔ DENY |
| `security-agent` | cluster-east | ⛔ DENY | ⛔ DENY | ⛔ DENY | ⛔ DENY |
| `security-agent` | cluster-west | ⛔ DENY | ⛔ DENY | ⛔ DENY | ⛔ DENY |
| `security-agent` | cluster-edge | ⛔ DENY | ⛔ DENY | ⛔ DENY | ⛔ DENY |

After the lease expires, the same 24 calls are repeated. Expected: all DENY. Observed: all 24 DENY, as expected ([full table](docs/results.md#decision-matrix)).

| Measured on real clusters | n | p50 | p95 |
|---|---|---|---|
| Lease created → first ALLOW at the gateway | 18 | 204 ms | 635 ms |
| Lease deleted → first DENY at the gateway | 18 | 197 ms | 601 ms |
| Lease expiry → first DENY (hub connected) | 4 | 156 ms | 398 ms |
| Lease expiry → first DENY (hub disconnected) | 2 | 19 ms | 19 ms |
| Rendered policy deleted on a cluster → object restored | 4 | 9925 ms | 12957 ms |
| Rendered policy deleted on a cluster → calls allowed again | 1 | 9730 ms | 9730 ms |
| Lease created → lease reports Ready (includes OCM status sync) | 9 | 416 ms | 853 ms |
| Host-to-pod probe round trip (measurement baseline) | 10 | 121 ms | 131 ms |

Simulated controller scale (envtest, no real clusters): a lease reached 100 logical clusters' ManifestWorks in 161 ms and was withdrawn in 218 ms.

Upstream conformance, run unmodified against a lab cluster: kube-agentic-networking conformance v0.2.0: **PASS: 6 passed, 0 failed, 3 skipped**.

Statement coverage of `internal/` (unit + integration): **93.5%**.
<!-- results:end -->

Full tables, raw evidence and methodology are in [docs/results.md](docs/results.md). All numbers come
from local kind clusters on one development host, except the GitHub-hosted reproduction and the
envtest scale simulation, and are not a production benchmark.

## Supported upstream versions

| Component | Version tested | Status | Used for |
|---|---|---|---|
| Kubernetes | v1.35 (kind node image) | stable | everything |
| Open Cluster Management | v1.3.1 | CNCF Sandbox project | `Placement`, `PlacementDecision`, `ManifestWork` |
| kube-agentic-networking | v0.2.0 | Kubernetes SIG Network subproject; **experimental API** | `XAccessPolicy` (v1alpha1), `XBackend` (v0alpha0) |
| Gateway API | v1.5.1 | Kubernetes SIG Network, GA | `Gateway`, `HTTPRoute` |
| Envoy | v1.36.10 | CNCF Graduated | data plane (via the reference implementation) |
| Model Context Protocol | 2025-06-18 client handshake | Linux Foundation (Agentic AI Foundation) | tool protocol |

The lab pins Envoy by tag and digest. It moved from v1.36.6 to v1.36.10 for the 1.36 security fixes.
[docs/results.md](docs/results.md) records the versions the published results were measured with.

The upstream kube-agentic-networking APIs are experimental and will change. FleetPermit pins the versions
above and runs its end-to-end suite against them. A weekly canary validates rendered policies against
the latest upstream `XAccessPolicy` CRD schema and reports newer upstream releases; it does not run
the upstream controller. Details: [docs/upstream-compatibility.md](docs/upstream-compatibility.md).

## Installation

Install from a release: check out the release tag (`git checkout v0.1.1`), or download the signed
chart attached to the release ([verifying releases](docs/operations.md#verifying-releases)). A `main`
checkout between releases does not match any published image.

On the Open Cluster Management hub:

```sh
helm install fleetpermit charts/fleetpermit -n fleetpermit-system --create-namespace
```

On every managed cluster that hosts a governed tool server (in addition to kube-agentic-networking):

```sh
kubectl apply -f config/managed-cluster/work-agent-rbac.yaml      # adds XAccessPolicy permissions to the OCM work agent
kubectl apply -f config/managed-cluster/default-deny-anchor.yaml  # edit namespace and targetRefs first
```

The anchor must target the same resource as your policies' `spec.target.ref`: the shipped file
targets an `XBackend`, and for policies that target a `Gateway` its `targetRefs` must point at that
Gateway ([details](docs/operations.md#install)).

A policy references an OCM `Placement` in its own namespace, and OCM only lets that Placement select
clusters when a `ManagedClusterSetBinding` binds a cluster set to the namespace. On the hub, for
example:

```sh
kubectl label managedcluster cluster-east cluster.open-cluster-management.io/clusterset=fleet env=production
kubectl apply -f - <<'YAML'
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSet
metadata: {name: fleet}
spec: {clusterSelector: {selectorType: ExclusiveClusterSetLabel}}
---
apiVersion: v1
kind: Namespace
metadata: {name: fleet}
---
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSetBinding
metadata: {name: fleet, namespace: fleet}
spec: {clusterSet: fleet}
YAML
```

[`config/samples`](config/samples) has a Placement (selecting `env=production`), a policy and a lease
to start from.

Images are published to `ghcr.io/fleetpermit`, and release images and assets are signed with
Sigstore cosign ([verifying releases](docs/operations.md#verifying-releases)). Every chart value
(registry, repository, tag, resources, replicas, metrics, RBAC, leader election) is configurable, so
you can rebuild with `make images` and publish anywhere. See [docs/operations.md](docs/operations.md).

## Development

```sh
make help               # every target
make verify             # gofmt, vet, generated code, manifests, Helm lint, shell syntax, secret scan, license headers
make test               # unit (race detector) + integration (real kube-apiserver via envtest)
make benchmark          # controller scale simulation, plus the real-cluster benchmark when the lab is up
```

CI's build and test steps call these `make` targets; release publishing lives in the release
workflow and `hack/` scripts. See [docs/testing.md](docs/testing.md) and
[CONTRIBUTING.md](CONTRIBUTING.md).

## Roadmap

v0.1 covers one placement provider (Open Cluster Management), one enforcement provider
(kube-agentic-networking) and one protocol (MCP). [ROADMAP.md](ROADMAP.md) lists the enhancements
under consideration, which follow user feedback and upstream progress, and what is out of scope.

## Contributing, security and license

Contributions are welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md) and [GOVERNANCE.md](GOVERNANCE.md).
Report vulnerabilities privately through GitHub, as described in [SECURITY.md](SECURITY.md).
FleetPermit is licensed under [Apache-2.0](LICENSE).

FleetPermit is an independent open-source project. It is built on open technologies from the
Kubernetes, CNCF and Linux Foundation ecosystems, and is not affiliated with or endorsed by them.
