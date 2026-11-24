<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->


# End-to-end testing

`test/e2e/` runs against a real NRI-enabled cluster with the operator already
installed. It cannot run under envtest or kind: pinning needs a live balloons
plugin.

The suite admits pods with a container named `vllm`, plus one `kserve-container`
case, in the namespace of the policy `E2E_POLICY_NAME` names. The chart has no default
target, so install the operator under test with both, or the install fails:

```bash
--set 'balloonPinningPolicy.match.byContainerName={vllm,kserve-container}' \
--set 'balloonPinningPolicy.namespaces={inference}'
```

Tests sit behind the `e2e` build tag, so `go test ./...` and `make test` never
pick them up. They use the ambient kubeconfig and skip when no cluster or no
provisioned node is available.

## Running

```bash
make test-e2e
# ALLURE_OUTPUT_PATH=$PWD go test -tags e2e ./test/e2e/... -count=1 -v -timeout 15m

make test-crd          # CRD-install + cert-manager ordering guards (envtest)
make test-e2e-guards   # the cluster-free e2e guards, as the pre-commit hook runs them
make test-e2e-sweep    # restore and delete what a crashed run left (see Cleanup)
```

A node with no Go toolchain and no checkout takes the suite as a packaged binary
(`go test -c -tags e2e ./test/e2e/ -o /tmp/e2e.test`, then scp). The two guards that
scan the package sources skip as `skipNoCapability` there — the binary does not carry
the `.go` files with it — so such a run can still exit 0. `make test-e2e-guards` and the pre-commit
hook enforce them from a checkout, where a SKIP fails the hook's `--- PASS:` grep.

`make test-e2e` writes Allure JSON to `./allure-results/` (gitignored). Render it
with the separate `allure` CLI:

```bash
make test-e2e-report    # writes allure-report/
allure open allure-report
```

> [!NOTE]
> A bare `go test -tags e2e ./test/e2e/...` writes results under the package
> directory. `make test-e2e` pins them at the repo root via `ALLURE_OUTPUT_PATH`.

A test that failed and then skipped is published **failed**, not skipped: `t.Errorf`
sets the failure flag without stopping the test, so a later `t.Skipf` leaves both
predicates true. Its message carries a `failed, then skipped:` prefix.

## Validation run

`make test-validation` runs every gated security e2e test against the
installed release. `hack/e2e-validation.sh` flips
`metrics.requireAuth` with `helm upgrade --reuse-values`, waits for the rollout, a Ready leader holding the Lease and each
node's BalloonsPolicy. Before the first upgrade it saves the release's user-supplied values
(`helm get values`) and on exit, failure included, puts exactly those back with `--reset-values -f`,
so a value that was unset is unset again. It mutates the installation: run it on a disposable cluster.

| Pass | `metrics.requireAuth` | Gates |
|---|---|---|
| `hardened` | `true` | `E2E_RUN_PINNING`, `E2E_MUTATING`, `E2E_RESTART`, `E2E_METRICS`, `E2E_DASHBOARDS`, `E2E_GUARD_ACTIVE_PODS`, `E2E_DRIFT_RESTART`, `E2E_FORGED_ADMISSION`, `E2E_RESERVE_AUTHORITY`, `E2E_CHAOS_STALE_NRT`, `E2E_WEBHOOK_CA`, `E2E_READINESS_RATE_LIMIT` |
| `scrape` | `false` (TLS kept), after every replica serves node series | same |
| `destructive` | `true` | `E2E_CHAOS_FAILURE_POLICY` and `E2E_READINESS_TLS`, `-run` those two tests |
| `netpol` | `true`, `networkPolicy.webhook.from` set | none, `-run TestWebhookPortRejectsNonAllowedPod` |

Every other pass runs with `webhook.from: []`. The `netpol` step sets it, then a server-side
dry-run of a pod the installed policy matches must come back `pinned-by`. If not, the script clears
`from` at once and fails the webhook NetworkPolicy test; otherwise it runs that test and clears `from`.
The `from` list is each control-plane node's InternalIP plus its
`projectcalico.org/IPv4IPIPTunnelAddr` and `IPv4VXLANTunnelAddr`, each `/32` (`/128` for IPv6). An empty list fails the webhook NetworkPolicy test.

`hardened` also sets `E2E_TLS_POLICY` and `E2E_POD_SECURITY`; `scrape` also sets `E2E_DATA_HANDLING`, whose metrics scan needs the open endpoint.

