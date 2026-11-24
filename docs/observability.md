<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Observability

The operator exposes Prometheus metrics, Kubernetes Events, structured logs and Grafana
dashboards. Use metrics for fleet trends, Events for one object, logs for the error behind
a decision.

<a id="glossary"></a>

## The words this operator uses

Both dashboards carry this glossary in a collapsed panel. Colour says one of exactly two things, never both:

- **Which pool** a number belongs to — a *fixed* colour, the one the per-machine core
  map paints those CPUs with: grey `#4b5563` free, `dark-purple` reserved, `#7EB26D`
  pinned. It labels the number; it never judges it.
- **Whether a number is bad** — green, orange, red from *thresholds*, meaning nothing to
  do, worth a look, act now. Only a number a reader can call bad gets these, and it
  moves as the value moves.

Each per-machine pool gauge is read against its own pool, with the denominator in the
title: the reserved gauge is CPU **time** (`872 of 6000 mCPU`), because that cpuset is
fixed and only what the unpinned pods burn says anything, while the dedicated gauge is
**occupancy** (`104 of 112 pinnable CPUs`), because a pinned workload holds its CPUs
whether or not it burns them. Both band at **70%** amber and **85%** red.

| Term | What it means |
|---|---|
| Pinned pod | A pod the operator bound to a fixed set of CPUs. No other pod is placed on those CPUs; host processes and interrupts are not isolated. |
| Skipped pod | A pod that matched a policy but was deliberately left unpinned — for example it requested a fractional CPU. It still runs, without dedicated CPUs. |
| Denied pod | A pod the operator refused at admission: no room for it, an unsupported span, a balloons plugin that was not ready (under `capacityCheck.mode: enforce`), or no machine provisioned for balloons. A denied pod never exists, so `kubectl get pods` cannot show it — the count is on `cpu_pinning_webhook_admission_seconds_count{result="denied"}` and the Event names the pod. |
| Balloon | The named set of CPUs a pinned pod's containers are placed into. Pods sharing a balloon share its CPUs. |
| Span | How many NUMA nodes one balloon may stretch across. `span1` stays inside one NUMA node, which keeps memory access local; a wider span buys room and memory bandwidth at the cost of remote-memory latency. The best span depends on the workload. |
| NUMA node | One CPU-and-memory locality domain of a machine. A balloon that stays inside one avoids remote-memory access. |
| Reserved pool | CPUs deliberately kept out of pinning, for everything unpinned: kubelet, system daemons, ordinary pods. |
| Hyperthread sibling | The second thread of a physical core. A machine's siblings read `held-back` (kept out of the pinning pool, so no pod is pinned to them; the OS can still run host work there), `reserved-pool` (they *are* the reserved pool, so no pinned pod uses them), `allocatable` (a pinned pod can land on one) or `no-siblings` (one thread per core). `cpu_pinning_ht_sibling_policy`. |
| Stranded capacity | Free CPUs the machine's **widest** balloon span cannot absorb, because a span-N balloon takes the same count on each of its N NUMA nodes. A narrower pod still places on them, so this is a limit on one shape and not a fault. `cpu_pinning_cpu_fragmentation`. |
| Drift | A pod still running with the pinning it was given after the configuration changed, so what it runs on no longer matches what it should. |
| Bypass | A pod the operator meant to pin but admitted **unpinned** after an internal error. `cpu_pinning_webhook_bypasses_total`. |
| Ghost allocation | CPUs still booked to a pod that no longer exists, so new pods can be refused while those CPUs idle. `cpu_pinning_ghost_allocations`. |

## Enable metrics

Metrics listen on `:8080/metrics` over HTTPS by default, and the endpoint
authenticates every client — so name the ServiceAccount your Prometheus runs as
under `metrics.readers`, or the scrape gets `403` and the dashboards stay empty.

```yaml
metrics:
  enabled: true
  secure: true
  requireAuth: true
  # The ServiceAccounts allowed to scrape. The chart cannot guess yours, so this
  # is empty by default and nothing may read /metrics until it is set.
  readers:
    - name: prometheus-kube-prometheus-prometheus
      namespace: monitoring
  serviceMonitor:
    enabled: true
  prometheusRule:
    enabled: true

grafanaDashboard:
  enabled: true
```

| Setting | Default | Effect |
|---|---:|---|
| `metrics.enabled` | `true` | Starts the metrics server and renders its Service |
| `metrics.secure` | `true` | Serves HTTPS with the cert-manager webhook certificate |
| `metrics.requireAuth` | `true` | Checks the client: bearer token via `TokenReview`, then `SubjectAccessReview` for `get /metrics`. `false` keeps HTTPS and drops only the client check, for a scraper that cannot present a token |
| `metrics.readers` | `[]` | ServiceAccounts (`name` + `namespace`) bound to the `…-metrics-reader` ClusterRole. Empty means nobody is authorized |
| `metrics.serviceMonitor.enabled` | unset | Renders a `ServiceMonitor` where the cluster serves `monitoring.coreos.com/v1`; with `requireAuth` it presents the scraping pod's projected ServiceAccount token |
| `metrics.prometheusRule.enabled` | unset | Recording rules and alerts, same capability default |
| `grafanaDashboard.enabled` | `false` | Creates the per-machine CPU maps and the cluster dashboard |

> [!NOTE]
> With `certManager.enabled=false`, a secure `ServiceMonitor` needs a serving certificate
> secret with `ca.crt` and metrics Service SANs. Set
> `metrics.serviceMonitor.allowExternalServingCert=true` to acknowledge that setup;
> certificate verification stays enabled. Unset, it is skipped there instead;
> `enabled: true` fails the render. `helm template` sees no CRDs, so set both explicitly in
> template-only pipelines.

