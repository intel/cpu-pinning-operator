<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Upgrade and compatibility

## Compatibility

CI never exercises pinning: no job runs a kubelet or an NRI-enabled runtime, and its
API tests use `envtest` at one version. Live pinning was tested by the maintainers with
the `-tags e2e` suite (pinning, mutating, restart, metrics, dashboard, guard-active-pods
and drift-restart gates) on three Intel® Xeon® processor-based targets (a multi-node
cluster, a 2-NUMA node and a 4-NUMA node), all on containerd 2.1.4.

| Component | Version | Status | Traces to |
|---|---|---|---|
| Kubernetes API server | 1.37.0 | tested: `envtest`, API only | `ENVTEST_K8S_VERSION` in `Makefile` |
| Kubernetes API server | 1.33.5 | tested: live pinning | maintainers' e2e run |
| Kubernetes API server | 1.29+, the stated floor | **untested** | a claim, not a test |
| kubelet, containerd, NRI on | 1.29.15, containerd 2.1.4 | tested: live pinning | maintainers' e2e run |
| containerd, NRI on | 2.1.4 | tested: live pinning | maintainers' e2e run |
| containerd, NRI on | other versions | **untested** | — |
| CRI-O, NRI on | 1.35 on OpenShift 4.22.4 | tested: install, plugin SCC admission and live pinning | [chart README, OpenShift](../deploy/cpu-pinning-operator/README.md#installing-on-openshift--okd) |
| CRI-O | other versions | **untested** | — |
| NRI balloons plugin | v0.15.2 | vendored; live pinning tested | `Chart.lock`, `charts/` |
| cert-manager | v1.20.3 | tested: vendored and installed | `Chart.lock`, `charts/` |
| Helm | v3.17.3 | tested: CI renders the chart | `.github/workflows/val-unit-tests.yml` |
| Helm | 3.14+, the stated floor | **untested** | a claim, not a test |

The 1.29.15 kubelet ran against a newer API server than the upstream version-skew policy
supports: that row is evidence for the kubelet and containerd pair, not for the skew.

Run the plugin on containerd 2.1.4, the tested version. On containerd 2.0.5 the plugin
can freeze the node under pod churn.

## Upgrade the chart

Helm installs `crds/` once and never updates them, on upgrade or rollback. Apply the
new checkout's CRDs **before** `helm upgrade`, so the chart's `default` policy and
`cluster` config are written against the new schema, then upgrade and check; an empty
diff passes:

```bash
kubectl apply -f deploy/cpu-pinning-operator/crds/
helm upgrade <release> ./deploy/cpu-pinning-operator -n <namespace> --wait
```

```console
$ kubectl diff -f deploy/cpu-pinning-operator/crds/ && echo "CRDs in sync with the chart"
CRDs in sync with the chart
```

The bundled plugin's `balloonspolicies.config.nri` and `noderesourcetopologies` CRDs ship in
the subchart's `crds/` and are not upgraded by Helm either. When the plugin version
changes, apply them the same way:
`tar xzf deploy/cpu-pinning-operator/charts/nri-resource-policy-balloons-*.tgz` and
`kubectl apply -f nri-resource-policy-balloons/crds/`.

Skip the CRD step and the API server rejects any field the stale CRD lacks:

```console
$ kubectl apply -f policy.yaml
Error from server (BadRequest): error when creating "policy.yaml": BalloonPinningPolicy in version "v1alpha1" cannot be handled as a BalloonPinningPolicy: strict decoding error: unknown field "spec.futureKnob"
```

The chart sets no rollout strategy, so Kubernetes defaults apply: `maxUnavailable`
25% rounds down to zero, `maxSurge` 25% rounds up to one. A new pod
must pass readiness, which includes `webhook-serving`, before an old one goes, so
the webhook endpoint survives the rollout at one replica or two.

Whenever no endpoint is Ready — the certificate trap below, or a crash with no
standby — the webhook's `failurePolicy: Ignore` admits a matched pod **unpinned**,
beyond `enforcePinning`'s reach. Run two replicas.

## Pods that are already pinned

The pod mutating webhook runs on `CREATE` only, so an upgrade re-pins nothing: a
running pod keeps the cpuset and hash stamped at admission. `spec.driftPolicy`
decides what reaches a pod the new configuration would place differently:

| `driftPolicy` | What the operator does |
|---|---|
| `eventsOnly`, the default | One Warning `Drifted` event per stale pod; `cpu_pinning_pods_drifted_total` increments. Nothing re-pins — restart the workload yourself |
| `restart` | Also evicts each drifted pod through the eviction API, so it re-admits under the new configuration. PodDisruptionBudget-aware, paced by default at one per 10s, burst 2 |

List them: `kubectl get events -A --field-selector reason=Drifted`.

## Certificates

Under the default `certManager.enabled: true` an upgrade needs no certificate
step: `Issuer` and `Certificate` are chart objects, cert-manager renews in
place, and the manager reloads without a restart. Lifecycle:
[TLS and certificates](secure-configuration.md#tls-and-certificates).

Traps:

- Changing `certManager.certificate.secretName` mints a new Secret. The manager
  mounts it and cannot start without it, so new pods stay in `ContainerCreating`
  and `helm upgrade --wait` looks hung.
- With `certManager.enabled: false`:
  - The chart forces the `BalloonPinningPolicy` guard webhook to
    `failurePolicy: Ignore`, whatever
    `webhook.validatingWebhookConfiguration.failurePolicy` says, unless you set
    `webhook.caBundle`. Without it the `caBundle` is a placeholder the API server
    cannot call, so the guard blocks no delete and no narrowing edit while pods depend
    on the policy.
  - Supply `webhook-server-cert` yourself, with the names
    [TLS and certificates](secure-configuration.md#tls-and-certificates) lists, or the
    rollout never becomes Ready.

## Roll back

Save the values and the manager image first, so a rollback can restore them exactly:

```bash
helm get values <release> -n <namespace> > values-before-upgrade.yaml
kubectl get deploy -n <namespace> -l app.kubernetes.io/name=cpu-pinning-operator \
  -o jsonpath='{.items[0].spec.template.spec.containers[0].image}'
helm rollback <release> <revision> -n <namespace>
```

A rollback reverts what Helm tracks. The chart registers no rollback hook, so four
things survive it:

| Survives | Why | What to do |
|---|---|---|
| CRDs | `crds/` is install-only here too | Apply the older chart's `crds/` by hand |
| Operator node labels | The strip Job is a `post-delete` hook: uninstall only | Leave them, or strip them by hand below |
| Per-node `BalloonsPolicy` CRs | The operator creates them, Helm does not | Nothing: the rolled-back manager rewrites each and prunes any node that stops qualifying |
| Running pinned pods | Pinning happens at pod `CREATE` | Restart the workload, or let `driftPolicy: restart` evict it |

The prefixed keys on your nodes:

```console
$ kubectl get nodes -o json | jq -r '[.items[].metadata.labels|keys[]
    |select(startswith("cpu-pinning-operator.intel.com/"))]|unique[]'
cpu-pinning-operator.intel.com/amx
cpu-pinning-operator.intel.com/balloons-enabled
cpu-pinning-operator.intel.com/cpu-allocation
cpu-pinning-operator.intel.com/cpu-manager-conflict
cpu-pinning-operator.intel.com/max-span
cpu-pinning-operator.intel.com/memory-per-numa-bytes
cpu-pinning-operator.intel.com/node-class
cpu-pinning-operator.intel.com/numa-nodes
cpu-pinning-operator.intel.com/package-topology
cpu-pinning-operator.intel.com/physical-cpus
cpu-pinning-operator.intel.com/reserved-cpus
cpu-pinning-operator.intel.com/threads-per-core
```

Strip those and the `topology-cache` annotation with the commands in
[Cleanup](troubleshooting.md#cleanup). Leave the two the operator only reads and you
set: `cpu-pinning-operator.intel.com/disabled`, and the class override named by
`spec.classification.overrideLabel`.

## Uninstall

`helm uninstall` runs two hooks whose phase matters. `pre-delete` removes the guard
`ValidatingWebhookConfiguration` before the chart's default policy, because a
registered `failurePolicy: Fail` config with a dead backend would deny that DELETE
and fail the uninstall. `post-delete` strips the node labels after the manager
Deployment is gone, so nothing re-labels behind it.

Both need `preDeleteHook.enabled` and `rbac.create`; with either off, do the two
steps by hand from the header comment of `templates/pre-delete-hook.yaml`. An
uninstall never removes the CRDs.
