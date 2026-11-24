<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Capacity-check internals: live occupancy, per-NUMA share, reservation race

This document covers the internal mechanics of the webhook's capacity check —
how live occupancy is reconstructed, the per-NUMA share model, the live/static
split, and the per-replica reservation race. For user-facing configuration of
`capacityCheck.mode`, see
[configuration.md](../configuration.md#balloonpinningpolicy-fields); for the
symptom/fix flow, see
[troubleshooting.md](../troubleshooting.md#the-capacity-check-passed-but-the-pod-still-wont-pin).

## Reserved pool vs. reservation store

The webhook's **reservation store** (`internal/webhook/reservation.go`, described
throughout this doc) is unrelated to the **reserved CPU pool**
([reserved-pool-internals.md](reserved-pool-internals.md)) despite the shared
"reserved" name root. Do not conflate them — they share no code, no lifetime, and
no purpose:

- The **reservation store** is an in-memory, per-webhook-process guard against
  burst over-admission. Each entry holds a recently-admitted pod's per-NUMA CPU
  and memory share and expires purely by a ~90 s TTL timer
  (`reservationTTL`) — the store never observes the `NodeResourceTopology` CR, so
  an entry lives out its full TTL even once the pin has landed in the CR sooner.
  Losing it (restart, TTL) is self-healing and never a correctness gate.
- The **reserved pool** is static per-node config: a CPU cpuset sized for system /
  non-model components (everything routed by `reservedPoolNamespaces`), rendered
  into the `BalloonsPolicy` CR (`reservedResources.cpu`) from `spec.balloon.reserved`
  and recomputed only at node reconcile.

Reserved-pool sizing does not consult the reservation store, and the capacity
check reads only the reserved pool's *size* — from the
`…/reserved-cpus` node label, to subtract it from static per-NUMA capacity (see
below); it never reads the pool's membership or its cpuset. A question like "can I
query the reservation store to see what's in the reserved pool?" always answers
*no* — they are separate subsystems.

## Per-NUMA CPU capacity model

For a pod that clears the topology gate, the webhook checks the CPU request fits
the per-NUMA room for the span, mode-aware:

- `physical-cores`: `request ≤ (physical_cores_per_numa − reserved_per_numa) × span`
- `hyperthreads`: `request ≤ (physical_cores_per_numa × threads_per_core − reserved_per_numa) × span`

The mode is the one resolved for the machine being decided about, read from its
`…/cpu-allocation` label — the same label the fragmentation gauge and drift detection
read. A pod already bound to a node is judged in that node's mode; an unbound pod's
candidates are each judged in their own, because there is no node to read for a pod the
scheduler has not placed. The cluster default stands in only for a node that has
published no mode, so an unlabelled node is judged rather than blocked.

`reserved_per_numa` is `ceil(reserved-cpus / numa-nodes)`: the reserved pool is
walled off from exclusive balloons, and `reservedCPUSet` spreads it evenly over the
NUMA nodes, taking the same number of CPUs from each. Without that subtraction the
static check believes every physical CPU is available to exclusive balloons and
admits pods the kubelet then cannot pin (`CreateContainerError`). It is subtracted
after the `threads_per_core` scaling and never multiplied by it, because
`reservedCPUSet` names one representative thread per physical core — the pool's
per-NUMA footprint is the same number of CPUs as it is of cores.

Three things it does not know:

- **No `…/reserved-cpus` label** (or an unparseable one) subtracts nothing, as with
  the other topology labels. Fails open.
- **Density mode** (`htSiblingPolicy: reserved`) stamps the HT-sibling count in that
  label instead of a pool size. Those siblings are not part of the physical-core pool
  counted here, so hardware capacity stands and the formula above does not apply: a
  share that is not credible as a resolved pool is discarded, meaning one above
  `balloons.MaxCredibleReservedPerNUMA` (the 25% clamp `ResolveReservedCPUs` puts on
  every pool it resolves) or one that would claim a whole NUMA node's cores. Both tests
  are in **cores** in both allocation modes — judged against the mode's scaled capacity,
  the same label would be credible in `hyperthreads` and not in `physical-cores`, and one
  node's `hyperthreads` capacity could come out below its `physical-cores` capacity.
  Fails open. The **live** path agrees: on a density node a core is free when its
  **representative** thread is free (`topology.FreeCoresPerNUMADensity`), since the
  siblings are the reserved pool. The ordinary rule would read 0 free per NUMA there, so
  `enforce` would deny every pinned pod. Density is `PodMutator.densityNode`: the
  cluster-global `htSiblingPolicy` plus the node's `…/cpu-allocation` label; a node
  without that label reads as non-density. That intent is then confirmed against the CR
  the occupancy comes from (`densityReservedIsSiblings`): a node whose reserved balloon
  is not exactly the sibling set falls back to the ordinary rule.
- **An unknown distribution.** `reservedCPUSet` declines to emit a cpuset on an
  unknown, partial or heterogeneous topology, and the policy then hands the plugin a
  bare count to place as it likes. The even split is an assumption there, not a
  reading, and this one does **not** fail open: on a node whose NUMA nodes differ in
  core count it can under-state one NUMA node's room. Bounded by the pool's 25% clamp,
  and corrected as soon as `NodeResourceTopology` publishes.

For a pod **already scheduled** to a node, the check uses that node's **live
per-NUMA free room**, read from its `NodeResourceTopology` CR (the same occupancy
the `cpu_assignment` metric reports), **minus that node's in-process
reservations**, so both CPUs already claimed by running balloons and demand
admitted-but-not-yet-published are subtracted rather than assumed free. When the
node has not been probed yet or its CR is missing/unparseable, the check falls
back to the **static** capacity formula above — also net of reservations — so
admission never blocks on missing data.

A scheduled pod does not merely read the reservation store, it **records** into
it: the webhook cannot steer a pod that is already bound, but its demand has to
be visible to every later admission for the publish lag, or a concurrent pod is
sized against room this one already occupies. See
[Pre-scheduled pods reserve too](#pre-scheduled-pods-reserve-too).

For an **unscheduled** pod the check runs together with live-headroom steering:
the webhook scans every balloons-enabled node's live per-NUMA free room minus
in-process reservations. If at least one node fits, the pod is admitted and
steered there. If no node fits and at least one reported live data, the check
fails with a detail identifying the tightest node — `enforce` denies at
admission, `warn` admits with a `CapacityCheckWarning` event. Only when **no**
node has live data yet (fresh cluster, topology probe still running) does the
unscheduled check fall back to the static per-NUMA formula so admission still
works during bring-up. This scan is split into a **lock-free read phase** and a
short **reserve-under-lock phase** so the reservation mutex is never held across
the per-node `NodeResourceTopology` reads — see
[Admission critical section](#admission-critical-section-lock-free-read--re-validate-under-lock).

**What "every balloons-enabled node" leaves out.** The winner becomes a REQUIRED
`kubernetes.io/hostname` term, so candidacy models the hard placement rules it can
evaluate from the Node object: `NoSchedule` / `NoExecute` taints the pod does not
tolerate — cordons (`Spec.Unschedulable`) and a `NotReady` `Ready` condition included,
see below — extended resources the pod requests that the node's `allocatable` does not
cover, the node's max-span and coLocate variant, `require`-mode `nodeFeatures`, and the
pod's own `spec.nodeSelector` / required `nodeAffinity`. Pod (anti)affinity, topology
spread and free extended-resource capacity are not modelled (see
[configuration.md](../configuration.md#balloonsclusterconfig-fields)),
so a pin can still be rejected for a rule the scan cannot see.

Taints are matched with `FindMatchingUntoleratedTaint` from
`k8s.io/component-helpers/scheduling/corev1` — the same helper the scheduler's taint
filter uses — so wildcard tolerations (`operator: Exists` with no key) count.
`PreferNoSchedule` does **not** exclude a node: it is a soft effect the scheduler may
ignore, so dropping the node could deny a pod that has somewhere to run.

`nodeStateTaints` feeds two more taints into that same match, derived from the Node's
own state rather than read from `Spec.Taints`: `node.kubernetes.io/unschedulable` for a
cordon, and `node.kubernetes.io/not-ready` / `node.kubernetes.io/unreachable` for a
`Ready` condition of `False` / `Unknown` (read with `GetNodeCondition` from
`k8s.io/component-helpers/node/util`). The node lifecycle controller stamps these
taints itself; deriving them closes the window before it does, and — the point of
routing them through the toleration match — a pod that tolerates one keeps the node.
A matched DaemonSet pod carries `node.kubernetes.io/unschedulable` by design, so it is
still reserved on a cordoned node. A Node with no `Ready` condition is
unknown, not `NotReady`, and stays a candidate.

All three are stamped **`NoSchedule`**, matching the controller's no-schedule pass —
and that effect is load-bearing. The `NoExecute` taints of the same `not-ready` /
`unreachable` keys are the *eviction* pair, which the apiserver's
`DefaultTolerationSeconds` plugin makes every pod tolerate for 300s; deriving the state
taints at `NoExecute` would therefore filter nothing at all. That is what
`TestAdmitWithHeadroomSkipsNotReadyNode/NoExecute_toleration_does_not_excuse_a_NotReady_node`
exists to catch.

The extended-resource check (`podExtendedRequests` / `nodeAdvertisesResources`) is
deliberately narrow. It aggregates the pod's requests with `PodRequests` from
`k8s.io/component-helpers/resource`, so init and sidecar containers count as the
scheduler counts them, keeps only the **extended** names (fully qualified, outside the
`kubernetes.io/` domain), and requires `Status.Allocatable` to cover each. Allocatable
is the node's ceiling, not its free room, which admission cannot see — so a node whose
devices are all in use stays a candidate for the scheduler to reject, and only a node
that cannot run the pod at the advertised ceiling is dropped. `cpu` and `memory` belong
to the per-NUMA model above; `ephemeral-storage` and `hugepages-*` are not filtered.

Concretely, on a four-node fleet whose roomiest node is reserved for one team:

```console
$ kubectl taint nodes worker-3 dedicated=inference:NoSchedule
node/worker-3 tainted
```

`worker-3` keeps `Spec.Unschedulable == false`, so the cordon check does not see it,
but a pod without a matching toleration is now reserved and pinned on the next-best
node. If the remaining candidates do not fit, the `CapacityCheckFailed` detail reports
the narrowed scan — `3 of 4 balloons-enabled nodes were candidates for this pod, 3
with live topology data` — rather than reading as a full fleet. A pod carrying
`dedicated=inference:NoSchedule` in its `tolerations` still gets `worker-3`.

Three residuals are accepted here, none of them closable at admission time:

- A taint applied **after** admission but before binding leaves the pod `Pending`,
  the same cost as the node filling up (see
  [Why enforce is strong-but-not-absolute](#why-enforce-is-strong-but-not-absolute)).
- Tolerations are read as of **this webhook's position in the mutating chain**. The
  apiserver's own `DefaultTolerationSeconds` / `PodTolerationRestriction` plugins run
  before any webhook, so their tolerations are visible; a toleration-injecting webhook
  ordered *after* `mpod.kb.io` is not (and `reinvocationPolicy: IfNeeded` would be
  worse — a re-invocation would book the headroom twice).
- An `Lt` / `Gt` toleration never matches, because the helper is called with
  comparison operators disabled (they need the alpha
  `TaintTolerationComparisonOperators` gate). With the gate on this is stricter than
  the scheduler; the direction is safe — a candidate is dropped, never wrongly pinned.

The webhook attacks the "admitted then can't pin" problem symmetrically for both
scheduling states:

1. **Scheduled pod** (`.spec.nodeName` already set): the capacity check compares the CPU
   request against that specific node's **live per-NUMA free room**, read from
   its `NodeResourceTopology` CR — the same occupancy the `cpu_assignment`
   metric reports — so CPUs already claimed by running balloons are subtracted,
   and against that node's outstanding **reservations**, so pods admitted inside
   the current publish lag are subtracted too. The check treats the pod's CPU
   request as a **per-NUMA share**: ceil(request / span) must fit on each of the
   span's NUMAs, not the full request per NUMA. On success the pod's own share is
   then recorded as a reservation on that node.
2. **Unscheduled pod** (the common case: normal `Deployment` / `StatefulSet`
   POST): the webhook scans every balloons-enabled node and computes live free
   per-NUMA on each (from the same `NodeResourceTopology` source, minus this
   webhook's outstanding reservations), applying the same per-NUMA share model
   (ceil(request / span) ≤ per-NUMA free room). The expensive per-node reads run
   **lock-free**; the reservation mutex is taken only to re-validate the fit against
   concurrent reservations and rank candidates — the commit that records this pod's
   own share happens after it is released, in the authoritative store (see
   [Admission critical section](#admission-critical-section-lock-free-read--re-validate-under-lock)).
   It then does one of three things:
   - If **at least one** node fits, it records a reservation on the fitting node the
     commit accepted (the best-ranked one absent contention)
     — so subsequent admissions in the same burst subtract this pod's demand
     from their own view — and injects a **REQUIRED**
     `kubernetes.io/hostname` nodeAffinity naming **exactly that one node**
     (`kubernetes.io/hostname In [<reserved node>]`), so the pod cannot run
     anywhere other than where its headroom is booked. A PREFERRED term could
     not hold it there. The cost: the required set is one node, so the pod has no
     fallback candidate and stays `Pending` if that node stops accepting it between
     admission and binding (a new taint, another workload filling it).
     The reservation expires purely by timestamp — after the configurable TTL
     (default 90 s; see
     [NRT publish latency requirement](#nrt-publish-latency-requirement)),
     **not** by observing the pin land in the CR — so the TTL must outlive the
     plugin's NRT publish latency. **Dry-run admissions**
     (`kubectl apply --dry-run=server`) perform the full capacity check, node
     selection, and affinity injection so the previewed pod spec is accurate,
     but skip recording the reservation, honoring the webhook's declared
     `sideEffects: None`. With nothing booked there is no single node to pin to, so
     a dry-run preview gets the whole fitting set REQUIRED plus a
     weight-50 **PREFERRED** term toward the best node.
   - If **no** node fits, it emits `CapacityCheckFailed` (mode `enforce`, and the
     admission is denied) or `CapacityCheckWarning` (mode `warn`, admission
     continues but the operator flags the situation with a concrete "best
     node had X free" event), so under `enforce` an oversubscribed burst is
     refused at admission instead of failing with `CreateContainerError` at NRI.
   - If **no node has live data yet** (fresh cluster right after install, or a
     webhook restart before the topology probe has populated any
     `NodeResourceTopology` CR), the check falls back to the static per-NUMA
     formula so admission never blocks on missing data. In **`enforce`** mode this
     fallback is itself **reservation-backed**: it scans the fleet's per-NUMA
     capacity *from the durable topology labels* minus outstanding reservations,
     ranks candidates under `Reservations.mu`, then commits through the reserve seam
     and pins the committed node — so a cold-start burst of concurrent admissions
     serializes against the in-memory store instead of all passing the same empty
     static view (see [cold-start burst safety](#cold-start-burst-safety-static-fallback-reservations)
     below). In **`warn`** and **`off`** modes the fallback stays a read-only
     static check (no reservation): `warn` never denies, so a reservation would
     add only bookkeeping, and `off` opts out of the gate entirely.

## Why enforce is strong-but-not-absolute

Together this drops most of the "admitted then can't pin" gap. It is still not
an absolute guarantee, for these reasons:

- **Static fallback.** When a node has not been probed yet or its
  `NodeResourceTopology` CR is missing/unparseable, the check for that node
  falls back to the **static** per-NUMA capacity formula (ceil(request / span)
  ≤ per-NUMA capacity). That formula nets out the reserved pool, which is static
  config, but not *allocated* balloons, which it has no reading of. A
  *corrupt* CR — one whose top-level `zones` is present but of the wrong type —
  counts as unparseable here: it is logged and read as **unknown occupancy**, not
  as an empty (fully-free) node. That closes the *direct* hole — the corrupt CR
  is never interpreted as fully-free — but it is **not a hard safety guarantee**:
  the node falls back to the coarser static bound like any other node without
  live data, and because that bound assumes an empty node it can still
  **over-admit** if the malformed CR was hiding real occupancy. The
  unscheduled path uses the static formula only when **no** node has live data
  — otherwise nodes without live data are simply skipped from the fit decision.
  In `enforce` mode this cold-start fallback is reservation-backed (see below),
  so concurrent cold-start admissions still serialize; but the static capacity is
  a coarser bound than live free room (it assumes an empty node), so it does not
  catch balloons already allocated but not yet reflected in the labels.
- **The reservation store is per-replica (the HA reservation model).** Reservations
  live in the webhook process's memory and serialize admissions **for that one
  replica**. The manager defaults to **2 replicas** for HA (every replica serves the
  webhook; leader election only gates reconcile/writes), and the webhook `Service`
  load-balances CREATEs across all replicas, so each replica holds an *independent*
  store and none sees the others' reservations. A burst of matched pods spread
  across `replicaCount` replicas would therefore have each replica independently
  reserve the same per-NUMA headroom and steer its pods onto the same best-fit
  node — over-reserving that node by up to `replicaCount ×` the per-replica
  over-reserve (2× at the default; N× at N replicas). That is why the
  **leader-authoritative reserve** (`webhook.reserveAuthority.enabled`, see
  [Leader-authoritative reserve](#leader-authoritative-reserve-ha-single-store)
  below) is **on by default above one replica**: it closes the window while the
  lease holder is active, by forwarding the
  reserve step to the lease holder so the fleet accounts against one store; a
  failover reopens it briefly (bounded to the reservation TTL, and narrowed by the
  successor's [leader-acquisition warm-up](#warm-up-on-leader-acquisition)). Where it
  is turned off, or where a `warn`/`off` policy takes the local fallback, the window is
  a bounded, transient one, not a permanent leak:
  - **Bounded by the reservation TTL.** A stale cross-replica reservation is only
    visible to its own replica and expires after the TTL (`DefaultReservationTTL`,
    90s; configurable — see "NRT publish latency requirement" below), so the worst
    case is confined to one TTL window per burst.
  - **Healed by NRT.** Once the pins land, the `NodeResourceTopology` CR reflects
    the real occupancy and *every* replica reads the same up-to-date free room on
    its next scan — the reservations become redundant and the fleet converges. NRT
    is the eventual source of truth; reservations are never a correctness gate.
  - **Observable.** Each replica increments
    `cpu_pinning_webhook_reservations_recorded_total{node}` when it records a
    reservation and scrapes as its own target, so several replicas incrementing for
    the same node in one window is the operator-visible signal that the split-brain
    window is biting. See [docs/observability.md](../observability.md).

  A webhook restart drops that replica's map, but the CR catches up within seconds.
  The reset is observable via `kube_pod_container_status_restarts_total` (container
  restart within the same Pod) or a `kube_pod_start_time` change (rollout replacing
  the Pod). Both are standard kube-state-metrics series; an in-process counter would
  reset with the process and be invisible to `increase()`.
  Strict cross-replica accounting — a **leader-authoritative reserve step** so all
  replicas share one reading — is the default above one replica
  (`webhook.reserveAuthority.enabled`, described in the next section); where it is
  turned off, the per-replica model bounds and observes the window rather than
  eliminating it.
  Deployments that turn forwarding off but want to *narrow* the window can set
  the opt-in `webhook.service.sessionAffinity: ClientIP`, which pins a burst from one
  client (apiserver source IP) to a single replica's store instead of spreading it —
  it does not close the window (a mixed-source burst still spreads), so it is a
  mitigation, not the fix.
- **Residual NRT lag.** The CR still lags a just-completed pin by a moment, so
  the very-first burst against a freshly-restarted webhook can still race —
  see the previous bullet.

Treat `enforce` as strong-but-not-absolute: a live failure is a real "won't
fit now" signal, but a live *pass* is not a hard guarantee across replicas or
immediately after a webhook restart. Losing a race is self-healing (the pod's
next admission attempt sees the up-to-date CR) and observable via the
`CapacityCheckWarning` / `CapacityCheckFailed` events + the `CapacityCheckFailures` metric.

When a CPU capacity check fails or warns, the event message shows both the
total request and the per-NUMA share: e.g. `request of 64 cores (32 per NUMA at
span 2) exceeds ... per-NUMA capacity of 56`. This makes it clear that span
scales the capacity side, not the request itself.

## Leader-authoritative reserve (HA single store)

Without it each replica reserves against its own in-memory store, which is why a
cross-replica burst can transiently over-reserve (previous section). The
**leader-authoritative reserve** (`webhook.reserveAuthority.enabled`, requires
`replicaCount > 1` and `leaderElection.enabled`; the chart only renders it when all
three hold) gives the fleet single-store correctness without a CRD or external
datastore, by forwarding **only the reserve step** to the lease holder.

It is **on by default**, because the chart also defaults to two replicas: shipping two
independent stores under a mechanism whose purpose is to stop over-admission would
protect only whichever replica happened to receive the pod. A single-replica install
still pays nothing — no forwarding client, no authority Runnable, no reserve port —
since the render gate (`cpu-pinning-operator.reserveAuthorityEnabled`) needs four things,
each of them a reason the endpoint could not work rather than a preference: the value,
a peer to forward to, a lease to elect the authority by, and a serving cert the manager
can verify a peer against. The manager exits at startup if that cert carries no `ca.crt`
or no DNS SAN, so an install providing its own must assert it has both via
`webhook.reserveAuthority.allowExternalServingCert`; until it does, that install keeps
per-replica stores.

The default carries an availability trade under `capacityCheck.mode: enforce`, where an
unanswerable commit denies rather than falling back to this replica's own store; that
fallback would reinstate two-ledger accounting at exactly the moment the shared ledger
matters. "When the commit still cannot be answered, the mode decides" further down this
section has the reasoning and the two operational consequences.

This closes
the cross-replica window while one authority holds the lease; on a failover the
window widens again: the new leader's store (the same one it already used for local
fallback as a follower) has no record of the deposed leader's reservations. The
successor narrows that window by re-deriving the set on leader acquisition — see
[Warm-up on leader acquisition](#warm-up-on-leader-acquisition) below for what it
recovers (each re-derived reservation expiring on the pod's own original schedule, not a
fresh TTL) and what it does not (that residue is exposed as with no warm-up: open until
NRT reflects the pin).

Every replica still serves admission fully and does the expensive work locally and
**lock-free**: reading the `NodeResourceTopology` CRs, scanning the fleet and
filtering by span/taint/readiness/device/variant/feature candidacy plus the pod's own
`nodeSelector` / required `nodeAffinity`. Only *ranking* (which subtracts this
replica's reservations) takes the local `Reservations.mu`. Only the final commit — the
short reserve-under-lock phase — is forwarded. This keeps the HA
read/scan/rank path on every replica (no single-replica bottleneck) while the actual
accounting happens against one store.

**Followers must see live topology for this to fire.** The live scan joins two
sources per node: the `NodeResourceTopology` CR (occupancy) and the in-memory
**topology cache** (the per-logical-CPU NUMA/core map the probe produced). The
CR is cluster-wide, but the topology cache is populated *from the probe*, and the
probe pod is a leader-only write — so a follower that only saw the leader's probe
would have an empty topology cache, its live scan would report "no live data,"
and it would silently drop to the static fallback (which crosses the reserve seam
only in `enforce` mode). To make the follower actually rank live and forward, the
topology map is published cluster-readably: the leader stamps it as a
deterministic `…/topology-cache` **annotation** on each Node in the same patch as
the topology labels, and an **un-leader-gated** `TopoCachePopulator` on every
replica watches Nodes and hydrates that replica's topology cache from the
annotation (`NeedLeaderElection()` reports **false**, mirroring the policy-index
populator and the scoped Node cache). Only the read-only topology cache is
un-gated this way — the probe pod and every BalloonsPolicy/CR write stay
leader-only, so followers gain the *read* data admission needs without becoming
writers. The annotation is stripped when a node is excluded from the pool or the
operator is uninstalled, so no stale topology lingers. This is what lets the
`warn`/`live` default (not just `enforce`) route a follower's reserve through the
authority: the follower has the live data to rank in the first place.

**The candidate protocol.** A follower forwards every node whose CPUs fit (memory fit is left to the authority, whose ledger may move the pod to other NUMAs), each
entry carrying the node's *raw* per-NUMA CPU and memory free room for **all** NUMAs, the
pod's per-NUMA share, span and packing mode. The follower's own store is empty, so it
does not pick NUMAs or the node. Under one lock the authority places each candidate
against its own ledger with `placeOnNode`, ranks the fitting ones with `rankedBefore`,
records the best and returns its node and NUMA set. The leader's local admission path
calls the same two functions, so both pick identically for the same state.
A candidate filled by another replica between the follower's lock-free read and the
commit is skipped, and the pod lands on the next-best one. The affinity's **required** hostname is the node the
**authority** committed to, not the follower's local top choice, so placement follows
the reservation by construction: the pod's only candidate node is the one the
headroom was booked on, and no scheduler score or higher-weighted preference (a
`nodeFeatures: <label>: prefer` term is weight 100) can move it elsewhere and leave
the accounting pointing at a node the pod never ran on. The cost: the required set
is one node, so if that node is tainted or
filled by another workload between admission and binding the pod stays `Pending`
instead of landing on the next-best fitting node. A `--dry-run=server` preview books
nothing, so it gets the fitting set REQUIRED with a weight-50 PREFERRED term toward
the local top choice.

Both reserve seams forward through the same path: the live twin (`admitWithHeadroom`)
and the cold-start static twin (`admitStaticHeadroom`) each release their local
ranking lock, then commit through the shared reserve seam. A `--dry-run=server`
preview injects affinity from the local ranking and returns **without** crossing the
seam, so it neither forwards nor consumes a headroom slot.

**Transport.** The reserve hop is a one-shot JSON POST over a **dedicated internal
mTLS port** (default `9444`, `webhook.reserveAuthority.port`), reusing the same
cert-manager serving cert the webhook already mounts — no new secret, no new CA. Both
ends verify the peer against that certificate's **DNS SAN**, not merely its CA chain:
followers dial the leader by **pod IP** (resolved from the coordination `Lease` holder
identity), so the follower pins `ServerName` to the serving-cert DNS name and Go
verifies the leader natively against *that* name, independent of the dial address;
the authority verifies the client cert's SAN in `VerifyPeerCertificate`
(`RequireAnyClientCert`, since Go does not name-check client certs). Because every
replica mounts the same serving-cert secret, a legitimate replica always presents
that identity, while a pod holding a different cert-manager-issued cert (same Issuer,
different Service) has a different SAN and is rejected — chaining to the shared CA is
not sufficient. A tight RPC timeout bounds the hop. The endpoint is HTTP/1.1
(the authority advertises only http/1.1), and a `NetworkPolicy` ingress rule
restricts it to the operator's own replicas (defense in depth on top of mTLS).
The tradeoff: a dedicated port and mTLS handshake per reserve versus threading the
reserve through the existing webhook port — chosen because the reserve is a distinct
internal control-plane hop with a different auth model (replica-to-replica, not
apiserver-to-webhook) and keeping it on its own port keeps the two policies and their
NetworkPolicy rules independent.

**Leader discovery** reads the coordination `Lease` the manager already maintains for
leader election: the holder identity is `<hostname>_<uuid>` where the hostname is the
holder's pod name, which resolves to a pod IP. The authoritative store is owned by a
`manager.Runnable` whose `NeedLeaderElection()` reports **true**, so the manager
starts it only on the lease holder and stops it on failover — the store lives exactly
where the one true accounting must. The leader's own `PodMutator` shares that same
store instance, so the leader accounts against one store whether a reserve arrives
over the RPC or in-process.

**A transport error is not a verdict.** A leader failover mid-call, an unreachable
holder or a decode failure says the fleet's answer is *unknown*, not that there is no
room, so it is surfaced to the admission path as an error and never as a false reject.
A *definite* authority reject (no fit on any candidate, no error) is honored as a
capacity denial — the gate and the placement filter share that one reading, so a pod
can never pass the gate yet be steered somewhere the authority would reject.

**Idempotency, so a retry is safe.** Each admission carries an `Idempotency-Key` header
derived from its admission UID (`reserveKey`). The authority records the node and NUMAs it
committed under that key, expiring the entry with the reservation it describes, and
**replays** it for a repeat of the same key instead of committing again. Only accepted
commits are recorded, so a rejection is re-evaluated on retry. An empty key skips the
ledger, so a single-replica install and the leader's own in-process reserves carry none.

That is what makes the follower's short RPC timeout safe. A follower that times out
cannot tell "not committed" from "committed, reply lost"; committing locally would leave
the pod with two reservations on two nodes, one phantom until the TTL. Instead it
**retries once on the same key** and gets its own commit back. The `r.Context().Err()`
check before the commit still stops the authority booking for a client that has gone;
that is a single sample, and the key covers the interval after it.

**When the commit still cannot be answered, the mode decides.** Under
`capacityCheck.mode: enforce` the pod is **denied**: a refused pod is retried by its own
controller, whereas an over-admitted one binds to a node whose balloon may not fit it,
fails `CreateContainerError`, and — already bound, not terminal — is neither rescheduled
nor replaced nor noticed by the drift reconciler. Under `warn` and `off` the reserve
**falls back to placing on the replica's local store** and self-heals via NRT.

Two operational consequences of the enforce path:

- **A leaderless window denies.** `LeaderDiscovery` reads the `Lease` through a cached
  client, so while the lease is unheld or names a departed pod the commit is
  unaccountable and the retry re-reads the same cache. Every rolling upgrade passes
  through that state, and the successor's warm-up keeps its listener closed a little
  longer (answering from an empty store is the over-admission this exists to prevent).
  Deployments recover without a human, but plan the rollout knowing it.
- **Only followers are denied.** The lease holder commits in-process, so denials land on
  `(N-1)/N` of admissions rather than fleet-wide.

Either outcome counts `reason="unaccountable"` on
`cpu_pinning_capacity_check_failures_total` under the mode's own label, so the warn/off
fallback is visible rather than only logged. A denial also emits a
`ReserveUnaccountable` Event and buckets as `result="error"` on
`cpu_pinning_webhook_admission_seconds`, where a refusal the operator decided on buckets as
`result="denied"`. That is what separates **"no room"**
(`capacity`/`memory`, `CapacityCheckFailed`) from **"cannot account"**: the first needs
capacity, the second needs the authority fixed. The client-visible message names neither
a node nor a fleet count, as every other capacity denial does.

On a leader failover the new leader's store — the same one it already ran local
fallback against — starts with no record of the previous leader's reservations, and the
admission window widens until NRT reflects those outstanding pins. The warm-up below
narrows that, but does not close it: it re-derives what it can observe before the
successor answers its first reserve. A retry that crosses a failover cannot be replayed
either, since the deposed authority's ledger goes with it; that residue is bounded by
the TTL like the rest.

### Warm-up on leader acquisition

Before the authority opens its reserve port it rebuilds the outstanding reservation set
from observable cluster state, so a successor accounts for the deposed leader's
admissions instead of starting empty (`PodMutator.WarmReservations` in
`internal/webhook/warm_reserve.go`, wired as `ReserveAuthority.Warm` and therefore run
only on the lease holder — a single-replica install has no authority Runnable and pays
nothing). The derivation is the **bound** part of the set the store holds: pods this
operator stamped that are assigned to a node but do **not** yet appear as a container
zone in that node's `NodeResourceTopology` CR. (The store also holds reservations for
pods admitted but not yet assigned to a node; those are not observable this way — see the
residue list below.)

The inputs are all things the operator already writes. Pinned pods come from the
`.metadata.annotations.pinned-by` field index the policy controller registers
(`controller.PodPinnedByField`), matched on `pinned-by: cpu-pinning-operator` — the same
list the drift reconciler walks, so the cost scales with the pinned-pod count, not the
cluster's pod count. Span comes from the balloon name the webhook stamped
(`match.SpanFromBalloonName`: `balloon-span2` → 2, `balloon-device` → 1) rather than
from re-matching the pod against the current policy, so the derivation stays faithful
to what was admitted even if the policy has changed since. Cores and bytes come from
the pod's own requests, and the per-NUMA share from the same `perNUMACPUShare` /
`perNUMAMemShare` helpers the admission path uses — never a second formula.

Concretely, for a node `worker-3` with 4 physical cores over 2 NUMAs (2 cores/NUMA)
holding one pod the deposed leader admitted:

```console
$ kubectl get pod vllm-a -n inference \
    -o jsonpath='{.metadata.annotations.cpu-pinning-operator\.intel\.com/pinned-by}{"\n"}'
cpu-pinning-operator
$ kubectl get pod vllm-a -n inference \
    -o jsonpath='{.metadata.annotations.balloon\.balloons\.resource-policy\.nri\.io/pod}{"\n"}'
balloon-span2
$ kubectl get pod vllm-a -n inference \
    -o jsonpath='{.spec.nodeName} {.spec.containers[0].resources.requests.cpu}{"\n"}'
worker-3 4
$ kubectl get noderesourcetopology worker-3 \
    -o jsonpath='{range .zones[?(@.type=="allocation for container")]}{.name}{"\n"}{end}'
inference/other-pod/vllm          # vllm-a is absent: its pin is not published yet
```

The successor seeds `ceil(4 cores / span 2) = 2` cores against each of the two NUMAs
the node's live free room ranks highest, so `worker-3` is not treated as fully
free, and the next 2-core pod is rejected (in `capacityCheck.mode: enforce`; `warn` admits
it with a `CapacityCheckWarning`) rather than silently over-admitted. The seeded state is
visible as `cpu_pinning_webhook_reservations_live{node="worker-3"} 1`.

Four properties keep the derivation from making things worse:

- **A pod already in the CR is never seeded.** Raw free room is the complement of the
  CR's occupied set, so seeding a published pod would charge the node twice. The
  deliberate, *bounded* NRT/reservation overlap the normal path accepts (see
  `DefaultReservationTTL`) stays bounded; a warm-up that seeded everything would make
  it unbounded.
- **Expiry comes from the pod, not from warm-up.** A seeded entry expires one
  reservation TTL after the pod's own `creationTimestamp` — the instant the deposed
  authority's entry would have expired, to within the admission latency between the two
  (`creationTimestamp` is second-granular and precedes the reserve, so the seeded entry
  expires marginally *earlier*, never later). A pod older than the TTL is therefore not
  seeded at all, so a stalled plugin cannot turn long-running pods into phantom headroom
  that re-arms on every failover.
- **Unknown data is skipped, not guessed.** A node whose CR is missing or malformed has
  *unknown* occupancy, so which pins it already reflects is unknown too; a node with
  neither live per-NUMA free room nor usable capacity labels offers no defensible NUMA
  set to hold a share against. Both are skipped whole — the same unknown-data handling
  the live capacity check uses when it falls back instead of blocking. A readable CR
  with **no zones** is the opposite case and is *not* skipped: it reflects nothing and
  raw free room reads the node as fully free, so every pinned pod on it is seeded (the
  plugin-just-restarted state, where the store is the only account of the demand).
- **It cannot wedge the lease.** Warm-up returns no error and is bounded by a timeout. A
  failing pod list logs and the authority serves from an empty store — the same
  availability trade the unreachable-authority fallback makes, not a crash or a stalled
  lease.

What it does **not** recover — so the failover window narrows rather than vanishes:

- **Pods not yet bound to a node.** A pod admitted and reserved moments before the
  failover but still unscheduled has no `.spec.nodeName`, so there is no node to hold
  its reservation against. Its demand is unaccounted until it binds and NRT catches up.
- **The exact NUMA distribution.** The per-node total is faithful, but the NUMA ids are
  re-chosen from *current* free room (`topSpanNUMAs`, net of what warm-up has already
  seeded on that node), so a seeded share can sit on different NUMAs than the deposed
  leader picked — pessimistic on one NUMA, optimistic on another, correct for the node.
- **Pods whose stamp cannot be sized.** A pod with no balloon annotation, a balloon name
  this operator does not generate (that one logged at `-v=1`), or a CPU request that
  rounds to 0 cores yields no share to hold, and is skipped.
- **Partially published pods.** One published container zone counts the whole pod as
  reflected, so a pod-scoped pin whose injected init-container zone landed first is not
  seeded even though the workload container's share is not yet in the CR's occupied set.
  Balloons are elastic, so that under-accounts the remainder until the rest of the zones
  land — the same direction as the NRT lag the reservation covers, never a double count.
- **Skipped nodes**, per the third property above.

In every one of those cases the exposure is that of a store with no warm-up, and no
worse: bounded by one reservation TTL, after which the deposed leader's own entries would
have expired too, and healed by NRT as soon as the pins land.

Two cases go the other way, and deliberately so. A pod this replica admitted through
its own **local fallback** during the leaderless gap is already in this store, and
warm-up cannot tell it apart from one the deposed leader held — reservations carry no
pod identity — so it is counted twice until it expires. That is the conservative
direction (over-reserve, never over-admit), it is bounded by the same TTL, and it
applies only to pods admitted in the gap. Likewise a stamped container that landed in the
**reserved pool** is seeded even though the reserved balloon's own zone already covers its
CPUs, because `ContainerZoneNames` skips reserved-parented zones — again over-reserve, for
one TTL.

So: while one authority holds the lease the cross-replica window is closed. Across a
failover, what the warm-up recovered is held only until each pod's *original* expiry
(`creationTimestamp` + TTL), not for a fresh TTL from the failover; what it missed carries
the same exposure as a store with no warm-up, open until NRT reflects that pod's pin. The failover
window therefore shrinks to the residue rather than disappearing.

**Zero cost when the gate is closed.** An install failing any of the four conditions
never constructs the forwarding client or the authority Runnable, never opens the reserve
port, never runs the warm-up, never keys an idempotency ledger, and reserves straight
against its one in-process store, as a single-replica install does. It also starts
no leader-discovery cache, which is a second watch stream (`newScopedLeaderCache` in
`cmd/main.go`, scoped to the operator's namespace and the one Lease name so it does not
pull in every node's `kube-node-lease`), and carries no `reserve-cache-sync` ready check.
That check exists because a follower marked Ready before the cache syncs would block its
first reserve on that sync inside the admission budget, which under `enforce` is a denial
in the middle of a rollout. `test/chart` asserts both directions of each conjunct.

**End-to-end coverage.** The scrape pass of `make test-validation` runs `E2E_RESERVE_AUTHORITY` (`test/e2e/reserve_authority_e2e_test.go`,
`reserve_cross_replica_e2e_test.go`) against the chart's default two replicas, with
`mode: enforce` and bursts sized from each node's published topology labels. Run it
against a multi-node cluster with multi-NUMA (2-NUMA, 4-NUMA) NRI nodes.

| test | proves |
|---|---|
| `TestReserveAuthorityBurstSingleLedger` | a concurrent over-capacity burst through the apiserver is booked into the lease holder's ledger and nowhere else |
| `TestReserveAuthorityCrossReplicaLedger` | once a wave sent to the leader fills its ledger, a follower refuses the next wave whole |
| `TestReserveAuthorityFailoverAcrossBurst` | across a forced lease handover the successor does not re-issue room the deposed leader gave away |
| `TestReserveAuthorityUnreachableDenies` | a follower under `enforce` denies while the reserve port is cut off, and admits again once it is back. Needs a CNI that enforces NetworkPolicy; on one that does not it skips, even under `E2E_STRICT` |

The burst and cross-replica tests assert the ledger rather than zero
`CreateContainerError`, because the injected `wait-for-balloons` init container holds an
over-admitted pod before `CreateContainer`, so that count cannot fail on its own. The
failover test also checks the real pods: admitted cores across both waves stay within the
node, admitted pods carry NRT cpusets of the requested width, and none hits
`CreateContainerError`. A single API server holds one HTTP/2 connection to
the webhook Service, so one replica serves the whole burst: the burst test cannot open
the cross-replica window there and skips. The cross-replica test does not depend on
that, because it posts AdmissionReviews to each replica directly through the apiserver
pod proxy.

## Pre-scheduled pods reserve too

Everything above about reservations is written from the steering path's point of
view, because that is where the placement decision is made. A pod that arrives
with `.spec.nodeName` already set — a DaemonSet pod, a static pod, anything bound
out of band — has no placement decision left to make, but it does have demand,
and that demand is invisible in the `NodeResourceTopology` CR for exactly the same
publish lag. So it reserves on its bound node, and the gates that admit it read
the store:

- **It records.** Its per-NUMA shares (`ceil(request/span)` for CPU and memory)
  go into the same store, through the same reserve seam, so in an HA install the
  fleet still accounts against one store. NUMAs are chosen from free room *net*
  of outstanding reservations — picking on raw room could land on the NUMAs
  another pod already holds and fail there while a sibling had space — while the
  `RawFree`/`MemFree` carried in the request stay raw, because the authority
  re-validates against its own reservations.
- **It is gated on the store.** `capacityFits` and `memoryCapacityCheck` subtract
  the bound node's reservations. Without this, recording would be pointless
  between two pre-scheduled pods: both would pass a gate that could not see the
  other, and only the second reserve would notice, too late.
- **The commit decides, not the gate.** The gate reads a snapshot and releases the
  lock, so two overlapping admissions can both pass it. The reserve commit is the
  serialization point; in `enforce` the pod that loses it is denied, the same way
  the steering path drives its verdict from the commit's `accepted`.
- **Dry-run records nothing**, per `sideEffects: None`.
- **The memory opt-out reaches the commit.** `mode: off`, and a `warn` gate that
  already found the bound node does not fit, clear `filterMem`: the request carries
  `MemChecked: false`, so the commit does not re-check memory. It still carries the
  bytes. Rejecting there would drop the CPU share with it, leaving the node reading
  emptier than it is. `enforce` keeps the check — its gate returns before
  `memGateFailed` can be set. Because `mode` is per policy and the store is per node,
  an `off`-mode pod can book a share no node could satisfy, and an `enforce`-mode pod
  on that node is then denied on memory until the reservation expires.
- **A node with nothing to charge is skipped, in every mode.** Neither live topology
  nor capacity labels yields no NUMAs to reserve against, so the pod is admitted with
  no accounting and a log line naming the node; NRT heals it once the pin
  lands. Not a provisioning race — the Node reconciler writes `balloons-enabled` and
  the topology labels in one patch — so reaching it means the label was set outside
  the operator, or a strip was interrupted.
- **An unaccountable reserve authority is fatal under `enforce`.** It follows the
  steering path's mode split: `warn`/`off` fall back locally, `enforce` denies.

One consequence for operators: `cpu_pinning_webhook_reservations_recorded_total` moves
for pre-scheduled pods as well as steered ones — see
[HA reservation window](../observability.md#ha-reservation-window-cpu_pinning_webhook_reservations_recorded_total).

## NRT publish latency requirement

The reservation store closes the burst race by carrying a steered pod's demand
in the webhook's memory **until the pin becomes visible in the
`NodeResourceTopology` CR** — at which point the live occupancy reading already
accounts for it and the reservation is redundant. Expiry is purely by timestamp
(`reservation.expires`, set to `now + TTL` when the reservation is recorded); the
store never observes the CR. So the correctness of the mechanism rests on one
timing assumption:

> **The NRI balloons plugin must publish a fresh pin into the
> `NodeResourceTopology` CR — and that update must reach the webhook's informer
> cache — within the reservation TTL.**

This *NRT publish latency* is the sum of two delays outside the operator's
control: the plugin's pin-to-CR write (timing owned by the external
`nri-resource-policy-balloons` DaemonSet) plus the informer cache lag before the
webhook reads the update back. On a healthy cluster it is a few seconds; the
default TTL (**90 s**, `webhook.DefaultReservationTTL`) is a safety valve well
above it, chosen so a pod that never actually schedules (stayed `Pending`, was
deleted) does not hold phantom headroom forever.

**If the actual publish latency exceeds the TTL**, a reservation expires *before*
the CR reflects its pin — a window in which the same per-NUMA capacity is counted
free twice: once by the now-expired reservation dropping out, and again because
the CR does not yet show the pin. A second pod in the same burst can then be
admitted and steered against headroom that double-counts the first pod's
capacity. The failure mode is admission-time oversubscription, not a correctness
violation (NRI still pins the pod that lands); it is **self-healing** once the CR
catches up, and observable via `CapacityCheckWarning` events. It matters most in
`enforce` mode and on a heterogeneous fleet, where the wrong steering decision can
route a pod onto a node that is actually full.

**Raising the TTL for slow clusters.** Operators whose plugin publishes slower
than the default — an overloaded API server, very large pod churn, or a plugin
running with an artificial delay — can raise the reservation lifetime with the
`--reservation-ttl` manager flag (chart value `webhook.reservationTTL`, a Go
duration such as `3m`). A non-positive value falls back to the 90 s default. The
TTL is threaded from the flag through `NewReservationStore` into every recorded
reservation; there is no per-node or per-pod override. Raising it trades a longer
phantom-headroom window for never-scheduled pods against tolerance of a slower
publish path — set it comfortably above your measured worst-case publish latency,
not arbitrarily high. See
[chart README](../../deploy/cpu-pinning-operator/README.md).

The 90 s default conflates two concerns — the never-scheduled-pod timeout and the
NRT-publish SLA — which is acceptable because the former is always the larger
bound. Expiry does not wait on the pin appearing in the CR, so the 90 s window is a
timing assumption.

## Cold-start burst safety: static-fallback reservations

The live-headroom path records a reservation on the chosen node so a burst of
concurrent admissions subtract each other's not-yet-reflected demand instead of
all racing the same stale snapshot. That only helps once **some** node has live
data. Right after install, or after a webhook restart before the topology probe
has populated any `NodeResourceTopology` CR, the live scan reports "no live data
anywhere" and the unscheduled path falls back to the static per-NUMA formula. A
naïve read-only fallback would reopen the burst race for exactly this window: two
concurrent pods both read the same empty state, both pass the static "any node
fits" check, and both get stamped — over-admitting the first wave of pods.

To close that window without denying availability, the **`enforce`-mode**
fallback is reservation-backed. It mirrors the live path's structure, sourcing
per-NUMA free room from the durable topology **labels** rather than the
not-yet-available CR:

- Per-NUMA capacity is `physical-cpus / numa-nodes` (× `threads-per-core` in
  `hyperthreads` mode) minus the reserved pool's per-NUMA share on each NUMA id —
  the same value the static `nodeFits` formula uses, because both read one
  `staticPerNUMACapacity` function, so the reservation-backed scan and the
  read-only check agree on capacity. With no occupancy to read, per-NUMA free
  equals capacity minus this webhook's outstanding reservations.
- Under one `Reservations.mu` critical section the scan filters by the same
  candidacy as the live path (untolerated taints including cordon and `NotReady`,
  requested extended resources, max-span, coLocate variant, require-mode nodeFeatures,
  the pod's own `nodeSelector` / required
  `nodeAffinity`), subtracts outstanding reservations and
  ranks candidates by the active packing strategy (spread → roomiest, binpack →
  least-fragmented). The commit and the affinity injection then happen after the
  lock is released, exactly as in the live path: a per-NUMA CPU **and** memory
  reservation on the committed node plus the REQUIRED `kubernetes.io/hostname`
  affinity naming it. The second concurrent admission observes the first's reservation
  subtracted from the static capacity and is denied (or steered away).
- Nodes with no usable topology labels are skipped, but a skip is remembered as
  **unknown capacity**, not absence. The path falls back to the read-only static
  check (which treats missing labels as "unknown → don't block") in either of two
  cases, so admission never fails on absent or partial data: (a) **no** candidate
  node had usable labels, or (b) the labeled candidates all failed to fit while a
  span/variant-eligible node was skipped for missing/invalid labels — its true
  capacity is unknown and could still host the pod, so a "nothing fits" deny would
  rest on partial information. A deny is emitted only when every span/variant
  candidate was fully labeled and none fit (a real, complete decision) — nothing
  is reserved in the fallback cases, so deferring is side-effect-free. This keeps
  enforce-mode cold-start no more aggressive at denying than the read-only path
  `warn` and `off` use.
- A `--dry-run=server` admission runs the full scan and injects the preview
  affinity but records **no** reservation, so repeated dry-runs (CI/CD, GitOps
  reconcile loops) never spuriously consume headroom — mirroring the live path's
  dry-run guard.

**`warn`** and **`off`** modes use the read-only static fallback (no
reservation): `warn` never denies, so a cold-start reservation would only add
bookkeeping without changing the outcome, and `off` opts out of the capacity gate
entirely.

The window is narrow (only until the first NRT CR for a balloons-enabled node is
cached — typically seconds after the node reconciler's probe completes) and
self-healing: the moment live data arrives, the reservation-backed live path
takes over automatically. As with all reservations this is per-replica
best-effort headroom accounting (see the per-replica bullet above), not a
cross-replica correctness gate.

## Admission critical section: lock-free read + re-validate-under-lock

The unscheduled path must do two things atomically enough to prevent
double-admission: decide which node a pod fits on, and record a reservation so
the next pod in the same burst subtracts this pod's demand. A naive
implementation holds the reservation mutex (`ReservationStore.mu`) across the
**entire** scan — including one `NodeResourceTopology` read per candidate node
(and, for memory-carrying pods, several `Pod` reads per node to reconstruct
occupancy). Those reads are the expensive part of admission, and holding the lock
across them **serializes every concurrent admission behind a chain of API reads**.
Under a burst, that lock-hold time can push admission latency past the apiserver's
**10 s mutating-webhook timeout**, and admissions fail even though the cluster has
capacity.

`admitWithHeadroom` therefore runs in **two phases**:

1. **Lock-free read phase (no mutex held).** Scan every balloons-enabled
   candidate node and read its live per-NUMA free room — CPU from the topology
   cache joined to the `NodeResourceTopology` CR, and (when the pod requests
   memory) free memory via the same CR plus the per-pod reads that reconstruct
   pinned-memory occupancy. These produce **raw** per-node capacity snapshots that
   carry **no** reservation subtraction. If no candidate produced live data, the
   function returns "fall back to static" here **without ever taking the lock**.

2. **Reserve-under-lock phase (mutex held).** Take `ReservationStore.mu`, then for
   each snapshot **re-validate the fit**: subtract the reservations that are live
   *right now* (freshly pruned, so a concurrent admission's reservation is
   visible) from the raw capacity, and re-check ceil(request / span) ≤ free. Rank
   the still-fitting nodes, then release the lock and commit through the reserve
   seam; the hostname affinity is injected from the node that commit returned. The
   critical section is **pure in-memory arithmetic**; it contains **no** API
   reads.

**Why this preserves no-double-admission.** The raw snapshot is the
reservation-*independent* capacity side of the fit inequality; the reservations
are the concurrent-demand side, and the two are combined **only under the lock**.
Two admissions that both read an empty store lock-free do **not** both win: the
first to acquire the lock records its reservation, and when the second acquires
the lock its re-validation subtracts that reservation and either steers to a
different node or reports no-fit. Because the fit re-check and the reservation
write happen under the same lock against the same freshly-read reservations, the
sum of what actually gets reserved can never exceed real per-NUMA capacity, without
holding the lock across API reads.

The raw capacity read lock-free can be *stale* by the time the lock is taken (a
pin may have landed in the CR meanwhile), but that is the
NRT-lag window the reservation store already covers; staleness only ever makes
the check slightly conservative or triggers the self-healing re-admission path,
never a double-reservation. This is enforced by two tests:
`TestAdmitWithHeadroomNRTReadsOutsideLock` (structural — the intercepted NRT `Get`
observes the mutex is acquirable, proving the read is not under the lock) and
`TestAdmitWithHeadroomConcurrentNoDoubleAdmission` (correctness — 40 goroutines
racing for 4 nodes' worth of headroom yield exactly 4 fits and no NUMA reserved
beyond capacity, under `-race`).

## Per-NUMA memory: live occupancy reconstructed operator-side

Alongside the CPU dimension the webhook runs a per-NUMA memory check. Workload
balloons pin memory to the span's NUMA nodes (`pinMemory: true`), so a
memory-heavy pod that fits on CPUs can still be OOM-killed if the span's NUMAs
lack the memory. It compares the pod's `requests.memory` **as declared**
(per-NUMA share `ceil(request / span)`) against per-NUMA free memory, mirroring
the CPU live/static split:

- **live** (preferred): full per-NUMA `MemTotal` minus the memory already-pinned
  pods claim (reconstructed operator-side — see below) minus in-process
  reservations.
- **static fallback**: the conservative per-NUMA memory floor
  `…/memory-per-numa-bytes` (the *smallest* per-NUMA `MemTotal` the probe read)
  × span, used when a node has no cached per-NUMA memory map or its
  `NodeResourceTopology` CR is absent/unparseable.

`span` scales the **capacity** side only; the request is never multiplied, so
the operator never reserves or accounts for more memory than the pod asked for
(the per-NUMA `ceil` is a conservative rounding, at most `span−1` bytes over the
request). The dimension is **skipped** when the pod declares no memory request,
or when a node has neither live data nor a `…/memory-per-numa-bytes` label
(unknown → never block on missing data). On failure it behaves exactly like the CPU check (`warn` emits
a `CapacityCheckWarning` and admits, `enforce` denies with `CapacityCheckFailed`, `off`
skips) and increments `capacity_check_failures_total{reason="memory"}`.

For an **unscheduled** pod the same per-NUMA memory reading also filters headroom
steering: the `kubernetes.io/hostname` nodeAffinity the webhook injects names a node
that fits **both** CPU and memory, so on a heterogeneous fleet
the pod can't be steered onto a CPU-fitting-but-memory-too-small node, and a
per-NUMA memory reservation is recorded alongside the CPU one. Filtering and
accounting are two distinct decisions: in `off` mode (and after the standalone
memory gate has already found no node fits) memory does **not** narrow the
candidate set — it must not drop otherwise-valid CPU steering — but the pod's real
per-NUMA memory demand is **still recorded** in the reservation. Zeroing it there
would let a memory-heavy pod reserve nothing and hide its demand from every later
admission in the burst window. The standalone gate still runs to surface a
memory-specific event when no node fits at all. The [cold-start reservation
path](#cold-start-burst-safety-static-fallback-reservations) reserves the memory
share too: its static memory floor is `…/memory-per-numa-bytes` per NUMA, so a
cold-start burst serializes on the memory dimension exactly as it does on CPU —
wherever memory filters at all. Where it does not, the share is recorded without a
re-check and the burst serializes on CPU alone.

> **Live occupancy is reconstructed operator-side.** The `NodeResourceTopology`
> CR exposes CPU resources per zone but **no per-NUMA memory bytes**, so the
> operator derives it: it joins the CR's `memory set` zone attribute (which NUMA
> nodes each pinned container's balloon pins memory to) to each pinned pod's own
> `requests.memory`, attributing `ceil(request / |memory set|)` per NUMA.
> Attribution is by *request*, not RSS — a pod under its request looks fuller
> than reality, so the estimate errs toward denying (consistent with enforce
> being strong-but-not-absolute). A cleaner long-term fix is a `memory`
> `ZoneResource` upstream in the balloons plugin, but it is not a prerequisite.

### Memory-check limitations (mirroring the CPU dimension's reservation model)

- **Occupancy is attributed by declared request, not RSS.** The live check
  attributes each pinned pod's full `requests.memory` to its memory-set NUMAs
  (ceil(request / |set|) per NUMA), so it errs toward denying (a pod under its
  request frees less room than the check thinks; a pod over its request is
  already OOM-killable). This matches the `enforce` strong-but-not-absolute
  contract.
- **Reservations are per-replica.** Memory reservations live in the webhook
  process's memory and serialize admissions for that one replica. A webhook
  restart drops the map (the `NodeResourceTopology` CR catches up within
  seconds); multi-replica HA introduces the same cross-replica over-admission
  window as CPU — bounded by the reservation TTL, healed by NRT, and observable
  via `cpu_pinning_webhook_reservations_recorded_total`. See the HA reservation
  model in the CPU section above.
- **Same TTL, same publish-latency assumption.** The memory reservation shares
  the CPU reservation's timestamp-based expiry and the single configurable TTL,
  so the [NRT publish latency requirement](#nrt-publish-latency-requirement)
  applies verbatim to the memory dimension — a publish latency above the TTL
  double-counts per-NUMA free memory for that window too.

## Fragmentation: why there is no automatic re-pack

A long-running node fragments: pods come and go, leaving free room unevenly
spread across its NUMA nodes. Because a balloon-span-N pod needs an equal share
on **each** of N NUMA nodes (not N cores anywhere), a large span can stop fitting
even when the node's **total** free-CPU count looks more than adequate.

There is deliberately **no automatic re-pack**: the operator does not evict
running pods to defragment, because it cannot know which of them is
business-critical, and eviction is never worth that risk. Bouncing a workload
yourself *may* free contiguous room — but this is not guaranteed: the balloons
allocator chooses the new layout, and it can re-place the pod in much the same
shape. Draining the node reliably clears it because the node empties, so there is
no fragmentation left to leave behind. See the user-facing remediation steps in
[troubleshooting.md](../troubleshooting.md#a-large-span-pod-wont-fit-though-the-node-has-free-cpus-fragmentation).
