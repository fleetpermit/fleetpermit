# FleetPermit Helm chart

Installs the FleetPermit controller and its CRDs on an Open Cluster Management hub.

```sh
helm install fleetpermit charts/fleetpermit -n fleetpermit-system --create-namespace
# or, from the signed chart attached to a release:
helm install fleetpermit fleetpermit-0.1.1.tgz -n fleetpermit-system --create-namespace
```

Every value is listed with its default in [`values.yaml`](values.yaml) and explained in the
[operations guide](https://github.com/fleetpermit/fleetpermit/blob/v0.1.1/docs/operations.md#helm-values).
The ClusterRole is generated from the controller's RBAC markers and contains no cluster-admin
permissions.

Cluster-scoped objects carry the release name when it is not `fleetpermit`. Namespaced objects have
fixed names, so to run several releases on one hub (for example one per team), install each in its
own namespace and give each a different `watchNamespace`.

Managed clusters additionally need
[`work-agent-rbac.yaml`](https://github.com/fleetpermit/fleetpermit/blob/v0.1.1/config/managed-cluster/work-agent-rbac.yaml)
and the
[default-deny anchor](https://github.com/fleetpermit/fleetpermit/blob/v0.1.1/config/managed-cluster/default-deny-anchor.yaml),
edited to target the same resource as your policies' `target.ref`.

CRDs are installed from `crds/` on first install only; Helm never upgrades them. Before every
`helm upgrade`, apply the new chart's CRDs yourself:

```sh
kubectl apply --server-side --force-conflicts -f charts/fleetpermit/crds/
```
