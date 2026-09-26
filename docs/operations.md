# Operations

## Prerequisites

| Where | What |
|---|---|
| Hub | Kubernetes with the Open Cluster Management hub (v1.3 tested): `Placement`, `PlacementDecision`, `ManifestWork` APIs. Tested on Kubernetes 1.35; the chart accepts 1.30 and later, which is untested. |
| Each governed managed cluster | OCM klusterlet; Gateway API; kube-agentic-networking (v0.2.0 tested) with a `Gateway` of class `kube-agentic-networking`; the tool server as an `XBackend`; workload identities the gateway trusts |

## Install

Install from a release: check out the release tag (`git checkout v0.1.1`) and use `charts/fleetpermit`,
or download the signed chart attached to the release and verify it first
([verifying releases](#verifying-releases)).

```sh
# hub
helm install fleetpermit charts/fleetpermit -n fleetpermit-system --create-namespace
kubectl -n fleetpermit-system rollout status deploy/fleetpermit-controller

# every managed cluster that hosts a governed tool server
kubectl apply -f config/managed-cluster/work-agent-rbac.yaml
kubectl apply -f config/managed-cluster/default-deny-anchor.yaml   # set namespace and backend name
```

The anchor must exist on every cluster that runs the backend, including clusters that no placement
currently selects. Without it, upstream enforces nothing on that backend
([ADR-3](design.md#adr-3-default-deny-anchor)).

A policy references an OCM `Placement` in its own namespace. OCM lets that Placement select clusters
only when a `ManagedClusterSetBinding` binds a cluster set to the namespace.
[`config/samples`](../config/samples) has a Placement, a policy and a lease to start from.

### Helm values

| Value | Default | Purpose |
|---|---|---|
| `image.registry` / `image.repository` / `image.tag` | `ghcr.io` / `fleetpermit/fleetpermit-controller` / chart appVersion | image location; set `registry: ""` for a fully qualified repository |
| `image.pullPolicy`, `imagePullSecrets` | `IfNotPresent`, `[]` | |
| `replicas` | `1` | more replicas are safe with leader election |
| `leaderElection.enabled` | `true` | |
| `watchNamespace` | `""` | restrict FleetPermit objects, placements and decisions to one namespace |
| `workExecutor` | `""` | `namespace/name` of a managed-cluster ServiceAccount that the OCM work agent checks FleetPermit's content against before applying it; see [RBAC](#rbac) |
| `logLevel` | `info` | controller log level, passed as `--zap-log-level` |
| `metrics.enabled` / `metrics.port` / `metrics.scrapeAnnotations` | `true` / `8080` / `true` | Prometheus endpoint and `prometheus.io/*` annotations |
| `tracing.otlpEndpoint` | `""` | enables OpenTelemetry trace export over OTLP/HTTP |
| `rbac.create`, `serviceAccount.*` | `true` | |
| `resources`, `podSecurityContext`, `securityContext`, `nodeSelector`, `tolerations`, `affinity` | hardened defaults | non-root, read-only root filesystem, all capabilities dropped |
| `podAnnotations`, `podLabels` | `{}` | extra annotations and labels on the controller pod |

### Building and publishing images elsewhere

```sh
make images IMAGE_REGISTRY=registry.example.com/platform IMAGE_TAG=v0.1.1
podman push registry.example.com/platform/fleetpermit-controller:v0.1.1   # or docker push
helm install fleetpermit charts/fleetpermit -n fleetpermit-system --create-namespace \
  --set image.registry=registry.example.com --set image.repository=platform/fleetpermit-controller --set image.tag=v0.1.1
```

Images are static Go binaries on `scratch`. They run as UID 65532 and contain only the binary and CA
certificates.

## Verifying releases

Release images and assets are signed keylessly with Sigstore cosign, using the GitHub OIDC identity
of the workflow that signed them. The signatures are recorded in the public Rekor transparency log.

- v0.1.0 was published before signing was automated. Its images and assets were signed afterwards by
  the manual [`sign-release`](../.github/workflows/sign-release.yaml) workflow.
- Later releases are signed by the [`release`](../.github/workflows/release.yaml) workflow when the
  tag is pushed.

The identity pattern below accepts either workflow.

```sh
# an image (use the digest from images.txt attached to the release)
cosign verify ghcr.io/fleetpermit/fleetpermit-controller@sha256:<digest> \
  --certificate-identity-regexp '^https://github.com/fleetpermit/fleetpermit/.github/workflows/(release|sign-release).yaml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# a release asset (the Helm chart, the SBOM)
cosign verify-blob fleetpermit-0.1.1.tgz --bundle fleetpermit-0.1.1.tgz.sigstore.json \
  --certificate-identity-regexp '^https://github.com/fleetpermit/fleetpermit/.github/workflows/(release|sign-release).yaml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

To install from the release assets, download the chart, its signature bundle and the image list,
verify the chart as shown above, then install the verified file:

```sh
gh release download v0.1.1 -R fleetpermit/fleetpermit -p 'fleetpermit-0.1.1.tgz*' -p images.txt
# verify fleetpermit-0.1.1.tgz with cosign verify-blob as above
helm install fleetpermit ./fleetpermit-0.1.1.tgz -n fleetpermit-system --create-namespace
```

Release tags and commits are also signed and show as Verified on GitHub.

## RBAC

Generated from the `+kubebuilder:rbac` markers in `internal/controller` ([`config/rbac/role.yaml`](../config/rbac/role.yaml)):

| API group | Resources | Verbs | Why |
|---|---|---|---|
| `fleetpermit.github.io` | fleetaccesspolicies | get, list, watch, update | reconcile policies; add and remove the cleanup finalizer |
| `fleetpermit.github.io` | toolaccessleases | get, list, watch | read leases; the controller never changes a lease's spec |
| `fleetpermit.github.io` | fleetaccesspolicies/status, toolaccessleases/status | get, patch | report status |
| `cluster.open-cluster-management.io` | placements, placementdecisions, managedclusters | get, list, watch | resolve placements; report unavailable clusters |
| `work.open-cluster-management.io` | manifestworks | get, list, watch, create, patch, delete | deliver, update and withdraw grants (updates are merge patches) |
| `coordination.k8s.io` (release namespace) | leases | get, list, watch, create, update, patch, delete | leader election |
| core, `events.k8s.io` (release namespace) | events | create, patch | events |

On the hub, the controller has no access to the Secret, workload or RBAC APIs. The e2e `RBAC`
scenario checks this with `kubectl auth can-i`.

The `manifestworks` permission is cluster-wide. The controller can therefore read every
`ManifestWork` on the hub, including content other tools deliver through ManifestWork (which can
embed Secrets), and it can create and update `ManifestWork` in every managed-cluster namespace. The
OCM work agent applies whatever a `ManifestWork` contains on its managed cluster. FleetPermit only
ever renders `XAccessPolicy` objects, but a stolen controller credential could read other tools'
content and deliver its own. Treat the controller's ServiceAccount and namespace as privileged, and
consider these controls:

- Set `--work-executor` (Helm value `workExecutor`) to a managed-cluster ServiceAccount that may only
  manage `xaccesspolicies`. The work agent then checks each object in FleetPermit's ManifestWorks
  against that ServiceAccount's permissions (a SubjectAccessReview) before applying it, and refuses
  what the ServiceAccount may not manage. It still writes with its own identity. OCM's hub webhook
  requires the controller to hold the `execute-as` permission on `manifestworks` for that
  ServiceAccount. The Helm chart adds a ClusterRole for exactly that ServiceAccount when
  `workExecutor` is set and `rbac.create` is true; otherwise, add the same rule yourself. The lab
  does not exercise this option.

  This does not stop a stolen controller credential on its own. The hub webhook checks `execute-as`
  only for a ManifestWork that names an executor, so the credential can create a ManifestWork without
  one, which the work agent applies with its own permissions. Only OCM's `NilExecutorValidating`
  feature gate on the hub (alpha, off by default in OCM v1.3) makes the webhook check ManifestWorks
  without an executor too.
- Grant write access to `toolaccessleases/status` and `fleetaccesspolicies/status` only to the
  controller. A lease without `spec.duration` keeps its expiry pinned in `status.expiresAt`, so a
  principal that can write lease status could extend such a lease up to the policy's `maxDuration`
  (never beyond it; see the [threat model](threat-model.md), T9).

On managed clusters, [`work-agent-rbac.yaml`](../config/managed-cluster/work-agent-rbac.yaml) adds
permissions to the OCM work agent. It is a ClusterRole labelled
`open-cluster-management.io/aggregate-to-work: "true"`, which OCM aggregates into the work agent's
role, and it grants get, list, watch, create, update, patch and delete on `xaccesspolicies`. It does
not restrict anything else the work agent can already do.

## Metrics

Served at `:8080/metrics`. No metric uses identities, lease names or cluster names as labels.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `fleetpermit_reconcile_total` | counter | `result` = success, error | policy reconciliations |
| `fleetpermit_reconcile_errors_total` | counter | none | reconciliations that returned an error |
| `fleetpermit_active_leases` | gauge | none | leases granting on at least one cluster |
| `fleetpermit_expired_leases_total` | counter | none | leases that transitioned to Expired |
| `fleetpermit_denied_leases_total` | counter | `reason` | leases that transitioned to Denied |
| `fleetpermit_authorized_clusters` | gauge | none | (policy, cluster) pairs holding grants |
| `fleetpermit_policy_propagation_seconds` | histogram | none | lease creation → first Ready on every target cluster; observed once per lease per controller process |
| `fleetpermit_lease_revocation_seconds` | histogram | none | expiry or denial → grants withdrawn everywhere (cleanup; the gateway already denies at expiry) |
| `fleetpermit_placement_changes_total` | counter | none | observed changes to a policy's selected clusters |

Useful alerts:

- `increase(fleetpermit_reconcile_errors_total[10m]) > 0`
- `fleetpermit_active_leases > 0` for longer than your longest `maxDuration` (leases should come and go)
- `histogram_quantile(0.95, sum by (le) (rate(fleetpermit_policy_propagation_seconds_bucket[6h]))) > 60`.
  Each lease adds one sample, so use a window that holds several leases. A lease that never becomes
  Ready adds no sample; watch its `Ready` condition for that.

controller-runtime's standard workqueue and REST client metrics are also exported.

## Tracing

Set `tracing.otlpEndpoint` (or `OTEL_EXPORTER_OTLP_ENDPOINT`). Spans: `fleetpermit.reconcile.policy`
(attribute `fleetpermit.policy`) and `fleetpermit.render.cluster` (attributes `fleetpermit.cluster`,
`fleetpermit.grants`, `fleetpermit.content_digest`).

## Day-2 operations

- Revoke a lease early with `kubectl -n <namespace> delete toolaccesslease <name>`. The grant is withdrawn from every
  cluster the hub can reach; [results.md](results.md) shows the measured "Lease deleted → first DENY"
  latency. A cluster the hub cannot reach keeps the grant until it expires.
- To stop a whole policy in an emergency, delete the `FleetAccessPolicy`. Its finalizer withdraws
  every grant from every cluster, and every lease of the policy that has not expired becomes `Denied`
  with reason `PolicyNotFound`. Expired leases stay `Expired`. Denied is terminal, so recreating the
  policy does not revive those leases.
- Deleting only the policy's placement also withdraws every grant, and the policy reports
  `Degraded/PlacementNotFound`. Its leases are not denied, though. They go to phase `Pending`
  (`NoEligibleClusters`), and a lease that has not expired is delivered again if the placement comes
  back. This pauses the policy and leaves its leases in place.
- To upgrade FleetPermit, check out the new release tag, apply the CRDs, then run `helm upgrade`.
  Helm installs the chart's `crds/` directory on first install only and never upgrades it:

  ```sh
  git checkout v0.1.1
  kubectl apply --server-side --force-conflicts -f charts/fleetpermit/crds/
  helm upgrade fleetpermit charts/fleetpermit -n fleetpermit-system
  kubectl -n fleetpermit-system rollout status deploy/fleetpermit-controller
  ```

  State lives in the API, and a restart neither withdraws nor re-creates grants. The upgrade from
  v0.1.0 to v0.1.1 adds the CRD rule that rejects lease durations below 10 s, and renames delivered
  objects; see the [changelog](../CHANGELOG.md).
- The chart deploys the controller image named by the chart's `appVersion`. A `main` checkout
  therefore deploys the image of that release, not the code you checked out, and while a release is
  being prepared but not yet tagged, that image does not exist. Install from a release tag or from
  the chart attached to the release. To run unreleased code, build images with `make images`, push
  them, and set `image.repository` and `image.tag`.
- Before upgrading upstream components, read [upstream-compatibility.md](upstream-compatibility.md).
  Run `make test-integration` with the new upstream CRD in `test/fixtures/upstream`, and run the e2e
  suite in the lab, before rolling out.
- The klusterlet's `workConfiguration.statusSyncInterval` sets how often the work agent reports
  status feedback and resource availability to the hub: 10 s by default in OCM v1.3.1 (the work
  agent's `--status-sync-interval` default), and the lab sets 10 s explicitly. It affects status
  latency and how quickly a deleted delivered object is noticed and re-applied. It has no effect on
  enforcement.
- To uninstall, delete every `FleetAccessPolicy` first, so that their finalizers withdraw every
  grant while the controller is still running, then remove the release, the CRDs and the namespace:

  ```sh
  kubectl delete fleetaccesspolicies --all --all-namespaces --wait
  helm uninstall fleetpermit -n fleetpermit-system
  kubectl delete -f charts/fleetpermit/crds/      # also deletes every remaining ToolAccessLease
  kubectl delete namespace fleetpermit-system
  ```

  Helm does not delete the CRDs, so until you do, the CRDs and your `ToolAccessLease` objects stay.
  On managed clusters, `work-agent-rbac.yaml` and the default-deny anchor stay until you delete them.
  While the anchor exists, the backend denies every call; delete it too if the backend should go back
  to upstream's behaviour without FleetPermit.
