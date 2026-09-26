# Demo recordings

Terminal recordings of real runs of [`demo/run.sh`](../run.sh) against the lab, captured with
`make demo-videos` ([`hack/record-demos.sh`](../../hack/record-demos.sh)). Every line comes from a
real command or a real MCP call through a real gateway.

| Recording | Shows |
|---|---|
| `demo-overview.cast` | fleet status, no-lease denial, lease activation on two clusters, wrong cluster, forbidden tool, wrong identity, the CEL time bound, expiry |
| `demo-security.cast` | permission and duration escalation denied, immutable leases, malformed policy rejected, a deleted grant restored from the hub |
| `demo-disconnected-expiry.cast` | the hub is paused, the lease still expires on every cluster, then the fleet converges on reconnect |

Play one locally with `asciinema play demo/recordings/demo-overview.cast`. MP4 versions are on the
[website](https://fleetpermit.github.io/demo.html).
