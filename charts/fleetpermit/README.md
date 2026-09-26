# FleetPermit Helm chart

Installs the FleetPermit controller and its CRDs on an Open Cluster Management hub.

```sh
helm install fleetpermit charts/fleetpermit -n fleetpermit-system --create-namespace
```

Every value is documented in [`values.yaml`](values.yaml): image registry, repository and tag,
resources, replicas, leader election, metrics, tracing, RBAC and the service account. The ClusterRole
is generated from the controller's RBAC markers and contains no cluster-admin permissions.

Managed clusters additionally need `config/managed-cluster/work-agent-rbac.yaml` and the default-deny
anchor. See [docs/operations.md](../../docs/operations.md).

CRDs are installed from `crds/` on first install. Helm does not upgrade CRDs, so after upgrading the
chart apply them with `kubectl apply -f charts/fleetpermit/crds/`.
