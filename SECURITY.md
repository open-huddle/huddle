# Security Policy

The Open Huddle project takes the security of its users and their data seriously. Because Open Huddle is designed to be deployed inside customer infrastructure — often handling regulated data under SOC 2 and HIPAA — we ask that vulnerabilities be reported privately and responsibly.

## Reporting a vulnerability

**Do not open a public GitHub issue for security problems.**

Report suspected vulnerabilities through one of the following private channels:

- **Email:** [security@open-huddle.org](mailto:security@open-huddle.org)
- **GitHub Security Advisory:** [Report a vulnerability](https://github.com/open-huddle/huddle/security/advisories/new)

Please include, where possible:

- A description of the issue and its impact
- Steps to reproduce, or a proof-of-concept
- The version, commit, or deployment configuration affected
- Any suggested mitigation

If you need to send sensitive material, request our PGP key by email and we will respond with current fingerprint and key material.

## Our commitment

We will:

1. **Acknowledge** your report within **3 business days**.
2. **Triage and assign a severity** (CVSS v3.1) within **7 business days**.
3. Keep you informed of progress toward a fix.
4. Credit you in the advisory and release notes, unless you prefer to remain anonymous.
5. Coordinate disclosure timing with you — our default embargo is **90 days** from initial report, or earlier if a fix is released and deployed.

## Scope

In scope:

- The Open Huddle server and client code in this repository
- Official Helm charts and Docker images published by the project
- Documented default configurations

Out of scope:

- Third-party components (Keycloak, LiveKit, PostgreSQL, etc.) — report those upstream
- Issues requiring physical access to a deployment
- Social engineering of contributors or users
- Denial of service through resource exhaustion without a clear amplification vector
- Self-inflicted misconfiguration of a deployment

## Safe harbor

We consider good-faith security research conducted under this policy to be authorized. We will not pursue legal action against researchers who:

- Make a good-faith effort to avoid privacy violations, data destruction, and service disruption
- Only interact with accounts or data they own, or have explicit permission from the account holder to access
- Report the vulnerability promptly and do not disclose it publicly before coordinated disclosure

## Supported versions

Open Huddle is currently pre-alpha. Until the first stable release, only the latest commit on `main` is supported for security fixes. A supported-versions table will be published once the project reaches a tagged release.

## Hardening and compliance

For guidance on deploying Open Huddle in a SOC 2 or HIPAA environment, see the deployment and compliance documentation (coming soon) in `docs/`.
