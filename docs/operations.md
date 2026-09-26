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
kubectl apply -f config/managed-cluster/default-deny-anchor.yaml   # edit namespace and targetRefs first
```

The anchor must exist on every cluster that runs the backend, including clusters that no placement
currently selects. Without it, upstream enforces nothing on that backend
([ADR-3](design.md#adr-3-default-deny-anchor)).

The anchor must target the same resource as the policies' `spec.target.ref`. The shipped file targets
the `XBackend` `fleet-tools` in `mcp-tools`; edit its namespace and `targetRefs` to match. For policies
that target a `Gateway`, point the anchor's `targetRefs` at that Gateway (group
`gateway.networking.k8s.io`, kind `Gateway`). kube-agentic-networking evaluates gateway-level and
backend-level policies as separate checks that must both allow a call, so an `XBackend` anchor next to
grants on the `Gateway` would deny every call. The lab exercises `XBackend` targets only.

A policy references an OCM `Placement` in its own namespace. OCM lets that Placement select clusters
only when a `ManagedClusterSetBinding` binds a cluster set to the namespace. The lab sets this up as
follows (repeat the label command for each cluster; use `env=staging` for the others), and the sample
Placement then selects the clusters labelled `env=production`:

```sh
# on the hub
kubectl label managedcluster cluster-east cluster.open-cluster-management.io/clusterset=fleet env=production
kubectl apply -f - <<'YAML'
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSet
metadata:
  name: fleet
spec:
  clusterSelector:
    selectorType: ExclusiveClusterSetLabel
---
apiVersion: v1
kind: Namespace
metadata:
  name: fleet
---
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSetBinding
metadata:
  name: fleet
  namespace: fleet
spec:
  clusterSet: fleet
