// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package metrics defines the operator's Prometheus metrics. NRT-sourced gauges
// are rebuilt from the NodeResourceTopology CR; per-balloon used/free/reserved
// are PromQL recording rules over cpu_assignment.
//
// The RBAC markers below are for the secure metrics endpoint (TokenReview +
// SubjectAccessReview); they live here because controller-gen only scans ./internal.
//
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "cpu_pinning"

const (
	ReasonFractional = "fractional" // request not whole-core / requests!=limits
	ReasonNoBalloon  = "no-balloon" // no balloons-enabled node / not provisioned
	ReasonExcluded   = "excluded"   // target node excluded from the pool
	ReasonAmbiguous  = "ambiguous"  // multi-container byContainerName match under containerSelection=wholePod
)

const (
	BypassNodeLookup    = "node-lookup"    // reading node/cluster max-span failed
	BypassCapacityCheck = "capacity-check" // a capacity-check capacity/memory read failed
	BypassMarshal       = "marshal"        // the mutated pod could not be serialized
	BypassDecode        = "decode"         // the request could not be decoded into a Pod
	BypassPolicyIndex   = "policy-index"   // no compiled policy set (defensive; the manager always wires one)
)

const (
	DriftEvictionEvicted     = "evicted"      // eviction API call accepted
	DriftEvictionRateLimited = "rate-limited" // deferred by the eviction rate limiter
	DriftEvictionPDBBlocked  = "pdb-blocked"  // deferred by a PodDisruptionBudget
	DriftEvictionError       = "error"        // eviction failed for another reason
)

// PinningBlocked causes: remedies differ, so they stay separate values.
const (
	PinBlockedNoBalloon  = "no-balloon"   // stamped balloon type is not defined on that node
	PinBlockedNoCapacity = "no-capacity"  // type exists but has no instance/CPUs left
	PinBlockedOther      = "plugin-error" // balloons refused it for a reason this operator does not classify
)

const (
	NodeProvisioned     = "provisioned"   // BalloonsPolicy first applied to a node
	NodeUpdated         = "updated"       // the apply changed the node's stored policy
	NodeDeprovisioned   = "deprovisioned" // node left the pool; its policy was removed
	NodeProvisionFailed = "failed"        // the node's topology probe failed
)

