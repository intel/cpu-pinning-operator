# CPU Pinning Operator

Kubernetes operator that gives the pods you choose dedicated, NUMA-local CPU cores on Intel® Xeon® servers.

## ✨ Highlights

**Get more from the Intel® Xeon® servers you already own.** Each CPU-heavy workload you choose gets its own dedicated share of the server.

* **More from existing hardware** - Higher utilisation and lower cost per request, without buying more servers.
* **Predictable performance** - Important workloads get dedicated cores, kept apart from other pods; host processes and interrupts are not isolated.
* **No application changes** - Install once, choose what to prioritise, see the result on a live dashboard.
* **Works where you already run** - Kubernetes and Red Hat® OpenShift®.

## 🔍 Overview

The CPU Pinning Operator pins selected CPU-heavy pods to cores of their own, so they stop competing for CPU time with other pods on the node. You choose the pods by container name or by pod labels, in the namespaces you list; the workload manifests do not change.

The operator reads each node's CPU and NUMA topology, writes a per-node policy for the [NRI balloons plugin](https://github.com/containers/nri-plugins), and annotates every matched pod at admission. The plugin then places the pod's containers on their dedicated cores. Pods that are not matched keep running on the shared pool. Host processes and interrupts are not isolated.

This image runs the operator's controller manager and admission webhook. The same image provides the topology probe and the readiness init container, so a cluster only needs this one image from the operator.

## 🚀 Usage

The image is deployed by the operator's Helm chart; it is not meant to be run on its own.

```bash
git clone https://github.com/intel/cpu-pinning-operator.git && cd cpu-pinning-operator
helm install cpu-pinning-operator ./deploy/cpu-pinning-operator \
  -n cpu-pinning-operator-system --create-namespace \
  --set 'balloonPinningPolicy.match.byContainerName={<comma-separated-container-names>}' \
  --set 'balloonPinningPolicy.namespaces={<workload-namespace>}'
```

Prerequisites: Kubernetes or Red Hat® OpenShift® with NRI enabled in the container runtime, and cert-manager. The chart bundles the NRI balloons plugin. See the [installation guide](https://github.com/intel/cpu-pinning-operator/blob/main/deploy/cpu-pinning-operator/README.md) for every option, including OpenShift.

## 🏷️ Tags

| Tag | Meaning |
|---|---|
| `<version>` (for example `1.0.0`) | A release, built from the `v<version>` tag of the repository |

Platform: `linux/amd64`. Pin the image by digest in production (`--set image.digest=sha256:<digest>`).

## 🔐 Verifying the image

Every release image carries a keyless Sigstore cosign signature, a signed SPDX SBOM attestation and SLSA build provenance, all made by the repository's release workflow:

```bash
IMAGE=docker.io/intel/cpu-pinning-operator@sha256:<digest>
IDENTITY='^https://github\.com/intel/cpu-pinning-operator/\.github/workflows/release\.yml@refs/tags/v.+$'
ISSUER=https://token.actions.githubusercontent.com

cosign verify --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER" "$IMAGE"
cosign verify-attestation --type spdxjson \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER" "$IMAGE"
```

The image runs as a non-root user on a minimal base without a shell, and ships the license texts of everything linked into the binary under `/licenses`.

## 🔗 Links

- Source code and documentation: https://github.com/intel/cpu-pinning-operator
- Security policy and vulnerability reporting: https://github.com/intel/cpu-pinning-operator/blob/main/SECURITY.md

## License

The CPU Pinning Operator is licensed under the Apache License, Version 2.0. Third-party components carry their own licenses; see the `NOTICE` file in the image under `/licenses`.

Copyright © 2026 Intel Corporation. Intel, the Intel logo and Xeon are trademarks of Intel Corporation or its subsidiaries. Other names and brands may be claimed as the property of others.
