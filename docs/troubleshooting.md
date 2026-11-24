<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Troubleshooting

Start with the pod, node, operator logs, and the balloons plugin logs:

```bash
kubectl -n <namespace> describe pod <pod>
kubectl describe node <node>
kubectl -n <operator-ns> logs deploy/<fullname> --since=15m
kubectl -n <balloons-ns> logs ds/nri-resource-policy-balloons --since=15m
```

> [!NOTE]
> `<operator-ns>` is the Helm release namespace. `<fullname>` is the release name when
> it contains `cpu-pinning-operator` (so `cpu-pinning-operator` for the documented
> install), otherwise `<release>-cpu-pinning-operator`. `<balloons-ns>` is the release
> namespace for the bundled plugin and usually `kube-system` for an external plugin.

<a id="pod-creation-denied-enforcepinning-cannot-be-satisfied"></a>
<a id="the-capacity-check-passed-but-the-pod-still-wont-pin"></a>
<a id="an-admitted-pod-stays-pending-on-the-node-the-webhook-reserved"></a>
<a id="a-large-span-pod-wont-fit-though-the-node-has-free-cpus-fragmentation"></a>
<a id="a-deployment-sits-below-its-replica-count-and-the-pods-never-start"></a>
<a id="webhook-admits-unmutated-right-after-the-operator-restarts"></a>
<a id="failurepolicy-fail-blast-radius-when-the-webhook-is-down"></a>
<a id="ghost-balloon-allocations-createcontainer-failed-no-suitable-balloon-instance-available"></a>

## Symptoms

