# Operations

## Prerequisites

| Where | What |
|---|---|
| Hub | Kubernetes ≥ 1.30 with the Open Cluster Management hub (v1.3 tested): `Placement`, `PlacementDecision`, `ManifestWork` APIs |
| Each governed managed cluster | OCM klusterlet; Gateway API; kube-agentic-networking (v0.2.0 tested) with a `Gateway` of class `kube-agentic-networking`; the tool server as an `XBackend`; workload identities the gateway trusts |

## Install

```sh
# hub
helm install fleetpermit charts/fleetpermit -n fleetpermit-system --create-namespace

# every managed cluster that hosts a governed tool server
kubectl apply -f config/managed-cluster/work-agent-rbac.yaml
kubectl apply -f config/managed-cluster/default-deny-anchor.yaml   # set namespace and backend name
```

The anchor must exist on **every** cluster that runs the backend, including clusters that no
placement currently selects. Without it, upstream enforces nothing on that backend
([ADR-3](design.md#adr-3-default-deny-anchor)).

### Helm values

| Value | Default | Purpose |
|---|---|---|
| `image.registry` / `image.repository` / `image.tag` | `ghcr.io` / `fleetpermit/fleetpermit-controller` / chart appVersion | image location; set `registry: ""` for a fully qualified repository |
| `image.pullPolicy`, `imagePullSecrets` | `IfNotPresent`, `[]` | |
| `replicas` | `1` | more replicas are safe with leader election |
| `leaderElection.enabled` | `true` | |
| `watchNamespace` | `""` | restrict FleetPermit objects, placements and decisions to one namespace |
| `workExecutor` | `""` | `namespace/name` of a managed-cluster ServiceAccount that the OCM work agent applies content as |
| `metrics.enabled` / `metrics.port` / `metrics.scrapeAnnotations` | `true` / `8080` / `true` | Prometheus endpoint and `prometheus.io/*` annotations |
| `tracing.otlpEndpoint` | `""` | enables OpenTelemetry trace export over OTLP/HTTP |
| `rbac.create`, `serviceAccount.*` | `true` | |
| `resources`, `podSecurityContext`, `securityContext`, `nodeSelector`, `tolerations`, `affinity` | hardened defaults | non-root, read-only root filesystem, all capabilities dropped |

### Building and publishing images elsewhere

```sh
make images IMAGE_REGISTRY=registry.example.com/platform IMAGE_TAG=v0.1.0
podman push registry.example.com/platform/fleetpermit-controller:v0.1.0
helm install fleetpermit charts/fleetpermit -n fleetpermit-system --create-namespace \
  --set image.registry=registry.example.com --set image.repository=platform/fleetpermit-controller --set image.tag=v0.1.0
```

Images are static Go binaries on `scratch`, run as UID 65532, and contain only the binary and CA
certificates.

## Verifying releases

From v0.1.0 on, release images and assets are signed keylessly with Sigstore cosign by the release
workflow. The workflow's GitHub OIDC identity is recorded in the public Rekor transparency log.

```sh
# an image (use the digest from images.txt attached to the release)
cosign verify ghcr.io/fleetpermit/fleetpermit-controller@sha256:<digest> \
  --certificate-identity-regexp '^https://github.com/fleetpermit/fleetpermit/.github/workflows/(release|sign-release).yaml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# a release asset (the Helm chart, the SBOM)
cosign verify-blob fleetpermit-0.1.0.tgz --bundle fleetpermit-0.1.0.tgz.sigstore.json \
  --certificate-identity-regexp '^https://github.com/fleetpermit/fleetpermit/.github/workflows/(release|sign-release).yaml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Release tags and every commit are also signed and show as Verified on GitHub.

## RBAC

Generated from the `+kubebuilder:rbac` markers in `internal/controller` ([`config/rbac/role.yaml`](../config/rbac/role.yaml)):

| API group | Resources | Verbs | Why |
|---|---|---|---|
| `fleetpermit.github.io` | fleetaccesspolicies, toolaccessleases | get, list, watch, update, patch | reconcile; add and remove the cleanup finalizer |
| `fleetpermit.github.io` | */status | get, update, patch | report status |
| `fleetpermit.github.io` | fleetaccesspolicies/finalizers | update | set the cleanup finalizer where the `OwnerReferencesPermissionEnforcement` admission plugin is enabled |
| `cluster.open-cluster-management.io` | placements, placementdecisions, managedclusters | get, list, watch | resolve placements; report unavailable clusters |
| `work.open-cluster-management.io` | manifestworks | get, list, watch, create, update, patch, delete | deliver and withdraw grants |
| `coordination.k8s.io` (release namespace) | leases | get, list, watch, create, update, patch, delete | leader election |
| core, `events.k8s.io` (release namespace) | events | create, patch | events |

The controller cannot read secrets, create workloads or modify RBAC. The e2e `RBAC` scenario checks this
with `kubectl auth can-i`. On managed clusters, [`work-agent-rbac.yaml`](../config/managed-cluster/work-agent-rbac.yaml)
extends the OCM work agent with `xaccesspolicies` only.

## Metrics

Served at `:8080/metrics`. No metric uses identities, lease names or cluster names as labels.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `fleetpermit_reconcile_total` | counter | `result` = success, error | policy reconciliations |
| `fleetpermit_reconcile_errors_total` | counter | — | reconciliations that returned an error |
| `fleetpermit_active_leases` | gauge | — | leases granting on at least one cluster |
| `fleetpermit_expired_leases_total` | counter | — | leases that transitioned to Expired |
| `fleetpermit_denied_leases_total` | counter | `reason` | leases that transitioned to Denied |
| `fleetpermit_authorized_clusters` | gauge | — | (policy, cluster) pairs holding grants |
| `fleetpermit_policy_propagation_seconds` | histogram | — | lease creation → Ready on every target cluster |
| `fleetpermit_lease_revocation_seconds` | histogram | — | expiry or denial → grants withdrawn everywhere (cleanup; the gateway denies at expiry) |
| `fleetpermit_placement_changes_total` | counter | — | observed changes to a policy's selected clusters |

Useful alerts:
- `increase(fleetpermit_reconcile_errors_total[10m]) > 0`
- `fleetpermit_active_leases > 0` for longer than your longest `maxDuration` (leases should come and go)
- `histogram_quantile(0.95, rate(fleetpermit_policy_propagation_seconds_bucket[1h])) > 60`

controller-runtime's standard workqueue and REST client metrics are also exported.

## Tracing

Set `tracing.otlpEndpoint` (or `OTEL_EXPORTER_OTLP_ENDPOINT`). Spans: `fleetpermit.reconcile.policy`
(attribute `fleetpermit.policy`) and `fleetpermit.render.cluster` (attributes `fleetpermit.cluster`,
`fleetpermit.grants`, `fleetpermit.content_digest`).

## Day-2 operations

- **Revoke early:** `kubectl delete toolaccesslease <name>`. The grant is withdrawn from reachable
  clusters within about a second in the lab. Clusters that cannot be reached keep it until its expiry.
- **Emergency stop for a policy:** delete the policy, or its placement. Every grant is withdrawn and
  every lease is denied.
- **Upgrade FleetPermit:** `helm upgrade`. State lives in the API, and a restart neither withdraws nor
  re-creates grants.
- **Upgrade upstream components:** see [upstream-compatibility.md](upstream-compatibility.md). Run
  `make test-integration` with the new upstream CRD in `test/fixtures/upstream` and the e2e suite in the
  lab before rolling out.
- **OCM status sync interval:** the klusterlet's `workConfiguration.statusSyncInterval` controls how
  quickly acceptance and drift are *reported* to the hub. It affects status and drift repair, not
  enforcement. The lab sets 10s.
- **Uninstall:** delete all `FleetAccessPolicy` objects first so their finalizers withdraw every grant,
  then `helm uninstall fleetpermit`.