// Workload labels carry a workload_ prefix: the scrape target's own
// namespace/pod/container labels would otherwise rename them to exported_*.
var (
	// PodsPinned has no node label: admission runs before scheduling. An empty
	// policy is never substituted, since any placeholder could collide with a real name.
	PodsPinned = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "pods_pinned_total",
		Help:      "Pods bound to a balloon by the mutating webhook, by the BalloonPinningPolicy that matched.",
	}, []string{"workload_namespace", "balloon", "policy"})

	// PodsSkipped has no node label: most reasons are decided without one.
	PodsSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "pods_skipped_total",
		Help:      "Matched pods left unpinned, by reason (fractional/no-balloon/excluded/ambiguous).",
	}, []string{"workload_namespace", "reason"})

	// CapacityCheckFailures node is empty for an unbound pod; never substitute a
	// placeholder, it could collide with a real node name.
	CapacityCheckFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "capacity_check_failures_total",
		Help: "Capacity check failures, by the node judged (empty for an unbound pod, where every candidate was), reason and mode. " +
			"topology/capacity/memory mean no room; " +
			"unaccountable means the reservation authority could not commit, which is a different remedy.",
	}, []string{"node", "workload_namespace", "reason", "mode"})

	// WebhookBypasses counts internal-error bypasses only, not skips, denials or failurePolicy.
	WebhookBypasses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "webhook_bypasses_total",
		Help:      "Pods admitted unmutated after a webhook internal error, by workload namespace and reason (node-lookup/capacity-check/marshal/decode/policy-index).",
	}, []string{"workload_namespace", "reason"})

	// DriftEvictions counts eviction attempts; node is always set (Running pods only).
	DriftEvictions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "drift_evictions_total",
		Help:      "Drift-driven eviction attempts under driftPolicy restart, by node and outcome (evicted/rate-limited/pdb-blocked/error).",
	}, []string{"node", "workload_namespace", "outcome"})

	// PodsDrifted counts detections, not evictions; eventsOnly pods appear only here.
	PodsDrifted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "pods_drifted_total",
		Help:      "Pods detected as drifted from the config they were pinned with, by node and workload namespace.",
	}, []string{"node", "workload_namespace"})

	// PinningBlocked counts reports, not pods: re-reporting a stuck pod keeps the alert firing.
	PinningBlocked = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "pods_pinning_blocked_total",
		Help: "Reports of a pinned pod whose container the NRI balloons plugin refused at CreateContainer, " +
			"by node, workload namespace and cause (no-balloon/no-capacity/plugin-error). At most one report " +
			"per pod per 10 minutes while the blocker is unchanged, and a pod still blocked is reported again, " +
			"so this is a count of reports over time and not of pods.",
	}, []string{"node", "workload_namespace", "cause"})

	// NodeProvisioning counts BalloonsPolicy writes and failed topology probes per node.
	NodeProvisioning = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "node_provisioning_total",
		Help:      "Node provisioning-lifecycle outcomes, by node and outcome. provisioned/updated/deprovisioned count a write to the node's BalloonsPolicy, so a resync that writes nothing is not counted; failed counts each failed topology probe, retried every minute while it fails.",
	}, []string{"node", "outcome"})

	WebhookAdmissionSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "webhook_admission_seconds",
		Help:      "Mutating webhook admission latency, by result (pinned/skipped/denied/error): denied is a pod the operator refused, error a pod it could not decide on.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"result"})

	NodeReservedCPUs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "node_reserved_cpus",
		Help:      "Reserved CPU-pool size (logical CPUs) the node's rendered policy applies, after the per-NUMA spread.",
	}, []string{"node"})

	// ReservedPoolUtilizationRatio is zero when the reserved size is 0.
	ReservedPoolUtilizationRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "reserved_pool_utilization_ratio",
		Help:      "Observed reserved-pool CPU demand (non-pinned pod CPU requests, rounded up to whole CPUs) divided by the applied reserved size, per node (>1 = oversubscribed).",
	}, []string{"node"})

	// NodeHTSiblingPolicy reports the per-node outcome, not the cluster-wide setting.
	NodeHTSiblingPolicy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "ht_sibling_policy",
		Help: "What became of each node's hyperthread siblings: =1 for the state that holds, 0 for the other three. " +
			"held-back is out of the pinnable pool so no pod is pinned there (the OS can still run host work), reserved-pool means the siblings ARE the " +
			"reserved pool, allocatable means a pinned workload can land on one, no-siblings is a node running one " +
			"thread per core.",
	}, []string{"node", "policy"})

	// ReservationsRecorded is per-replica and not incremented for dry-run.
	ReservationsRecorded = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "webhook_reservations_recorded_total",
		Help:      "Headroom reservations recorded by this webhook replica's in-memory store, per node (per-replica; a spike on one node across replicas signals the HA cross-replica over-admission window).",
	}, []string{"node"})

	// ReservationsLive is not recomputed at scrape time.
	ReservationsLive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "webhook_reservations_live",
		Help:      "Unexpired headroom reservations held by this webhook replica's in-memory store, per node (0 right after a reserve-authority failover means the successor derived none).",
	}, []string{"node"})

	PluginNotReady = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "webhook_plugin_not_ready_total",
		Help:      "Pre-flight readiness check found the balloons plugin not ready on a scheduled pod's target node.",
	}, []string{"node"})

	BalloonsPolicyReconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "balloonspolicy_reconcile_errors_total",
		Help:      "Failed per-node BalloonsPolicy CR renders/applies.",
	}, []string{"node"})

	// CPUAssignment uses balloon="reserved"/"free" with empty workload labels for unowned CPUs.
	CPUAssignment = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "cpu_assignment",
		Help:      "=1 for the workload/balloon owning a logical CPU (from NodeResourceTopology); balloon=free/reserved for unassigned CPUs.",
	}, []string{"node", "cpu", "numa", "core", "balloon", "workload_pod", "workload_namespace"})

	// PoolMembership is per container, not per CPU: per-CPU series for the shared
	// reserved cpuset would break cpu_assignment's max by (..., cpu) de-duplication.
	PoolMembership = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "pool_membership",
		Help: "CPUs in the cpuset one container was placed on, per NodeResourceTopology container zone " +
			"(one series per container, reserved pool included); containers of one pod in one balloon " +
			"share a cpuset, so a per-pod total needs max, not sum.",
	}, []string{"node", "balloon", "workload_namespace", "workload_pod", "workload_container"})

	// GhostAllocations is deleted, not left stale, when the count is unknown.
	GhostAllocations = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "ghost_allocations",
		Help:      "Current count of ghost balloon allocations per node (NodeResourceTopology container zones whose pod no longer exists); 0 when clean.",
	}, []string{"node"})

	// NodeLogicalCPUs is hardware inventory; it exceeds NodePinnableCPUs when siblings are excluded.
	NodeLogicalCPUs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "node_logical_cpus",
		Help:      "Total logical CPUs per NUMA node (static hardware topology; see node_pinnable_cpus for the pool a workload can be pinned to).",
	}, []string{"node", "numa"})

	// NodePinnableCPUs is the free/used denominator, absent until first provisioning.
	NodePinnableCPUs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "node_pinnable_cpus",
		Help:      "Logical CPUs per NUMA node that a workload can be pinned to, as the node's generated policy resolved it (logical CPUs minus any the policy excluded from the plugin's allocatable pool).",
	}, []string{"node", "numa"})

	// CPUManagerConflict is absent when the kubelet config is unreadable; absent is not compatible.
	CPUManagerConflict = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "cpu_manager_conflict",
		Help: "=1 when the node's kubelet runs the CPU Manager static policy alongside the NRI balloons plugin, " +
			"so both allocate the same CPUs and placement there is unpredictable; 0 when the kubelet leaves cpusets " +
			"to the plugin. Absent means the kubelet configuration could not be read (unknown), not compatible.",
	}, []string{"node"})

	// CPUFragmentation is 0 on single-NUMA nodes. Unit follows cpuAllocation.
	CPUFragmentation = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "cpu_fragmentation",
		Help: "Free CPUs the node's largest balloon span cannot absorb due to NUMA imbalance (stranded capacity; unit follows cpuAllocation). " +
			"A narrower balloon has no such constraint and can still be placed on them, so a non-zero value is a limit on the widest shape and not a fault.",
	}, []string{"node"})
)

// collectors is every metric declared above; one missing here is never registered.
var collectors = []prometheus.Collector{
	PodsPinned,
	PodsSkipped,
	CapacityCheckFailures,
	WebhookBypasses,
	DriftEvictions,
	PodsDrifted,
	PinningBlocked,
	NodeProvisioning,
	WebhookAdmissionSeconds,
	ReservationsRecorded,
	ReservationsLive,
	PluginNotReady,
	NodeReservedCPUs,
	ReservedPoolUtilizationRatio,
	NodeHTSiblingPolicy,
	BalloonsPolicyReconcileErrors,
	CPUAssignment,
	PoolMembership,
	NodeLogicalCPUs,
	NodePinnableCPUs,
	CPUFragmentation,
	GhostAllocations,
	CPUManagerConflict,
}

func init() {
	ctrlmetrics.Registry.MustRegister(collectors...)
}