| Variable | Default | Effect |
|---|---|---|
| `E2E_RELEASE_NAME`, `E2E_OPERATOR_NAMESPACE` | `cpu-pinning-operator`, `cpu-pinning-operator-system` | Release upgraded and tested |
| `VALIDATION_CHART` | `deploy/cpu-pinning-operator` | Chart passed to `helm upgrade` |
| `E2E_HELM_BIN` | `helm` | Helm binary |
| `VALIDATION_TIMEOUT` | `85m` | `go test -timeout` of the full passes |
| `VALIDATION_SHORT_TIMEOUT` | `30m` | `go test -timeout` of the `-run` passes (`destructive`, `netpol`) |
| `E2E_WEBHOOK_FROM_CIDR` | derived | Comma-separated `netpol` ipBlocks instead of the derived list; a bare address gets `/32` |
| `E2E_PASS` | set per pass | Allure result is `<Test> [<pass>]` with a `pass` parameter, and FullName `…<Test>[<pass>]` so historyId differs per pass. Unset: unchanged |
| `DRY_RUN=1` | off | Print the helm, kubectl and go commands only |

Each pass writes `validation-results/<pass>/e2e.log` and `allure-results/` under
`ALLURE_OUTPUT_PATH`. The summary lists each security test's verdict per pass and names any that
ran in no pass. Those are reported, not failed. The target exits non-zero when any pass fails.

Worst case is about 5h15m: two full passes at 85m plus a 5m
sweep, two `-run` passes at 30m plus 5m, five helm upgrades at up to 13m each.

## Cleanup

Every object the suite creates carries `cpu-pinning-e2e/run=<runID>`, one random ID per
process. `e2eCreate` stamps it, and `TestEveryCreateIsLabelled` fails on any `Create(` that
bypasses it. The sweeper, `e2eSweep` in `test/e2e/sweep_test.go`, works in two steps.

| When | What it does |
|---|---|
| First `newClient` call, so a cluster-free run never sweeps | Delete labelled leftovers of any run; snapshot the shared objects to `E2E_SWEEP_SNAPSHOT` (default `$TMPDIR/cpu-pinning-e2e-snapshot.json`); leftovers it cannot delete, or a snapshot it cannot take or save, fail every cluster test |
| After `m.Run`, on SIGTERM/SIGINT, and 5m (at most a quarter of it) before `-timeout` | Restore the operator, delete every labelled object, namespaces last (waits up to 3m), then restore the rest of the snapshot; a failure fails the run. On a signal or the deadline, tests may still be running, so the on-disk snapshot is kept for `make test-e2e-sweep` |
| `make test-e2e-sweep` | The same from the on-disk snapshot, after a crash or a SIGKILL; it fails without a kubeconfig, and refuses a snapshot whose API server host and kube-system UID are not this cluster's. `hack/e2e-validation.sh` runs it after every pass, stopping when it fails, and from its EXIT trap, after the Helm values |

Snapshotted, spec only: every unlabelled BalloonPinningPolicy, the `cluster`
BalloonsClusterConfig, the balloons DaemonSet's pod-template affinity and nodeSelector,
each node's cordon and labels except the operator's `cpu-pinning-operator.intel.com/` ones,
the `mpod.kb.io` failurePolicy, the operator Deployment's replicas and the operator
NetworkPolicy's ingress. Those last three are restored first, in that order, since the
policy restores need the operator's webhooks up; a Deployment the helm-uninstall test
removed is left absent. The policy restores run after the labelled pods are gone, since a
guarded policy rejects a match change that drops a pod it holds.
Nothing else unlabelled is touched. Cleanup deletes go through `e2eDelete`, which retries
and then fails the test naming the object.

## Environment variables

Gates default off. A missing cluster or provisioned node is a skip, not a fail; missing
*configuration* is not — the suite provisions what it can, else fails naming the variable
to set.

Every skip goes through one of three helpers in `skip_test.go`, so why a test did not run
is recorded: `skipNoCapability` (the cluster cannot present it), `skipNoConfig` (it
could, but was not set up — red under `E2E_STRICT`), `skipNotSelected` (an opt-in gate is
off, or the install is in a mode this assertion does not apply to — never red). The run ends with the tally:
`e2e: 19 skip(s) — 12 no capability, 5 no configuration, 2 not selected`.

