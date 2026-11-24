<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Troubleshooting

`<operator-ns>` is the release namespace. `<fullname>` is the release name when it
contains `cpu-pinning-operator`, otherwise `<release>-cpu-pinning-operator`.
`<balloons-ns>` is the release namespace for the bundled plugin, usually
`kube-system` for one installed separately.

Source: [troubleshooting.md](https://github.com/intel/cpu-pinning-operator/blob/main/docs/troubleshooting.md).

Start here:

```bash
kubectl -n <namespace> describe pod <pod>
kubectl describe node <node>
kubectl -n <operator-ns> logs deploy/<fullname> --since=15m
kubectl -n <balloons-ns> logs ds/nri-resource-policy-balloons --since=15m
```

Pinning happens only at pod `CREATE`. After any fix below, recreate the affected pod.

## Symptoms

| Symptom | Likely cause | Check | Fix |
|---|---|---|---|
| Pod has no balloon annotation | CPU request fractional or different from the limit; namespace outside the policy; no rule matches | `NotPinned` event, CPU resources, namespace labels, the `BalloonPinningPolicy` | Equal whole-core request and limit; correct `namespaces` or `match` |
| Pod creation denied: `enforcePinning cannot be satisfied` | No provisioned node, or mutation failed at `node-lookup`, `capacity-check` or `marshal` | `PinningDenied` events; nodes labelled `balloons-enabled=true` | Restore provisioning or API access, then retry |
| Pod stays in `Init` at `wait-for-balloons` | Plugin pod absent or not Ready on the target node | Init log and the readiness endpoint, below | Fix the plugin DaemonSet (NRI on?); point the operator at the right balloons namespace |
| `wait-for-balloons` logs `reading readiness CA` or `certificate signed by unknown authority` | The namespace's `<fullname>-readiness-ca` copy is missing or stale | Compare its `ca.crt` with `webhook-server-cert` | Wait a minute for the operator's resync; the init container keeps polling |
| `CreateContainer FAILED: no balloon for annotation` | Stale annotation, missing balloon type, or stale node policy | Pod annotation vs node `max-span` label vs `BalloonsPolicy` | Recreate the pod; request a supported span |
| `CreateContainer FAILED: no suitable balloon instance available` | Balloon capacity full, or a ghost allocation | `cpu_pinning_ghost_allocations`; NRT container zones | Free capacity; for a confirmed ghost restart only that node's plugin pod (below) |
| Deployment stays below its replica count | Plugin refused a pinned container; Kubernetes keeps retrying | The `PinningBlocked` Event, which names the pod and `cause` — not `kubectl get pods`, whose status alternates | Fix the `cause` (`no-balloon`, `no-capacity`, `plugin-error`), then recreate the pod |
| Pod `Pending` after `coLocate` | Package topology or affinity cannot be met | Injected affinity vs node labels | Relax `coLocate` or add a compatible node |
| Memory-heavy pod rejected with free CPUs | Per-NUMA memory is insufficient | Capacity event, node NUMA memory | Lower the memory request or use a larger span or node |
| Large span fails with free CPUs | Free cores fragmented across NUMA nodes | `cpu_pinning_cpu_fragmentation{node}` | Drain and repack, or request a smaller span |
| Capacity event shows less per-NUMA room than `physical-cpus / numa-nodes` | The reserved pool is subtracted | Node `…/reserved-cpus` and `…/numa-nodes` labels | Expected. Lower the request, raise the span, or shrink `spec.balloon.reserved` |
| Node has no `BalloonsPolicy` | Topology probe failed, node excluded, or cluster config missing | Node events and labels, operator log | Fix probe access or config; remove the exclusion |
| `CPUManagerCompatible` is `False`, or pinned pod runs on CPUs its balloon does not own | A kubelet runs CPU Manager `static` | Node `CPUManagerConflict` event, `cpu_pinning_cpu_manager_conflict{node}`, the node's `configz` | Set `cpuManagerPolicy: none` and restart the kubelet, or label the node `cpu-pinning-operator.intel.com/disabled=true` |
| `CPUManagerCompatible` is `Unknown` / `PolicyUnread` | kubeadm's `kube-system/kubelet-config` is absent or unreadable | `cpuManagerPolicy` on each node via `configz` | Rules nothing out: check every node by hand |
| Two pods share a physical core | `cpuAllocation: hyperthreads`, or an old policy remains | Cluster config and rendered policy | `physical-cores` with `htSiblingPolicy: exclude`; recreate the pods |
| Drifted pods do not restart | `driftPolicy: eventsOnly`, a PDB blocks eviction, or pacing | `Drifted` / `DriftEvictionDeferred` events | Set `restart`; allow the disruption; wait |
| Pod admitted unpinned during an operator restart | No Ready webhook endpoint under `failurePolicy: Ignore` | Pod creation time vs webhook endpoints | Keep `replicaCount` 2; recreate the pod. Use `Fail` only if blocked admission is acceptable |
| `failurePolicy: Fail` blocks pod creation cluster-wide | No Ready endpoint and a broad namespace scope | Webhook config and endpoints | Scale the operator up; narrow the namespace scope |
| Matched pods unpinned or in `Init` after the plugin was removed | Plugin uninstalled or suspended while pinned pods ran | NRT zones once the plugin is back | Reinstall the plugin, wait for Ready on every pinning node, then recreate the pods that started meanwhile |
| Operator fails after enabling the chart NetworkPolicy | DNS, API server, metrics, webhook or plugin-readiness path blocked | Denied flows | Add the missing CIDR or namespace rule |
| Labels remain after uninstall | Post-delete hook disabled or interrupted | `cpu-pinning-operator.intel.com/*` node labels | Cleanup commands in `install-upgrade-uninstall.md` |
| Dashboards empty | Scrape gets `401` / `403` | Scrape status code | `metrics.readers`; see `verify-and-observe.md` |

## Admission and readiness

```bash
kubectl get events -n <namespace> --field-selector reason=PinningDenied \
  -o custom-columns=WHEN:.lastTimestamp,MESSAGE:.message
kubectl get nodes -l cpu-pinning-operator.intel.com/balloons-enabled=true

kubectl -n <namespace> logs <pod> -c wait-for-balloons
kubectl get --raw \
  '/api/v1/namespaces/<operator-ns>/pods/https:<manager-pod>:8082/proxy/balloons/ready?node=<node>'
kubectl -n <balloons-ns> get pods -o wide | grep <node>
```

HTTP 200 means Ready; 503 carries the reason the init container waits. With
`balloonsReady.secure: false`, drop the `https:` prefix. The manager image has no
shell, so do not `exec` into it.

## Ghost allocations

A ghost is an NRT container zone whose pod no longer exists. Confirm the pod is gone,
then restart only the plugin pod on that node; the DaemonSet recreates it:

```bash
kubectl get noderesourcetopology <node> -o yaml
kubectl get pod -n <namespace> <pod>          # must report NotFound

selector=$(kubectl -n <balloons-ns> get ds nri-resource-policy-balloons -o json \
  | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')
kubectl -n <balloons-ns> delete pod -l "$selector" --field-selector spec.nodeName=<node>
```

## Alert remedies

Every alert's annotation links its row in the project's troubleshooting doc.

| Alert | Meaning | First action |
|---|---|---|
| `CPUPinningWebhookBypass` | Pods admitted with no balloon annotation, running unpinned. Clears on its own while those pods keep running unpinned | Read `reason` (`node-lookup`, `capacity-check`: a cluster read failed; `marshal`, `decode`, `policy-index`: a manager fault), fix it, then delete the affected pods |
| `CPUPinningWebhookLatencyHigh` | Admission p99 is near the API server's timeout, after which `Ignore` admits unpinned | Compare `cpu_pinning_webhook_admission_seconds` with the API server's `apiserver_admission_webhook_admission_duration_seconds` |
| `CPUPinningPinningBlocked` | Plugin refused pinned containers | `PinningBlocked` Events and `cause`; then recreate the pods |
| `CPUPinningNodeFragmented` | Free CPUs the widest span cannot use; severity `info` | Nothing unless a widest-span pod waits; then repack, narrow the span, or `spec.balloon.packing: binpack` |
| `CPUPinningDriftEvictionsStuck` | Drifted pods are not draining | The PDB named in `DriftEvictionDeferred` events |
| `CPUPinningBalloonsPolicyReconcileErrors` | One node's `BalloonsPolicy` keeps failing to render or apply | That node's Events and the operator log |
| `CPUPinningReservedPoolOversubscribed` | Reserved-pool demand exceeds its size | Raise `spec.balloon.reserved` |
| `CPUPinningNodeProvisionFailed` | A node's topology probe failed; the node is out of the pool | Operator log and node Events; it retries every minute |
