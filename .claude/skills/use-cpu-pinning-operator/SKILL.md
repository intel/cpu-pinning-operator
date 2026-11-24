---
name: use-cpu-pinning-operator
description: >-
  Install, upgrade, uninstall and run the CPU Pinning Operator (cpu-pinning-operator)
  on Kubernetes or OpenShift with Helm, so CPU-heavy pods get dedicated NUMA-local
  Intel Xeon cores: check prerequisites, choose pods with a BalloonPinningPolicy,
  confirm pinning, diagnose a pod that is not pinned, denied or stuck in Init. Use for
  "pin a CPU-heavy pod to dedicated cores", "why is my pod not pinned", "is pinning
  working". Not for kubelet static CPU Manager tuning, GPU placement, changing the
  operator's code, or profiling a workload.
license: Apache-2.0
---
<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# use-cpu-pinning-operator

The CPU Pinning Operator gives matched pods dedicated, NUMA-local CPU cores without
editing the workload manifest. It writes a per-node `BalloonsPolicy` for the NRI
balloons plugin and annotates each matched pod at admission; the plugin does the
pinning. Use this skill to take a cluster from "nothing installed" to "this pod runs
on its own cores, and here is the evidence".

Commands assume release `cpu-pinning-operator` in namespace
`cpu-pinning-operator-system`, run from a checkout of the release tag of
<https://github.com/intel/cpu-pinning-operator> (no chart repository is published).
Paths such as `./deploy/cpu-pinning-operator` are relative to that checkout. When the project's own docs disagree with this skill,
the docs win.

## Recognize

Load this skill when the user wants to:

- install, upgrade, roll back or uninstall the operator, including on OpenShift / OKD;
- make a CPU-heavy workload run on dedicated, NUMA-local cores: a database, an
  analytics or HPC job, packet processing, a latency-sensitive service, an inference
  server;
- find out whether pinning works, or why one pod is not pinned;
- act on an operator alert (`CPUPinning…`) or an empty dashboard.

## Inspect

Before installing or changing anything, establish the facts. Each check, the
expected answer and the fix are in `references/install-upgrade-uninstall.md`
("Prerequisites").

```bash
kubectl version                                   # API server 1.29+
kubectl get nodes -o wide                         # runtime: containerd or CRI-O
kubectl get --raw /api/v1/nodes/<node>/proxy/configz | jq -r .kubeletconfig.cpuManagerPolicy
# → none   (static fights the balloons plugin for the same CPUs)
kubectl get deploy -n cert-manager                # cert-manager present?
helm version --short                              # 3.14+
helm list -A | grep cpu-pinning-operator          # already installed? which namespace?
kubectl api-resources | grep -i securitycontextconstraints   # non-empty = OpenShift
```

Also read the workload: its namespace, container names, pod labels and CPU requests. A
pin needs a CPU request **equal to its limit, in whole cores**. For a container-name
match, the matched container must have one; for a whole-pod match (`byPodLabels`,
`containerSelection: wholePod`), one such container in the pod is enough, so a
fractional sidecar does not stop it. A pod with none is skipped, by design.

## Decide

| Question | Evidence | Choice |
|---|---|---|
| Plain Kubernetes or OpenShift? | `securitycontextconstraints` resource exists | OpenShift: add `-f deploy/cpu-pinning-operator/values-openshift.yaml` |
| Is NRI on? | containerd 2.x enables it by default; without it the plugin pod never becomes Ready | Turn it on before installing; otherwise matched pods wait in `Init` |
| Bundled balloons plugin or one already installed? | `kubectl get ds -A \| grep nri-resource-policy-balloons` | Already installed: `--set nri-resource-policy-balloons.enabled=false` |
| How to name the pods? | container name, or pod labels | `balloonPinningPolicy.match.byContainerName` or `.byPodLabels` |
| Which namespaces? | a fixed list, or "any namespace with this label" | `balloonPinningPolicy.namespaces` or `.namespaceSelector` |
| Refuse pods that cannot fit? | the user prefers a rejected create over a pod pinned past capacity | `balloonPinningPolicy.capacityCheck.mode: enforce` (default `warn`). Under `enforce` admission rejects the pod, so no Pod object exists: a Deployment stays below its replica count with failed-create events on its ReplicaSet |
| Several workload families? | more than one runtime or team | `balloonPinningPolicy.create=false`, then apply one `BalloonPinningPolicy` each |

The chart's `default` policy has no target until you give it one: the install fails
and says what to set. The full value table, how the span (NUMA nodes a pod covers) is
read from the pod, overlapping policies and a custom policy are in
`references/choosing-what-gets-pinned.md`.

## Act

Confirm the namespaces and match rule with the user before installing: a policy
mutates every matching pod created afterwards.

