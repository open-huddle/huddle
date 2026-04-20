# Open Huddle Governance

This document describes how Open Huddle is governed: who makes decisions, how those decisions are made, and how people move between roles. It is deliberately lightweight at this pre-alpha stage and will evolve as the community grows.

## Principles

1. **Transparency.** Decisions happen in public, asynchronously, in issues and pull requests. Private channels are reserved for security, conduct, and personal matters.
2. **Meritocracy, earned.** Influence is granted based on sustained, high-quality contribution — not on seniority, affiliation, or declared intent.
3. **Documented decisions.** Significant technical and project decisions are recorded (Architecture Decision Records, PR descriptions, release notes) so that contributors joining later can understand the why.
4. **Compliance is non-negotiable.** Changes that weaken SOC 2 / HIPAA posture require explicit justification and maintainer approval.

## Roles

### Users

Anyone who deploys or uses Open Huddle. Users are encouraged to file bugs, request features, and ask questions in GitHub Discussions.

### Contributors

Anyone who has contributed code, documentation, design, triage, or review to the project. Contributors do not need a formal role to participate — opening a PR is enough.

### Reviewers

Trusted contributors who have demonstrated sustained, high-quality review activity in a specific area. Reviewers can `/lgtm` PRs and triage issues but cannot merge.

**Promotion criteria:**
- Sustained contribution over at least 3 months
- Deep familiarity with a subsystem or area
- Nominated by a maintainer; approved by simple majority of maintainers

### Maintainers

Contributors with merge rights. Maintainers are responsible for reviewing and merging PRs, triaging issues, cutting releases, and upholding the project's technical and compliance standards.

**Promotion criteria:**
- At least 6 months as an active contributor or reviewer
- Significant, high-quality contributions across multiple areas
- Nominated by an existing maintainer; approved by two-thirds of maintainers
- Committed to upholding the [Code of Conduct](CODE_OF_CONDUCT.md)

Current maintainers are listed in [MAINTAINERS.md](MAINTAINERS.md).

### Technical Steering Committee (TSC)

The TSC is a small group of senior maintainers responsible for the project's technical direction, scope, and cross-cutting architectural decisions. The TSC exists once there are at least five maintainers; until then, the maintainers collectively perform its function.

**Responsibilities:**
- Resolving technical disputes that cannot be settled at the PR level
- Approving changes to project scope (adding or removing core features)
- Approving changes to the core stack (a new database, a new runtime, etc.)
- Approving changes to this governance document
- Appointing and removing maintainers

**Composition:** 3–7 members, elected from among active maintainers for 12-month terms. No single employer may hold more than one-third of TSC seats, to keep the project vendor-neutral.

## Decision making

### Everyday decisions (lazy consensus)

Most decisions — bug fixes, small features, documentation, refactors — are made by **lazy consensus**. A PR is merged once:

- It has at least one approving review from a maintainer in the relevant area
- All discussion is resolved
- CI is green
- No other maintainer has raised a blocking objection

If objections are raised, the PR author and reviewers work to resolve them. If agreement cannot be reached, the decision escalates to the TSC.

### Significant decisions (RFC)

Changes that meet any of the following require a written proposal (an **RFC**) in a dedicated issue before implementation begins:

- New subsystems or services
- Changes to the public API or on-disk / on-wire formats
- Changes to the core stack (Go/React/Postgres/LiveKit/etc.)
- Changes to this governance document or the [Code of Conduct](CODE_OF_CONDUCT.md)
- Changes to licensing, compliance posture, or security model

An RFC must:

1. Describe the problem and the proposed solution
2. Enumerate alternatives considered
3. Call out compliance, security, and operational implications
4. Remain open for comment for at least 7 days

RFCs are approved by simple majority of the TSC (or of maintainers, before the TSC exists).

### Voting

When a vote is required (maintainer promotion, TSC election, RFC approval), votes are cast as comments on the relevant issue or PR using `+1`, `-1`, or `0`. Votes are open for at least 72 hours. Maintainers are expected to participate in votes within a reasonable timeframe or declare abstention.

## Conflicts of interest

Maintainers must recuse themselves from decisions in which they have a material conflict of interest (employer commercial interests, personal relationships, etc.). Disclose; do not hide.

## Code of Conduct

All participants in Open Huddle governance are bound by the [Code of Conduct](CODE_OF_CONDUCT.md). Enforcement actions that affect a maintainer's standing are decided by the TSC.

## Amendments

This document is amended by the RFC process described above. Proposed changes should be opened as a PR against this file, with a linked RFC issue.
