# Contributing to FleetPermit

Thanks for your interest! FleetPermit welcomes issues, reviews, documentation and code.

## Ground rules

- Be kind; see the [code of conduct](CODE_OF_CONDUCT.md).
- Security issues go through GitHub private vulnerability reporting, not public issues
  ([SECURITY.md](SECURITY.md)).
- New direct dependencies must come from the Kubernetes, CNCF or Linux Foundation ecosystems and be
  recorded in [DEPENDENCIES.md](DEPENDENCIES.md), with version, governance and license.
- Keep claims honest: documentation that states a behaviour or a number must point to a test or a
  measurement.

## Development setup

You need Go (see `go.mod`), Helm and Python 3. For the lab you also need podman (or docker), kind,
kubectl, clusteradm and jq.

```sh
make help               # all targets
make verify             # everything CI checks statically
make test               # unit + integration (envtest downloads kube-apiserver and etcd)
make demo-up test-e2e   # the multi-cluster lab and end-to-end scenarios
make results            # regenerate test-results/results.json, docs/results.md and the README block
```

After changing anything in `api/` or an RBAC marker, run `make generate` and commit the result.

## Pull requests

1. Open an issue first for API changes, security-model changes or new dependencies.
2. Keep changes focused, and add tests at the lowest layer that proves the behaviour:
   - pure logic → unit test next to the code;
   - anything involving the API server, CRD validation or the reconciler → `test/integration`;
   - anything that depends on real gateways or OCM → a scenario in `test/e2e/run.sh`.
3. `make verify test` must pass. CI runs the same targets.
4. Describe the problem, the change and how you tested it.

## Commit messages

Use the imperative mood ("Add lease capacity reporting"), explain the *why* in the body, and sign off
if your employer requires it. Do not include credentials, internal hostnames or personal data in
commits.

## Project layout

```
api/v1alpha1/                     CRD types (FleetAccessPolicy, ToolAccessLease)
cmd/fleetpermit-controller/       controller entry point
internal/lease/                   pure lease evaluation
internal/enforcement/             enforcement seam + kube-agentic-networking renderer
internal/placement/               placement seam + Open Cluster Management provider
internal/controller/              reconciler and status
internal/digest/, internal/metrics/
config/                           generated CRDs and RBAC, samples, managed-cluster manifests
charts/fleetpermit/               Helm chart
demo/                             lab scripts, demo MCP server and probe
test/integration/, test/e2e/      envtest suite; multi-cluster scenarios and benchmark
hack/                             verification, results, conformance and release tooling
docs/                             architecture, design decisions, security, operations, results
```