| Symptom | Likely cause | Check | Fix |
|---|---|---|---|
| Pod has no balloon annotation | CPU request is fractional, request differs from limit, namespace is outside policy scope, or no rule matches | Inspect CPU resources, namespace labels, and `BalloonPinningPolicy` | Use equal whole-core request/limit; correct policy scope or match rule |
| Pod creation is denied with `enforcePinning cannot be satisfied` | No provisioned node, or mutation failed at `node-lookup`, `capacity-check`, or `marshal` | List `PinningDenied` events and balloons-enabled nodes | Restore provisioning or API access; repeat a bare `kubectl run` after recovery |
| Pod stays in `Init` at `wait-for-balloons` | Plugin pod is absent or not Ready on the target node | Read init log and readiness endpoint | Fix plugin DaemonSet; set the correct balloons namespace |
| `wait-for-balloons` logs `reading readiness CA` or `certificate signed by unknown authority` | The namespace's `<fullname>-readiness-ca` copy is missing, stale or edited | Compare its `ca.crt` with the one in `webhook-server-cert`; read the manager's `readiness-ca` log | Wait a minute for the operator's resync; it keeps polling meanwhile |
| `CreateContainer FAILED: no balloon for annotation` | Stale annotation, missing balloon type, or node policy is stale | Compare pod annotation, node `max-span`, and `BalloonsPolicy` | Recreate pod; reconcile node policy; request a supported span |
| `CreateContainer FAILED: no suitable balloon instance available` | Balloon capacity is full or leaked by a ghost allocation | Check `cpu_pinning_ghost_allocations` and the `NodeResourceTopology` (NRT) container zones | Free capacity; restart only the plugin pod on the affected node for a confirmed ghost |
| Pod remains `Pending` after `coLocate` | Required package topology or node affinity cannot be met | Inspect injected affinity and node labels | Relax `coLocate` or add a compatible node |
| Capacity check passed but NRI refused the pod | NRT was stale, static fallback was used, or another admission consumed capacity | Compare admission time, NRT update, reservations, and plugin log | Wait for NRT; free capacity; check the reserve authority is reachable, since a fallback splits HA accounting |
| Capacity event reports less per-NUMA capacity than `physical-cpus / numa-nodes` | The reserved CPU pool is subtracted: capacity is `physical-cpus / numa-nodes − ceil(reserved-cpus / numa-nodes)`, since the pool is walled off from exclusive balloons. Under `htSiblingPolicy: reserved` that label is the HT-sibling count instead, and nothing is subtracted | Read the node's `…/reserved-cpus` and `…/numa-nodes` labels and compare | Expected, not a fault. Lower the request, raise the span, or shrink `spec.balloon.reserved` if the pool is over-sized |
| Reserved pod remains `Pending` on one hostname | Node became NotReady, tainted, or lost requested resources after admission | Inspect pod affinity and node conditions | Recover node or recreate pod for a new placement decision |
| Memory-heavy pod is rejected with free CPUs | Per-NUMA memory is insufficient | Inspect capacity event and node NUMA memory | Lower memory request or use a larger node/span |
| Large span fails with free CPUs | Free cores are fragmented across NUMA nodes | Check `cpu_pinning_cpu_fragmentation{node}` | Drain/repack node or request a smaller span |
| Node has no `BalloonsPolicy` | Topology probe failed, node is excluded, or cluster config is missing | Inspect node events, labels, and operator log | Fix probe access/config; remove exclusion; reconcile cluster config |
| Pinned pod runs on CPUs its balloon does not own | The kubelet runs the CPU Manager `static` policy, so it hands out exclusive cpusets the plugin also allocates | Read the Node's `CPUManagerConflict` event, `cpu_pinning_cpu_manager_conflict{node}`, and the `CPUManagerCompatible` condition on the `BalloonsClusterConfig`. All three come from kubeadm's `kube-system/kubelet-config`, so they stay silent for a kubelet reconfigured by hand — and when that ConfigMap is absent, carries no `kubelet` key, holds YAML that does not parse, carries a `cpuManagerPolicy` that is not a string, omits the key in a document that does not declare `kind: KubeletConfiguration`, or names an unrecognised policy, the condition reports `Unknown`/`PolicyUnread` and the nodes carry no `cpu-manager-conflict` label, which rules nothing out. A `kind: KubeletConfiguration` document that simply omits `cpuManagerPolicy` is not one of those cases: the field's API default is `none`, so it reads as compatible. Check `cpuManagerPolicy` on the node itself | Set `cpuManagerPolicy: none` and restart the kubelet, or exclude the node with `cpu-pinning-operator.intel.com/disabled=true`. The operator flags the node, it does not arbitrate the two allocators |
| Two pods share a physical core | `cpuAllocation: hyperthreads`, density mode, or old policy remains | Inspect cluster config and rendered policy | Use `physical-cores` with `htSiblingPolicy: exclude`; recreate affected pods |
| Deployment stays below replica count | Plugin refused a pinned container; Kubernetes keeps retrying `CreateContainer` | Check the `PinningBlocked` Event, which names the pod, and `cpu_pinning_pods_pinning_blocked_total`, which counts reports rather than pods — **not** `kubectl get pods`, whose `STATUS` alternates between `CreateContainerError` and `CrashLoopBackOff` across the kubelet's retry backoff | Fix the `cause` (`no-balloon`, `no-capacity` or `plugin-error`), then recreate pod |
| Drifted pods do not restart | `driftPolicy` is `eventsOnly`, PDB blocks eviction, or rate limiter paces rollout | Check `Drifted`/`DriftEvictionDeferred` events | Set `restart`; allow PDB disruption; wait for paced rollout |
| Refused pod never recovers on its own | `CreateContainerError` is not terminal and the pod is already bound, so nothing retries the placement | Check `PinningBlocked` Events | Recreate the pod. Exception: a `Running` pod whose config also moved under `driftPolicy: restart` is evicted by drift and does converge |
| Pod was admitted unmutated during restart | Webhook was unavailable while `failurePolicy: Ignore` | Compare pod creation time with webhook endpoints and operator readiness | Wait for Ready endpoint, then recreate pod; use `Fail` only if blocked admission is acceptable |
| One replica pins while another bypasses | Replica caches differ during startup or reserve authority is unreachable | Compare per-replica logs and readiness | Restore all replicas; verify cache sync and authority connectivity |
| Render fails with `found in Chart.yaml, but missing in charts/` | A subchart archive under `deploy/cpu-pinning-operator/charts/` was deleted, or a version in `Chart.yaml` moved without replacing the archive next to it | `ls deploy/cpu-pinning-operator/charts/` and compare the filenames with the `dependencies` versions in `Chart.yaml` | Restore the archive (`git checkout -- deploy/cpu-pinning-operator/charts/`), or bump the subchart deliberately — see [development.md](internal/development.md#testing) |
| `failurePolicy: Fail` blocks cluster pod creation | Webhook has no ready endpoint and namespace selector is broad | Inspect webhook config and endpoints | Scale operator up or temporarily switch to `Ignore`; narrow namespace scope |
| Labels remain after uninstall | Post-delete cleanup hook was disabled or interrupted | List `cpu-pinning-operator.intel.com/*` node labels | Remove labels with commands below |
| Matched pods run unpinned, or wait in `Init`, after the plugin was removed | The NRI plugin was uninstalled or suspended while pinned pods ran | [The NRI plugin was removed](#the-nri-plugin-was-removed-while-pods-were-pinned) | Reinstall the plugin, then recreate the affected pods |
| Operator breaks after NetworkPolicy enabled | DNS, API server, metrics, health, webhook, or plugin-readiness path is blocked | Inspect denied flows and chart NetworkPolicy | Add the missing CIDR/namespace rule; keep default required ports |

## Alert remedies

Every alert's own annotation links its row here.

| Alert | What happened | Look at first | What ends the page |
|---|---|---|---|
| <a id="cpupinningwebhookbypass"></a>`CPUPinningWebhookBypass` | Pods were admitted with no balloon annotation and run unpinned on shared cores. Per workload namespace and `reason`, over a 15-minute window, so it clears about 15 minutes after the last bypass **while those pods keep running unpinned** | `reason`: `node-lookup` and `capacity-check` mean a cluster read failed; `marshal`, `decode` and `policy-index` are manager faults. Then the manager log | Fix the cause, then delete the affected pods so admission runs again. The alert clearing is not the fix |
| <a id="cpupinningwebhooklatencyhigh"></a>`CPUPinningWebhookLatencyHigh` | p99 admission latency is above 8s. The target is 5s, the alert fires at 8s, and the API server aborts the call at `timeoutSeconds=10` and, under `failurePolicy: Ignore`, admits the pod unpinned | API-server round-trip time, then the per-node capacity and NRT lookups | Restore API-server latency. To tell the two apart, compare the operator's own decision time, `cpu_pinning_webhook_admission_seconds` (per `result`), with the API server's `apiserver_admission_webhook_admission_duration_seconds` for the webhook; a gap between them is network or API-server time |
| <a id="cpupinningpinningblocked"></a>`CPUPinningPinningBlocked` | The balloons plugin refused pinned containers, which stay bound to the node and never become Ready. The value counts refusal reports, about one per blocked pod per 10 minutes — two pods blocked for an hour read about twelve, not two | The `PinningBlocked` Events, which name the pods, and the `cause` label — not `kubectl get pods`, per the `Deployment stays below replica count` row above | `no-capacity`: free room or request a narrower span. `no-balloon`: reconcile the node's `BalloonsPolicy`. Then recreate the pods |
| <a id="cpupinningnodefragmented"></a>`CPUPinningNodeFragmented` | Free CPUs exist that the node's widest balloon span cannot absorb. Nothing is broken: a narrower pod still places on them, and only widest-span placements are affected, which is why it ships `severity: info` | `cpu_pinning_cpu_fragmentation{node="<node>"}`, counted in that node's own `cpuAllocation` unit | Nothing, unless a widest-span pod is waiting. Then drain and repack the node, request a narrower span, or set `spec.balloon.packing: binpack` in `BalloonsClusterConfig` for new pods |
| <a id="cpupinningdriftevictionsstuck"></a>`CPUPinningDriftEvictionsStuck` | Pods whose pinned config drifted are not draining: eviction is persistently blocked by a PodDisruptionBudget, or failing | The `Drifted` and `DriftEvictionDeferred` events, and the PDB they name | Allow the disruption, or fix the eviction error, per the `Drifted pods do not restart` row above |
| <a id="cpupinningballoonspolicyreconcileerrors"></a>`CPUPinningBalloonsPolicyReconcileErrors` | One node's `BalloonsPolicy` render or apply keeps failing, so that node's pinning config is stale or absent | That Node's Events and the operator log | Fix what the log names, usually API access or an unusable cluster config, then wait one reconcile |
| <a id="cpupinningreservedpooloversubscribed"></a>`CPUPinningReservedPoolOversubscribed` | Reserved-pool demand on the node exceeds the provisioned reserved size, so system and unpinned pods contend for reserved CPUs | `cpu_pinning_reserved_pool_utilization_ratio{node="<node>"}` and `cpu_pinning_node_reserved_cpus`, the applied pool it divides by | Raise `spec.balloon.reserved` — see [reserved pool sizing](configuration.md#reserved-pool-sizing) — and let the node re-provision |
| <a id="cpupinningnodeprovisionfailed"></a>`CPUPinningNodeProvisionFailed` | A node's CPU topology probe failed, so the operator cannot render its `BalloonsPolicy` and the node stays out of the pinning pool | The operator log and that Node's Events | Restore the probe path, then the retry that runs every minute provisions the node |

## Verify pinning

The annotation is intent. `NodeResourceTopology` and plugin logs show the actual
assignment:

```bash
kubectl -n <balloons-ns> get balloonspolicies
kubectl -n <balloons-ns> logs ds/nri-resource-policy-balloons | grep assigning
kubectl get noderesourcetopology <node> -o yaml
```

A successful NRT container zone is named `<namespace>/<pod>/<container>` and carries
its cpuset.

## Admission and readiness

List denials when the pod object was never created:

```bash
kubectl get events -n <namespace> --field-selector reason=PinningDenied \
  -o custom-columns=WHEN:.lastTimestamp,MESSAGE:.message
kubectl get nodes -l cpu-pinning-operator.intel.com/balloons-enabled=true
```

Inspect the per-node plugin readiness gate without executing in the manager image,
which has no shell:

```bash
kubectl -n <namespace> logs <pod> -c wait-for-balloons
kubectl get --raw \
  '/api/v1/namespaces/<operator-ns>/pods/https:<manager-pod>:8082/proxy/balloons/ready?node=<node>'
kubectl -n <balloons-ns> get pods -o wide | grep <node>
```

HTTP 200 means Ready. HTTP 503 includes the reason the init container is waiting.
With `balloonsReady.secure: false` drop the `https:` prefix.

## Cleanup

Remove leftovers after an interrupted uninstall:

```bash
kubectl delete validatingwebhookconfiguration \
  <fullname>-validating-webhook --ignore-not-found

# The labels in ManagedNodeLabels (internal/controller/node_controller.go), then
# the topology cache annotation, as the post-delete hook strips them.
P=cpu-pinning-operator.intel.com
kubectl label nodes --all $P/balloons-enabled- $P/max-span- $P/numa-nodes- \
  $P/physical-cpus- $P/threads-per-core- $P/memory-per-numa-bytes- \
  $P/package-topology- $P/amx- $P/node-class- $P/reserved-cpus- \
  $P/cpu-allocation- $P/cpu-manager-conflict-
kubectl annotate nodes --all $P/topology-cache-
```

## The NRI plugin was removed while pods were pinned

Uninstalling or suspending the balloons plugin does not undo pinning that is already
in place: running containers keep their cpusets. Nothing pins a container that starts
on the node while the plugin is gone:

| What starts | What happens |
|---|---|
| A new or re-created matched pod | The `wait-for-balloons` gate holds it in `Init` while the operator sees no Ready plugin on its node. A pod that reaches `Running` in that window carries the pinned annotation and hash but runs on the shared CPUs |
| A container restart in an existing pod | Can stall, on a runtime that needs the plugin to create containers |

The drift check reads the NRT the plugin publishes, and nothing updates it while the
plugin is gone, so a pod that came up unpinned may get no `Drifted` event. This is a
known gap ([threat model, current risk 4](threat-model.md#current-risks)). Check the
NRT zones after the plugin is back, as in [Verify pinning](#verify-pinning).

Recover: reinstall the plugin, wait until its pod is Ready on every pinning node, then
delete the pods that started while it was gone so they are re-admitted and pinned.

## Ghost allocations

A ghost is an NRT container zone whose pod no longer exists. Confirm the pod is
gone before restarting anything:

```bash
kubectl get noderesourcetopology <node> -o yaml
kubectl get pod -n <namespace> <pod>

selector=$(kubectl -n <balloons-ns> get ds nri-resource-policy-balloons -o json \
  | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')
kubectl -n <balloons-ns> delete pod \
  -l "$selector" --field-selector spec.nodeName=<node>
```

The DaemonSet recreates the plugin pod. Restart only the affected node; the operator
reports ghosts but does not mutate NRT.

## More detail

Configuration semantics live in [configuration.md](configuration.md). Metrics,
alerts, dashboards, and logs live in [observability.md](observability.md).
