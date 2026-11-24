<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Install, upgrade, uninstall

Release `cpu-pinning-operator`, namespace `cpu-pinning-operator-system`, commands run
from a checkout of the release tag:

```bash
git clone https://github.com/intel/cpu-pinning-operator
cd cpu-pinning-operator
git checkout v1.0.0
```

Source: [README](https://github.com/intel/cpu-pinning-operator/blob/main/README.md),
[chart README](https://github.com/intel/cpu-pinning-operator/blob/main/deploy/cpu-pinning-operator/README.md),
[upgrade.md](https://github.com/intel/cpu-pinning-operator/blob/main/docs/upgrade.md).

## Prerequisites

| Need | Detail | Check |
|---|---|---|
| Kubernetes | 1.29+ is the stated floor (untested). Tested: API server 1.33.5 with live pinning, 1.37.0 API only | `kubectl version` |
| Runtime with NRI on | Tested: containerd 2.1.4; CRI-O 1.35 on OpenShift 4.22.4. Other versions untested. On containerd 2.0.5 the plugin can freeze the node under pod churn | containerd 2.x enables NRI by default; if the config turns it off, set `disable = false` under `[plugins."io.containerd.nri.v1.nri"]` and restart containerd |
| kubelet CPU Manager | Policy `none` on every pinning node. `static` allocates the same CPUs as the plugin | `kubectl get --raw /api/v1/nodes/<node>/proxy/configz \| jq -r .kubeletconfig.cpuManagerPolicy` prints `none` |
| cert-manager | Install **before** the chart; the webhook needs its certificates. Tested v1.20.3 | `kubectl get deploy -n cert-manager` |
| Helm | 3.14+ is the stated floor; CI renders with v3.17.3 | `helm version --short` |
| NRI balloons plugin | Bundled and on by default (`nri-resource-policy-balloons.enabled: true`), v0.15.2, runs in the release namespace | To use one you installed yourself, set `nri-resource-policy-balloons.enabled=false`; the operator then looks in `kube-system`, or in `initContainer.balloons.namespace` when set |
| Image | `<image-example>` (the chart's `image.repository` default), linux/amd64 only | A mirror sets `image.repository` (and `image.digest` to pin a build) |
| Pod | CPU request equal to the limit, in whole cores, or the pod is skipped | `kubectl get pod <pod> -o jsonpath='{.spec.containers[*].resources}'` |

Without NRI the plugin pod never becomes Ready and matched pods wait in `Init`.

## Install

```bash
# 1. cert-manager
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm install cert-manager jetstack/cert-manager --version v1.20.3 \
  -n cert-manager --create-namespace --set crds.enabled=true --wait

# 2. the operator. The pods to pin and the namespaces to watch are required;
#    without them the render fails and the error says what to set.
helm install cpu-pinning-operator ./deploy/cpu-pinning-operator \
  -n cpu-pinning-operator-system --create-namespace \
  --set 'balloonPinningPolicy.match.byContainerName={<container-example>}' \
  --set 'balloonPinningPolicy.namespaces={<workload-namespace>}'

# 3. wait until the webhook serves and a node is provisioned
kubectl -n cpu-pinning-operator-system rollout status deploy/cpu-pinning-operator
kubectl wait bcc/cluster --for=condition=Ready --timeout=10m   # reason NodesProvisioned
```

Bundling cert-manager as a subchart (`--set cert-manager.enabled=true`) exists for
dev/test only and is not reliably single-step on a bare cluster: the chart's
`Certificate` and `Issuer` can be validated before cert-manager's CRDs are registered
(`no matches for kind "Certificate"`). Install cert-manager first.

### OpenShift / OKD

A clean install fails on OpenShift's SecurityContextConstraints without extra grants.
Add the shipped overlay; no manual `oc adm policy add-scc-to-user` is needed:

```bash
helm install cpu-pinning-operator ./deploy/cpu-pinning-operator \
  -n cpu-pinning-operator-system --create-namespace \
  -f deploy/cpu-pinning-operator/values-openshift.yaml \
  --set 'balloonPinningPolicy.match.byContainerName={<container-example>}' \
  --set 'balloonPinningPolicy.namespaces={<workload-namespace>}'
```

The overlay sets two coupled toggles and one default:

- `openShift.enabled=true` renders a least-privilege SCC, ServiceAccount and RBAC for
  the operator's topology probe only. The manager pods pass `restricted-v2` unchanged.
- `nri-resource-policy-balloons.openshift.grant-scc=true` makes the balloons subchart
  render its own SCC and run its DaemonSet as `spc_t`, so it can write its hostPath
  cache under SELinux enforcing.
- `balloonsClusterConfig.balloon.reservedNamespaces` is scoped to `kube-system` and
  `openshift-*`. The chart-wide default (`["*", "kube-system"]`) sends every unpinned
  pod to the reserved pool, a node-exhaustion hazard on a shared worker.

On OpenShift with more than one Prometheus data source, set
`grafanaDashboard.datasourceUid` (for example to the `openshift-thanos` source).

## Upgrade

Helm installs `crds/` once and never updates them, on upgrade or rollback. Every time:

```bash
helm get values cpu-pinning-operator -n cpu-pinning-operator-system > values-before-upgrade.yaml
kubectl get deploy -n cpu-pinning-operator-system -l app.kubernetes.io/name=cpu-pinning-operator \
  -o jsonpath='{.items[0].spec.template.spec.containers[0].image}'   # note it for a rollback
kubectl apply -f deploy/cpu-pinning-operator/crds/
helm upgrade cpu-pinning-operator ./deploy/cpu-pinning-operator -n cpu-pinning-operator-system --wait
kubectl diff -f deploy/cpu-pinning-operator/crds/ && echo "CRDs in sync with the chart"
```

When the bundled plugin's version changes, its `balloonspolicies.config.nri` and
`noderesourcetopologies` CRDs need the same treatment:
`tar xzf deploy/cpu-pinning-operator/charts/nri-resource-policy-balloons-*.tgz`, then
`kubectl apply -f nri-resource-policy-balloons/crds/`.

Skip the CRD step and the API server rejects any new field:
`strict decoding error: unknown field "spec.<field>"`.

| Before upgrading, check | Why |
|---|---|
| `replicaCount` is 2 (the default) | With no Ready endpoint, the webhook's `failurePolicy: Ignore` admits matched pods **unpinned**, beyond `enforcePinning`'s reach |
| `metrics.readers` lists your Prometheus ServiceAccount | Metrics require auth; an unlisted scraper gets `403` and dashboards go empty |
| `certManager.certificate.secretName` is unchanged | A new name mints a new Secret; new manager pods stay in `ContainerCreating` and `helm upgrade --wait` looks hung |
| With `certManager.enabled: false`: `webhook.caBundle` is set and `webhook-server-cert` exists | Without `caBundle` the guard webhook is forced to `failurePolicy: Ignore`; without the Secret the rollout never becomes Ready |

### Running pods are not re-pinned

The pod webhook runs only on `CREATE`, so an upgrade re-pins nothing. Each policy's
`spec.driftPolicy` decides what happens to a pod the new configuration would place
differently:

| `driftPolicy` | Effect |
|---|---|
| `eventsOnly` (default) | One Warning `Drifted` event per stale pod; restart the workload yourself |
| `restart` | Also evicts each drifted pod through the eviction API (PodDisruptionBudget-aware, paced) so it is re-admitted |

List them: `kubectl get events -A --field-selector reason=Drifted`.

## Roll back

```bash
helm rollback cpu-pinning-operator <revision> -n cpu-pinning-operator-system
```

A rollback reverts only what Helm tracks:

| Survives | What to do |
|---|---|
| CRDs | Apply the older chart's `crds/` by hand |
| Operator node labels | Leave them, or strip them as in Cleanup below |
| Per-node `BalloonsPolicy` objects | Nothing: the rolled-back manager rewrites them |
| Running pinned pods | Restart the workload, or let `driftPolicy: restart` evict it |

## Uninstall

```bash
helm uninstall cpu-pinning-operator -n cpu-pinning-operator-system
```

A `pre-delete` hook removes the guard `ValidatingWebhookConfiguration`; a
`post-delete` hook strips the operator's node labels. Both need `preDeleteHook.enabled`
and `rbac.create`. CRDs stay. Removing them also removes every `BalloonPinningPolicy`
and `BalloonsClusterConfig`, so confirm with the user first:

```bash
kubectl delete crd balloonpinningpolicies.cpu-pinning-operator.intel.com \
  balloonsclusterconfigs.cpu-pinning-operator.intel.com
```

The plugin's `balloonspolicies.config.nri` and `noderesourcetopologies` CRDs also stay.
They are shared with any other NRI plugin; leave them unless none uses them.

## Cleanup after an interrupted uninstall

`<fullname>` is the release name when it contains `cpu-pinning-operator`, otherwise
`<release>-cpu-pinning-operator`.

```bash
kubectl delete validatingwebhookconfiguration \
  <fullname>-validating-webhook --ignore-not-found

P=cpu-pinning-operator.intel.com
kubectl label nodes --all $P/balloons-enabled- $P/max-span- $P/numa-nodes- \
  $P/physical-cpus- $P/threads-per-core- $P/memory-per-numa-bytes- \
  $P/package-topology- $P/amx- $P/node-class- $P/reserved-cpus- \
  $P/cpu-allocation- $P/cpu-manager-conflict-
kubectl annotate nodes --all $P/topology-cache-
```

Leave the two labels the operator only reads and you set:
`cpu-pinning-operator.intel.com/disabled`, and the class override named by
`spec.classification.overrideLabel`.
