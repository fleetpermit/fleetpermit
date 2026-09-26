# FleetPermit Helm chart

Installs the FleetPermit controller and its CRDs on an Open Cluster Management hub.

```sh
helm install fleetpermit charts/fleetpermit -n fleetpermit-system --create-namespace
```

Every value is listed with its default in [`values.yaml`](values.yaml) and explained in
[docs/operations.md](../../docs/operations.md#helm-values). The ClusterRole is generated from the
controller's RBAC markers and contains no cluster-admin permissions. Cluster-scoped objects carry the
release name when it is not `fleetpermit`, so several releases (for example one per team with
`watchNamespace`) can share a hub.

Managed clusters additionally need `config/managed-cluster/work-agent-rbac.yaml` and the default-deny
anchor. See [docs/operations.md](../../docs/operations.md).

CRDs are installed from `crds/` on first install only; Helm never upgrades them. Before every
`helm upgrade`, apply them yourself:

```sh
kubectl apply --server-side --force-conflicts -f charts/fleetpermit/crds/
```
