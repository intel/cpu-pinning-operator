<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Choosing what gets pinned

Two cluster-scoped kinds: `BalloonsClusterConfig` (a singleton named `cluster`, short
name `bcc`) holds cluster-wide defaults; `BalloonPinningPolicy` (short name `bpp`,
one per workload family) selects namespaces and pods. A pod is pinned when its
namespace matches and a container name or a pod label matches. Nobody edits the
workload: admission adds the balloon annotation, a `wait-for-balloons` init
container and node affinity.

Source: [configuration.md](https://github.com/intel/cpu-pinning-operator/blob/main/docs/configuration.md).

## The chart's policy

With `balloonPinningPolicy.create=true` (the default) the chart creates one policy,
`default`, and fails to render until `balloonPinningPolicy.match` and
`balloonPinningPolicy.namespaces` (or `namespaceSelector`) are set.

| Goal | Values |
|---|---|
| Match by container name | `--set 'balloonPinningPolicy.match.byContainerName={<container-example>}' --set 'balloonPinningPolicy.namespaces={<workload-namespace>}'`. List several names; an entry containing `*`, `?` or `[` is a glob |
| Match on pod labels | `--set 'balloonPinningPolicy.match.byPodLabels.app=<app-example>'`; a pod matches on **any** one pair |
| Other namespaces | `balloonPinningPolicy.namespaces`; the webhook bootstraps on these exact names (`webhook.targetNamespaces` overrides) |
| Namespaces by label | `balloonPinningPolicy.namespaceSelector`, below |
| Refuse pods that cannot fit | `balloonPinningPolicy.capacityCheck.mode: enforce` (default `warn`) |
| Manage policies yourself | `--set balloonPinningPolicy.create=false`, then apply your own |

Namespaces by label, so onboarding a namespace is a label rather than a policy edit.
An empty selector (`{}`) matches every namespace, so scope it:

```yaml
balloonPinningPolicy:
  namespaceSelector:
    matchLabels:
      cpu-pinning-operator.intel.com/pinning: enabled
webhook:                 # optional: same selector before the first reconcile
  namespaceSelector:
    matchLabels:
      cpu-pinning-operator.intel.com/pinning: enabled
```

## How a request becomes a balloon

1. Balloon size is the CPU request of what was matched, in whole cores rounded up. A
   container-name match sizes by the matched container alone; a whole-pod match sums
   every container. An 8-CPU `<container-example>` container with a 2-CPU sidecar
   needs 8 dedicated cores under a name-matched policy, 10 under a whole-pod match.
   Requests are never rewritten.
2. The span (how many NUMA nodes the pod covers) is read from the first configured
   source that yields an integer: the pod annotation named in `annotation` (the chart
   sets `cpu-pinning-operator.intel.com/tp`), then `argRegex` against the joined
   container args, then the container env var `envVar`, then `default` (1). A source
   a policy omits is skipped. Under the chart's policy the quickest way to set the
   span for any workload is the annotation, e.g. `cpu-pinning-operator.intel.com/tp: "2"`
   on the pod template.
3. A multiplier is read the same way (annotation
   `cpu-pinning-operator.intel.com/multiplier`, its own `argRegex`, `envVar`,
   `default` 1).

   The chart's default intent is set up for model servers that take tensor-/pipeline-
   parallel flags (`--tensor-parallel-size` / `-tp`, `--pipeline-parallel-size` /
   `-pp`, env `VLLM_*`); for other workloads set the span with the annotation or your
   own `argRegex`/`envVar`.
4. The final span is `span × multiplier`, rounded up to 1, 2, 4 or 8. The limit is the
   `cpu-pinning-operator.intel.com/max-span` label of the pod's node, or for a pod not
   yet scheduled, the largest max-span any balloons-enabled node advertises. Above that
   limit, `capacityCheck.mode: enforce` rejects the pod; `warn` pins it at the capped
   span with a `CapacityCheckWarning` event; `off` caps it silently. The pod gets
   `balloon-span<N>`.
5. A pod requesting any device or extended resource ignores all of this and gets
   `balloon-device`, which prefers CPUs local to its device when the plugin can resolve
   the device's NUMA node (that needs the kubelet `KubeletPodResourcesGet` gate).

## Policy fields that change behaviour

| Field | Default | Effect |
|---|---|---|
| `spec.capacityCheck.mode` | `warn` | `enforce` denies a pod that cannot fit; `warn` admits it with a `CapacityCheckWarning` event; `off` skips the check |
| `spec.scheduling.enforcePinning` | `true` | Deny a matched pod when no node is provisioned yet or pinning fails; an admitted pod gets required affinity to `balloons-enabled=true` nodes |
| `spec.scheduling.nodeFeatures` | chart: `cpu-pinning-operator.intel.com/amx: prefer` | Per feature label: `prefer`, `require` or `ignore` |
| `spec.match.containerSelection` | `wholePod` | `largestContainer` pins the highest-CPU candidate; under `wholePod` a name glob hitting several containers skips the pod as ambiguous |
| `spec.intent.coLocate` | `""` | `package` keeps the span in one CPU package; `numa` spreads it across NUMA nodes |
| `spec.driftPolicy` | `eventsOnly` | `restart` evicts a pod whose pinned config changed |
| `spec.guardActivePods` | `false` | Reject deleting the policy, or `namespaces`/`match` edits, that would orphan pinned pods |

## A custom policy

For a workload that states its NUMA span in its own flag (install the chart with
`--set balloonPinningPolicy.create=false`, or keep both and mind the ordering below):

```yaml
apiVersion: cpu-pinning-operator.intel.com/v1alpha1
kind: BalloonPinningPolicy
metadata:
  name: my-app
spec:
  namespaces: [<workload-namespace>]
  match:
    byContainerName: [<container-example>]
  intent:
    span:
      annotation: cpu-pinning-operator.intel.com/tp
      argRegex: '(?:^|\s)--numa-nodes(?:=|\s+)(\d+)'
      default: 1
```

Label-matched, span from an env var, AMX required, restart on drift:

```yaml
apiVersion: cpu-pinning-operator.intel.com/v1alpha1
kind: BalloonPinningPolicy
metadata:
  name: my-app-amx
spec:
  namespaceSelector:
    matchLabels:
      cpu-pinning-operator.intel.com/pinning: enabled
  match:
    byPodLabels:
      app: <app-example>
    containerSelection: largestContainer
  intent:
    span:
      envVar: NUMA_SPAN
      default: 1
  scheduling:
    nodeFeatures:
      cpu-pinning-operator.intel.com/amx: require
  driftPolicy: restart
```

**Overlapping policies.** When several policies match one pod, the name that sorts
first (byte-wise) wins. Prefix names to set the order, specific before broad:
`00-db-large`, `01-db-default`, `02-catch-all`. The chart's `default` sorts after
digits, so a numbered policy beats it.

## Cluster defaults (`BalloonsClusterConfig` `cluster`)

The chart owns this object; set it through `balloonsClusterConfig.*` values, or the
next `helm upgrade` reverts a hand edit.

| Field | Default | Effect |
|---|---|---|
| `spec.balloon.cpuAllocation` | `physical-cores` | N CPUs = N whole cores. `hyperthreads` uses the full logical pool: denser, pods may share a core |
| `spec.balloon.htSiblingPolicy` | `exclude` | `reserved` hands excluded siblings to the reserved pool instead |
| `spec.balloon.packing` | `spread` | `binpack` fills nodes first |
| `spec.balloon.reservedNamespaces` | `["*", "kube-system"]` | Whose unpinned pods land on the reserved pool. On a shared worker scope it to `[]` or an explicit list |
| `spec.balloon.reserved` | chart: `cpus: 12` | At most one of `cpus` (logical CPUs), `percent` (of physical cores) or `measured`; the result is clamped per node |
| `spec.excludeNodeSelector` | unset | Nodes the operator leaves alone; a single node can also carry `cpu-pinning-operator.intel.com/disabled=true` |
| `spec.nodeConfigOverrides[]` | unset | First matching `nodeSelector` wins; only `cpuAllocation` and `reserved` resolve per node |
| `spec.webhook.failurePolicy` | `Ignore` | `Fail` blocks pod creation in target namespaces while the webhook is down, instead of admitting unpinned |

A node's resolved mode is its `cpu-pinning-operator.intel.com/cpu-allocation` label
(`kubectl get nodes -L cpu-pinning-operator.intel.com/cpu-allocation`).
