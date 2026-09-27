# Troubleshooting

Start with the conditions. Every state FleetPermit reports carries a reason.

```sh
kubectl get fap,tal -n <ns>
kubectl describe tal <lease> -n <ns>
kubectl get fap <policy> -n <ns> -o jsonpath='{range .status.clusters[*]}{.name}{"\t"}{.ready}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
```

| Symptom | Likely cause | What to check |
|---|---|---|
| Lease `Denied` | it asks for more than the policy allows | the `Denied` condition message names the tool, subject or duration |
| Lease `Pending` with `PolicyNotFound` | the lease was created before its policy, or refers to a policy name that does not exist in its namespace | create the policy before the deadline named in the condition message (5 minutes after the lease's creation); after that the lease is `Denied` and needs to be created again. Check `spec.policyRef.name` for a typo |
| Lease `Pending` with `NoEligibleClusters` | its `clusters` are not in the placement, the placement selects nothing, or the placement was deleted | `kubectl get placementdecision -n <ns> -l cluster.open-cluster-management.io/placement=<name> -o yaml` |
| Lease `Pending` with `CapacityExceeded` | the upstream rule limit is reached on every requested cluster | the lease activates by itself when another lease expires or is deleted; otherwise reduce concurrent leases or split the policy |
| Policy `Degraded/CapacityExceeded` | standing grants (`lease.required: false`) do not fit the upstream rule limit on the listed clusters | reduce the policy's subjects; the API server accepts at most 5 on a standing policy |
| Policy `Degraded/PlacementNotFound` | the placement is missing or in another namespace | the Placement must be in the policy's namespace: `kubectl get placement -n <ns>` |
| Policy `Ready` with reason `NoClustersSelected` and `CLUSTERS 0/0`, although clusters exist | the policy's namespace has no `ManagedClusterSetBinding`, the clusters are not in the bound cluster set, or no cluster matches the Placement's selector | `kubectl get managedclustersetbinding -n <ns>`; the Placement's conditions in `kubectl get placement <name> -n <ns> -o yaml`; `kubectl get managedcluster --show-labels` |
| Cluster reason `Applying` for a long time | the work agent has not applied the current generation | `kubectl get manifestwork -n <cluster> -l app.kubernetes.io/managed-by=fleetpermit -o yaml`; work agent logs on the cluster |
| Cluster reason `DeliveryFailed` | the controller could not render or deliver the ManifestWork: a rendering error, the hub rejected the ManifestWork (for example OCM's webhook, or a missing `execute-as` permission when `workExecutor` is set), or another policy owns a ManifestWork with the same name | the cluster's message in `kubectl get fap <policy> -n <ns> -o yaml`; the controller log (`kubectl -n fleetpermit-system logs deploy/fleetpermit-controller`). Retries back off from 1 s up to 2 minutes, so after a fix that raises no event it can take up to 2 minutes to clear |
| Cluster reason `ApplyFailed` | the XAccessPolicy CRD is missing, or the work agent lacks RBAC | install kube-agentic-networking CRDs; apply `config/managed-cluster/work-agent-rbac.yaml` |
| Cluster reason `AwaitingAcceptance` | the enforcement controller has not written `Accepted` yet, or OCM's status feedback has not reported the current content digest | `kubectl get xaccesspolicy -n <target-ns> -o yaml` on the cluster; the klusterlet's `statusSyncInterval`; the status feedback in `kubectl get manifestwork -n <cluster> <name> -o jsonpath='{.status.resourceStatus.manifests[*].statusFeedback}'` on the hub |
| Cluster reason `AwaitingAcceptance` with "reports its status feedback more than once" | the ManifestWork was edited on the hub to add feedback rules; FleetPermit does not trust duplicate values | the ManifestWork's `manifestConfigs`; FleetPermit restores its own rules on the next reconcile, and hub write access to ManifestWork should be restricted |
| Cluster reason `Delivering` for a long time | the cluster was placed again while its previous ManifestWork is still being deleted, which waits for the work agent | `kubectl get manifestwork -n <cluster> -l app.kubernetes.io/managed-by=fleetpermit` on the hub; the work agent on that cluster |
| Cluster listed as `Revoking` | the cluster left the placement, or the placement was deleted, and its ManifestWork still exists or is being deleted; the withdrawal waits while the cluster is offline | `kubectl get managedcluster <cluster>`; the ManifestWork in the cluster's namespace on the hub |
| Cluster reason `RejectedByEnforcement` | upstream rejected the policy (for example a per-target limit or invalid CEL) | the XAccessPolicy's `status.ancestors[].conditions`; the upstream per-target limit is 5 policies |
| Cluster reason `Drifted` | the delivered object was deleted or changed on the cluster | FleetPermit has asked OCM to re-apply; check who changed it (audit log) |
| Cluster reason `ClusterUnavailable` (policy `Degraded/ClustersFailed`) | OCM reports the cluster unavailable; status may be stale. A withdrawal from the cluster, or a re-delivery that must first remove its previous ManifestWork, waits for it to reconnect | `kubectl get managedcluster <cluster>`; the klusterlet on that cluster. Grants on it still expire on time, and the policy resumes by itself when the cluster is available again |
| Lease `Active` but `Degraded/CapacityExceeded` | more concurrent grants than the upstream rule limit on some clusters | the condition lists the clusters; reduce concurrent leases per policy, or split the policy |
| Calls allowed on a cluster that should deny | the default-deny anchor is missing | `kubectl get xaccesspolicy -n <target-ns>` on the cluster; install the anchor |
| Every call denied at `initialize` | the caller holds no active grant on that cluster (no lease, lease expired or withdrawn, cluster not placed), or is not a policy subject. An untrusted certificate fails the TLS handshake instead. | `kubectl get tal -o wide` for the subject and phase; the caller's SPIFFE ID (`openssl x509 -noout -ext subjectAltName` on its certificate); for TLS failures, the gateway trust bundle |
| Calls denied at `tools/call` though the lease is Active | the tool name differs (exact match), or the lease is not yet Ready on that cluster | `kubectl get tal -o wide`; the rendered CEL rule on the cluster |

## Lab

| Symptom | Fix |
|---|---|
| `Cannot connect to Podman` | `podman machine start`. The lab needs about 8 CPUs and 16 GiB (`podman machine set --cpus 8 --memory 16384` while the machine is stopped). |
| Image pulls rate-limited | kind nodes pull with their own containerd, so an image in podman's store does not help by itself. Pull it with podman, save it and load it into the node: `podman pull <image>`, `podman save -o image.tar <image>`, `KIND_EXPERIMENTAL_PROVIDER=podman kind load image-archive image.tar --name fleetpermit-<cluster>` (for example `fleetpermit-cluster-east`). Then re-run `demo/scripts/lab-up.sh`; the scripts are idempotent |
| `clusteradm join` hangs | the hub API must be reachable from the managed-cluster nodes; the lab uses the kind network with `--force-internal-endpoint-lookup` |
| No Gateway address | MetalLB is not ready yet: `kubectl -n metallb-system get pods` |
| Want a clean slate | `make demo-down && make demo-up` (only lab-created clusters are deleted) |

To inspect the lab directly: `export KUBECONFIG=$PWD/.work/lab/kubeconfig`, then use contexts
`kind-fleetpermit-hub` and `kind-fleetpermit-cluster-{east,west,edge}`.