```bash
# 1. cert-manager first (skip if present)
helm repo add jetstack https://charts.jetstack.io && helm repo update
helm install cert-manager jetstack/cert-manager --version v1.20.3 \
  -n cert-manager --create-namespace --set crds.enabled=true --wait

# 2. the operator, with the bundled balloons plugin
helm install cpu-pinning-operator ./deploy/cpu-pinning-operator \
  -n cpu-pinning-operator-system --create-namespace \
  --set 'balloonPinningPolicy.match.byContainerName={<container-example>}' \
  --set 'balloonPinningPolicy.namespaces={<workload-namespace>}'
#    OpenShift / OKD: add  -f deploy/cpu-pinning-operator/values-openshift.yaml
#    by pod label instead: --set 'balloonPinningPolicy.match.byPodLabels.app=<app-example>'

# 3. wait for the webhook and at least one provisioned node
kubectl -n cpu-pinning-operator-system rollout status deploy/cpu-pinning-operator
kubectl wait bcc/cluster --for=condition=Ready --timeout=10m   # reason NodesProvisioned
```

A pod created before step 3 finishes is admitted unpinned (the webhook mutates only
at `CREATE`) or denied by `enforcePinning`; recreate it afterwards.

Upgrade: Helm never updates `crds/`, so apply the new checkout's CRDs before every
`helm upgrade`. Upgrade, rollback, uninstall and what each leaves behind are in
`references/install-upgrade-uninstall.md`.

## Verify

The annotation is intent; the plugin's assignment is the proof. Check both:

```bash
NS=<workload-namespace>; OPNS=cpu-pinning-operator-system
# intent: balloon.balloons.resource-policy.nri.io/container.<name> (or /pod) = balloon-span<N>
kubectl -n $NS get pod <pod> -o json | jq '.metadata.annotations
  | with_entries(select(.key | test("balloon|cpu-pinning-operator")))'
# operator health: Ready=True NodesProvisioned, CPUManagerCompatible=True
kubectl get bcc cluster -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
# actual: a NodeResourceTopology zone <ns>/<pod>/<container> carrying its cpuset
kubectl -n $OPNS logs ds/nri-resource-policy-balloons | grep assigning
kubectl get noderesourcetopology <node> -o yaml
```

Policy status, the per-node `BalloonsPolicy`, metrics and dashboards are in
`references/verify-and-observe.md`, with a smoke-test pod for a cluster that has no
workload yet.

## Recover

Match the symptom to a row in `references/troubleshooting.md`, run its check, apply its
fix, then recreate the affected pod: pinning happens only at pod `CREATE`, so fixing
the cause never re-pins a pod that already exists. The most common rows:

| Symptom | First suspect |
|---|---|
| No balloon annotation | request is fractional or differs from the limit; namespace or name outside the policy |
| Denied: `enforcePinning cannot be satisfied` | no node provisioned yet, or the webhook could not read the cluster |
| Stuck in `Init` at `wait-for-balloons` | balloons plugin pod absent or not Ready on that node (NRI off?) |
| `CreateContainer FAILED: no suitable balloon instance available` | node full, or a ghost allocation |
| Deployment below its replica count | `PinningBlocked` Events name the pod and the `cause` |
| `CPUManagerCompatible` is `False` or `Unknown` | a kubelet runs the `static` CPU Manager policy, or its config is unreadable |

Restart only what a row names. Do not delete a `BalloonPinningPolicy`, a CRD or the
plugin to "reset" pinning: running pods keep their cpusets, and new ones go unpinned
or wait.

## Report

Return to the user:

- what was installed or changed (chart version, values given, OpenShift overlay or not);
- for each workload asked about: its pod annotation, the balloon it got, and the
  cpuset from the plugin log or `NodeResourceTopology`;
- the `BalloonsClusterConfig` conditions;
- anything left unpinned, with the troubleshooting row that explains it and the
  action taken or still needed.

"Installed" without the cpuset evidence is not a verified result.

## When not to use this

- Building, testing or changing the operator's code or CRDs: the project's
  contributor docs, <https://github.com/intel/cpu-pinning-operator/tree/main/docs/internal>.
- Tuning the kubelet `static` CPU Manager or Topology Manager instead of this
  operator: out of scope, and the two must not run together on a pinning node.
- GPU or accelerator placement: out of scope. A pod requesting a device gets the
  `balloon-device` balloon and no span is read from it.
- "My pinned workload is still slow": profile the workload itself with a CPU profiling
  tool. Pinning stops pods sharing cores; it does not isolate host processes or
  interrupts.
- Hardening a production install (NetworkPolicy, Pod Security, image digest): follow
  <https://github.com/intel/cpu-pinning-operator/blob/main/docs/secure-configuration.md>.

## References

- `references/install-upgrade-uninstall.md` — prerequisites and checks, install,
  OpenShift, upgrade, rollback, uninstall, cleanup
- `references/choosing-what-gets-pinned.md` — chart values, span rules, custom and
  overlapping policies, cluster defaults
- `references/verify-and-observe.md` — every check, a smoke-test pod, metrics, dashboards
- `references/troubleshooting.md` — symptom table, alert remedies, recovery commands
- Project documentation: <https://github.com/intel/cpu-pinning-operator/tree/main/docs>
