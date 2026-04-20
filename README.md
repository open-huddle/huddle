# Open Huddle

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Status: Pre-Alpha](https://img.shields.io/badge/status-pre--alpha-orange.svg)](#status)
[![Contributor Covenant](https://img.shields.io/badge/Contributor%20Covenant-2.1-4baaaa.svg)](CODE_OF_CONDUCT.md)

> Open, self-hostable team collaboration. Messaging, channels, voice and video — in your infrastructure, with your data.

Open Huddle is an open-source alternative to proprietary collaboration suites such as Microsoft Teams, built for organizations that need to own their communication stack. It is designed from day one for enterprise-scale self-hosted deployments (targeting 10,000+ users), with **SOC 2** and **HIPAA** compliance as first-class concerns rather than retrofits.

## Status

**Pre-alpha. Under active initial construction.** The project is not yet usable. Interfaces, schemas, and architecture are subject to change without notice.

## Planned features

- Rich text messaging with threads, reactions, and mentions
- Public and private channels
- One-to-one and group voice and video calls (WebRTC)
- Full-text search across messages, channels, and attachments
- Enterprise SSO (OIDC, SAML, LDAP / Active Directory) via Keycloak
- Compliance-grade audit logging
- Self-hosted deployment via Helm (Kubernetes) or Docker Compose
- End-to-end encrypted messaging (MLS — future)

## Design principles

1. **100% OSI-approved FOSS.** No SSPL, no BSL, no source-available-but-not-free dependencies.
2. **Self-hosted first.** Customers own their infrastructure and their data. The project is not built around a hosted service.
3. **Compliance by architecture.** Event-sourced audit trail, mTLS east-west, encryption at rest, least-privilege access — built in, not bolted on.
4. **Enterprise-scale by default.** Every design decision assumes a 10,000-user deployment is possible on the chosen stack.
5. **Standard building blocks.** Where a well-maintained FOSS component exists, use it. Do not reinvent Keycloak, LiveKit, or OpenSearch.

## Technology

Summarized; full rationale lives in [`docs/`](docs/) (coming soon).

| Layer | Technology |
|---|---|
| Backend | Go, chi, Connect-Go, Ent + Atlas (planned) |
| Frontend | React, TypeScript, Vite, Ant Design, Connect-ES |
| Realtime voice / video | LiveKit (SFU), coturn |
| Data | PostgreSQL, Valkey, SeaweedFS |
| Search | OpenSearch, Apache Tika |
| Events | NATS JetStream, Debezium |
| Identity | Keycloak |
| Secrets | OpenBao |
| Gateway | Traefik (Kubernetes Gateway API) |
| Observability | OpenTelemetry, Prometheus, Loki, Tempo, Grafana |
| Security | Linkerd (mTLS), Falco, OPA / Gatekeeper, Trivy, ClamAV |
| Platform | Kubernetes + Helm, Docker Compose |

## Getting started

Development instructions will be published once the initial scaffold is ready. Follow the repository or watch for the first tagged release.

## Contributing

We welcome contributions — see [`CONTRIBUTING.md`](CONTRIBUTING.md) and our [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md). All contributions must be signed off per the [Developer Certificate of Origin](https://developercertificate.org/).

## Security

Please do **not** report security vulnerabilities through public GitHub issues. See [`SECURITY.md`](SECURITY.md) for the responsible disclosure process.

## License

Licensed under the [Apache License, Version 2.0](LICENSE). See [`NOTICE`](NOTICE) for attribution requirements.
