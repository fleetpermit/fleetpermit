# Governance

FleetPermit is an independent open-source project. It is not part of, or endorsed by, the CNCF, the
Linux Foundation or the Kubernetes project, although it builds on their technologies.

## Roles

- **Contributors**: anyone who opens an issue, reviews, or submits a pull request.
- **Maintainers**: review and merge changes, triage issues, cut releases and handle security
  reports. Current maintainers:

  | Maintainer | GitHub |
  |---|---|
  | Sandeep Bazar | [@sandeepbazar](https://github.com/sandeepbazar) |

## Decisions

Day-to-day decisions happen in pull requests by lazy consensus: a change merges when a maintainer
approves it and no maintainer objects. Design changes that affect the API, the security model or the
dependency policy need an issue or design document first and approval from a majority of maintainers.
Disagreements that cannot be resolved in review are decided by majority vote of the maintainers.

## Becoming a maintainer

Contributors who have made sustained, high-quality contributions (code, reviews, docs or triage) over
several months can be nominated by any maintainer. A nomination is approved by a majority of existing
maintainers. The project aims for maintainers from more than one organisation as it grows.

## Principles that changes must respect

1. Vendor neutrality: architectural dependencies come from the Kubernetes, CNCF or Linux Foundation
   ecosystems ([DEPENDENCIES.md](DEPENDENCIES.md)).
2. Fail closed.
3. Claims in documentation must be backed by tests or measurements.

## Changes to this document

Changes to governance require approval from a majority of maintainers.
