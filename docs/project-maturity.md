# Project maturity

FleetPermit is an independent project. It is **not** a CNCF project and makes no claim to be one. This
page is a candid self-check against the kinds of criteria the CNCF uses for its maturity levels, so
users and contributors can see where the project stands and what is still missing.

## In place today

| Area | Status | Evidence |
|---|---|---|
| Open source license | Apache-2.0 | [LICENSE](../LICENSE) |
| Governance and maintainer process | documented | [GOVERNANCE.md](../GOVERNANCE.md) |
| Code of conduct | Contributor Covenant 2.1 | [CODE_OF_CONDUCT.md](../CODE_OF_CONDUCT.md) |
| Contribution guide | documented | [CONTRIBUTING.md](../CONTRIBUTING.md) |
| Security policy | private reporting, response targets | [SECURITY.md](../SECURITY.md) |
| Security self-assessment | TAG Security template structure | [security-self-assessment.md](security-self-assessment.md) |
| Threat model | per-threat mitigations and residual risk | [threat-model.md](threat-model.md) |
| Roadmap | documented | [ROADMAP.md](../ROADMAP.md) |
| Vendor neutrality | Kubernetes, CNCF and Linux Foundation dependencies only | [DEPENDENCIES.md](../DEPENDENCIES.md) |
| Reproducible end-to-end evidence | multi-cluster suite, independently reproduced in CI | [results.md](results.md) |
| Upstream conformance | upstream suite run unmodified | [results.md](results.md#upstream-conformance) |
| CI on every change | verify, vulnerability scan, unit, integration, images | [.github/workflows](../.github/workflows) |
| Supply chain | pinned actions, signed commits and tags, SPDX SBOM per release, OpenSSF Scorecard | release workflow, scorecard workflow |
| Release | v0.1.0 pre-release with multi-arch images and a Helm chart | [CHANGELOG.md](../CHANGELOG.md) |

## Not yet in place

These cannot be produced by code alone. They are listed so nobody mistakes the current state for a
mature one:

- **Adopters.** No production adopters are known. The project will list adopters only when they ask
  to be listed.
- **Maintainer diversity.** There is one maintainer today. Incubation and graduation expect maintainers
  from several organisations.
- **Independent security audit.** None has been performed.
- **OpenSSF Best Practices badge.** Not yet applied for.
- **Signed container images with provenance.** Planned ([ROADMAP.md](../ROADMAP.md)).
- **DCO sign-off enforcement.** To be adopted with the first external contributions.
- **API stability.** APIs are `v1alpha1`, and the upstream Agentic Networking APIs they build on are
  experimental.
