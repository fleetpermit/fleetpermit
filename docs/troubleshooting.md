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
| Lease `Pending` with `NoEligibleClusters` | its `clusters` are not in the placement, or the placement selects nothing | `kubectl get placementdecision -n <ns> -l cluster.open-cluster-management.io/placement=<name> -o yaml` |
| Policy `Degraded/PlacementNotFound` | the placement is missing or in another namespace | placements must be in the policy's namespace, which needs a `ManagedClusterSetBinding` |
| Cluster reason `Applying` for a long time | the work agent has not applied the current generation | `kubectl get manifestwork -n <cluster> -l app.kubernetes.io/managed-by=fleetpermit -o yaml`; work agent logs on the cluster |
| Cluster reason `ApplyFailed` | the XAccessPolicy CRD is missing, or the work agent lacks RBAC | install kube-agentic-networking CRDs; apply `config/managed-cluster/work-agent-rbac.yaml` |
| Cluster reason `AwaitingAcceptance` | the enforcement controller has not written `Accepted` yet, or OCM has not synced status feedback | `kubectl get xaccesspolicy -n <target-ns> -o yaml` on the cluster; the klusterlet `statusSyncInterval` |
| Cluster reason `RejectedByEnforcement` | upstream rejected the policy (for example a per-target limit or invalid CEL) | the XAccessPolicy's `status.ancestors[].conditions`; the upstream per-target limit is 5 policies |
| Cluster reason `Drifted` | the delivered object was deleted or changed on the cluster | FleetPermit has asked OCM to re-apply; check who changed it (audit log) |
| Cluster reason `ClusterUnavailable` | OCM reports the cluster unavailable; status may be stale | `kubectl get managedcluster <cluster>`; grants on it still expire on time |
| Lease `Degraded/CapacityExceeded` | more concurrent grants than the upstream rule limit | fewer concurrent leases per policy, or split the policy |
| Calls allowed on a cluster that should deny | the default-deny anchor is missing | `kubectl get xaccesspolicy -n <target-ns>` on the cluster; install the anchor |
| Every call denied at `initialize` | caller identity not trusted, or not the policy subject | the caller's SPIFFE ID (`openssl x509 -noout -ext subjectAltName` on its certificate) and the gateway trust bundle |
| Calls denied at `tools/call` though the lease is Active | the tool name differs (exact match), or the lease is not yet Ready on that cluster | `kubectl get tal -o wide`; the rendered CEL rule on the cluster |

## Lab

| Symptom | Fix |
|---|---|
| `Cannot connect to Podman` | `podman machine start`. The lab needs about 8 CPUs and 16 GiB (`podman machine set --cpus 8 --memory 16384` while the machine is stopped). |
| Image pulls rate-limited | pre-pull with podman and re-run `demo/scripts/lab-up.sh`; the scripts are idempotent |
| `clusteradm join` hangs | the hub API must be reachable from the managed-cluster nodes; the lab uses the kind network with `--force-internal-endpoint-lookup` |
| No Gateway address | MetalLB is not ready yet: `kubectl -n metallb-system get pods` |
| Want a clean slate | `make demo-down && make demo-up` (only lab-created clusters are deleted) |

To inspect the lab directly: `export KUBECONFIG=$PWD/.work/lab/kubeconfig`, then use contexts
`kind-fleetpermit-hub` and `kind-fleetpermit-cluster-{east,west,edge}`.
