<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Verify and observe

`NS` is the workload namespace, `OPNS` the release namespace. The bundled plugin runs
in the release namespace; an externally installed one usually in `kube-system`.

Source: [observability.md](https://github.com/intel/cpu-pinning-operator/blob/main/docs/observability.md),
[troubleshooting.md, Verify pinning](https://github.com/intel/cpu-pinning-operator/blob/main/docs/troubleshooting.md#verify-pinning).

## Smoke test

For a cluster with no workload yet, with the install from `SKILL.md` (container
`<container-example>` in `<workload-namespace>`):

```bash
kubectl create namespace <workload-namespace>
kubectl apply -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: pinning-demo
  namespace: <workload-namespace>
spec:
  containers:
    - name: <container-example>
      image: <image-example>
      resources:
        requests: {cpu: "4", memory: 256Mi}
        limits: {cpu: "4", memory: 256Mi}
YAML
kubectl -n <workload-namespace> get pod pinning-demo -o jsonpath='{.metadata.annotations}'
# → includes "balloon.balloons.resource-policy.nri.io/container.<container-example>":"balloon-span1"
```

Delete `pinning-demo` when the checks below pass.

## The checks, in order

```bash
NS=<workload-namespace>; OPNS=cpu-pinning-operator-system

# 1. Pod annotations: balloon.balloons.resource-policy.nri.io/pod (or /container.<name>)
#    = balloon-span<N>, and cpu-pinning-operator.intel.com/pinned-by = cpu-pinning-operator
kubectl -n $NS get pod <pod> -o json | jq '.metadata.annotations
  | with_entries(select(.key | test("balloon|cpu-pinning-operator")))'
kubectl -n $NS get pods -o custom-columns='POD:.metadata.name,PINNED:.metadata.annotations.cpu-pinning-operator\.intel\.com/pinned-by'

# 2. Cluster config conditions: Ready (reason NodesProvisioned), CPUManagerCompatible,
#    ExcludeNodeSelectorValid
kubectl get bcc cluster -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
kubectl get bcc cluster -o jsonpath='{.status.perNode}'
kubectl get bpp -o jsonpath='{range .items[*]}{.metadata.name}: {.status.conditions[?(@.type=="Ready")].reason} boundPods={.status.boundPodsTotal}{"\n"}{end}'

# 3. Per-node BalloonsPolicy, named node.<node>
kubectl get nodes -L cpu-pinning-operator.intel.com/balloons-enabled,cpu-pinning-operator.intel.com/max-span
kubectl -n $OPNS get balloonspolicies
kubectl -n $OPNS get balloonspolicy node.<node> -o yaml

# 4. What the plugin actually assigned
kubectl -n $OPNS logs ds/nri-resource-policy-balloons | grep assigning
kubectl get noderesourcetopology <node> -o yaml   # zone <ns>/<pod>/<container> with its cpuset
```

Steps 1 and 2 show intent and operator health; only step 4 shows the cpuset a
container really got. A missing NodeResourceTopology object, or one with no zones, means
the pinning is **unverified**, not absent: the plugin may not have published its topology
yet, and the operator's drift check waits in that case rather than reporting the pod.
Settle it with the plugin's `assigning` log line for the container, or the container's
own cpuset (`kubectl -n $NS exec <pod> -c <container> -- grep Cpus_allowed_list /proc/self/status`).

| Condition on `bcc cluster` | Healthy | Otherwise |
|---|---|---|
| `Ready` | `True`, reason `NodesProvisioned` | `NoNodesProvisioned`: no node has a `BalloonsPolicy` yet |
| `CPUManagerCompatible` | `True` | `False`: a kubelet runs the `static` policy. `Unknown` / `PolicyUnread`: kubeadm's `kube-system/kubelet-config` could not be read, which rules nothing out; check each node's `configz` |
| `ExcludeNodeSelectorValid` | `True` | `False`, reason `InvalidSelector`: `spec.excludeNodeSelector` does not parse and is ignored, so the nodes it meant to exclude are **not** excluded |

## Metrics

Metrics are HTTPS on port 8080 and authenticate every client. List the ServiceAccount
your Prometheus runs as, or the scrape gets `403` and the dashboards stay empty:

```yaml
metrics:
  readers:
    - name: prometheus-kube-prometheus-prometheus
      namespace: monitoring
  serviceMonitor:
    enabled: true
  prometheusRule:
    enabled: true        # recording rules and the CPUPinning* alerts
grafanaDashboard:
  enabled: true
```

Confirm the grant:
`kubectl auth can-i --as=system:serviceaccount:<ns>:<sa> get /metrics`.
No `cpu_pinning_` series at all: read the scrape's status code first. `401` means no
token was presented (a hand-rolled scrape, or one through the API server's pod proxy);
`403` means the ServiceAccount is not in `metrics.readers`.

```promql
sum by (workload_namespace) (cpu_pinning_pods_pinned_total)                # pods pinned
sum by (workload_namespace, reason) (cpu_pinning_webhook_bypasses_total)  # admitted UNPINNED; should stay 0
sum by (workload_namespace, reason) (cpu_pinning_pods_skipped_total)      # intentional skips
sum by (node, cause) (cpu_pinning_pods_pinning_blocked_total)             # plugin refusals, in reports not pods
```

Group workload counters on `workload_namespace`, not `namespace`: `namespace` is the
scrape target's own label.

## Dashboards

With `grafanaDashboard.enabled=true`, a Grafana running the dashboards sidecar imports
the ConfigMaps labelled `grafana_dashboard: "1"`:

```bash
kubectl -n cpu-pinning-operator-system get configmap -l grafana_dashboard=1
```

| Page | Grafana path |
|---|---|
| Cluster | `/d/cpu-pinning-fleet` |
| Per machine | `/d/cpu-pin-node-<node>`, generated by the operator |

Leave `grafanaDashboard.datasourceUid` empty unless several Prometheus data sources
exist.