| Var | Default | Effect |
|---|---|---|
| `E2E_STRICT=1` | set by `make test-e2e` | A `skipNoConfig` skip fails instead. Gates and capability skips unaffected; `E2E_STRICT= make test-e2e` opts out |
| `E2E_NAMESPACE` | from an installed policy | Namespace for test pods; created if absent and deleted at the end if the suite created it. Must be webhook-intercepted |
| `E2E_OPERATOR_NAMESPACE` | `cpu-pinning-operator-system` | Operator Deployment, Lease, metrics Service |
| `E2E_OPERATOR_DEPLOYMENT` | `cpu-pinning-operator` | Manager Deployment name |
| `E2E_OPERATOR_CONFIGMAP` | `<deploy>-config` | Operator config ConfigMap |
| `E2E_POLICY_NAME` | `default` | `BalloonPinningPolicy` the mutating cases edit and the namespace comes from |
| `E2E_BALLOONS_NAMESPACE` | operator's `BALLOONS_NAMESPACE` | `BalloonsPolicy` CRs and plugin DaemonSet |
| `E2E_BALLOONS_DAEMONSET` | operator's `BALLOONS_DAEMONSET` | Plugin DaemonSet name |
| `E2E_DASHBOARD_NAMESPACE` | `cpu-pinning-operator-system` | Grafana dashboard ConfigMaps |
| `E2E_LEADER_LEASE` | `cpu-pinning-operator` | Leader-election Lease (`--leader-election-id`) |
| `E2E_METRICS_PORT` | `8080` | Metrics port |
| `E2E_METRICS_SCHEME` | `https` | `http` only when `metrics.secure=false` |
| `E2E_MANAGER_SELECTOR` | (chart default) | Label selector for the manager pod |
| `E2E_RUN_PINNING=1` | off | Live NRI pin + no-collision |
| `E2E_SATURATE=1` | off | With pinning: oversubscribe the node |
| `E2E_DASHBOARDS=1` | off | Per-node dashboard ConfigMaps |
| `E2E_RESTART=1` | off | Delete the operator pod and assert recovery |
| `E2E_METRICS=1` | off | Scrape leader `/metrics` for `cpu_pinning_*` series. The scrape goes through the apiserver pod proxy, which cannot present a credential (the apiserver deletes `Authorization` once it has authenticated the caller), so on a default install the endpoint answers 401: the positive series assertions skip as `skipNotSelected` (a deployment mode the run did not select, so they stay yellow under `E2E_STRICT` — see `skipNoMetrics`) and `TestMetricsRejectsUnauthenticatedScrape` runs. The same applies to the metric-delta assertions of suites that do not need the endpoint for their primary assertions (capacity-check, container-selection): each lives in a `metric delta` subtest, so it skips on its own while the admission assertions around it still run. Install with `--set metrics.requireAuth=false` (TLS is kept) to swap which half runs |
| `E2E_MUTATING=1` | off | Shared-CR mutating suite |
| `E2E_LABEL_NAMESPACE` | unset | Existing label-targeted namespace for `TestWebhookTargetsNamespaceByLabel`, instead of its own |
| `E2E_DEVICE_PINNING=1` | off | Stamp `balloon-device` for an extended resource |
| `E2E_DEVICE_RESOURCE` | `nvidia.com/gpu` | Resource the device test requests |
| `E2E_DRIFT_RESTART=1` | off | `driftPolicy: restart` eviction + PDB |
| `E2E_GUARD_ACTIVE_PODS=1` | off | Validating webhook blocks delete/narrow |
| `E2E_CHAOS_CORDON=1` | off | Cordon one node; affinity must miss it (≥2 nodes) |
| `E2E_CHAOS_FAILURE_POLICY=1` | off | Scale operator to 0, `failurePolicy=Fail` |
| `E2E_WEBHOOK_CONFIG_NAME` | `<deploy>-mutating-webhook` | Mutating webhook object |
| `E2E_CHAOS_PDB_STUCK=1` | off | Fleet `cpuAllocation` flip behind a blocking PDB |
| `E2E_CHAOS_STALE_NRT=1` | off | Freeze plugin; over-admitted pods stay in Init |
| `E2E_CHAOS_PLUGIN_UNINSTALL=1` | off | Suspend plugin + SIGKILL; pin or `Drifted` |
| `E2E_CHAOS_IMAGE` | `registry.k8s.io/pause:3.9` | Image whose PID 1 can be killed |
| `E2E_HELM_UNINSTALL_CLEANUP=1` | off | **Destructive** `helm uninstall` + GC |
| `E2E_RELEASE_NAME` | `cpu-pinning-operator` | Helm release to uninstall |
| `E2E_HELM_BIN` | `helm` | Helm binary |
| `E2E_MEASURED_FILLER_CORES` | (computed) | Filler size for measured reserved sizing |
| `E2E_RESERVE_AUTHORITY=1` | off | Leader-authoritative reserve tests: the cross-replica ledger proof, the lease handover and the unreachable authority. Needs the default `webhook.reserveAuthority.enabled` and ≥2 Ready replicas, else they skip |
| `E2E_RESERVE_AUTHORITY_NEGATIVE=1` | off | With the above: negative control against `reserveAuthority.enabled=false`. `TestReserveAuthorityCrossReplicaLedger` then asserts over-admission **did** happen, so both halves of the proof are green runs. It *inverts* the precondition rather than relaxing it: set against an install that still carries `--reserve-authority`, every reserve-authority test skips (`skipNoConfig`) before creating a pod, instead of failing on over-admission ~100 s in |
| `E2E_TLS_POLICY=1` | off | TLS policy handshakes. Dials pod IPs directly when routable, else from a curl pod (`E2E_WEBHOOK_PROBE_IMAGE`) in `cpu-pinning-e2e-tls-probe`; for `:9444` from a manager-labelled, never-Ready pod in the operator namespace. Suites curl cannot offer go over `kubectl port-forward` |
| `E2E_KUBECTL_BIN` | `kubectl` | kubectl for the TLS policy port-forward |
| `E2E_POD_SECURITY=1` | off | Restricted-PSA dry-run in namespace `e2e-pod-security` |
| `E2E_DATA_HANDLING=1` | off | Planted-marker data-handling scan in namespace `cpu-pinning-e2e-data-handling` |
| `E2E_WEBHOOK_POD_PORT` | `9443` | Manager admission port inside the pod, for the direct per-replica admissions |
| `E2E_RESERVE_PORT` | `9444` | `webhook.reserveAuthority.port`, matched against the NetworkPolicy rule that opens it |
| `E2E_NETWORK_POLICY` | `cpu-pinning-operator` | NetworkPolicy whose `reserve` ingress rule is removed to sever the authority |
| `E2E_WEBHOOK_PROBE_IMAGE` | `curlimages/curl:8.11.1` | Image the webhook-ingress probe pod runs; needs curl and a shell, and must tolerate uid 100 — the probe pins `RunAsUser`, because the default image's `USER` is a name (`curl_user`) and the kubelet refuses `RunAsNonRoot` it cannot verify numerically. A cluster that allocates uids per namespace (OpenShift's `restricted-v2` SCC) refuses the pin; the probe then retries without it and runs as the uid that cluster assigns |
| `E2E_WEBHOOK_CA=1` | off | Verify the webhook's serving cert against both caBundles |
| `E2E_READINESS_TLS=1` | off | Corrupt the target namespace's readiness CA copy, restored after |
| `E2E_READINESS_RATE_LIMIT=1` | off | Burst one manager replica's balloons-ready port from a probe pod |
| `E2E_FORGED_ADMISSION=1` | off | Forged-AdmissionReview burst against the reservation ledger. Waits out **two** full `webhook.reservationTTL` periods (read from the manager's args), so it is off by default |

## Test cases

Pinning is proven by the NRT container-zone cpuset, not by the annotation.
Each created object is deleted in `t.Cleanup`. Tests read node labels instead
of hardcoding a topology.

The fleet can be mixed, so a test that reasons about one node must make sure
its pods are on that node:

| What the test asserts | Reads | Why |
|---|---|---|
| The span an **unbound** pod is stamped with | `clusterMaxSpan`: the largest `max-span` among balloons-enabled nodes, a missing or invalid label counting as 1 | The webhook caps an unbound pod by the widest node, not the first one listed |
| A pod's NRT cpuset, or a no-collision check between pods | The pods are bound by `spec.nodeName` (`widestSpanNode`, so the bound cap equals the unbound one), or the test reads the node from the scheduled pod's `spec.nodeName` | An unbound pod can land on any node, and a collision only means something within one node |
| Admission of an over-capacity unbound pod | `ccOverCapacityCores`: one more than the largest per-NUMA capacity in the fleet | The capacity gate passes an unbound pod if *any* node fits it |
| Admission on one specific node (density mode) | The pod is bound by `spec.nodeName` | Same reason: another node's free room would mask this node's verdict |

`TestPodNRTReadsBindThePod` enforces the second row from source.

Per-node `cpu_pinning_*` gauges are scraped through `scrapeOperatorMetricsForNode`, which
re-scrapes for up to 90s while a named series has no sample for the node: those gauges
are Set inside a reconcile pass that first resolves the node's topology, so a manager
that has just started (rollout, reinstall, lease handover) serves none of them while
the node render has long quiesced — `waitForNodeSettled` watches the render, not the
serving replica. It waits on *absence* only, so a series present with a wrong value
fails at once, and a genuine absence still fails (or skips) after the wait.

| Test | Gate | Asserts |
|---|---|---|
| `TestNoBareSkips` | — | every `Skip` routes through a classification helper, or ends the line with `// bare skip: intentional`; names file:line, and fails if it scanned files but not `skip_test.go`. No cluster |
| `TestMetricAssertionsAreNotSilentlyDropped` | — | no site outside `helpers_test.go` takes the second value off a `scrapeMetrics*` helper (the shape `after, ok := …; if ok { t.Errorf(…) }`, which reports PASS having asserted nothing); a metric assertion that needs the endpoint belongs in a subtest scraping via a `…OrSkip` form. Carries a positive control over the regressed shapes; one marker comment exempts a deliberate discard. No cluster |
| `TestSourceScanSkipRoutingNeedsSources` | — | the two source-scanning guards above skip as `skipNoCapability` when the run has no package sources to scan — a binary from `go test -c` shipped to a node with no checkout — and fail when sources are there but the file they key on is not (a rename disarming the guard). Run with `E2E_STRICT` set, which changes neither. No cluster |
| `TestMetricsSkipRoutingIsAuthAware` | — | an empty scrape reaches `skipNotSelected` only when *every* attempted replica answered 401 (`allScrapesRefused`); a mixed outcome — one refusal, one broken replica — and every other empty scrape reach `skipNoConfig`; run with `E2E_STRICT` set, where only the second is a failure. No cluster |
| `TestReserveAuthorityModeSkipIsSymmetric` | — | `raAuthorityModeSkip` over all four combinations of `--reserve-authority` present/absent × `E2E_RESERVE_AUTHORITY_NEGATIVE` set/unset: each mode runs only on the install it describes, the two mismatches route to `skipNoConfig` and name the Helm value to change. No cluster |
| `TestProbeStallNamesTheKubeletsReason` | — | the skip a stalled webhook-ingress probe pod carries names the waiting container's reason **and** the kubelet's message (`CreateContainerConfigError` + the non-numeric-user refusal), falling back to the phase when nothing is waiting. No cluster |
| `TestNodeMetricsMissingTracksThePublishWindow` | — | `nodeMetricsMissing` over the expositions a rollout serves: no `cpu_pinning_` node series at all, the NRT-driven gauge up while the node-reconcile ones are not (different controllers Set them, so they do not recover in step), both up, and another node's series only. No cluster |
| `TestPodNRTReadsBindThePod` | — | no function both builds a pod (`vllmPod`, `ccPod`, `chaosDeployment`) and reads a node's NRT for it (`containerCPUSet`, `cpuToCore`) without touching `NodeName` or `NodeSelector`; `…CatchesTheShape` proves it reports the unbound shape and passes the bound one. No cluster |
| `TestReservedFootprintModelsTheSpread` | — | the footprint subtest's expectation over 1, 2 and 4 NUMA nodes and bare counts: it accepts the spread physical-cores count (8 for hyperthreads 12 on 4 NUMA nodes) and never the hyperthreads count a dropped `÷threadsPerCore` conversion would render; the verdict skips as inconclusive only when the observed count is itself that ambiguous count, and fails any count outside the candidates. No cluster |
| `TestProbeWatchJudgesOnlyTheProbesContent` | — | the reserved/exclude probe's verdict tracking over a restore race: an operator restore triggers one re-apply (not a second on a stale event), a verdict on the restore is refused, and a late verdict on an earlier probe generation is accepted. No cluster |
| `TestSaturationCountsOnlyCapacityDenials` | — | the saturation test counts only the operator's capacity denial as overflow, never a quota rejection. No cluster |
| `TestWatchRevisionsKeepsItsCursor` | — | `watchRevisions` against a scripted apiserver: it opens at the caller's write, resumes a closed stream from the delivered revision even after the callback re-versioned its copy, and re-reads only on a 410, which it reports as a gap. `…ReturnsStreamedErrors`: any other streamed error (a 500) is returned, not re-read. No cluster |
| `TestServingChainVerdict`, `TestRateLimitVerdict`, `TestReadinessInitLogVerdict` | — | the verdicts of the three tests below over built certificates, 429 sequences and init logs. No cluster |
| `TestTLSProbeJudgesListenerPolicy` | — | the TLS policy probe and judge against local listeners: hardened and 1.3-only configs pass; TLS 1.0/1.1, CBC, ChaCha20 or 1.2-on-a-1.3-port fail. No cluster |
| `TestCurlProbeFeedsTheSameJudge` | — | every planned TLS 1.2 suite has a curl (OpenSSL) name, and curl exit codes parse into the same judge: a weak version or suite fails, the 1.3 client-cert refusal passes, an unreachable or truncated log is not a refusal. A suite curl cannot offer is re-tested through crypto/tls: accepted RC4 fails, refused passes, an unreachable fallback leaves a gap. `TestTLSProbeReadsPortForwardPort` parses `kubectl port-forward` output. No cluster |
| `TestRestrictedGapsJudgesShapes` | — | the real probe-pod and `wait-for-balloons` builders have every restricted-PSA hardening field; a stripped container shows all seven gaps. No cluster |
| `TestPIIMatcherFlagsPlantedAndGeneric` | — | the data-handling matcher flags each planted value, an email, `Bearer`, a JWT and a private-key header, and passes ordinary log and metric lines. No cluster |
| `TestLeaderPodOnceRejectsStaleLease` | — | a Lease naming a gone pod is no leader; `leaderPodName` retries until its ctx ends (2 minutes for callers riding out a handover), and a cancelled ctx ends it at once. No cluster |
| `TestRaxLedgerHeldNeedsTheAuthority` | — | the reservation drain skips its TTL wait only when the lease holder was scraped and holds nothing for the node; followers' zeros alone wait. No cluster |
| `TestStaleNRTVerdict` | — | the stale-NRT batch verdict: admitted past the frozen room is over-admission, exactly at it with only capacity denials holds the line, anything else is inconclusive. No cluster |
| `TestStaleNRTFrozenCapacity` | — | the stale-NRT room comes from the frozen NRT's occupancy (one NUMA node held leaves room for one pod, not two), and an unreadable topology annotation falls back to the labels. No cluster |
| `TestAllureStatus` | — | status mapping truth table; failed-and-skipped is `failed`. No cluster |
| `TestAllurePassName` | — | `E2E_PASS` puts the pass in the name, a parameter and the historyId; unset leaves the result unchanged. No cluster |
| `TestAllureFailedThenSkippedIsFailed` | — | re-execs the binary to run `TestAllureAdapterChildFixture` (skips otherwise); the emitted result JSON for a failed-then-skipped test says `failed`. No cluster |
| `TestNodeProvisioning` | — | topology labels; `BalloonsPolicy/node.<name>` has reserved + span1 + device |
| `TestPhysicalCoresExcludeSiblings` | — | `exclude-cpuset` present; leaves have no `hideHyperthreads` |
| `TestPCTPriorityPolicyAcceptedByPlugin` | — | `cpuClasses` = gpu-hp/high + cpu-lp/low; device → gpu-hp, leaves → cpu-lp, composites/reserved unset; plugin status `Success` at the newest generation, watched; a missing verdict names the plugin pod's state |
| `TestWebhookStampsWholeCoreVLLM` | — | `balloon-span<snap(TP)∩max-span>` + pinned labels; init if ready-endpoint set, mounting the namespace's CA ConfigMap when it is https |
| `TestWebhookStampsKServePredictor` | — | `kserve-container` pod with `--tensor-parallel-size=2` gets `balloon-span<snap(2)∩max-span>` from the installed default policy. Skips as `skipNoConfig` when that policy does not list `kserve-container` |
| `TestWebhookSpanSnapsUpCappedByMaxSpan` | — | TP=3 → snap 4, capped by the cluster-wide `max-span` |
| `TestWebhookSkipsFractionalCPU` | — | `500m` request is not pinned |
| `TestWebhookIgnoresUnmatchedNamespace` | — | pod in `default` is untouched. Skips as inconclusive unless `default` is provably unmatched — `policyTargetsNamespace` resolves exact names, globs **and** `spec.namespaceSelector` against the namespace's labels, and answers "targeted" whenever that cannot be read, so a policy doing its job is never reported as a scoping regression |
| `TestWebhookTargetsNamespaceByLabel` | `E2E_MUTATING` | creates a uniquely labelled namespace and a policy selecting the label; once the webhook selector includes it, a `vllm` pod there is stamped. Deletes both |
| `TestTwoPoliciesPinTheirOwnWorkloads` | `E2E_MUTATING` | two policies in two new namespaces (`vllm` with `--tensor-parallel-size`, `sglang` with `--tp-size`): each pod gets its own policy's span, both namespaces are in the webhook selector, each `status.boundPods` lists only its own pod. Deleting one leaves the other's pod and status unchanged and stops stamping in its namespace |
| `TestOverlappingPoliciesPickOneDeterministically` | `E2E_MUTATING` | two policies with different span intents match the same `vllm` pod: each pod is stamped by exactly one, and the same one for every pod and after recreating the policies in reverse order. Fails while `Index.Match` order follows the unordered informer List |
| `TestNodeResourceTopologyPublished` | — | NRT CR has zones |
| `TestReservedPoolMetricsExported` | `E2E_METRICS` | `node_reserved_cpus` + utilization |
| `TestHTSiblingPolicyMetricExported` | `E2E_METRICS` | `cpu_pinning_ht_sibling_policy` indicators |
| `TestNodeProvisioningMetricExported` | `E2E_METRICS` | `cpu_pinning_node_provisioning_total` |
| `TestCPUFragmentationMetricExported` | `E2E_METRICS` | `cpu_pinning_cpu_fragmentation{node}` |
| `TestCPUManagerConflictMetricExported` | `E2E_METRICS` | `cpu_pinning_cpu_manager_conflict{node}` is exported and agrees with the node's three-state conflict label — 0 with `=false`, 1 with `=true` (absent = no reading at all: no `kube-system/kubelet-config`, no `kubelet` key, an unparseable document or an unrecognised policy, so it skips as a missing capability; a document that omits `cpuManagerPolicy` reads as the default `none` and exports 0) |
| `TestCPUManagerConditionMatchesNodeLabels` | — | `CPUManagerCompatible` on the singleton follows the three-state `cpu-manager-conflict` label on the balloons-enabled nodes: False when one carries `=true`, Unknown when one carries no label (its kubelet configuration was never read), True only when all carry `=false`; the whole pool shares one verdict (the reading is cluster-wide); a flagged node stays in `status.perNode` |
| `TestMetricsRejectsUnauthenticatedScrape` | `E2E_METRICS` | a credential-less scrape of the leader's `/metrics` is refused (401), not served; skips when the manager runs `--metrics-require-auth=false` or `--metrics-secure=false` |
| `TestTLSListenersNegotiateApprovedProtocols` | `E2E_TLS_POLICY` | each TLS version and each TLS 1.2 suite offered alone. `:8082` and the leader's `:9444` accept only TLS 1.3; `:9443` and `:8080` accept TLS 1.2+ with ECDHE AES-GCM suites only. Ports come from the manager's pod template. A port the NetworkPolicy opens only to named peers and the probe cannot reach skips as not selected. A suite curl cannot offer (OpenSSL 3: RC4, 3DES) is re-tested with crypto/tls over `kubectl port-forward` (`E2E_KUBECTL_BIN`); without one it is a coverage gap |
| `TestOperatorPodsAdmittedUnderRestrictedPSA` | `E2E_POD_SECURITY` | after a privileged-pod control is refused, the manager, topology-probe and `wait-for-balloons` shapes are admitted (server dry-run) under `enforce=restricted` and carry the hardening fields. The NRI plugin DaemonSet is excluded |
| `TestNoPersonalDataInOperatorOutputs` | `E2E_DATA_HANDLING` | a pod plants a fake email, token, arg and annotation in fields the policy's intents parse. None of them, and no generic PII shape, appears in manager or plugin logs, events, metrics, BalloonsPolicies, the operator's node keys or ConfigMaps |
| `TestPerNodeDashboardConfigMap` | `E2E_DASHBOARDS` | per-node canvas ConfigMap |
| `TestActualPinningNoCollision` | `E2E_RUN_PINNING` | Both pods bound to `widestSpanNode`; NRT cpuset size; the two share no physical core |
| `TestClusterSaturationNoCollision` | `E2E_RUN_PINNING` + `E2E_SATURATE` | Pods bound to `widestSpanNode`; overflow is denied by the capacity check (any other Create error fails) or stays unpinned; no double-book; at least one pod pinned |
| `TestOperatorPodDeletionRecovers` | `E2E_RESTART` | reconcile + admission recover after pod delete |
| `TestDevicePodDeviceLocality` | `E2E_DEVICE_PINNING` | device pod stamped `balloon-device` (annotation only) |
| `TestGuardActivePodsBlocksDeleteAndNarrowing` | `E2E_GUARD_ACTIVE_PODS` | guarded policy rejects Delete and narrowing Update |
| `TestDriftRestartEvictsAndMeters` | `E2E_DRIFT_RESTART` | intent change evicts; PDB blocks |
| `TestChaosCordonNotSteered` | `E2E_CHAOS_CORDON` | hostname affinity misses the cordoned node |
| `TestChaosWebhookFailurePolicyBlastRadius` | `E2E_CHAOS_FAILURE_POLICY` | `Fail` blocks only in-selector namespaces |
| `TestChaosFleetFlipPDBStuckAlerts` | `E2E_CHAOS_PDB_STUCK` | fleet flip + PDB: drifted, not evicted |
| `TestChaosPluginUninstallRestartRepinnedOrDetected` | `E2E_CHAOS_PLUGIN_UNINSTALL` | restart is pinned **or** `Drifted` |
| `TestStaleNRTInitGateHoldsOverAdmitted` | `E2E_CHAOS_STALE_NRT` | frozen plugin: over-admitted pods stay Init. Waits for the node's pinned and terminating pods to leave, then out its live reservations; judges admissions against the frozen NRT's free room |
| `TestHelmUninstallCleansUpResources` | `E2E_HELM_UNINSTALL_CLEANUP` | GC drops CR; hook strips labels |
| `TestGatedMutatingSuite` | `E2E_MUTATING` | sequential shared-CR scenarios below |
| `TestReserveAuthorityBurstSingleLedger` | `E2E_RESERVE_AUTHORITY` | concurrent burst of `2*numa+2` pods through the apiserver: admitted CPU ≤ pinnable cores, and `cpu_pinning_webhook_reservations_live{node}` rising on **exactly** the lease holder. Skips as inconclusive unless the burst denied and ≥2 replicas served it — which a single-apiserver control plane cannot produce |
| `TestReserveAuthorityCrossReplicaLedger` | `E2E_RESERVE_AUTHORITY` | two waves of AdmissionReviews posted straight at a named replica through the apiserver pod proxy: wave A saturates the lease holder's ledger, wave B must be refused whole by a follower. With `E2E_RESERVE_AUTHORITY_NEGATIVE` the same waves must over-admit |
| `TestReserveAuthorityFailoverAcrossBurst` | `E2E_RESERVE_AUTHORITY` | real pods either side of a forced lease handover: admitted CPU ≤ pinnable cores, every admitted pod pinned to exactly its requested width in NRT, zero `CreateContainerError` |
| `TestReserveAuthorityUnreachableDenies` | `E2E_RESERVE_AUTHORITY` | removes the `reserve` ingress rule from the operator's NetworkPolicy: a follower must answer the unaccountable denial under `enforce`, and admit the same pod again once the rule is back |
| `TestWebhookPortRejectsNonAllowedPod` | the installed policy | a pod in its own namespace POSTs a forged AdmissionReview at the webhook Service, bypassing the apiserver, and must be refused. The same pod first requests the open balloons-ready port as a control, so a broken probe cannot read as a block. Skips unless `networkPolicy.webhook.from` is actually set on the install, since the default leaves the port open |
| `TestWebhookServingCertChainsToInjectedCABundle` | `E2E_WEBHOOK_CA` | the `tls.crt` of the Secret the live Deployment mounts at `--cert-dir` verifies for `<svc>.<ns>.svc` against every mutating and validating `caBundle` calling that Service. Needs `get` on that Secret. A subtest has a probe pod check the Service presents the same leaf (SHA-256); only that subtest skips when `networkPolicy.webhook.from` closes the port to pods |
| `TestReadinessInitRefusesUnverifiedServer` | `E2E_READINESS_TLS` | with a foreign CA in the target namespace's copy (re-written over the operator's resync), a matched pod stays in Init 30s past its first `x509:` failure, and its init log has no `http://` line. Starts once the copy is restored |
| `TestReadinessEndpointRateLimitsPerSource` | `E2E_READINESS_RATE_LIMIT` | 30 requests from one probe pod at one replica's pod IP: the first 5 are served, no more than 5 + 2/s pass, the rest get 429 with `Retry-After: 1`, and a request after 4s is served |
| `TestForgedAdmissionBurstContainedByReservationTTL` | `E2E_FORGED_ADMISSION` | negative test: a burst of forged AdmissionReviews fills a node's reservation ledger, then the containment is asserted — `cpu_pinning_webhook_reservations_live{node}` back at its baseline one TTL later with a real pod admitted and pinned, no pod created for the reviews (an Event per verdict is expected and asserted, in the forged pod's namespace only), a review for an untargeted namespace allowed with no patch, and every answer carrying only the submitted pod's own mutations. Proves **ledger containment only**: the burst arrives over the privileged `pods/proxy` path, so it says nothing about port reachability |

