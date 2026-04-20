# Release and Lifecycle Policy

Open Huddle is built for enterprise, self-hosted environments. This document outlines our release cadence, backporting guarantees, and deprecation policies to ensure stability and SOC 2 / compliance predictability.

## Release Cadence

1. **Major Releases (v1.0.0, v2.0.0)** 
   - Contain significant new subsystems or backward-incompatible breaking changes.
   - Released asynchronously, no more than once per year.
2. **Minor Releases (v1.1.0, v1.2.0)**
   - Contain new non-breaking features, optimizations, and substantial bug fixes.
   - Released on a predictable quarterly cadence.
3. **Patch Releases (v1.1.1, v1.1.2)**
   - Contain zero-downtime bug fixes, security patches, and minor non-breaking tweaks.
   - Released as needed.

## Provenance & Artifacts

To satisfy SOC 2 software supply chain controls:
- Every tagged release must go through automated CI/CD pipelines. No manual builds.
- Docker images must be signed using Sigstore (Cosign).
- Software Bill of Materials (SBOMs) must be generated and attached to each GitHub Release as SPDX JSON.

## Backporting & Support Lifecycles

Self-hosted deployments require a reliable patch window:
- **Latest Minor**: Fully supported. All security and critical bug fixes go here.
- **Previous Minor**: Supported for **Security fixes only** for a period of 6 months following the release of a newer minor version.
- **End of Life (EOL)**: Any release older than the previous minor version.

*Note: As Open Huddle is currently pre-alpha, stable backports and SLAs are not yet enforced.*

## Deprecation Policy

When deprecating configurations, APIs, or officially supported deployment methods (e.g., specific Kubernetes versions):
1. The feature must be explicitly marked as `DEPRECATED` in the release notes and documentation.
2. A console/log warning should be output for administrators.
3. The feature cannot be removed for at least **one full Major release** or **6 months**, whichever is longer, to give enterprises a sufficient window to migrate.
