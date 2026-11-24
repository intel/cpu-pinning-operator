<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->


# Development

```bash
make hooks          # enable the pre-commit gate — do this first
make build          # go build
make test           # unit + chart-render tests, plus envtest guards when binaries are present
make test-crd       # envtest guards only; fetches control-plane binaries
make generate       # regenerate CRDs, deepcopy, and the RBAC/webhook baselines
make docker-build   # cpu-pinning-operator:latest
make lint deadcode  # reporting only, always exit 0
```

`make fmt`, `make vet`, `make helm-lint`, `make helm-template`, and the
`helm-install` / `helm-upgrade` / `helm-uninstall` trio round out the set.

The image is one multi-stage Dockerfile: a Go builder stage and a minimal runtime stage with no shell. Every dependency
resolves from the module proxy, so `make docker-build` needs no extra build context.
The injected `wait-for-balloons` init container reuses this image and runs
`/manager wait-for-balloons`, so there is no second image to build.

---

## Code generation

`make generate` runs `controller-gen` over four marker-driven artifacts. All four sit
under a drift gate: CI re-runs generation and fails on any diff; the pre-commit hook
does the same when `controller-gen` is installed.

| Artifact | Source of truth | Output |
|---|---|---|
| `DeepCopy` methods | `+kubebuilder:object` on the API types | `api/v1alpha1/zz_generated.deepcopy.go` |
| CRDs | `api/v1alpha1/*_types.go` structs and validation markers | `deploy/cpu-pinning-operator/crds/*.yaml` |
| RBAC `ClusterRole` | `+kubebuilder:rbac` in `internal/controller/` and `internal/webhook/` | `deploy/generated/role.yaml` |
| Webhook configs | `+kubebuilder:webhook` in `internal/webhook/` | `deploy/generated/manifests.yaml` |

The markers make the code authoritative for the permission set and the webhook shape:
a reconciler that reaches for a new API resource declares it next to the call.

### `deploy/generated/` is a baseline, not a shipped manifest

The chart hand-templates `rbac.yaml`, `mutatingwebhook.yaml`, and
`validatingwebhook.yaml` because they carry Go template expressions controller-gen
cannot emit: release-scoped names, the cert-manager CA-inject annotation, the
chart-value `failurePolicy` and `timeoutSeconds`, and the bootstrap
`namespaceSelector` that the BalloonPinningPolicy reconciler overwrites at runtime.

So `controller-gen` writes the canonical set to `deploy/generated/`, and
`test/generated/` fails when the chart templates diverge from it. RBAC must match the
flattened `(apiGroup, resource)` to verbs set exactly — no more, for least privilege,
and no less, or the operator breaks. Webhooks compare only the marker-expressible
shape.

Changing a permission or a webhook shape: edit the marker, `make generate` and commit
`deploy/generated/`, mirror it into the chart template, then `make test`.

---

## Testing