`TestGatedMutatingSuite` subtests, in order:
`ContainerSelectionGlobSingleHit`, `ContainerSelectionAmbiguousWholePod`,
`ContainerSelectionLargestContainer`, `CapacityCheckEnforceDeniesOverCapacityPod`,
`CapacityCheckWarnAdmitsOverCapacityPod`, `CapacityCheckOffSkipsCheck`,
`DensityModeAbsorbsSiblingsIntoReserved`,
`ReservedFootprintPhysicalCoresDividesByThreads`,
`NodeConfigOverrideAppliesPerNode`, `MeasuredReservedSizing`,
`ReservedCpusetCannotOverrideExcludeCpuset`.

The reserved-pool subtests compare **rendered** counts. A homogeneous node's
policy names a reserved cpuset spread evenly over its NUMA nodes, which rounds
the resolved size up to a multiple of the NUMA count (`balloons.ReservedPerNUMA`):
6 cores on 4 NUMA nodes render as 8. The expected value is spread the same way
before it is compared.

Tests that wait on the plugin's verdict (`status.nodes.<node>`) read it through
`watchBalloonsPolicy`, a watch rather than a poll. That field holds only the
plugin's latest verdict, and the operator re-renders the policy on any Node
event, so a poll can miss a verdict that the next one overwrites.
The probe opens its watch at its own patch's resourceVersion, and a closed
watch resumes from the last revision delivered. Only an expired history forces
a re-read, and the skip message reports that gap.
`ReservedCpusetCannotOverrideExcludeCpuset` counts a verdict on any generation
that carried its cpuset, not only the last one it wrote. Its cleanup waits up to
3m for the plugin pod to be Ready with a verdict on the restored spec, and fails
the probe if it is not, so a plugin crash-looping during the probe is reported
there rather than in the next verdict-reading test (`TestPCTPriorityPolicyAcceptedByPlugin`). When no verdict arrives, the skip
names the plugin pod's readiness, restarts and last exit. A plugin that is down
writes no status at all.

## CRD-install regression guard

`test/crdinstall/` installs the generated CRDs into a real envtest apiserver —
the only place the server's CEL rule-cost estimator runs. It runs under
`go test ./...` when envtest binaries are present (`KUBEBUILDER_ASSETS` or the
`setup-envtest` store) and skips when they are not, except with `CI` set, where
it fails. `make test-crd` fetches the binaries.

`ENVTEST_K8S_VERSION` pins the apiserver to the generation the `k8s.io/*`
modules target.

| Test | Asserts |
|---|---|
| `TestGeneratedCRDsInstall` | both CRDs install without blowing the CEL budget |
| `TestReservedMutualExclusion` | at most one of `reserved.cpus`/`percent`/`measured` |
| `TestNodeConfigOverridesMaxItems` | 33 overrides rejected (`maxItems=32`) |
| `TestCertManagerPrerequisiteResolvesOperatorCertResources` | Issuer + Certificate apply after cert-manager CRDs |
| `TestWithoutCertManagerPrerequisiteOperatorCertFailsNoMatch` | Certificate apply fails with `NoKindMatchError` |
