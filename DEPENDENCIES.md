# Dependency and License Policy

Open Huddle targets strict compliance and heavily regulated enterprise deployments (SOC 2, HIPAA, FedRAMP). Consequently, our software supply chain and third-party dependencies are strictly controlled to avoid viral licensing, legal ambiguity, and unvetted code.

## Allowed Licenses

All incoming code, linked libraries, and frontend packages MUST be licensed under OSI-approved permissive licenses. The permitted list includes:
- Apache 2.0
- MIT
- BSD (2-Clause and 3-Clause)
- ISC

## Prohibited Licenses

The following licenses are **strictly forbidden** from being linked, bundled, or compiled into Open Huddle:
- **GPL, AGPL, LGPL** (or any other copyleft licenses that mandate disclosure of proprietary code).
- **SSPL, BSL** (Server Side Public License, Business Source License, or "source available" non-OSI licenses).
- Unlicensed or improperly licensed code.

### Exception: Isolated External Infrastructure

We may recommend or utilize containerized infrastructure (e.g., database servers like Valkey or PostgreSQL, logging stacks like Grafana Loki) that operate under external licenses, provided they are:
1. Run as separate, unmodified daemons over standard generic protocols (e.g., HTTP, TCP, Redis protocol).
2. Not statically or dynamically linked into our application binaries.

## Adding a New Dependency

1. **Audit First**: Verify the dependency's license in its root repository.
2. **Review Risk**: Ensure the dependency is actively maintained and does not introduce massive transitive dependency graphs unnecessarily.
3. **Approval**: All new dependencies require explicit PR approval from an `@open-huddle/maintainers` representative.

## Third-Party Notices & SBOMs

To fulfill attribution requirements and enterprise procurement audits:
1. All permitted dependencies must be correctly attributed.
2. The `NOTICE` file in the root directory must be regenerated when dependencies are added, updated, or removed.
3. CycloneDX or SPDX Software Bill of Materials (SBOM) manifests are generated automatically by CI on every release and provided alongside our artifacts.
