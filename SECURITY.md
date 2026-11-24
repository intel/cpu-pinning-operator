<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Security Policy

Intel is committed to rapidly addressing security vulnerabilities affecting our
customers and providing clear guidance on the solution, impact, severity and
mitigation.

## Reporting a Vulnerability

Please report any security vulnerabilities in this project utilizing the guidelines
[here](https://www.intel.com/content/www/us/en/security-center/vulnerability-handling-guidelines.html).

## Supported versions

Security fixes go to `main` and the latest `1.x` release.

| Version | Supported |
|---------|-----------|
| `main` | Yes |
| Latest `1.x` release | Yes |
| Older releases | No. Upgrade to the latest release |

## Scope

In scope is the operator itself: the controller-manager, the mutating admission
webhook, the topology-probe pod, the injected `wait-for-balloons` init container, the
generated RBAC, and the Helm chart under `deploy/`.

The [threat model](docs/threat-model.md) enumerates the operator's assets, trust
boundaries, and known risks; [secure configuration](docs/secure-configuration.md)
covers the hardening defaults, what data the operator reads and stores, and how its
certificates are rotated and replaced.

Vulnerabilities in the operator's runtime dependencies (Kubernetes, containerd/CRI-O,
the NRI balloons plugin, cert-manager, the base images) belong to those
projects. If such an issue is exploitable specifically through this operator's
configuration, report it through the channel above.
