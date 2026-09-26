# Demo recordings

Terminal recordings of real runs of [`demo/run.sh`](../run.sh) against the lab, captured with
`make demo-videos` ([`hack/record-demos.sh`](../../hack/record-demos.sh)).

The `$ command` lines in a recording are simplified for readability. They show the equivalent
`kubectl` or container command (for example `kubectl create -f incident-42.yaml`), while the script
runs helper functions with the lab's kubeconfig, contexts and inline manifests. Every ALLOWED and
DENIED line, and every status output, is real output from that run. Each ALLOWED or DENIED line is
one MCP call through a real gateway.

| Recording | Shows |
|---|---|
| `demo-overview.cast` | fleet status, no-lease denial, lease activation on two clusters, wrong cluster, forbidden tool, wrong identity, the CEL time bound, expiry |
| `demo-security.cast` | permission and duration escalation denied, immutable leases, malformed policy rejected, a deleted grant restored from the hub |
| `demo-disconnected-expiry.cast` | the hub is paused, the lease still expires on every cluster, then the fleet converges on reconnect |

Play one locally with `asciinema play demo/recordings/demo-overview.cast`. MP4 versions are on the
[website](https://fleetpermit.github.io/demo.html).
