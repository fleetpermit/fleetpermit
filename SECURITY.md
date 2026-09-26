# Security policy

## Reporting a vulnerability

Do not open a public issue for a security problem.

Report it privately through GitHub: open the repository's Security tab and choose
Report a vulnerability ([direct link](https://github.com/fleetpermit/fleetpermit/security/advisories/new)).
Only the maintainers can see the report.

Include the affected version or commit, a description of the issue and its impact, and steps
to reproduce (a failing test or a lab scenario is ideal).

## What to expect

- An acknowledgement within 5 business days.
- An assessment and, where confirmed, a fix plan within 30 days. Complex issues may take longer, and
  we will keep you informed.
- Coordinated disclosure through a GitHub Security Advisory, crediting you unless you prefer otherwise.

## Scope

In scope: the FleetPermit controller, its CRDs and validation, the Helm chart, rendered enforcement
objects, and the lab scripts where they could affect a user's environment.

Vulnerabilities in upstream projects (Open Cluster Management, kube-agentic-networking, Envoy,
Kubernetes) should be reported to those projects. Tell us as well if FleetPermit's use of them makes an
issue exploitable.

## Supported versions

FleetPermit is pre-1.0. Security fixes are made on `main` and in the latest release.

## Security design

See [docs/security-model.md](docs/security-model.md) and [docs/threat-model.md](docs/threat-model.md)
for the trust assumptions, mitigations and known limitations.