YAML
```

[`config/samples`](../config/samples) has a Placement, a policy and a lease to start from.

### Helm values

| Value | Default | Purpose |
|---|---|---|
| `image.registry` / `image.repository` / `image.tag` | `ghcr.io` / `fleetpermit/fleetpermit-controller` / chart appVersion | image location; set `registry: ""` for a fully qualified repository |
| `image.pullPolicy`, `imagePullSecrets` | `IfNotPresent`, `[]` | |
| `replicas` | `1` | more replicas are safe with leader election |
| `leaderElection.enabled` | `true` | |
| `watchNamespace` | `""` | restrict FleetPermit objects, placements and decisions to one namespace; policies and events outside it are ignored |
| `workExecutor` | `""` | `namespace/name` of a managed-cluster ServiceAccount that the OCM work agent checks FleetPermit's content against before applying it; see [RBAC](#rbac) |
| `logLevel` | `info` | controller log level, passed as `--zap-log-level` |
| `metrics.enabled` / `metrics.port` / `metrics.scrapeAnnotations` | `true` / `8080` / `true` | Prometheus endpoint and `prometheus.io/*` annotations |
| `tracing.otlpEndpoint` | `""` | enables OpenTelemetry trace export over OTLP/HTTP |
| `rbac.create` | `true` | create the controller's ClusterRole, Role and bindings |
| `serviceAccount.create` / `serviceAccount.name` / `serviceAccount.annotations` | `true` / `""` / `{}` | the controller's ServiceAccount; an empty name means `fleetpermit-controller` |
| `resources`, `podSecurityContext`, `securityContext`, `nodeSelector`, `tolerations`, `affinity` | hardened defaults | non-root, read-only root filesystem, all capabilities dropped |
| `podAnnotations`, `podLabels` | `{}` | extra annotations and labels on the controller pod |

### Building and publishing images elsewhere

```sh
make images IMAGE_REGISTRY=registry.example.com/platform IMAGE_TAG=v0.1.1
podman push registry.example.com/platform/fleetpermit-controller:v0.1.1   # or docker push
helm install fleetpermit charts/fleetpermit -n fleetpermit-system --create-namespace \
  --set image.registry=registry.example.com --set image.repository=platform/fleetpermit-controller --set image.tag=v0.1.1
```

`make images` builds for the host's architecture only. For multi-architecture images (linux/amd64
and linux/arm64), run `hack/publish-images.sh <registry> <tag>`, which builds with podman, pushes the
manifests and writes the digests to `dist/images.txt`.

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
verify the chart as shown above, then install the verified file. The download below uses the GitHub
CLI (`gh`); you can also download the same files from the release page:

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
| `fleetpermit.github.io` | fleetaccesspolicies | get, list, watch, patch | reconcile policies; add and remove the cleanup finalizer with a merge patch that leaves the spec alone |
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

  On each managed cluster, create the executor ServiceAccount and give it the same permissions as
  `work-agent-rbac.yaml`, in the policies' target namespace. The work agent itself still needs
  `work-agent-rbac.yaml`, because it writes with its own identity. For example, with
  `workExecutor: mcp-tools/fleetpermit-executor`:

  ```yaml
  apiVersion: v1
  kind: ServiceAccount
  metadata: {name: fleetpermit-executor, namespace: mcp-tools}
  ---
  apiVersion: rbac.authorization.k8s.io/v1
  kind: Role
  metadata: {name: fleetpermit-executor, namespace: mcp-tools}   # the policies' spec.target.namespace
  rules:
    - apiGroups: ["agentic.networking.x-k8s.io"]
      resources: ["xaccesspolicies"]
      verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  ---
  apiVersion: rbac.authorization.k8s.io/v1
  kind: RoleBinding
  metadata: {name: fleetpermit-executor, namespace: mcp-tools}
  roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: fleetpermit-executor}
  subjects:
    - {kind: ServiceAccount, name: fleetpermit-executor, namespace: mcp-tools}
  ```

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
| `fleetpermit_lease_revocation_seconds` | histogram | none | expiry or denial → grants withdrawn everywhere. Observed once every cluster that held the grant reports content without it; for a cluster that left the placement, once its ManifestWork is gone (cleanup; the gateway already denies at expiry). A cluster that stays offline delays the sample |
| `fleetpermit_placement_changes_total` | counter | none | changes to a policy's selected clusters that this controller process observes; a change made while the controller was down is not counted |

Useful alerts:

- `increase(fleetpermit_reconcile_errors_total[10m]) > 0`. Lease status writes are conditional on the
  resource version that was read, so a concurrent update causes a conflict that is retried and also
  counts as a reconcile error. A single error can be such a harmless conflict, so check the controller
  log before acting.
- `fleetpermit_active_leases > 0` for longer than your longest `maxDuration` (leases should come and go)
- `histogram_quantile(0.95, sum by (le) (rate(fleetpermit_policy_propagation_seconds_bucket[6h]))) > 60`.
  Each lease adds one sample, so use a window that holds several leases. A lease that never becomes
  Ready adds no sample; watch its `Ready` condition for that.

controller-runtime's standard workqueue and REST client metrics are also exported.

## Tracing

Set `tracing.otlpEndpoint` (or `OTEL_EXPORTER_OTLP_ENDPOINT`). Spans: `fleetpermit.reconcile.policy`
(attribute `fleetpermit.policy`) and `fleetpermit.render.cluster` (attributes `fleetpermit.cluster`,
`fleetpermit.grants`, `fleetpermit.content_digest`). Spans are flushed when the controller exits. In
v0.1.0 the trace exporter could not start; use v0.1.1 or later for tracing.

## Day-2 operations

- Revoke a lease early with `kubectl -n <namespace> delete toolaccesslease <name>`. The grant is withdrawn from every
  cluster the hub can reach; [results.md](results.md) shows the measured "Lease deleted → first DENY"
  latency. A cluster the hub cannot reach keeps the grant until it expires.
- To stop a whole policy in an emergency, delete the `FleetAccessPolicy`. Its finalizer withdraws
  every grant from every cluster, and every lease that was evaluated against the policy becomes
  `Denied` with reason `PolicyNotFound`; a lease already past its recorded expiry becomes `Expired`,
  and expired leases stay `Expired`. Denied is terminal, so recreating the policy does not revive
  those leases. A lease that was never evaluated (for example one applied just before the policy)
  waits in `Pending` for up to 5 minutes after its creation and would activate if the policy were
  created again in that time, so delete such leases too. After that it is denied.
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
  v0.1.0 to v0.1.1 renames delivered objects (see the [changelog](../CHANGELOG.md)) and adds three
  CRD rules:

  - a lease `duration` of at least 10 s;
  - at most 5 subjects on a policy with `lease.required: false`;
  - `target.ref` as `XBackend` in `agentic.networking.x-k8s.io` or `Gateway` in
    `gateway.networking.k8s.io`, where the group may be omitted and then follows the kind.

  Existing objects that break a rule stay in place, and the API server applies the rules when their
  spec is next changed. The controller adds and removes its finalizer with a merge patch that leaves
  the spec alone, so such a policy can still be deleted (`TestUpgradeFromV010`).
- The chart deploys the controller image named by the chart's `appVersion`. A `main` checkout
  therefore deploys the image of that release, not the code you checked out, and while a release is
  being prepared but not yet tagged, that image does not exist. Install from a release tag or from
  the chart attached to the release. To run unreleased code, build and push images, then set
  `image.registry`, `image.repository` and `image.tag` (see
  [Building and publishing images elsewhere](#building-and-publishing-images-elsewhere)), or set
  `image.registry=""` and give a fully qualified `image.repository`.
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
