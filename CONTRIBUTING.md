# Contributing to Open Huddle

Thank you for your interest in contributing. Open Huddle is an open-source project that depends on contributions from the community. This document explains how to participate.

## Code of Conduct

All participants in this project are expected to follow our [Code of Conduct](CODE_OF_CONDUCT.md). By contributing, you agree to uphold it.

## Developer Certificate of Origin (DCO)

Open Huddle uses the [Developer Certificate of Origin](https://developercertificate.org/) rather than a Contributor License Agreement. Every commit must be signed off, certifying that you wrote the contribution or otherwise have the right to submit it under the project's license.

Sign off each commit with `git commit -s`. The sign-off appears as a trailer in the commit message:

```
Signed-off-by: Your Name <your.email@example.com>
```

Pull requests with unsigned commits will be rejected by automation. See the [DCO text](https://developercertificate.org/) for full wording.

## Reporting bugs

Before opening a bug report:

1. Search [existing issues](https://github.com/open-huddle/huddle/issues) to avoid duplicates.
2. Confirm the bug against the latest `main` branch where possible.
3. Use the **Bug report** issue template and fill in every section.

## Proposing features

Open a **Feature request** issue first, before writing code. Large, unsolicited PRs are at high risk of rejection — we would rather discuss the design with you up front than ask you to throw away work.

## Reporting security vulnerabilities

Do **not** open a public issue for security problems. Follow the process in [`SECURITY.md`](SECURITY.md).

## Development setup

Detailed instructions will be added once the initial scaffold lands. At minimum you will need:

- Go (latest stable)
- Node.js (LTS) and pnpm
- Docker + Docker Compose
- `make`

## Coding standards

- **Go:** formatted with `gofumpt`, linted with `golangci-lint`. Run `make lint` before submitting.
- **TypeScript / React:** formatted with Prettier, linted with ESLint. Run `pnpm lint` before submitting.
- **Commit messages:** follow [Conventional Commits](https://www.conventionalcommits.org/). Examples: `feat(messaging): add reactions`, `fix(auth): handle expired refresh tokens`, `docs(readme): update stack table`.
- **Tests:** new features and bug fixes require tests. PRs without tests will be asked to add them.

## Pull request process

1. Fork the repository and create a topic branch from `main`.
2. Keep changes focused. One logical change per PR.
3. Ensure your commits are signed off (`git commit -s`).
4. Run the full test suite and linters locally.
5. Fill in the PR template, including the compliance checklist where applicable.
6. A maintainer will review within a reasonable timeframe. Expect feedback and iteration.

## Licensing of contributions

By contributing, you agree that your contributions will be licensed under the [Apache License, Version 2.0](LICENSE).
