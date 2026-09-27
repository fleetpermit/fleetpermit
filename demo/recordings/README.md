# Demo recordings

Terminal recordings of real runs of [`demo/run.sh`](../run.sh) against the lab, captured with
`make demo-videos` ([`hack/record-demos.sh`](../../hack/record-demos.sh)). That target writes the
casts, MP4 videos and poster images to `dist/video/` (set `FP_VIDEO_OUT` to change it), refreshes the
README GIFs in `docs/assets/`, copies the `.cast` files into this directory and writes a plain-text
transcript of each next to it. When `../fleetpermit.github.io` is a checkout of the website, it also
copies the MP4 videos and poster images there.

The `$ command` lines in a recording are simplified for readability. They show the equivalent
`kubectl` or container command (for example `kubectl create -f incident-42.yaml`), while the script
runs helper functions with the lab's kubeconfig, contexts and inline manifests. Every ALLOWED and
DENIED line, and every status output, is real output from that run. Each ALLOWED or DENIED line is
one MCP call through a real gateway.

| Recording | Transcript | Shows |
|---|---|---|
| `demo-overview.cast` | [`demo-overview.txt`](demo-overview.txt) | fleet status, no-lease denial, lease activation on two clusters, wrong cluster, forbidden tool, wrong identity, the CEL time bound, expiry |
| `demo-security.cast` | [`demo-security.txt`](demo-security.txt) | permission and duration escalation denied, immutable leases, malformed policy rejected, a deleted grant restored from the hub |
| `demo-disconnected-expiry.cast` | [`demo-disconnected-expiry.txt`](demo-disconnected-expiry.txt) | the hub is paused, the lease still expires on every cluster, then the fleet converges on reconnect |

The transcripts are the text alternative to the videos. Each one is the terminal output of the
recording with colour and cursor escape sequences removed, rendered by
[`hack/cast-to-text.py`](../../hack/cast-to-text.py) (for example
`hack/cast-to-text.py demo/recordings/demo-overview.cast`).

Play one locally with `asciinema play demo/recordings/demo-overview.cast`. The casts use the
asciicast v3 format, so they need asciinema 3.0 or later. MP4 versions are on the
[website](https://fleetpermit.github.io/demo.html).