> [!WARNING]
> **No `cpu_pinning_` series anywhere, and a healthy operator?** Check the scrape's status
> code before anything else. `401` means the client presented no token — a hand-rolled
> scrape, or a request through the API server's pod proxy, which strips the caller's
> credential. `403` means the token was resolved but that ServiceAccount is not in
> `metrics.readers`. Confirm the grant with
> `kubectl auth can-i --as=system:serviceaccount:<ns>:<sa> get /metrics`.

<a id="webhook-bypasses-vs-apiserver-level-fail-open"></a>
<a id="ha-reservation-window-cpu_pinning_webhook_reservations_recorded_total"></a>
<a id="ghost-balloon-allocations-cpu_pinning_ghost_allocations"></a>
<a id="drift-driven-evictions-driftpolicy-restart"></a>

## Metrics

All names use the `cpu_pinning_` prefix.

| Metric | Type | Labels | Means |
|---|---|---|---|
| `cpu_pinning_pods_pinned_total` | counter | `workload_namespace,balloon,policy` | Pods annotated for a balloon, labelled with the matching `BalloonPinningPolicy`'s name (empty if none) |
| `cpu_pinning_pods_skipped_total` | counter | `workload_namespace,reason` | Intentional skip: fractional, no balloon, excluded, or ambiguous |
| `cpu_pinning_capacity_check_failures_total` | counter | `node,workload_namespace,reason,mode` | Capacity warning or rejection; `unaccountable` means the reservation commit failed. `node` is the machine whose capacity was judged, empty for a pod not yet bound (see [which counters name a node](#which-counters-name-a-node)). It also counts pre-scheduled pods whose per-NUMA memory share does not fit |
| `cpu_pinning_webhook_bypasses_total` | counter | `workload_namespace,reason` | Best-effort pod admitted unmutated after an internal webhook error |
| `cpu_pinning_webhook_admission_seconds` | histogram | `result` | Admission latency by outcome: `pinned`, `skipped`, `denied` (the operator refused the pod) or `error` (it could not decide) — see [what an admission result means](#what-an-admission-result-means) |
| `cpu_pinning_webhook_plugin_not_ready_total` | counter | `node` | Scheduled pod found the plugin not Ready |
| `cpu_pinning_webhook_reservations_recorded_total` | counter | `node` | Reservations recorded by this webhook replica |
| `cpu_pinning_webhook_reservations_live` | gauge | `node` | Unexpired reservations last published by this replica |
| `cpu_pinning_pods_drifted_total` | counter | `node,workload_namespace` | Pod config or actual pinning no longer matches desired state |
| `cpu_pinning_drift_evictions_total` | counter | `node,workload_namespace,outcome` | Restart-policy eviction result |
| `cpu_pinning_pods_pinning_blocked_total` | counter | `node,workload_namespace,cause` | Reports that the plugin refused a pinned pod's container at `CreateContainer`; one per pod per 10 minutes while it stays blocked, so this counts reports and not pods (see [reports, not pods](#a-blocked-pod-is-counted-in-reports-not-pods)) |
| `cpu_pinning_node_provisioning_total` | counter | `node,outcome` | What happened to the machine in `node`. `provisioned`, `updated` and `deprovisioned` count a write to its `BalloonsPolicy`, so a resync that changes nothing counts nothing; `failed` counts each failed topology probe (see [what a configuration change counts as](#what-a-configuration-change-counts-as)) |
| `cpu_pinning_balloonspolicy_reconcile_errors_total` | counter | `node` | Per-node policy render/apply failed |
| `cpu_pinning_node_reserved_cpus` | gauge | `node` | Reserved-pool size applied to the node: the CPUs its policy reserves, after the even per-NUMA spread (6 on 4 NUMA nodes is 8). The `reserved-cpus` node label carries the size before the spread |
| `cpu_pinning_reserved_pool_utilization_ratio` | gauge | `node` | Rounded whole-CPU demand divided by the applied reserved size; above 1 is oversubscribed |
| `cpu_pinning_ht_sibling_policy` | gauge | `node,policy` | What became of the node's hyperthread siblings: the state that holds is 1, the other three 0 — `held-back`, `reserved-pool`, `allocatable` or `no-siblings` (see [the two totals](#two-totals-and-which-one-free-is-counted-against)) |
| `cpu_pinning_ghost_allocations` | gauge | `node` | `NodeResourceTopology` (NRT) container zones whose pod no longer exists; absent means unknown |
| `cpu_pinning_cpu_manager_conflict` | gauge | `node` | 1 when the cluster's kubelet configuration sets the CPU Manager `static` policy, so the kubelet and the plugin allocate the same CPUs and placement on that node is unpredictable; 0 when the policy is `none`, so the kubelet leaves cpusets to the plugin — including a `kind: KubeletConfiguration` document that omits the optional `cpuManagerPolicy`, whose API default is `none`, which is the usual shape on a stock kubeadm cluster; absent means the configuration could not be read (no ConfigMap, no `kubelet` key, a document whose YAML does not parse, a `cpuManagerPolicy` that is not a string, or an omitted `cpuManagerPolicy` in a document that does not declare `kind: KubeletConfiguration`), or names a policy the operator does not recognise — unknown either way, and never compatible. Read from kubeadm's `kube-system/kubelet-config`, so every node shares one verdict and a kubelet reconfigured by hand is not detected |
| `cpu_pinning_cpu_assignment` | gauge | `node,cpu,numa,core,balloon,workload_pod,workload_namespace` | Actual per-CPU owner rebuilt from NRT |
| `cpu_pinning_pool_membership` | gauge | `node,balloon,workload_namespace,workload_pod,workload_container` | CPUs in the cpuset one container was placed on, per NRT container zone — the reserved pool included |
| `cpu_pinning_node_logical_cpus` | gauge | `node,numa` | Hardware logical CPU count per NUMA node |
| `cpu_pinning_node_pinnable_cpus` | gauge | `node,numa` | Per NUMA node, the logical CPUs a workload can be pinned to, as the node's generated policy resolved them; absent until the node is provisioned |
| `cpu_pinning_cpu_fragmentation` | gauge | `node` | Free CPUs stranded by NUMA imbalance for the largest supported span, counted in the unit that node's own `cpuAllocation` allocates in; absent while that mode is unresolved |

The counters above that are about a workload name its namespace `workload_namespace`, not
`namespace`: a `ServiceMonitor` scrape attaches the target's own namespace as `namespace`
and Prometheus renames the colliding metric label to `exported_namespace`, so grouping on
`namespace` would group every tenant under the operator's own. Group on
`workload_namespace`:

```promql
sum by (workload_namespace) (cpu_pinning_pods_pinned_total)
# => {workload_namespace="inference"}  1
```

`cpu_pinning_cpu_assignment` carries one active series per pinnable CPU, not per logical CPU: a
CPU the node's policy excludes from the allocatable pool gets no series. Pod churn leaves
stale label combinations until retention expires; shorten retention or query the recording
rules if TSDB size becomes a problem.

### What an admission result means

Every admission the webhook handles is counted once on
`cpu_pinning_webhook_admission_seconds`, under one of four `result` values, which split by what
a reader has to do about them.

| `result` | What the operator did | What to do about it |
|---|---|---|
| `pinned` | Stamped the pod for a balloon | Nothing |
| `skipped` | Deliberately left the pod unpinned: no policy selects it, its container match is ambiguous, its CPU request is fractional or unset, its node is outside the pinning pool, or the cluster has no balloons-enabled node yet | Nothing. This is the normal case and usually far the largest of the four. `cpu_pinning_pods_skipped_total` carries the reason for the matched pods; a pod no policy selects increments nothing |
| `denied` | Refused the pod: the requested span is not supported, there is no room for its CPU or memory request, the balloons plugin on its node is not ready, or `enforcePinning` had no balloons-enabled node to route it to | Act on what the check refused: give the fleet room or lower the request for a capacity refusal, recover the plugin or provision the node for the other two. `cpu_pinning_capacity_check_failures_total{mode="enforce"}` names the check and the machine; the `CapacityCheckFailed`, `PluginNotReady` and `PinningDenied` Events name the pod |
| `error` | Reached no decision at all: the mutation aborted inside the operator, or the reservation authority could not be reached | Fix the operator. Read the manager log, plus `cpu_pinning_webhook_bypasses_total` and `cpu_pinning_capacity_check_failures_total{reason="unaccountable"}` |

On the cluster page these four are the **Admission decisions** bars. `denied` and `error`
are not the same event: `error` means the operator failed, never that it said no, so a
denial issued *because* it failed counts as `error`.

A refused pod never exists, so it is in no `kubectl get pods` listing and only this series
records the refusal as a decision:

```promql
sum by (result) (max by (pod, result) (last_over_time(cpu_pinning_webhook_admission_seconds_count[30d])))
# => {result="pinned"} 32   {result="skipped"} 1841   {result="denied"} 4
```

`{result="error"}` has no series above: an absent value means the event has not happened.

### A blocked pod is counted in reports, not pods

`cpu_pinning_pods_pinning_blocked_total` is a heartbeat, not a population. A pinned pod whose
container the balloons plugin refused is reported once the refusal has persisted 30
seconds, and again every 10 minutes for as long as the pod stays blocked, which is what
keeps `CPUPinningPinningBlocked` firing while the pod is stuck. So the counter holds **reports
over time**, and grows while nothing changes; **pods blocked right now** is what an
operator wants and no metric carries it, because the counter has no pod label.

Two pods blocked on one machine for an hour read:

```promql
sum(cpu_pinning_pods_pinning_blocked_total{cause="no-capacity"})
# => 12       two pods, six reports each: one at 30s and one per 10 minutes after

increase(cpu_pinning_pods_pinning_blocked_total{cause="no-capacity"}[15m])
# => 2 to 4   what the alert reads, extrapolated; it only needs this above 0
```

The pod count is 2 and the counter never says so. The Events name the pods:

```console
kubectl get events -A --field-selector reason=PinningBlocked
```

These pods are a different population from the [`denied`](#what-an-admission-result-means)
ones above: a denied pod never exists, while every pod counted here was admitted, exists
and cannot start. Neither number is a subset of the other.

### Two totals, and which one free is counted against

`cpu_pinning_node_logical_cpus` is the hardware: every logical CPU the probe found.
`cpu_pinning_node_pinnable_cpus` is the pool the node's generated policy leaves a workload,
and it is the one free and used are read against. On the machine below they differ by
half the box.

That machine has 224 logical CPUs on 2 NUMA nodes, 2 threads per core, provisioned in
`physical-cores` mode with `htSiblingPolicy: exclude`, so its policy carries
`availableResources.cpu: exclude-cpuset:112-223`. Those upper threads never enter the
plugin's allocatable pool: nothing claims them and nothing can, so they are not free
capacity:

```promql
max by (numa) (cpu_pinning_node_logical_cpus{node="worker-1"})
# => {numa="0"} 112   {numa="1"} 112     the hardware

max by (numa) (cpu_pinning_node_pinnable_cpus{node="worker-1"})
# => {numa="0"} 56    {numa="1"} 56      what a workload can be placed on
```

`cpu_pinning:cpu_free:per_numa` is read against the pool, so it reports what the plugin will
actually hand out: on a machine of this shape whose pinnable CPUs are all claimed it reads
0 while `cpu_pinning_pods_pinning_blocked_total{cause="no-capacity"}` climbs. Everywhere else
nothing is excluded and the two totals are equal:

| Mode | What happens to the hyperthread siblings | `node_pinnable_cpus` | `ht_sibling_policy` reads |
|---|---|---|---|
| `physical-cores` + `htSiblingPolicy: exclude` | dropped from the allocatable pool via `exclude-cpuset` | logical CPUs minus the siblings | `held-back` |
| `physical-cores` + `htSiblingPolicy: reserved` | they *become* the reserved pool, and are published as claimed by it | every logical CPU | `reserved-pool` |
| `hyperthreads` | nothing is excluded | every logical CPU | `allocatable` |
| any mode, hyperthreading off in firmware | there is no sibling to act on | every logical CPU | `no-siblings` |

The gauge reports the machine, not the setting: `cpuAllocation` is per-node overridable and
the hardware differs across a fleet, so one cluster-wide `htSiblingPolicy` reads
`held-back` on one machine and `allocatable` on the next. The last two rows both hold
nothing back and are different facts: siblings a pinned pod can land on, against no
siblings at all. Read the label of the series at 1:

```promql
cpu_pinning_ht_sibling_policy{node="worker-2"}   # cpu-allocation=hyperthreads
# => {policy="allocatable"} 1   the other three 0
```

The reserved pool is inside `node_pinnable_cpus` in every mode: its CPUs are published as
claimed, so they already count as used. On the machine above, 6 reserved CPUs and 104
workload CPUs leave 2 of the 112 pinnable.

A node the operator has not provisioned since it started has no
`cpu_pinning_node_pinnable_cpus` series at all. That absence is deliberate: consumers fall back
to `cpu_pinning_node_logical_cpus`, whereas a 0 would report a machine nothing can run on.

### Fragmentation is counted in each node's own unit

`cpu_pinning_cpu_fragmentation` is the free capacity one max-span balloon cannot absorb,
counted in the unit that node's `cpuAllocation` allocates in: physical cores under
`physical-cores`, logical threads under `hyperthreads`. `cpuAllocation` is per-node
overridable through `BalloonsClusterConfig.spec.nodeConfigOverrides`, so the unit is
read per node, from the `cpu-pinning-operator.intel.com/cpu-allocation` label the operator
stamps on every machine it provisions — the same label admission reads.

Two machines of identical hardware in an identical state therefore report different
numbers. Both below have 2 NUMA nodes of 2 hyperthreaded cores, with NUMA 1 fully busy
and NUMA 0 fully idle:

```promql
cpu_pinning_cpu_fragmentation{node="worker-1"}   # cpu-allocation=physical-cores
# => 2      NUMA 0's two idle cores are stranded

cpu_pinning_cpu_fragmentation{node="worker-2"}   # cpu-allocation=hyperthreads
# => 4      NUMA 0's four idle threads are stranded
```

A machine whose mode is not resolved has no series, the same absence
`cpu_pinning_node_pinnable_cpus` uses: the sibling rule differs between the modes, so a value
counted under a guessed mode answers a different question. `CPUPinningNodeFragmented` fires at
severity `info` and the fleet overview marks the machine amber, so a wrong unit
misreports a healthy machine.

### A fleet free-CPU total is not a number of pods that fit

Across a fleet that mixes the three modes, free CPUs stop being equivalent units: one free
CPU under `htSiblingPolicy: exclude` is a whole physical core, while one on a
`hyperthreads` machine is a single thread whose sibling may be busy. A fleet sum adds the
two, so it answers **how the fleet's CPU is allocated** and never **will a pod fit**:

```promql
# the fleet total: correct arithmetic, not a number of pods
sum(cpu_pinning:cpu_free:per_numa)
# => 6         2 on the exclude machine, 4 on the hyperthreads machine

# the same two machines, each against its own pool
sum by (node) (cpu_pinning:cpu_free:per_numa)
# => {node="worker-1"} 2   {node="worker-2"} 4
```

Admissibility is therefore a per-machine question: a machine can take a pinned pod when at
least one NUMA node of its own pinnable pool still has a free CPU, the narrowest pod there
is. That is the same test the webhook's headroom pre-flight applies at span 1.

```promql
# machines with room for one more pinned pod; the `* 0` term makes a fleet with
# no room read 0 rather than empty
count(count by (node) (max by (node, cpu) (cpu_pinning_cpu_assignment{balloon="free"})))
  or (count(max by (node) (cpu_pinning_node_logical_cpus)) * 0)
# => 2         both machines above still have free CPUs
```

A pod whose balloon spans several NUMA nodes needs more: its share must fit on every NUMA
node of the span, so the free CPUs must also not be stranded
(`cpu_pinning_cpu_fragmentation`, and the **Machines the widest span cannot fill** tile).

### Which counters name a node

A counter carries `node` when its increment records a decision about one machine, and omits
it where no machine is known at that point. `cpu_pinning_pods_pinning_blocked_total` never
carries an empty `node`: a pod with no `spec.nodeName` is not reported there. A metric label
colliding with a target label is renamed `exported_<name>`, and this chart's
`ServiceMonitor` attaches `container`, `endpoint`, `instance`, `job`, `namespace`,
`pod` and `service`, no `node` — which is why `node` needs no prefix and the workload's
namespace does.

| Counter | `node` | What it names | Example series |
|---|---|---|---|
| `cpu_pinning_pods_pinning_blocked_total` | always set | The machine whose kubelet refused the container | `{node="worker-1",workload_namespace="inference",cause="no-capacity"} 6` — six reports, which can be one pod blocked for an hour |
| `cpu_pinning_capacity_check_failures_total` | empty for an unbound pod | The machine whose capacity was judged. An unscheduled pod is judged against every candidate at once, so none is named, and no placeholder is substituted — it could collide with a real node name | `{node="worker-1",…,reason="capacity",mode="enforce"} 1` beside `{workload_namespace="inference",reason="capacity",mode="warn"} 1`; an empty label is not stored, so `{node=""}` selects the unbound ones |
| `cpu_pinning_node_provisioning_total` | always set | The machine whose `BalloonsPolicy` was written, or whose probe failed | `{node="worker-1",outcome="updated"} 3` |
| `cpu_pinning_pods_drifted_total` | always set | The machine the drifted pod runs on; drift is detected against that node's labels and `NodeResourceTopology` | `{node="worker-1",workload_namespace="inference"} 2` |
| `cpu_pinning_drift_evictions_total` | always set | The machine the evicted pod ran on | `{node="worker-1",…,outcome="pdb-blocked"} 1` |
| `cpu_pinning_pods_pinned_total` | **none** | The annotation is stamped during admission, where an unscheduled pod has no node. Placement is per node in `cpu_pinning_pool_membership` | `count by (node) (cpu_pinning_pool_membership{workload_namespace="inference"})` is the per-machine view |
| `cpu_pinning_pods_skipped_total` | **none** | `fractional`, `ambiguous` and `no-balloon` are decided without reading a node | `{workload_namespace="inference",reason="fractional"} 4` |
| `cpu_pinning_webhook_bypasses_total` | **none** | `decode` and `policy-index` abort before the pod is usable; `node-lookup` is itself the failure to read a node | `{workload_namespace="inference",reason="node-lookup"} 1` |

So a per-machine failure view reads:

```promql
# which machine is refusing pinned pods, and why. The value counts reports, not pods
sum by (node, cause) (increase(cpu_pinning_pods_pinning_blocked_total[15m]))
# => {node="worker-1", cause="no-capacity"}  2

# capacity denials one machine made, and the converse: admissions no machine in the
# fleet had room for. Prometheus stores no empty label, so those carry no `node`
sum by (node, reason) (cpu_pinning_capacity_check_failures_total{node!=""})
sum by (reason) (cpu_pinning_capacity_check_failures_total{node=""})
# => {reason="capacity"}  1
```

`CPUPinningPinningBlocked` groups on `node`, `workload_namespace` and `cause`, so it fires once
per machine, tenant and cause, naming all three:
`Pinned pods cannot start in inference on worker-1 (no-capacity)`. A tenant blocked on ten
machines fires ten alerts; to page once per tenant, group on `alertname` and
`workload_namespace` in Alertmanager.

### What a configuration change counts as

`cpu_pinning_node_provisioning_total` answers one question: did something change on my machines.
Its three write outcomes count writes, so a reconcile that writes nothing counts nothing —
and the node reconciler re-renders each machine's `BalloonsPolicy` on a 10-minute resync and
on every Node update the kubelet posts, several times an hour on an idle machine.

`failed` is the exception and is not a write: it counts each failed topology probe, and
the reconciler retries one every minute, so it keeps rising for as long as a machine is
broken. That is what keeps `CPUPinningNodeProvisionFailed` firing while the machine stays
broken, so exclude it when checking the write outcomes:

```promql
cpu_pinning_node_provisioning_total{outcome!="failed"}
# => {node="worker-1", outcome="updated"} 12      # still 12 twenty minutes later
```

```console
$ kubectl get balloonspolicy -n <balloons-ns> node.worker-1 \
    -o jsonpath='{.metadata.generation}'
194                                              # unchanged across the same window
```

A write outcome moving beside a frozen `generation` contradicts itself, and `generation` is
the one to believe: it is the API server's record of the stored spec changing. Each
`outcome` reads as:

| `outcome` | Counted when | What an operator does |
|---|---|---|
| `provisioned` | The machine's `BalloonsPolicy` did not exist and was created | Nothing; the machine has joined the pinning pool |
| `updated` | The apply changed the stored spec | Find out what changed: `kubectl describe node worker-1` carries the `BalloonsPolicyUpdated` event with the span and allocation mode applied |
| `deprovisioned` | The `BalloonsPolicy` was deleted, because the machine left the pool | Confirm the exclusion was intended (`kubectl get node worker-1 -o jsonpath='{.metadata.labels}'`) |
| `failed` | The machine's topology probe failed, once per retry | The machine is not pinning anything; read its Events and the operator log. A `BalloonsPolicy` the operator could not apply is counted by `cpu_pinning_balloonspolicy_reconcile_errors_total` instead |

Not counted, because they change nothing the machine pins with: repairing an
operator-managed label on the CR and backfilling the `BalloonsClusterConfig`
`ownerReference`. `CPUPinningNodeProvisionFailed` aggregates `max by (node)`, so a failure on
one machine is one alert however many replicas export the counter.

### Who is in which pool (`cpu_pinning_pool_membership`)

`cpu_pinning_cpu_assignment` is one series per pinnable CPU, so it cannot name the pods in the
reserved pool: those containers share one cpuset, and a series per container would put many
owners on the same CPU and break the `max by (..., cpu)` de-duplication every consumer
relies on. `cpu_pinning_pool_membership` is the same data at container cardinality — one series
per NRT `allocation for container` zone, every balloon included — valued with the size of
the cpuset the container was placed on.

| Label | Means | Example |
|---|---|---|
| `node` | The machine the container runs on | `worker-1` |
| `balloon` | The balloon zone it sits under, instance suffix stripped; `reserved` is the shared pool | `balloon-span1` |
| `workload_namespace` / `workload_pod` / `workload_container` | The container, all three prefixed so the scrape target's own `namespace`, `pod` and `container` labels cannot shadow them; the operator's own target carries `container="manager"` | `inference` / `demo-7c9f4d8b6-xk2wq` / `demo` |

A zone whose name does not split into a non-empty `namespace`, `pod` **and**
`container` is skipped — `bar`, `/foo/bar` and `foo/bar` all are — so the metric never
carries a row nobody can act on. Where NRT holds two zones for one container in **one
balloon** — the ghost-allocation signature — the largest cpuset wins, so the value never
understates what the container is holding. Two zones for one container in **different**
balloons stay two series: nothing in the CR says which is live, and both pools really are
holding those CPUs. `cpu_pinning_ghost_allocations` skips zones parented to `reserved`, so it
counts a duplicate only when neither zone is in the shared pool.

Who is sharing the reserved pool on one machine:

```promql
max by (workload_namespace, workload_pod, workload_container) (
  cpu_pinning_pool_membership{node="worker-1", balloon="reserved"})
```

That returns one series per unpinned container, each valued with the pool's size —
identical on every row, which is why the dashboard puts it in the panel title and not in
a column. The series count is how many containers time-share the pool, and it is **not**
`cpu_pinning_reserved_pool_utilization_ratio`: that divides their summed CPU *requests* by the
pool size, and a container that declares no request contends for the pool all the same.

What each container actually burns is not this operator's to report; join the kubelet's
cAdvisor counter, which is dense enough to `rate()` over the dashboard's range:

```promql
sum by (namespace, pod, container) (
  rate(container_cpu_usage_seconds_total{node="worker-1", container!=""}[$__range]))
and on (namespace, pod, container)
label_replace(label_replace(label_replace(
  max by (workload_namespace, workload_pod, workload_container) (
    cpu_pinning_pool_membership{node="worker-1", balloon="reserved"}),
  "namespace", "$1", "workload_namespace", "(.+)"),
  "pod", "$1", "workload_pod", "(.+)"),
  "container", "$1", "workload_container", "(.+)")
```

The `and on` half is not optional: cAdvisor keeps a series until retention expires, so the
usage query alone also returns departed containers; intersecting it with the membership
gauge keeps them out. Usage is shown in millicores, the unit
Kubernetes states requests in, so an idle container reads `0 mCPU`. Without cAdvisor the
column is absent rather than zero: "nobody measured it" and "uses no CPU" lead to opposite
decisions. There is no throttling column, because CFS throttling comes from CPU *limits*
while a cpuset restricts *which* CPUs a container may use.

Everything in that section counts **containers**, and every header and title says so. The
container is the unit the data already uses — one NRT zone, one cpuset, one cAdvisor
series — and the honest one for the shared pool, where a pod's sidecar contends for those
CPUs like anything else.

Aggregate with `max`, never `sum`: containers of one pod in one balloon share a
cpuset, so `sum by (workload_pod)` multiplies that pod's CPUs by its container count —
two containers in a 4-CPU balloon read 8, where `max by (workload_pod)` reads 4. A pod
whose containers landed in *different* balloons (`preferSpreadingPods`) is understated
for the same reason, so a pinned pod's CPU **count** still comes from
`cpu_pinning_cpu_assignment`, whose `count by (workload_pod) (max by (workload_pod, cpu)
(...))` counts distinct CPUs; `cpu_pinning_pool_membership` supplies only the listing.

### Read the failure signals

| Signal | Read it as | Action |
|---|---|---|
| `cpu_pinning_webhook_bypasses_total` increases | A best-effort policy ran a pod unpinned | Inspect `reason`; recreate the pod after recovery |
| `cpu_pinning_webhook_admission_seconds` p99 above the 5s target (the alert fires at 8s) | The API server aborts at 10s and admits unpinned | Check API and capacity-lookup latency |
| `cpu_pinning_webhook_admission_seconds_count` by `result` moves | See [what an admission result means](#what-an-admission-result-means) | `denied`: give the fleet room or recover the plugin. `error`: read the manager log and check the reservation authority's lease |
| `cpu_pinning_pods_pinning_blocked_total` increases | Bound pod cannot start and will not recover itself | Fix `no-balloon`, `no-capacity` or `plugin-error` on the machine in `node`; recreate pod |
| `node_provisioning_total{outcome="failed"}` increases | The machine in `node` did not join the pinning pool | Read that Node's events and the operator log |
| `ghost_allocations > 0` persists | Plugin capacity leaked after a failed container create | Confirm ghost, restart plugin pod on that node |
| `cpu_manager_conflict == 1` | The cluster's kubelets also allocate exclusive cpusets, so pinning is not authoritative on any node in the pool | Set `cpuManagerPolicy: none` in the kubelet configuration and restart the kubelets, or take the nodes out of the pool with `cpu-pinning-operator.intel.com/disabled=true` |
| `drift_evictions_total{outcome=~"pdb-blocked|error"}` persists | Drift rollout cannot progress | Fix PDB or eviction error |
| `reserved_pool_utilization_ratio > 1` | Reserved pool is undersized | Increase fixed reserve or measured headroom |
| `cpu_fragmentation > 0` persists | No room for a max-span balloon; narrower pods still place | Drain/repack node, or pin the waiting pod at a narrower span |

> [!NOTE]
> Do not hunt blocked pods with `kubectl get pods`. Their `STATUS` alternates between
> `CreateContainerError` and `CrashLoopBackOff` across the kubelet's retry backoff, so a
> single listing tells you nothing — use the `PinningBlocked` Events. The series stays
> non-zero until a human acts, with one exception: a `Running` pod whose config also moved
> under `driftPolicy: restart` is evicted by drift instead, and that converges.

`cpu_pinning_webhook_bypasses_total` cannot see API-server fail-open when the webhook is
unreachable: the webhook never handled that request. Correlate pod creation time with
webhook endpoints and API-server admission metrics.

`cpu_pinning_webhook_reservations_recorded_total` and `cpu_pinning_webhook_reservations_live` are
per-replica; group by scrape target when investigating HA. With the default
`webhook.reserveAuthority.enabled` only the lease holder's series moves; a concurrent spike
for one node on two targets means two stores took requests, so the authority is unreachable
and the mode fell back.

## Recording rules and alerts

Rendered where the monitoring CRDs exist. Shipped recording rules:

* **`cpu_pinning:cpu_used:per_numa`** - Claimed CPUs per node and NUMA.
* **`cpu_pinning:cpu_free:per_numa`** - Pinnable capacity minus claimed CPUs, floored at 0. The
  floor matters where a mode excludes hyperthread siblings containers still hold: those
  CPUs count as claimed but sit outside the pinnable total, so the subtraction can cross.
  The over-claim stays readable in `cpu_pinning:cpu_used:per_numa`, unfloored:

  ```promql
  cpu_pinning:cpu_used:per_numa{node="worker-1"}   # => 58
  max by (numa) (cpu_pinning_node_pinnable_cpus{node="worker-1"})  # => 56
  cpu_pinning:cpu_free:per_numa{node="worker-1"}   # => 0, not -2
  ```
* **`cpu_pinning:cpu_used:per_balloon`** - Claimed CPUs per balloon.

Eight alerts ship; each one's remedy is in
[alert remedies](troubleshooting.md#alert-remedies), and every alert's own annotation
links its row there. `CPUPinningWebhookBypass` is the critical one. Set
`metrics.prometheusRule.alerts=false` to keep only recording rules, and add
`metrics.prometheusRule.labels` when Prometheus selects rules by label.

## Dashboards

`grafanaDashboard.enabled=true` provides two dashboards, both ordered **most
important first, then what relates to it, expert detail last**; each states its own
order in its "how to read this page" panel.

| Page | Where | What it covers |
|---|---|---|
| Cluster | `/d/cpu-pinning-fleet`, shipped by the chart | The fleet: how full it is and whether anything is wrong, the machine table (its **Status** column sorted worst first, and the picker into a machine's own page), how the operator is behaving, then failures and logs, both collapsed |
| Per machine | `/d/cpu-pin-node-<node>`, generated by the operator | One machine: what it is, its per-core CPU map, how full it is, who shares which CPUs pool by pool, then expert detail and logs |

Each links to the other; one page per machine exists because the map is laid out from that
machine's topology.

Every panel says what its number is, what healthy looks like and what to do otherwise, in
at most 400 characters, and shows its scale: a threshold, a maximum, or the total it is out
of. `test/chart` enforces both limits. Counters are never read through `increase()`/`rate()`:
they move a few times a day, so those functions' edge extrapolation is unbounded on them,
and `test/chart` fails any dashboard that tries. The counts in **How the
operator is behaving** read the counter samples themselves, across every process the
operator has run — see [counts that survive a rollout](#counts-that-survive-a-rollout).

A machine's shape and class are written into its page at provisioning time, so the
dashboards do not depend on kube-state-metrics exporting node labels; changing either regenerates the page.
Everything a metric carries is read live, so without kube-state-metrics the
`kube_node_info` and `kube_node_status_capacity` columns are absent, and say so.

Leave `grafanaDashboard.datasourceUid` empty unless several Prometheus data sources
exist, such as `openshift-thanos` on OpenShift.

### Counts that survive a rollout

Every counter lives in the operator process, so it starts at zero when that process does.
The panels still read a figure a rollout does not clear, because each process is its own
series — the scrape labels every sample with the `pod` it came from — and a pod that is gone
keeps its final sample until retention drops it. The total is therefore the sum of the last
sample each process exported:

```promql
sum by (outcome) (max by (pod, node, outcome) (last_over_time(cpu_pinning_node_provisioning_total[30d])))
# => {outcome="updated"} 328   # 30 processes, on a cluster rolled out ~20 times that day
sum by (outcome) (cpu_pinning_node_provisioning_total)
# => {outcome="updated"} 4     # the same cluster, reading the counter as it stands
```

`last_over_time` returns a sample that was really observed, never an extrapolation, and a
fresh replica starting at zero *adds* to the sum instead of replacing a share of it, so a
rolling restart shows no dip. `max by (pod, ...)` is the deduplication: a process scraped
by two targets is in the window twice. Separate processes by `pod` and never by
`instance` — pod addresses are recycled, so two processes can share one address.

Three panels read this way: **Admission decisions**, **Machine configuration changes**, and
both figures on **How long a decision takes**, whose histogram buckets are counters like
any other:

```promql
histogram_quantile(0.99, sum by (le) (
  max by (pod, result, le) (last_over_time(cpu_pinning_webhook_admission_seconds_bucket[30d]))))
# => 0.445                     # p99 decision time, over every process
```

What that figure is, exactly:

* **It sums each process's running total, for every process with a sample in the
  trailing 30 days** (or all of retention when retention is shorter). A process alive
  longer than the window adds its earlier events too, so the figure can exceed what
  happened inside the window; it never exceeds the operator's lifetime total.
* **It is not monotonic.** The window trails, so when the oldest process falls out of it
  that process's count leaves the sum and the panel can step down by a few while the
  operator keeps working. Read it as what the operator has done lately.
* **It needs the `pod` label on the scrape.** Prometheus adds it when it discovers the
  operator through the chart's ServiceMonitor, which is the default. A hand-written scrape
  config that drops `pod` leaves every process in one group, and the panels then read the
  largest process instead of the sum — check with
  `count by (pod) (cpu_pinning_node_provisioning_total)`, one series per process.
* **It undercounts a container that restarts in place.** A restart that replaces the pod
  gives the new process a new series, which is what this reading is built for; a container
  restarting *inside* its pod keeps the series, so the counter resets within it and only
  the run after the reset is read. `sum(resets(cpu_pinning_node_provisioning_total[24h]))`
  reports whether that has happened.
* **It is computed in the panel, not in a recording rule.** The window is paid for when a
  human opens the dashboard, and on a fleet retaining 30 days these queries touch millions
  of samples — fine for an instant query, not for an alert or a rule.

**There is no admission-latency trend**, and this mechanism cannot produce one: bucket
counts only ever grow, so a quantile graphed over them is a running to-date figure in which
a regression starting now is diluted by every admission before it. The latency panel is
therefore a single tile, drawn against the 10s the API server allows.

### Operator logs panel (Loki)

The logs rows are **off by default and omitted from the dashboards entirely**: a Loki
datasource variable on a cluster without Loki resolves to nothing, and Grafana reports
`Datasource was not found` on it. Turning them on adds a logs row to each page, with a
picker between the operator's log and the balloons plugin's:

```yaml
grafanaDashboard:
  lokiLogs:
    enabled: true
  # optional: pin by uid; a non-empty value renders the section on its own.
  lokiDatasourceUid: ""
  # stream labels: OTLP/k8sattributes by default, promtail's in the chart README. The
  # panels select both components' namespaces, which need not be the same.
  lokiLabels:
    namespace: k8s_namespace_name
    workload: k8s_deployment_name
    node: k8s_node_name
    pluginWorkload: k8s_daemonset_name
```

#### Two balloons-plugin ERROR messages are hidden

The panels ship a **Plugin's benign errors** picker set to *Hidden*, which appends a
LogQL line filter to the query:

```logql
{k8s_daemonset_name="nri-resource-policy-balloons", k8s_node_name="<node>"}
  != "libmem: unknown allocation"
  != "PodResources API Get method disabled"
```

Both messages come from the plugin (`v0.15.2`) and neither says anything about the cluster.
They are nearly every `ERROR` line it writes, so its level reads as a permanent fault unless
they are hidden. Real errors (`CreateContainer FAILED: balloons: balloon allocation for
container …`) are kept.

| Message | Why it is benign | Rate |
| --- | --- | --- |
| `dismissContainer: failed to release memory for <ns>/<pod>/<container>: libmem: unknown allocation: no request with ID <id>` | The plugin releases a memory allocation for every container leaving a balloon, whether or not it made one. Only the composite `balloon-span*` types set `pinMemory: true`; the policy itself sets `pinMemory: false`, so a container in any other balloon — reserved, default, device — never had an allocation and its release always fails. | One per terminating unpinned container, i.e. pod churn |
| `failed to get pod resources for <ns>/<pod>: rpc error: code = Unknown desc = PodResources API Get method disabled` | The kubelet's PodResources `Get` is behind the alpha `KubeletPodResourcesGet` gate, off by default. `List` is unaffected and pinning works. | One per pod created |

For a container that **is** in a `balloon-span*` the first message would mean a real
bookkeeping failure, and the filter hides it there too. It cannot be the first sign of one:
a memory allocation that fails to be made logs `allocMem: … failed to allocate memory
for …` at `ERROR`, which the filter does not touch.

Set the picker to *Shown (unfiltered stream)* to read the stream as it is. **Nothing is
dropped at the log shipper** — the lines are in Loki and in `kubectl logs`:

```bash
kubectl -n <plugin-ns> logs ds/nri-resource-policy-balloons | grep 'libmem: unknown allocation'
```

#### The `KubeletPodResourcesGet` gate stays off

The alpha `--feature-gates=KubeletPodResourcesGet=true` would silence the second message,
but it is a per-node kubelet change the chart cannot make and the operator does not need it:
`List` works, pinning works, and the only cost is a log line.

The one consequence is device locality. `balloon-device` carries
`preferCloseToDevices: ["podresourceapi:*/*"]`, which resolves per container through this
API, so without the gate a device pod is still routed to `balloon-device` and still pinned,
but its CPUs are not steered to the device's NUMA node. The plugin's `List()` fallback for
this case is dead in `v0.15.2`: it matches on the text
`"pod resource API Get method disabled"` while the kubelet returns
`"PodResources API Get method disabled"`
([nri-plugins#801](https://github.com/containers/nri-plugins/issues/801); the first message
is [nri-plugins#800](https://github.com/containers/nri-plugins/issues/800)).

`agent.podResourceAPI` stays `true` in the generated policy: turning it off would remove the
second message at the source, but it disables the plugin's pod-resources client
outright — both `Get` and `List` — so device locality would be lost everywhere.

## Logs and Events

Logs go to stdout through controller-runtime zap.

```bash
kubectl -n <operator-ns> logs deploy/<fullname> -f   # <fullname>: see troubleshooting.md
kubectl -n <namespace> get events --sort-by=.lastTimestamp
```

`log.level` controls verbosity. `log.development=true` enables console encoding and
stack traces. Drift, blocked pinning, provisioning, denial, and bypass decisions
also emit Kubernetes Events.

Recovery commands live in [troubleshooting.md](troubleshooting.md).