Unit tests run anywhere. The envtest guards (`test/crdinstall/` and the
webhook-reconcile guard in `internal/controller/`) need control-plane binaries;
`make test-crd` fetches them. See
[e2e-internals.md](e2e-internals.md#crd-install-regression-guard).

`test/chart/` runs `helm template` and asserts invariants a chart refactor could
break silently. The subchart archives are committed under
`deploy/cpu-pinning-operator/charts/`, so a full render needs no fetch and no
`helm dependency` step. Without helm on `PATH` these tests **skip**, keeping an
offline `make test` green — but they **fail** when `CI` is set.

Bumping a subchart version is deliberate work with its own testing, not a side
effect of a helm command: change the version in `Chart.yaml`, run
`helm dependency update deploy/cpu-pinning-operator` (which rewrites `Chart.lock`,
fetches the new archive and deletes the superseded one).
If you ever put an archive in `charts/` by hand, delete the old one yourself: a
leftover `.tgz` is still a subchart and still renders.

One invariant worth naming: the manager's `readinessProbe` must stay `/readyz` and
never `/healthz`. `/readyz` gates on `informer-cache-sync` (`cmd/main.go` — the
`AddReadyzCheck("informer-cache-sync", …)` call), so an
HA follower is not Ready until its policy index and topology cache are populated. A
`/healthz` probe would report Ready at process start and let the webhook admit matched
pods unmutated during cold start.

End-to-end tests need a real, NRI-enabled cluster with the operator installed —
envtest and kind cannot pin CPUs — so they sit behind the `e2e` build tag and use the
ambient kubeconfig:

```bash
make test-e2e                                     # provisioning, webhook, physical-cores, metrics
E2E_RUN_PINNING=1 make test-e2e                   # assert NRI actually pinned
E2E_RUN_PINNING=1 E2E_SATURATE=1 make test-e2e    # oversubscribe, assert no collisions
E2E_DASHBOARDS=1 make test-e2e                    # per-node dashboard ConfigMaps
E2E_RESTART=1 make test-e2e                       # delete the operator pod, assert recovery
E2E_STRICT= make test-e2e                         # skip, don't fail, on a half-set-up cluster
```

`make test-e2e` sets `E2E_STRICT=1`: a test that could not run because the cluster was
not set up for it fails instead of skipping. Opt-in gates and capabilities the hardware
lacks still skip.

Pinning is asserted against the `NodeResourceTopology` CR, which is what NRI actually
did; a stamped annotation is only admission intent. The suite reads each node's own
topology labels instead of assuming a shape. The full case list and every `E2E_*`
variable are in [e2e-internals.md](e2e-internals.md).

`make deadcode` and `make lint` cover what `go vet` cannot: unused exported symbols
and unreachable features. Neither is part of the pre-commit gate and both always exit
0. A finding is not automatically a defect, so triage each one.

---

## Repository layout

| Path | Purpose |
|---|---|
| `cmd/main.go` | Manager wiring: scheme, controllers, webhook server |
| `api/v1alpha1/` | `BalloonsClusterConfig` and `BalloonPinningPolicy` types |
| `internal/topology/` | Per-node `/sys` probe and parser |
| `internal/balloons/` | Balloon-type generation and per-node `BalloonsPolicy` assembly |
| `internal/controller/` | Node, cluster-config, pinning-policy, NRT-metrics, and pod-drift reconcilers |
| `internal/webhook/` | Pod mutating webhook: annotation stamp and init injection |
| `internal/match/` | Namespace-scoped match index, span and multiplier extraction, `snap()` |
| `internal/readiness/` | Per-node readiness endpoint and `wait-for-balloons` poller |
| `internal/dashboard/` | Per-node Grafana canvas rendering and ConfigMap writer |
| `internal/metrics/` | Prometheus metric registration |
| `deploy/cpu-pinning-operator/` | Helm chart |
| `deploy/generated/` | Marker-generated RBAC and webhook baselines for the drift guard |
| `test/generated/` | Drift guard: chart templates vs those baselines |
| `test/e2e/` | End-to-end tests, build tag `e2e`, real NRI cluster |

---

## OpenShift and Pod Security

Every workload runs unprivileged — the manager, the injected init container, and the
topology probe, which only reads world-readable `/sys` and `/proc/cpuinfo` and runs in
the operator's own namespace, not `kube-system`.

* **OpenShift** - The manager and init container satisfy `restricted-v2`. The probe's fixed uid 1000 needs the custom SCC that `openShift.enabled=true` renders, and the balloons plugin its subchart's own SCC: install with `values-openshift.yaml` ([chart README](../../deploy/cpu-pinning-operator/README.md#installing-on-openshift--okd)).
* **Pod Security Admission** - The operator's own pods are `restricted`-compatible;
  the bundled balloons plugin is not (`hostPath`). Set
  `podSecurityStandards.enabled=true` with `namespace.create=true` to have the chart
  stamp the PSA labels, and run the plugin in another namespace
  ([secure-configuration.md](../secure-configuration.md#start-here-the-hardened-install)).

The manager's default `securityContext` in `values.yaml` is what passes those gates:
`readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`,
`runAsNonRoot: true`, `runAsGroup: 65532`, `capabilities.drop: [ALL]`, and
`seccompProfile.type: RuntimeDefault`.

> [!IMPORTANT]
> The read-only root filesystem needs one writable path. The chart mounts an
> `emptyDir` at `/tmp`, which Go's `os.TempDir()` resolves to and which also covers
> the default `webhook.certDir`. If you move `webhook.certDir` outside `/tmp` while
> keeping `readOnlyRootFilesystem: true`, add a writable volume for it.
