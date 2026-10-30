# OpenSandbox Maintainers

This file lists OpenSandbox's Project Maintainers and component maintainers. Roles and responsibilities are described in [CONTRIBUTING.md](CONTRIBUTING.md#project-roles-and-technical-steering-committee) and [GOVERNANCE.md](GOVERNANCE.md), under the [Technical Charter](CHARTER.md).

## Project Maintainers and Technical Steering Committee

The following Project Maintainers serve as the voting members of the Technical Steering Committee (TSC):

| GitHub | Scope |
| --- | --- |
| [@jwx0925](https://github.com/jwx0925) | Repository-wide stewardship and technical oversight |
| [@hittyt](https://github.com/hittyt) | Repository-wide stewardship and technical oversight |
| [@Pangjiping](https://github.com/Pangjiping) | Repository-wide stewardship and technical oversight |
| [@ninan-nn](https://github.com/ninan-nn) | Repository-wide stewardship and technical oversight |

## Component Maintainers

Component maintainers guide development and review contributions in their areas. Component maintainership alone does not confer TSC membership or voting rights. Project Maintainers may also maintain individual components.

The following assignments reflect the explicit path rules in [CODEOWNERS](.github/CODEOWNERS), which controls review routing. Paths without an explicit rule fall back to the Project Maintainers listed above.

| Area | Path | Maintainers |
| --- | --- | --- |
| Lifecycle server | `server/` | @Pangjiping, @hittyt, @jwx0925, @Generalwin, @ninan-nn |
| Execution daemon | `components/execd/` | @Pangjiping, @hittyt, @ninan-nn |
| Ingress gateway | `components/ingress/` | @Pangjiping, @hittyt, @Generalwin, @Spground |
| Egress controls | `components/egress/` | @Pangjiping, @hittyt, @jwx0925 |
| Kubernetes controller | `kubernetes/` | @Spground, @Generalwin, @fengcone, @kevinlynx, @ninan-nn, @hittyt, @Pangjiping |
| SDKs | `sdks/` | @ninan-nn, @jwx0925, @hittyt, @Pangjiping |
| API specifications | `specs/` | @jwx0925, @hittyt, @ninan-nn |
| OpenSandbox Enhancement Proposals | `oseps/` | @Spground, @Generalwin, @fengcone, @kevinlynx, @Pangjiping, @ninan-nn, @jwx0925, @hittyt |

## Membership Changes

Follow the nomination, advancement, and removal procedures in [GOVERNANCE.md](GOVERNANCE.md#becoming-a-maintainer). Update this file and [CODEOWNERS](.github/CODEOWNERS) together when membership or component ownership changes.
