<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Secure configuration and hardening

Which security controls the chart turns on by default, which knob controls each one,
and which hardening options are off by default. Values, ports and flags below are from the shipped
chart (`deploy/cpu-pinning-operator`) and `cmd/main.go`; snippets assume release
`cpu-pinning-operator` in namespace `cpu-pinning-operator-system`.

Trust boundaries and STRIDE threats: [threat-model.md](threat-model.md). What data is
read and stored: [Data handling](#data-handling). Restricting who may edit the
pinning CRDs: [admission-policies.md](admission-policies.md).

These settings reduce risk; how much depends on your cluster (a CNI that enforces
NetworkPolicy, cert-manager, API server settings). No product or component can be
absolutely secure.

## Start here: the hardened install

A default `helm install` runs the operator's own pods unprivileged, with settings that
meet the `restricted` Pod Security Standard, and serves both the webhook and the metrics endpoint over cert-manager TLS. The
bundled balloons plugin is the exception: its DaemonSet mounts `hostPath` volumes,
which the `restricted` and `baseline` levels forbid. Four
things are worth changing for a multi-tenant cluster or one with stricter security
requirements, and none of
them alter how pinning behaves:

```yaml
# hardened-values.yaml — layer on top of the chart defaults.

# 1. Turn on the default-deny NetworkPolicy and scope the metrics scrape to the
#    namespace your Prometheus runs in. The endpoint already authenticates its
#    clients (see "The metrics endpoint"), so this narrows who can reach it at
#    all — defence in depth over the identity check, not a substitute for it.
networkPolicy:
  enabled: true
  metrics:
    from:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
  # Pin the webhook port to the addresses your API server dials from. Left empty
  # (the default) ANY pod can post a crafted AdmissionReview at the webhook, and
  # the webhook requires no client cert. See "Restricting the webhook port" for
  # finding the CIDRs — a wrong value stops admission, so verify after upgrading.
  webhook:
    from:
      - ipBlock:
          cidr: 10.0.0.0/28
  egress:
    # Keep only the port your API server actually answers on.
    apiServerPorts:
      - 6443
    dns: true

# 2. Pin the operator image. `repository` defaults to the published
#    docker.io/intel/cpu-pinning-operator; set it to your mirror if you mirror
#    the image. A `digest` renders
#    "<repository>@<digest>" and takes precedence over `tag`, which renders
#    "<repository>:<tag>" and follows the chart's appVersion when left empty.
image:
  repository: registry.example.com/cpu-pinning-operator
  digest: "sha256:<hex>"      # or an immutable release tag you control, via `tag`

# 3. Run the operator namespace under the restricted Pod Security Standard (the
#    namespace and its labels are created by the commands below). The operator's
#    own workloads meet it. The bundled balloons plugin does not (hostPath), so
#    install it separately in a namespace that allows privileged pods, such as
#    kube-system (command below), and turn the subchart off.
nri-resource-policy-balloons:
  enabled: false               # the operator then expects the plugin in kube-system

probe:
  # The probe runs the operator image itself, so it needs no reference of its own
  # and nothing extra to mirror. Set it only to name another mirror of that image.
  image: ""
  # 4. Restrict where the topology probe may run, instead of tolerating every
  #    taint. Topology is only needed on nodes you intend to pin on. This must be
  #    an explicit NON-EMPTY list: an empty list falls back to tolerate-all (see
  #    "Container and pod hardening").
  tolerations:
    - key: workload
      operator: Equal
      value: inference
      effect: NoSchedule
```

```bash
# cert-manager is a PREREQUISITE, not a bundled default (cert-manager.enabled is
# false). Install it first or the webhook has no serving cert and the API server
# cannot call it.
helm repo add jetstack https://charts.jetstack.io && helm repo update
helm install cert-manager jetstack/cert-manager --version v1.20.3 \
  -n cert-manager --create-namespace --set crds.enabled=true --wait

# The balloons plugin, at the version the chart vendors, outside the restricted namespace.
helm repo add nri-plugins https://containers.github.io/nri-plugins && helm repo update
helm install balloons nri-plugins/nri-resource-policy-balloons \
  --version v0.15.2 -n kube-system

# The operator namespace, created and labelled before the install. Helm needs the
# release namespace to exist before it creates anything, so the chart's own
# namespace.create cannot make it here, and --create-namespace would leave it
# unlabelled.
kubectl create namespace cpu-pinning-operator-system
kubectl label ns cpu-pinning-operator-system \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted

helm install cpu-pinning-operator ./deploy/cpu-pinning-operator \
  -n cpu-pinning-operator-system \
  -f hardened-values.yaml \
  --set 'balloonPinningPolicy.match.byContainerName={vllm}' \
  --set 'balloonPinningPolicy.namespaces={inference}'
```

Two further changes are deployment decisions rather than pure hardening, because
each trades availability for a stronger guarantee — see
[Admission webhooks](#admission-webhooks) before taking them:

```yaml
webhook:
  mutatingWebhookConfiguration:
    failurePolicy: Fail        # default Ignore: an outage admits pods unpinned
balloonsClusterConfig:
  webhook:
    failurePolicy: Fail        # also here, or the singleton reconciler reverts it
balloonPinningPolicy:
  guardActivePods: true        # default unset: policy deletes are unguarded
```

## Default security settings

What you get from `helm install` with no overrides. "Key" is the `values.yaml`
path; templates are under `deploy/cpu-pinning-operator/templates/`.

| Control | Default | Key | Rendered by |
|---|---|---|---|
| Dedicated ServiceAccount (not `default`) | created | `serviceAccount.create: true` | `serviceaccount.yaml` |
| Operator RBAC as one ClusterRole plus one release-namespace Role, no wildcards, no `secrets`, no `clusterrolebindings` | created | `rbac.create: true` | `rbac.yaml` |
| Webhooks served over TLS, CA injected into **both** webhook configurations | on | `certManager.enabled: true` | `certificate.yaml`, `mutatingwebhook.yaml`, `validatingwebhook.yaml` |
| Metrics served over HTTPS with the same cert | on | `metrics.secure: true` | `deployment.yaml` (`--metrics-secure=true`) |
| Metrics clients authenticated: bearer token resolved by `TokenReview`, then authorized for `get` on `/metrics` by `SubjectAccessReview` | on | `metrics.requireAuth: true` | `deployment.yaml` (`--metrics-require-auth=true`), `rbac.yaml` (`…-metrics-reader`) |
| Prometheus scrape verifies the cert (no `insecureSkipVerify`) **and** presents its own ServiceAccount token | on when the ServiceMonitor is rendered | `metrics.serviceMonitor.*` | `servicemonitor.yaml` |
| Manager: read-only root filesystem, all capabilities dropped, no privilege escalation, `RuntimeDefault` seccomp, non-root | on | `securityContext`, `podSecurityContext` | `deployment.yaml` |
| Minimal non-root image (`FROM scratch`: the static binary and its licenses, no OS packages, no shell; `USER 65532:65532`) | built in | — | `Dockerfile` |
| Injected `wait-for-balloons` init container hardened the same way (read-only rootfs, drop `ALL`, non-root, `RuntimeDefault`) | built in | — | `internal/webhook/inject.go` |
| Topology probe unprivileged: uid 1000, drop `ALL`, `RuntimeDefault`, no host namespaces, no `hostPath` | built in | — | `internal/topology/discover.go` |
| CPU/memory requests and limits on every container the chart or operator creates | set | `resources` | `deployment.yaml`, hook Jobs, init container |
| Mutating webhook scoped to named namespaces, `sideEffects: None`, bounded timeout | the policy's namespaces, 10s | `webhook.targetNamespaces`, `…timeoutSeconds` | `mutatingwebhook.yaml` |
| Policy-guard validating webhook fails closed | `Fail` | `webhook.validatingWebhookConfiguration.failurePolicy` | `validatingwebhook.yaml` |
| Uninstall hook Jobs use their own single-purpose ClusterRoles, deleted after they run | on | `preDeleteHook.enabled` | `pre-delete-hook.yaml` |
| Webhook cert secret mounted read-only | on | — | `deployment.yaml` (`readOnly: true`) |
| No outbound egress beyond the API server, cluster DNS, the operator's own readiness Service (admission preflight) and other replicas' reserve port (HA) | by design | — | see [Data handling](#data-handling) |

## Identity and RBAC

The manager runs as one ServiceAccount bound to a single ClusterRole and to one Role in
the release namespace (`<fullname>-probe-pods`). No aggregation, no wildcards, no
`secrets` grant, and no `serviceaccounts` or `clusterrolebindings` verbs. Grants live
in `templates/rbac.yaml`, kept in step with the generated baseline in
`deploy/generated/role.yaml`. With `rbac.create: false`, grant both yourself.

| API group / resource | Verbs | Why |
|---|---|---|
| `""` / `nodes` | `get, list, watch, patch` | Provisioning; publishes topology labels |
| `""` / `pods` | `get, list, watch` | Reads for the readiness endpoint, the webhook and drift |
| `""` / `pods` (Role, release namespace only) | `create, delete` | Creates/deletes the topology-probe pods, which run in `POD_NAMESPACE` |
| `""` / `pods/log` | `get` | Reads the probe pod's stdout (that is how topology is returned) |
| `""` / `pods/eviction` | `create` | Only used by `driftPolicy: restart` |
| `""` / `namespaces` | `list, watch` | Resolves policy `namespaces` / `namespaceSelector` |
| `""` / `events` | `create, patch` | Provisioning, drift, bypass and preflight events |
| `""` / `configmaps` | `get, list, watch, create, update, delete` | Per-node Grafana dashboard ConfigMaps; one `<fullname>-readiness-ca` ConfigMap (the public `ca.crt`) in each namespace the webhook targets; the same `get` reads kubeadm's `kube-system/kubelet-config` for the cluster's CPU Manager policy |
| `apps` / `daemonsets` | `get` | Finds the balloons DaemonSet's pod selector |
| `coordination.k8s.io` / `leases` | `get, list, watch, create, update` | Leader election |
| `config.nri` / `balloonspolicies` | `get, list, watch, create, patch, delete` | Writes the per-node `BalloonsPolicy` CR the plugin consumes |
| `topology.node.k8s.io` / `noderesourcetopologies` | `get, list, watch` | Live per-NUMA capacity for the admission preflight |
| `cpu-pinning-operator.intel.com` / both CRDs | `get, list, watch` (+ `status`: `update`) | Reads its own config; never writes the spec |
| `authentication.k8s.io` / `tokenreviews`, `authorization.k8s.io` / `subjectaccessreviews` | `create` | Authenticates and authorizes metrics clients |
| `admissionregistration.k8s.io` / `mutatingwebhookconfigurations` | `get, list, watch`; `patch` only on the release's own configuration (`resourceNames`) | Reconciles the webhook's `namespaceSelector` and `failurePolicy`; cannot write any other mutating webhook |

> [!WARNING]
> **Most grants are cluster-scoped, including some verbs the operator uses in only a
> few namespaces.** `pods: create/delete` is a release-namespace Role, because a pod
> may reference any ServiceAccount in the namespace it is created in with no separate
> permission check, so a holder can act as any ServiceAccount in that namespace.
> `pods/log: get` is cluster-wide although probe pods run only in `POD_NAMESPACE`. The
> ConfigMap write verbs are cluster-wide because the operator writes in
> `grafanaDashboard.namespace` and in every namespace the webhook targets (the
> readiness CA copies). Cluster-wide `configmaps: get`
> is what lets the CPU Manager check read kubeadm's `kube-system/kubelet-config`
> without a grant of its own; the alternative, reading each kubelet's `/configz`
> through `nodes/proxy: get`, is **deliberately not granted** — RBAC cannot scope a
> subresource to a path, so that verb would admit the kubelet's whole read surface
> (`/pods`, `/metrics`, `/logs/*`, the node's `/var/log` tree) on every node. Treat
> this ServiceAccount as a high-value identity and see
> [Hardening options that are off by default](#hardening-options-that-are-off-by-default).

> [!WARNING]
> **`serviceAccount.create: false` with no `serviceAccount.name` falls back to the
> namespace `default` ServiceAccount**, binding all of the above to an identity every
> other pod in that namespace can get a token for. Always set an explicit name when you
> manage the ServiceAccount yourself.

## Admission webhooks

Two webhooks, with deliberately opposite `failurePolicy` defaults because their blast
radii are opposite.

| | Mutating (`mpod.kb.io`) | Validating (`vbpp.kb.io`) |
|---|---|---|
| Path | `/mutate-v1-pod` on `CREATE` of `v1/pods` | `/validate-v1alpha1-balloonpinningpolicy` |
| Does | Stamps the balloon annotation, injects the `wait-for-balloons` init container, adds node affinity. Never rewrites CPU requests | Rejects policy deletes and narrowing edits that would drift already-pinned pods |
| Scope | `namespaceSelector`, bootstrapped from `webhook.targetNamespaces` (default: the exact names in `balloonPinningPolicy.namespaces`), then reconciled from the union of active policies' namespaces | `DELETE` and `UPDATE` on `balloonpinningpolicies` only |
| `failurePolicy` | `Ignore` - fail-open | `Fail` - fail-closed, and forced to `Ignore` when `certManager.enabled: false` and `webhook.caBundle` is empty |

* **No target names matches no namespace** - empty `targetNamespaces` and no chart policy namespaces; the safe default direction.
* **`webhook.namespaceSelector: {}` matches *every* namespace** - Kubernetes semantics. Always scope it with `matchLabels`.
* **`failurePolicy: Ignore` is fail-open**: a pod the webhook never answers for runs unpinned, with no cpuset guarantee and without the `enforcePinning` affinity that would have held it `Pending`. A call that never reaches the webhook cannot be counted by the operator: watch the API server's admission-webhook metrics and check matched pods for the `pinned-by` annotation (check 10 in [Verifying a deployment](#verifying-a-deployment)). `cpu_pinning_webhook_bypasses_total` counts only the bypasses the webhook itself handled.
* **The validating guard fails closed** so a safety check cannot be silently bypassed. `preDeleteHook.enabled` removes it during `helm uninstall` so the uninstall exits 0 on the first try.

Availability of the admission path: `replicaCount: 2`,
`podDisruptionBudget.minAvailable: 1` (rendered only when `replicaCount > 1`), soft
anti-affinity, and leader election. Keep `replicaCount` at `min(nodes, 3)` - surplus
replicas share a node's fate and buy nothing.

## TLS and certificates

| Item | Value |
|---|---|
| Issuers | `cpu-pinning-operator-selfsigned-issuer` signs the CA `cpu-pinning-operator-ca` (`certManager.ca.duration`, 1 year; renewed with its key kept); `cpu-pinning-operator-ca-issuer` signs the serving cert. Namespace-scoped |
| Certificate | `cpu-pinning-operator-serving-cert` |
| Secret | `webhook-server-cert` (`certManager.certificate.secretName`) |
| SANs | `cpu-pinning-operator-webhook.<ns>.svc[.cluster.local]`, `cpu-pinning-operator-balloons-ready.<ns>.svc[.cluster.local]`, plus `cpu-pinning-operator-metrics.<ns>.svc[.cluster.local]` when `metrics.enabled` |
| Mount | read-only at `webhook.certDir`, default `/tmp/k8s-webhook-server/serving-certs`, `defaultMode: 420` |
| CA distribution | `cert-manager.io/inject-ca-from: <ns>/cpu-pinning-operator-serving-cert` annotation on both webhook configurations |
| Rotation | cert-manager renews; the mounted secret updates in place and every listener picks it up without a restart (the reserve authority and its followers re-read it on each new connection) |
| Protocol | TLS 1.3 only on 8082 and 9444, whose clients are all the operator's own; TLS 1.2 or 1.3 on 9443 and 8080 (kube-apiserver, Prometheus) |
| TLS 1.2 ciphers | `ECDHE-ECDSA-AES256-GCM-SHA384`, `ECDHE-RSA-AES256-GCM-SHA384`, `ECDHE-ECDSA-AES128-GCM-SHA256`, `ECDHE-RSA-AES128-GCM-SHA256`, in that order; TLS 1.3 suites are Go's fixed set |

One cert-manager `Certificate` backs every TLS listener. There is no second cert stack and nothing self-signed inside the operator.

**This setup has no certificate revocation.** The chart's CA publishes no CRL or
OCSP responder and no client checks either, so a reissued leaf does not invalidate the
old one. If the leaf or CA key leaks, replace both: delete the `cpu-pinning-operator-ca`
and `webhook-server-cert` Secrets, then check each consumer before treating the old
keys as retired: the new `caBundle` on both webhook configurations, the readiness CA
ConfigMap copies (about two minutes), and any scraper that caches the old `ca.crt`
(restart it). A new certificate does not undo anything done with the leaked key. **The Secrets outlive an uninstall** unless
cert-manager runs with `--enable-certificate-owner-ref`, because by default it creates
them without an owner reference; delete them, or the namespace, to retire the keys.

**cert-manager must already be installed.** Two similarly named values do different
things, and only one is on by default:

| Value | Default | What it controls |
|---|---|---|
| `cert-manager.enabled` (subchart) | `false` | Whether this chart **installs cert-manager itself**. Off, so it does not — cert-manager is a prerequisite you install as its own release |
| `certManager.enabled` (this chart) | `true` | Whether this chart renders **its own** `Issuer` + `Certificate` for the serving cert, using an already-installed cert-manager |

So the chart creates cert-manager *custom resources* but not a cert-manager
*installation*. Install it first, or `webhook-server-cert` is never created and the
manager cannot start - it mounts the secret as a required volume, so the failure surfaces
as `ContainerCreating`, never as a webhook quietly serving plaintext.

The CA is self-signed by design: every consumer pins it - the API server via the
injected `caBundle`, Prometheus via `ca.crt`, and the `wait-for-balloons` init
containers via the operator's per-namespace copy of `ca.crt`. To use your own PKI set
`certManager.enabled: false`, supply `webhook-server-cert` with a valid `ca.crt` and
the SANs above, and set `balloonsReady.allowExternalServingCert: true` (or
`balloonsReady.secure: false`); otherwise the render fails. For the HA reserve
authority also set `webhook.reserveAuthority.allowExternalServingCert: true`.

In that mode nothing injects the CA. Pass it as `webhook.caBundle` and the chart
renders it on both webhook configurations, with the policy guard keeping its
configured `failurePolicy`:

```bash
helm upgrade --install ... --set certManager.enabled=false \
  --set webhook.caBundle=$(base64 -w0 ca.crt)
```

Left empty, both render the placeholder `caBundle: Cg==` and the guard is forced to
`Ignore`. When the CA changes, upgrade with the new bundle.

Check 4 in [Verifying a deployment](#verifying-a-deployment) confirms the CA landed.

## Listeners and network exposure

Every listener the manager opens, what protects it, and whether the chart
publishes it as a Service:

| Port | Listener | Transport | Authenticated | Service | Caller |
|---|---|---|---|---|---|
| 9443 | Webhook (`/mutate-v1-pod`, `/validate-v1alpha1-balloonpinningpolicy`) | TLS (serving cert) | The API server verifies the server via the injected CA; the server does not require a client cert, so the network path is the only control — see [Restricting the webhook port](#restricting-the-webhook-port) | `cpu-pinning-operator-webhook` (443 → `webhook-server`) | API server |
| 8080 | Metrics (`/metrics`) | HTTPS by default (`metrics.secure: true`) | Yes — the client's bearer token, by `TokenReview` + `SubjectAccessReview` (`metrics.requireAuth: true`); see below | `cpu-pinning-operator-metrics` | Prometheus |
| 8081 | Health probes (`/healthz`, `/readyz`) | Plain HTTP | No | none (pod IP only) | kubelet |
| 8082 | Per-node balloons readiness (`/balloons/ready?node=<name>`) | HTTPS, TLS 1.3 only, by default (`balloonsReady.secure: true`); the client verifies it against `ca.crt` | No (but rate-limited per source IP: 2 req/s, burst 5) | `cpu-pinning-operator-balloons-ready` | The injected init container, the webhook pre-flight |
| 9444 | Internal reserve authority | TLS 1.3 only, **required client certificate** verified against the serving CA **and** the serving-cert DNS SAN (mTLS) | Yes | none (pod IP only) | Other operator replicas |

`webhook.port` is the port the webhook server listens on and the webhook Service
targets; `webhook.service.port` is the Service port the API server calls.

* **Metrics (8080) authenticate every client.** The manager resolves the request's bearer token with a `TokenReview` (anonymous requests are refused outright, so no token means `401`) and then authorizes that user for `get` on the non-resource URL `/metrics` with a `SubjectAccessReview` (`403` if unauthorized). Both answers are cached, so a scrape loop costs no `TokenReview` per scrape. **Your scraper's ServiceAccount must be bound to the `cpu-pinning-operator-metrics-reader` ClusterRole or the dashboards go empty** — list it under `metrics.readers` (`[{name, namespace}]`) and the chart renders the binding; the chart cannot guess it, so nothing is bound by default. The rendered ServiceMonitor already presents the *scraping* pod's projected token (`bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token`), which is Prometheus's own identity. `metrics.requireAuth: false` keeps TLS and drops only the client check, for a scraper that cannot present a token; the network policy is then the only control left.
* **Health and readiness (8081) are unauthenticated plain HTTP** - no Service publishes them, so on a default install they are reachable only on the pod IP. They disclose liveness and cache-sync state, nothing more.
* **Balloons-readiness (8082) asks no client credentials and is published cluster-wide** - deliberate: the poller sends none and the endpoint returns one per-node boolean. TLS protects the verdict, since a spoofed "ready" would start a workload unpinned. Each source IP is capped at 2 req/s (burst 5) before the API read, returning 429, so it cannot amplify API load.
* **The reserve authority (9444) is the only mutually authenticated listener.** On by default, but it renders only at `replicaCount > 1` with `leaderElection.enabled` and a serving cert carrying `ca.crt` plus a DNS SAN — so a single-replica install opens no listener, and one supplying its own cert opens none until `webhook.reserveAuthority.allowExternalServingCert` confirms it has both. It gives the fleet one accounting store rather than one per replica; its cost is that under `capacityCheck.mode: enforce` a reserve it cannot commit denies the pod.

### Restricting the webhook port

`networkPolicy.webhook.from` is `[]` by default, and an empty peer list means **any
source**: every pod in the cluster can POST a crafted `AdmissionReview` straight at
`:9443`, bypassing the API server, and no client certificate distinguishes it from the
real one. A forged burst consumes the in-memory reservation ledger and denies or
mis-steers real pods. Set the value on a multi-tenant cluster:

```yaml
networkPolicy:
  webhook:
    from:
      - ipBlock:
          cidr: 10.0.0.0/28          # the control-plane subnet
      - ipBlock:
          cidr: 172.20.9.3/32        # a single off-subnet API server
```

Where the API server is host-networked — kubeadm, self-managed, OpenShift — only
`ipBlock` peers can work: no `podSelector` or `namespaceSelector` matches a
host-networked caller. The addresses are site-specific, which is why the chart
cannot ship a default. On a tunnelled control plane the call arrives from a pod in
your own cluster instead, and a `namespaceSelector`+`podSelector` is the correct
peer there — see the per-control-plane table below.

**Finding the addresses.** Start from the API server's own endpoint list, then
confirm what the webhook actually sees:

```bash
# 1. The addresses the cluster reaches the API server on — the control-plane node
#    IPs on kubeadm/self-managed, the private-endpoint ENI/NIC IPs on a managed
#    control plane. Usually the same addresses it dials out from.
kubectl get endpoints kubernetes -n default -o jsonpath='{.subsets[*].addresses[*].ip}{"\n"}'

# 2. What the webhook pod actually receives, which is the authoritative answer
#    (SNAT, an egress gateway or a tunnel can change it). Run on the node hosting a
#    manager replica, while admission traffic is flowing:
sudo conntrack -L -p tcp --dport 9443 2>/dev/null | awk '{print $5}'   # src=<addr>
# Cilium clusters can read it without node access:
hubble observe --namespace cpu-pinning-operator-system --port 9443 --last 20
```

| Control plane | Where the connection originates | Peer to use |
|---|---|---|
| kubeadm / self-managed | the control-plane nodes, host-networked | one `ipBlock` per node IP (`/32`), or their enclosing subnet: `kubectl get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}'` |
| OpenShift / OKD, incl. ROSA and ARO | `kube-apiserver` static pods on the master nodes, host-networked | the same, with the OpenShift role label: `oc get nodes -l node-role.kubernetes.io/master ...` |
| EKS (private or public endpoint) | ENIs the managed control plane places **in your subnets**, so step 1 already lists them | the ENI IPs as `/32`, or the cluster subnet CIDRs if AWS may re-place them |
| GKE (private cluster) | the peered control-plane range, `--master-ipv4-cidr` | that `/28`: `gcloud container clusters describe <c> --format='value(privateClusterConfig.masterIpv4CidrBlock)'` |
| AKS, GKE with konnectivity, any tunnelled control plane | a **pod** in your own cluster (`konnectivity-agent` in `kube-system`) that proxies the call — not an `ipBlock` at all | a `namespaceSelector`+`podSelector` for that agent, verified with step 2 |

> [!WARNING]
> **Naming the wrong CIDR silently stops pinning.** Every `AdmissionReview` then fails
> at the network, and under the default `failurePolicy: Ignore` matched pods are
> admitted unpinned rather than rejected. The operator never sees those calls, so
> `cpu_pinning_webhook_bypasses_total` stays flat: verify with the API server's
> admission-webhook metrics and the mutation check (check 10) in
> [Verifying a deployment](#verifying-a-deployment) right after the upgrade; `--set-json 'networkPolicy.webhook.from=[]'` restores the open
> rule. The validating policy guard fails **closed**, so a wrong value also blocks
> `BalloonPinningPolicy` edits — which is the louder, and faster, symptom.

> [!NOTE]
> This control needs a CNI that enforces NetworkPolicy. Where one is missing, or where
> the control plane is tunnelled through addresses you cannot pin, the exposure stays
> open: requiring an apiserver **client certificate** on `:9443` would close it
> independently of the network, but it needs an `AdmissionConfiguration` kubeconfig
> installed on every control-plane node — impossible on a managed control plane, and a
> rotation the operator cannot automate, where a stale client cert fails admission
> cluster-wide. It is deliberately not implemented; see
> [threat-model.md](threat-model.md) (current risks).

To narrow 8082 to one namespace you must **replace** the chart policy, not add to it -
NetworkPolicies are additive, so an extra policy can never subtract. Set
`networkPolicy.enabled: false` and apply your own, using
`helm template … --set networkPolicy.enabled=true` as the starting point:

```yaml
- ports:
    - port: balloons-ready
      protocol: TCP
  from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: inference
```

## Container and pod hardening

Every workload the operator or its own chart templates create runs unprivileged.
None use `hostPID`, `hostIPC`, `hostNetwork`, host ports or `hostPath` volumes. The
bundled balloons plugin DaemonSet is not covered here: it mounts `hostPath` volumes
(see [the hardened install](#start-here-the-hardened-install)).

| Workload | Non-root | Caps | Privilege escalation | Root FS | Seccomp |
|---|---|---|---|---|---|
| Manager (`deployment.yaml`) | `runAsNonRoot: true`, `runAsGroup: 65532`; image `USER 65532:65532` | `drop: [ALL]` | `false` | `readOnlyRootFilesystem: true` | `RuntimeDefault` |
| Injected `wait-for-balloons` init container | `runAsNonRoot: true`, no pinned uid (stays `restricted-v2`-compatible) | `drop: [ALL]` | `false` | `true` | `RuntimeDefault` |
| Pre-delete / post-delete hook Jobs | inherit `podSecurityContext` and `securityContext` from values | `drop: [ALL]` | `false` | `true` | `RuntimeDefault` |
| Topology probe (`internal/topology/discover.go`) | `runAsNonRoot: true`, `runAsUser: 1000` | `drop: [ALL]` | `false` | `readOnlyRootFilesystem: true` | `RuntimeDefault` |

* **`/tmp` is the only writable path** in the manager, an `emptyDir`, because Go's `os.TempDir()` resolves there and the root filesystem is read-only.
* **`runAsGroup` is pinned, `runAsUser` is not**, for the manager and the two hook Jobs that share the same `securityContext`. An OpenShift SCC constrains the uid (`runAsUser: {type: MustRunAsRange}`, allocated from the namespace's `openshift.io/sa.scc.uid-range`) but has no `runAsGroup` strategy at all — only `fsGroup` and `supplementalGroups` — so a literal gid is admitted as written while a literal uid would need its own SCC, as the probe's does. `runAsGroup: 65532` therefore pins what the image's `USER 65532:65532` already made the effective primary gid, and the uid stays the platform's to assign.
* **The probe runs the operator's own image** with its `topology-probe` subcommand, so it carries no shell and pulls nothing extra: an air-gapped cluster needs only the operator image mirrored. Its uid is pinned to 1000 rather than left to the image's `USER` because the OpenShift SCC below grants exactly that uid; the probe binary is statically linked and needs no passwd entry, so any non-root uid runs it.
* **`podSecurityStandards.enabled: false` by default**, and the labels render only when `namespace.create: true`. Everything above complies with `restricted`; enforce it only with the balloons subchart off and the plugin in another namespace.

> [!WARNING]
> **On vanilla Kubernetes the probe pod has no ServiceAccount of its own.** The pod spec
> leaves `serviceAccountName` empty, so the API server admits it under the namespace
> `default` ServiceAccount and automounts that token - even though the probe never calls
> the Kubernetes API. Keep the operator namespace free of unrelated grants so that
> identity stays worthless. In OpenShift mode a dedicated probe ServiceAccount sets
> `automountServiceAccountToken: false`.

> [!NOTE]
> `openShift.enabled: true` renders a purpose-built SCC (`allowPrivilegedContainer: false`,
> `allowHostDirVolumePlugin: false`, `requiredDropCapabilities: [ALL]`, `runAsUser` range
> 1000-1000) bound by an RBAC `use` grant to a dedicated `cpu-pinning-operator-probe`
> ServiceAccount, never to the namespace `default`.

## Resource limits and availability

Every container the chart creates has both requests and limits, so a runaway operator
cannot starve a node and the pods are not `BestEffort`:

| Container | Requests | Limits |
|---|---|---|
| Manager | `cpu: 100m`, `memory: 128Mi` | `cpu: 500m`, `memory: 512Mi` |
| Hook Jobs (pre-delete, post-delete) | `cpu: 10m`, `memory: 32Mi` | `cpu: 100m`, `memory: 128Mi` |
| Injected `wait-for-balloons` init container | `cpu: 10m`, `memory: 32Mi` | `cpu: 100m`, `memory: 128Mi` |
| Topology probe (`probe.resources`) | `cpu: 10m`, `memory: 32Mi` | `cpu: 100m`, `memory: 128Mi` |

The probe's requests are not only a capacity statement: a namespace carrying a
`LimitRange` or a requests-enforcing `ResourceQuota` rejects a pod that declares
none, which would leave every node undiscovered.

These are deliberately small; raising them is a capacity decision for very large fleets,
not a hardening step.

> [!NOTE]
> The manager's limits are unrelated to the pinning **reserved pool**
> (`balloonsClusterConfig.balloon.reserved`). That is node capacity carved out for
> unpinned workloads, and inflating it costs the fleet real cores.

## Hardening options that are off by default

The last column is why the chart cannot ship the recommended value as the
default. Most rows are pure hardening; the `failurePolicy` and `guardActivePods`
rows trade availability for a stronger guarantee, so read
[Admission webhooks](#admission-webhooks) before taking them.

| Setting | Default | Recommended | Why it is off by default |
|---|---|---|---|
| `networkPolicy.metrics.from` | `[]` (any source) | a `namespaceSelector` for your Prometheus namespace | The monitoring namespace's name/labels are site-specific |
| `networkPolicy.webhook.from` | `[]` (any source) | `ipBlock`s for the addresses your API server dials from ([how to find them](#restricting-the-webhook-port)) | The API server is host-networked, so only an `ipBlock` can name it and no default is knowable; a wrong one stops admission |
| `podSecurityStandards.enabled` | `false` | label the namespace `restricted` yourself ([the hardened install](#start-here-the-hardened-install)) | Only renders when the chart creates the namespace, which Helm needs to exist before the install |
| `image.digest` | `""` | a digest, which pins the exact operator image and wins over `image.tag` (an empty `tag` follows the chart's `appVersion`, which a registry can still move) | A digest names one build, so it cannot be set ahead of the image a release publishes |
| `probe.image` | `""` (follows `image`) | leave empty so the probe tracks the operator image, digest included; a full reference only to name another mirror of that same image | The probe ships in the operator image, so there is nothing separate to pin |
| `probe.tolerations` | tolerate all taints | the narrowest **non-empty** set that still covers your pinning nodes (`[]` falls back to tolerate-all) | Topology must be discoverable on tainted nodes out of the box |
| `balloonPinningPolicy.guardActivePods` | unset (off) | `true` for policies whose pods matter | It can block a legitimate policy edit, so it is opt-in per policy |
| `webhook.mutatingWebhookConfiguration.failurePolicy` | `Ignore` | `Fail` **only** with a narrow `namespaceSelector` | Fail-open keeps an operator outage from becoming an admission outage |
| `metrics.enabled` | `true` | leave on and scope the network path; `false` only if you cannot | Turning it off loses the bypass alerting that makes fail-open visible |

Two gaps cannot be closed from values and are recorded here for transparency:

- **Webhook caller authentication.** `webhookServerOptions()` sets no `ClientCAs`
  and no `ClientAuth`, so `:9443` authenticates nobody: the caller is trusted
  because of where it connects from. Mitigate with `networkPolicy.webhook.from`,
  which is the whole control on that path. The reason a client certificate is not
  offered is in [Restricting the webhook port](#restricting-the-webhook-port).
- **Cluster-wide RBAC for namespace-local actions.** `pods/log: get` and the
  `configmaps` write verbs are cluster-wide (`pods: create/delete` already sits in
  a release-namespace Role). With your own RBAC (`rbac.create: false`) you can
  move `pods/log: get` into that Role. ConfigMap `create, update, delete` can move
  into Roles in `grafanaDashboard.namespace` **and** in every namespace the
  webhook targets, where the readiness CA copies live; add one for each new
  target namespace, or its init containers cannot verify the endpoint. Keep
  `configmaps: get, list, watch` cluster-wide.

## Verifying a release

Each release image carries a keyless Sigstore cosign signature, a signed SPDX SBOM
attestation and SLSA build provenance, all made from the public repository on a `v*`
tag. Verify by digest with cosign v3 or later and
[slsa-verifier](https://github.com/slsa-framework/slsa-verifier):

```bash
IMAGE=docker.io/intel/cpu-pinning-operator@sha256:<digest>
IDENTITY='^https://github\.com/intel/cpu-pinning-operator/\.github/workflows/release\.yml@refs/tags/v.+$'
ISSUER=https://token.actions.githubusercontent.com

cosign verify --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER" "$IMAGE"
cosign verify-attestation --type spdxjson \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER" "$IMAGE"
slsa-verifier verify-image "$IMAGE" \
  --source-uri github.com/intel/cpu-pinning-operator --source-tag v<version>
```

The issuer proves a GitHub Actions run signed the image; the identity proves that run
was this repository's release workflow on a release tag. Install with the digest you
verified (`--set image.digest=sha256:<digest>`).

## Verifying a deployment

```bash
NS=cpu-pinning-operator-system

# 1. Container hardening actually applied (expect true / [ALL] / RuntimeDefault).
kubectl -n $NS get deploy cpu-pinning-operator \
  -o jsonpath='{.spec.template.spec.containers[0].securityContext}{"\n"}'

# 2. The manager runs as its own ServiceAccount, not "default".
kubectl -n $NS get deploy cpu-pinning-operator \
  -o jsonpath='{.spec.template.spec.serviceAccountName}{"\n"}'

# 3. Metrics really are HTTPS and authenticated (expect --metrics-secure=true
#    and --metrics-require-auth=true), one flag per line.
kubectl -n $NS get deploy cpu-pinning-operator \
  -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}' | grep metrics

# 3b. An anonymous scrape is refused. The API server strips the caller's
#     credential before proxying, so this request reaches the endpoint with none:
#     expect 'Error from server (Unauthorized)', never Prometheus text.
POD=$(kubectl -n $NS get pod -l control-plane=controller-manager \
  -o jsonpath='{.items[0].metadata.name}')
kubectl get --raw "/api/v1/namespaces/$NS/pods/https:$POD:8080/proxy/metrics" | head -1

# 3c. Your scraper may read them (expect yes). Substitute its ServiceAccount.
kubectl auth can-i --as=system:serviceaccount:monitoring:prometheus get /metrics

# 4. The webhook CA was injected (expect a long base64 blob, not "Cg==").
kubectl get mutatingwebhookconfiguration cpu-pinning-operator-mutating-webhook \
  -o jsonpath='{.webhooks[0].clientConfig.caBundle}{"\n"}' | cut -c1-40

# 5. failurePolicy and scope are what you intended.
kubectl get mutatingwebhookconfiguration cpu-pinning-operator-mutating-webhook \
  -o jsonpath='{.webhooks[0].failurePolicy}{"  "}{.webhooks[0].namespaceSelector}{"\n"}'
kubectl get validatingwebhookconfiguration cpu-pinning-operator-validating-webhook \
  -o jsonpath='{.webhooks[0].failurePolicy}{"\n"}'      # expect Fail

# 6. The serving cert is Ready and covers both Services' SANs.
kubectl -n $NS get certificate cpu-pinning-operator-serving-cert
kubectl -n $NS get certificate cpu-pinning-operator-serving-cert -o jsonpath='{.spec.dnsNames}{"\n"}'

# 7. The operator holds no secrets grant, and tenants cannot edit pinning config.
kubectl auth can-i --as=system:serviceaccount:$NS:cpu-pinning-operator get secrets -A
kubectl auth can-i --as=system:serviceaccount:tenant-a:default \
  update balloonsclusterconfigs.cpu-pinning-operator.intel.com

# 8. The NetworkPolicy is present and selects the manager, and who may reach the
#    webhook port (an empty result means any pod in the cluster can).
kubectl -n $NS get networkpolicy cpu-pinning-operator -o jsonpath='{.spec.podSelector}{"\n"}'
kubectl -n $NS get networkpolicy cpu-pinning-operator \
  -o jsonpath='{range .spec.ingress[?(@.ports[0].port=="webhook-server")]}{.from}{"\n"}{end}'

# 9. No manager pod runs as root (expect "true" on every row). The bundled balloons
#    plugin, when installed here, is not covered and is left out.
kubectl -n $NS get pods -l app.kubernetes.io/name=cpu-pinning-operator \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[*].securityContext.runAsNonRoot}{"\n"}{end}'

# 10. Watch for fail-open bypasses (non-zero means pods were admitted unpinned).
#     Query cpu_pinning_webhook_bypasses_total in Prometheus, or watch the shipped
#     CPUPinningWebhookBypass alert. That counter only
#     sees bypasses the webhook itself handled, so for the webhook-unreachable
#     case list pods in your pinning namespaces that the webhook never stamped
#     (PINNED should read "cpu-pinning-operator" on every pod you expect pinned):
kubectl -n inference get pods -o custom-columns='POD:.metadata.name,PINNED:.metadata.annotations.cpu-pinning-operator\.intel\.com/pinned-by'
```

## Data handling

The operator is not designed to collect personal data, and it sends nothing outside the
cluster: no telemetry, phone-home or update check. It reads hardware-topology facts and
Kubernetes objects (pods, nodes, namespaces and its own CRDs), which hold whatever
names, labels, annotations and pod-spec values your users put there. It caches those
objects in memory and writes some of that metadata (pod and namespace names) into its
logs, Events, metrics labels and status. From a policy's `intent` it parses only an
annotation, the container args and the literal `value` of one env var; `valueFrom` is
never resolved.

| Category | Handled? |
|----------|----------|
| Personal data / PII | Not collected on purpose; any that users put in object names, labels, annotations or pod specs passes through the caches, logs and Events above |
| Workload or application Secrets | **No**, no Secret is read |
| The operator's own webhook TLS Secret | Yes, mounted read-only into the manager |
| Workload payloads (model weights, prompts, inference traffic, app data) | **No** |
| Hardware topology | Yes, from world-readable sysfs/procfs |
| Kubernetes objects and their metadata | Yes, standard reconciliation, in memory and in the outputs above |

It stores data only as Kubernetes objects, each living as long as what it describes:
per-node `BalloonsPolicy` CRs and node labels (until the node leaves the pinning
pool), the balloon annotation, init container and node affinity on matched pods
(the pod's lifetime), readiness CA ConfigMaps in targeted namespaces, the optional
Grafana dashboard ConfigMaps, Events (standard TTL) and the leader-election Lease.
Its caches and the webhook's reservation ledger are in memory only and rebuilt from
cluster state on restart. API calls use the cluster's TLS connection to the API
server; the webhook and the replicas' reserve port (mTLS) are always TLS, and metrics
and the readiness endpoint are TLS by default (`metrics.secure`, `balloonsReady.secure`).
The health probes on 8081 and cluster DNS are plaintext.

## See also

- [threat-model.md](threat-model.md) — assets, trust boundaries, STRIDE threats.
- [admission-policies.md](admission-policies.md) — Kyverno/Gatekeeper examples
  restricting who may edit the pinning CRDs.
- [configuration.md](configuration.md) — every CRD knob, including
  `failurePolicy`, `guardActivePods` and `capacityCheck`.
- [Installing on OpenShift / OKD](../deploy/cpu-pinning-operator/README.md#installing-on-openshift--okd)
  — the SCC overlay.
- [observability.md](observability.md) — metrics reference and the transport
  security details behind the scrape.
- [chart README](../deploy/cpu-pinning-operator/README.md) — the main values; `values.yaml` documents every key.
- [SECURITY.md](../SECURITY.md) — how to report a vulnerability privately.
