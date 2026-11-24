<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->
# Configuration

Set cluster-wide defaults in **`BalloonsClusterConfig`** (a singleton named `cluster`).
Select which workloads get pinned with **`BalloonPinningPolicy`** (one or more).
Both are cluster-scoped. You don't edit a workload spec; admission adds the
annotation, an init container and node affinity to the Pod.

**Any workload can be pinned.** Matching is by container name or pod label, so the
operator has no notion of a runtime, and the chart ships no default target: you
name the pods and namespaces at install, as in
[The chart's policy](#the-charts-policy). The chart's default intent reads the span
off `--tensor-parallel-size` in the container's args, which a vLLM-backed KServe
runtime passes through. Spell the flag with dashes: the server also accepts
`--tensor_parallel_size`, but the default `argRegex` does not read it. `byContainerName` takes a **list**, and any entry
containing `*`, `?` or `[` is matched as a glob, so one rule can cover many names.
`byPodLabels` matches a pod carrying **any** of its pairs. You can also apply as many
`BalloonPinningPolicy` objects as you like. The two examples below are a
by-name one and a by-label one for that reason. Reading the span off a flag, an env
var or an annotation is a policy field, not code — a new runtime needs a policy edit
and nothing else.

- [The chart's policy](#the-charts-policy)
- [Two working examples](#two-working-examples)
- [BalloonsClusterConfig fields](#balloonsclusterconfig-fields)
- [BalloonPinningPolicy fields](#balloonpinningpolicy-fields)
- [`cpuAllocation`: physical cores vs hyperthreads](#cpuallocation-physical-cores-vs-hyperthreads)
- [Reserved pool sizing](#reserved-pool-sizing)
- [Mixed fleets: per-node overrides](#mixed-fleets-per-node-overrides)
- [Fleet size](#fleet-size)
- [Environment variables](#environment-variables)
- [Verifying](#verifying)

---

## The chart's policy

With `balloonPinningPolicy.create=true` (the default) the chart creates one policy,
`default`, and fails to render until it has a `match` and a namespace target. Set
`create=false` to apply your own policies instead.

| workload | values |
|---|---|
| bare vLLM Deployment in `inference` | `--set 'balloonPinningPolicy.match.byContainerName={vllm}' --set 'balloonPinningPolicy.namespaces={inference}'` |
| KServe predictor in `inference` | `-f docs/examples/values/kserve.yaml`, below |

```yaml
balloonPinningPolicy:
  match:
    byContainerName: [kserve-container]   # the InferenceService predictor
  namespaces: [inference]
```

The webhook bootstraps on the exact names in `namespaces`; set
`webhook.targetNamespaces` to override.

KServe runs every runtime in `kserve-container`, so one policy cannot tell vLLM from
SGLang by name. [`policy-kserve.yaml`](examples/policy-kserve.yaml) lists both runtimes'
flags as `argRegex` alternatives; the first group that matches sets the span.

## Two working examples

**1. Pin by container name, in two namespaces.** `byContainerName` is a list and
accepts globs; this example lists one entry, and `vllm` being that entry is the only
vLLM-specific thing about it. The chart already owns `BalloonsClusterConfig/cluster` and
a `default` policy, so apply this with the chart installed with
`--set balloonsClusterConfig.create=false --set balloonPinningPolicy.create=false`, or
set the same fields through chart values; otherwise the next `helm upgrade` reverts the
`cluster` object, and an overlapping `default` policy sorts first and wins.

```yaml
apiVersion: cpu-pinning-operator.intel.com/v1alpha1
kind: BalloonsClusterConfig
metadata:
  name: cluster                      # the only accepted name
spec:
  balloon:
    cpuAllocation: physical-cores    # N CPUs = N whole cores
    reservedNamespaces: []           # route only kube-system to the reserved pool
    reserved:
      percent: 10                    # 10% of physical cores for system work
---
apiVersion: cpu-pinning-operator.intel.com/v1alpha1
kind: BalloonPinningPolicy
metadata:
  name: vllm
spec:
  namespaces: [inference, inference-staging]
  match:
    byContainerName: [vllm]
  intent:
    span:
      argRegex: '(?:^|\s)--tensor-parallel-size(?:=|\s+)(\d+)'   # span from the TP flag
      default: 1
  capacityCheck:
    mode: enforce                    # refuse a pod that cannot fit
  scheduling:
    enforcePinning: true             # Pending beats running unpinned
```

**2. A different runtime: match by label, require AMX, restart on drift.** Same
operator, no code change — the span comes from an env var instead of a flag.

```yaml
apiVersion: cpu-pinning-operator.intel.com/v1alpha1
kind: BalloonPinningPolicy
metadata:
  name: sglang-amx
spec:
  namespaceSelector:
    matchLabels:
      cpu-pinning-operator.intel.com/pinning: enabled
  match:
    byPodLabels:
      app: sglang
    containerSelection: largestContainer
  intent:
    span:
      envVar: SGLANG_TP_SIZE         # an env var your launcher sets
      default: 1
    multiplier:
      annotation: cpu-pinning-operator.intel.com/dp-size
      default: 1
    coLocate: numa                   # spread the span across NUMA nodes
  scheduling:
    nodeFeatures:
      cpu-pinning-operator.intel.com/amx: require   # only AMX-capable nodes
  driftPolicy: restart               # evict when the pinned config changes
  guardActivePods: true              # block deletes and edits that orphan pinned pods
```

---

## BalloonsClusterConfig fields

Cluster-global. One object, named `cluster`; the API server rejects any other name.

| field | values | default | effect |
|---|---|---|---|
| `spec.balloon.cpuAllocation` | `physical-cores`, `hyperthreads` | `physical-cores` | Whole cores, or the full logical pool |
| `spec.balloon.htSiblingPolicy` | `exclude`, `reserved` | `exclude` | Drop HT siblings from the pool, or hand them to the reserved pool |
| `spec.balloon.packing` | `spread`, `binpack` | `spread` | `spread` takes the node with the most free room: smaller blast radius, more fragmentation. `binpack` fills nodes first |
| `spec.balloon.reservedNamespaces` | list of namespace globs | `["*", "kube-system"]` | Whose unpinned pods land on the reserved pool |
| `spec.balloon.reserved.cpus` | integer | unset | Reserved size as an explicit logical-CPU count |
| `spec.balloon.reserved.percent` | integer | unset | Reserved size as a percentage of physical cores |
| `spec.balloon.reserved.measured.headroomPercent` | integer | `50` | Headroom above observed reserved demand |
| `spec.balloon.reserved.measured.min` | integer | `4` | Floor for measured sizing, and the cold-start value |
| `spec.nodeConfigOverrides[].nodeSelector` | label selector | required | Which nodes this override applies to |
| `spec.nodeConfigOverrides[].balloon` | `BalloonSpec` | — | Per-node values; only `cpuAllocation` and `reserved` resolve |
| `spec.classification.overrideLabel` | label key | unset | Label that forces a node's class by hand |
| `spec.classification.amxNodeSelector` | label selector | unset | Which nodes count as AMX-capable |
| `spec.classification.gpuOnlyNodeSelector` | label selector | unset | Which nodes are GPU-only |
| `spec.classification.cpuAMXMinLogicalCPUs` | integer | unset (normalized to 64) | Logical-CPU floor for the CPU-AMX class |
| `spec.classProfiles` | map of class to `BalloonSpec` | unset | Per-class balloon settings |
| `spec.webhook.failurePolicy` | `Ignore`, `Fail` | `Ignore` | Whether a webhook outage blocks pod creation |
| `spec.excludeNodeSelector` | label selector | unset | Nodes the operator leaves alone |

> [!WARNING]
> `reservedNamespaces` defaults to `["*", "kube-system"]`, which routes **every**
> unpinned pod onto the exclusive reserved pool. That is right for a dedicated
> inference node and a node-exhaustion hazard on a shared OpenShift worker. Scope it
> to `[]` or an explicit list there.

> [!NOTE]
> `failurePolicy: Ignore` means a matched pod can be admitted unpinned while the
> webhook is down. `Fail` closes that gap and blocks pod creation in the target
> namespaces instead. See [secure-configuration.md](secure-configuration.md).

---

## BalloonPinningPolicy fields

One per workload family. A pod is pinned when its namespace matches and either a container name or a pod label matches.

| field | values | default | effect |
|---|---|---|---|
| `spec.namespaces` | list of names or globs | unset | Namespaces this policy targets |
| `spec.namespaceSelector` | label selector | unset | Namespaces by label instead of by name |
| `spec.match.byContainerName` | list of container names | unset | Match a container by name; an entry containing `*`, `?` or `[` is matched as a glob |
| `spec.match.byPodLabels` | label map | unset | Match a pod carrying **any** one of these key/value pairs |
| `spec.match.containerSelection` | `wholePod`, `largestContainer` | `wholePod` | `wholePod`: a `byPodLabels` match sizes the balloon from the whole pod, and a name glob hitting more than one container skips the pod as ambiguous. `largestContainer`: pin the highest-CPU candidate container |
| `spec.intent.span.annotation` | annotation key | unset | Read the span from an annotation |
| `spec.intent.span.argRegex` | regex with one capture group | unset | Read the span from a container arg |
| `spec.intent.span.envVar` | env var name | unset | Read the span from an env var |
| `spec.intent.span.default` | integer | `1` | Span when no source matches |
| `spec.intent.multiplier` | same shape as `span` | unset | Second parallel dimension, multiplied into the span |
| `spec.intent.coLocate` | `package`, `numa`, `""` | `""` | `package` keeps the span inside one CPU package; `numa` spreads it across NUMA nodes for memory bandwidth. For one NUMA node, use span 1 |
| `spec.capacityCheck.mode` | `enforce`, `warn`, `off` | `warn` | `enforce` denies a pod that cannot fit; `warn` admits it and emits a `CapacityCheckWarning` event — a span above the node's max-span is capped to it, a capacity shortfall is pinned at the requested span and may then be refused by the plugin; `off` skips the check |
| `spec.scheduling.enforcePinning` | boolean | `true` | Deny a matched pod at admission when no node is provisioned yet or pinning fails; an admitted pod gets required affinity to `balloons-enabled=true` nodes, so it stays `Pending` until one fits |
| `spec.scheduling.nodeFeatures` | map of feature to `prefer`, `require`, `ignore` | unset | Steer or restrict by node feature label |
| `spec.driftPolicy` | `eventsOnly`, `restart` | `eventsOnly` | Emit an event on drift, or evict the pod |
| `spec.guardActivePods` | boolean | `false` | Reject deleting the policy while it holds pinned pods, and `namespaces`/`match` edits that would stop matching one. Intent and sizing edits pass. When `status.boundPods` is capped, every update is rejected |

Span comes from the first source that matches, in the order `annotation`, `argRegex`,
`envVar`, then `default`. The final balloon is `snap(span × multiplier)` rounded up to
`{1, 2, 4, 8}` and capped by the node's `max-span` label.

**Overlapping policies.** When more than one policy matches a pod, the one whose name
sorts first (byte-wise) wins, on every replica.

- If you expect overlaps, prefix the names to set the order: `00-vllm-large`, `01-vllm-default`, `02-catch-all`.
- Put specific policies before broad ones, such as glob or label-selector matches.

> [!NOTE]
> `intent` is runtime-agnostic. A new runtime needs a policy edit, not a code change:
> point `argRegex` or `envVar` at whatever flag that runtime uses for tensor
> parallelism.

> [!IMPORTANT]
> **Device pods ignore `intent` entirely.** A pod requesting any device or extended
> resource (`nvidia.com/gpu`, `gpu.intel.com/i915`, an FPGA, a NIC) is detected
> automatically and routed to the `balloon-device` type, which prefers CPUs local to its
> granted device when the plugin can resolve the device's NUMA node; that needs the
> kubelet `KubeletPodResourcesGet` gate (see [architecture.md](architecture.md#balloon-sizing)). Its span and multiplier flags are not read. Detection is
> resource-agnostic and needs no configuration, so a new accelerator vendor works with
> no operator change.

---

## `cpuAllocation`: physical cores vs hyperthreads

The cluster default in `spec.balloon`; class profiles and per-node overrides can change it per node (see [Mixed fleets](#mixed-fleets-per-node-overrides)).

| mode | pool | a request for N CPUs | use when |
|---|---|---|---|
| `physical-cores` (default) | siblings removed via `availableResources: exclude-cpuset` | consumes N whole physical cores | latency matters; two pinned workloads should not share a core |
| `hyperthreads` | full logical-CPU pool | consumes N logical CPUs, possibly siblings of one core | density matters more than interference |

Excluded CPUs stay schedulable by the OS for non-balloon work.

### `htSiblingPolicy`: recovering the excluded half

`exclude` (default) keeps a pinned core's sibling thread out of the pool, which avoids SMT interference from other pods, and costs roughly half the pool. `reserved`
recovers those cores and accepts interference:

* **Siblings stay in the pool** - they are named in the reserved pool's cpuset instead of being excluded.
* **Pinned balloons still never get them** - the plugin allocates workload balloons from what remains after the reserved cpuset, so pinned pods see only the representative threads.
* **The cost is SMT interference** - reserved-pool load lands on the sibling of a latency-sensitive core. That is why `exclude` ships as the default.
* **Sizing changes** - in this mode the reserved pool *is* the sibling set, so `reserved.cpus`, `.percent` and `.measured` do not apply.
* **No effect in `hyperthreads` mode** - there is no exclusion to drop.

---

## Reserved pool sizing

The reserved pool carries system and non-model work. Set at most one of the three
sources; the resolved value is then clamped per node.

| source | meaning |
|---|---|
| `reserved.cpus` | Explicit logical-CPU count |
| `reserved.percent` | Percentage of the node's physical cores |
| `reserved.measured` | Observed demand plus `headroomPercent`, floored at `min` |

Clamping, in order: raise to the system-pod floor, cap at 25% of physical cores, cap
so a workload minimum stays free, round down to even for sibling alignment, then in
`physical-cores` mode convert a logical baseline to physical cores.

> [!WARNING]
> `reserved.percent` is already a fraction of the physical core count, so it is **not**
> converted again in `physical-cores` mode. The logical baselines are. Getting this
> backwards halves the pool.

On a homogeneous node the operator names the reserved CPUs as a NUMA-balanced,
sibling-free cpuset (`cpuset:0-1,4-5`) instead of a bare count, so placement is
deterministic. The count is rounded up to a multiple of the NUMA node count for a
symmetric split, which can name slightly more CPUs than the bare count.

Mechanics and source links: [internal/reserved-pool-internals.md](internal/reserved-pool-internals.md).

---

## Mixed fleets: per-node overrides

`spec.nodeConfigOverrides` is an ordered list. The first entry whose `nodeSelector`
matches a node wins; nodes matching nothing use `spec.balloon`. Only `cpuAllocation`
and `reserved` resolve per node — everything else stays cluster-global.

```yaml
spec:
  balloon:
    cpuAllocation: physical-cores
    reserved:
      percent: 10
  nodeConfigOverrides:
    - nodeSelector:
        matchLabels:
          node-role.kubernetes.io/shared: ""
      balloon:
        cpuAllocation: hyperthreads   # density on shared workers
        reserved:
          cpus: 16
```

Node classification (`spec.classification`, `spec.classProfiles`) assigns each node one
of four classes — `mixed-cpu-amx-gpu`, `cpu-amx`, `gpu-only`, `cpu-only` — from its
features, then applies that class's `BalloonSpec`. Set `classification.overrideLabel`
to pin a node's class by hand.

The resolved values land on `BalloonsClusterConfig` status as
`perNode[].cpuAllocation` and `perNode[].reservedCPUs`. Each `BalloonPinningPolicy`
reports its own pinned pods under `status.boundPods` (capped at 100) and
`status.boundPodsTotal`.

### Which mode a pod is judged and pinned under

The resolved mode is published on each node, and that label is what the capacity check
and drift detection read — never the cluster default:

```bash
kubectl get nodes -L cpu-pinning-operator.intel.com/cpu-allocation
# NAME       STATUS   ...   CPU-ALLOCATION
# worker-1   Ready    ...   physical-cores
# worker-2   Ready    ...   hyperthreads
```

Under `capacityCheck.mode: enforce`, a 12-CPU request at span 1 on a node with 8 cores
and two threads per core is refused on `worker-1` (8 per NUMA node) and admitted on `worker-2` (16). Changing a node's mode
drifts the pods pinned under the old one, so `driftPolicy: restart` re-pins them.

A pod admitted before the scheduler placed it has no node to read a mode from. It takes
the mode of the node admission steers it to, or of the nodes it can land on when they all
run the same mode. When those nodes run two modes, a pod admission committed to no
node — nothing fit, nothing had published live topology yet — is left with no mode at all. Such a pod
still drifts on a policy change, but not on a mode change; recreate it to re-pin it.

To keep a workload on one mode, give its pods a `nodeSelector` on
`cpu-pinning-operator.intel.com/cpu-allocation`. The capacity check honours the
`nodeSelector`, so the pod is judged and pinned in that mode even on a fleet running two
modes. Required `spec.scheduling.nodeFeatures` labels narrow the nodes the same way.

---

## Fleet size

**The design is reasoned at 1000 balloons-enabled nodes.** That figure is arithmetic
over the operator's own limits, not measured at this scale:

* **`BalloonsClusterConfig.status.perNode` grows one entry per provisioned node** -
  this is the first structure that grows linearly with the pool. One entry serializes
  to 192 bytes, so 1000 nodes is ~188 KiB, roughly 8x under the ~1.5 MiB an object may
  occupy in etcd (`--max-request-bytes`, 1.5 MiB by default):

  ```bash
  kubectl get balloonsclusterconfig cluster -o jsonpath='{.status.perNode[0]}'
  # → {"cpuAllocation":"hyperthreads","features":{"cpu-pinning-operator.intel.com/amx":"true"},
  #    "maxSpan":2,"name":"worker-1","numaNodes":2,"physicalCPUs":112,"reservedCPUs":12}
  ```

* **A config change re-provisions the fleet in paced waves** - 10 nodes enqueued per
  second, 5 reconciled at a time (`NODE_RECONCILE_CONCURRENCY`), so 1000 nodes are
  queued over ~100 s.
* **A node event costs one label test per node in the fleet, plus real work per node
  in the pool** - the reconcilers that report the pool list nodes by label, so a node
  outside it is tested and skipped rather than copied out of the cache and examined,
  so the fleet-sized term is the cheap one. Compare the two counts:

  ```bash
  kubectl get nodes -l cpu-pinning-operator.intel.com/balloons-enabled=true --no-headers | wc -l
  # → 1          the pool

  kubectl get nodes --no-headers | wc -l
  # → 1          the fleet; equal here, every node in this cluster is balloons-enabled
  ```

Nothing in the code blocks a larger fleet than the figure above, but no figure is
calculated for one. The pool size the operator itself reports:

```bash
kubectl get balloonsclusterconfig cluster -o jsonpath='{.status.provisionedNodes}'
# → 1
```

---

## Environment variables

Set on the manager Deployment. Chart values cover the rest — see
[the chart README](../deploy/cpu-pinning-operator/README.md).

| variable | default | effect |
|---|---|---|
| `NODE_RECONCILE_CONCURRENCY` | `5` | Parallel node reconciles; cuts fleet-wide convergence time after a config change |
| `BALLOONS_NAMESPACE` | `kube-system`; the chart sets the release namespace while the bundled plugin is on | Namespace the NRI balloons DaemonSet runs in |
| `BALLOONS_DAEMONSET` | chart value | DaemonSet name the readiness endpoint checks |
| `READINESS_CA_CONFIGMAP` | chart value when `balloonsReady.secure` | Name of the `ca.crt` ConfigMap the operator syncs into targeted namespaces and init containers mount; unset syncs nothing |
| `DASHBOARD_ENABLED` | `false` | Render the per-node Grafana canvas ConfigMap |

---

## Verifying

```bash
# <balloons-ns> = kube-system (external install), or the release namespace

# physical-cores, exclude mode: the sibling exclusion is rendered
kubectl -n <balloons-ns> get balloonspolicy node.<name> \
  -o jsonpath='{.spec.availableResources.cpu}'
# → exclude-cpuset:64-127

# density mode: no exclusion, siblings in reserved instead
kubectl -n <balloons-ns> get balloonspolicy node.<name> \
  -o jsonpath='{.spec.reservedResources.cpu}'
# → cpuset:64-127

# reserved pool on a homogeneous node: an explicit NUMA-balanced cpuset
kubectl -n <balloons-ns> get balloonspolicy node.<name> \
  -o jsonpath='{.spec.reservedResources.cpu}'
# → cpuset:0-1,4-5      (a bare count on heterogeneous nodes)

# what became of each node's hyperthread siblings: the state at 1 holds. allocatable
# is a node whose siblings a pinned pod can use, no-siblings a node without HT.
# /metrics is HTTPS on metrics.port (8080) and authenticates every client, so the
# scrape needs a token for a subject authorized for `get /metrics` — a ServiceAccount
# listed under metrics.readers. See secure-configuration.md ("Listeners and network
# exposure"). -k: the serving certificate covers the Service name, not localhost.
# <operator-deploy> is the manager Deployment the release rendered (`get deploy`);
# above replicaCount 1 only the leader reconciles, so forward that pod instead — the
# leader-election lease (`--leader-election-id`) names it in .spec.holderIdentity.
kubectl -n <operator-ns> port-forward deploy/<operator-deploy> 8080:8080 &
curl -sfk -H "Authorization: Bearer $(kubectl -n <reader-ns> create token <reader-sa>)" \
  https://127.0.0.1:8080/metrics | grep cpu_pinning_ht_sibling_policy
# → cpu_pinning_ht_sibling_policy{node="node1",policy="held-back"} 1
# → cpu_pinning_ht_sibling_policy{node="node1",policy="reserved-pool"} 0
# → cpu_pinning_ht_sibling_policy{node="node1",policy="allocatable"} 0
# → cpu_pinning_ht_sibling_policy{node="node1",policy="no-siblings"} 0

# what the plugin actually assigned
kubectl -n <balloons-ns> logs ds/nri-resource-policy-balloons | grep assigning

# resolved per-node values
kubectl get balloonsclusterconfig cluster -o jsonpath='{.status.perNode}'
```

---

## Related

| I want to… | file |
|---|---|
| Harden the install | [secure-configuration.md](secure-configuration.md) |
| Understand the components | [architecture.md](architecture.md) |
| Fix a pod that will not pin | [troubleshooting.md](troubleshooting.md) |
| Read the metrics | [observability.md](observability.md) |
| Set a chart value, install on OpenShift, tune the reservation TTL, or uninstall | [chart README](../deploy/cpu-pinning-operator/README.md) |
