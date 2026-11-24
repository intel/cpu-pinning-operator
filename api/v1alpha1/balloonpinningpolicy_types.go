// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MatchSpec defines which pods/containers are pinning targets.
type MatchSpec struct {
	// ByContainerName lists container names that mark a pod as a pinning target.
	// An entry containing a shell glob metacharacter (*, ?, [) is matched as a
	// glob against container names; otherwise it is an exact name.
	ByContainerName []string `json:"byContainerName,omitempty"`
	// ByPodLabels is a set of label key/value pairs; a pod carrying any of them is a pinning target.
	ByPodLabels map[string]string `json:"byPodLabels,omitempty"`

	// ContainerSelection picks what to pin when a match hits more than one
	// container (a byPodLabels match, or byContainerName entries that hit several):
	//   wholePod (default) — pin the whole pod, sizing the balloon from the pod's
	//     aggregate CPU request. A multi-hit byContainerName match is ambiguous
	//     here, so the pod is skipped with a Warning event.
	//   largestContainer — pin only the candidate with the highest CPU request
	//     (ties broken by spec order); sidecars stay in the reserved pool. If that
	//     container fails the whole-core guard the pod is skipped.
	// A single-container match is unaffected.
	// +kubebuilder:validation:Enum=wholePod;largestContainer
	// +kubebuilder:default=wholePod
	ContainerSelection string `json:"containerSelection,omitempty"`
}

// IntentSourceSpec declares where the webhook reads one integer intent value
// off a matched pod. Sources are tried in order annotation, argRegex, envVar,
// default; the first that yields a value wins and omitted sources are skipped.
type IntentSourceSpec struct {
	// Annotation is a pod annotation key holding a stringified integer, e.g.
	// "cpu-pinning-operator.intel.com/tp". Highest precedence.
	Annotation string `json:"annotation,omitempty"`

	// ArgRegex is a Go regexp (RE2 syntax; see https://pkg.go.dev/regexp/syntax)
	// evaluated against the joined container args string. The first non-empty
	// numeric capture group is the value. RE2 does not support backreferences
	// or lookaround — express alternatives with `|` instead.
	// Examples:
	//   vLLM TP:  "(?:^|\\s)(?:--tensor-parallel-size|-tp)(?:=|\\s+)(\\d+)"
	//   SGLang:   "(?:^|\\s)--tp-size(?:=|\\s+)(\\d+)"
	// (?:^|\\s) anchors the flag to the start of an argument, so a model path
	// like "/models/llama-tp=8" is not read as a flag.
	ArgRegex string `json:"argRegex,omitempty"`

	// EnvVar is a container environment variable name whose value is a
	// stringified integer, e.g. "VLLM_TENSOR_PARALLEL_SIZE".
	EnvVar string `json:"envVar,omitempty"`

	// Default is the value used when no configured source produces an integer.
	// Must be ≥ 1 (an intent value of 0 would produce an empty span).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Default int `json:"default,omitempty"`
}

// IntentSpec declares where the webhook reads a matched pod's placement intent,
// so a new runtime is onboarded by editing the policy.
type IntentSpec struct {
	// Span is the primary parallel-size hint that sizes the span-N balloon a
	// matched pod is pinned to. If omitted, the span is 1. Examples:
	//   vLLM tensor-parallel:  annotation=".../tp", argRegex="(?:^|\s)(?:--tensor-parallel-size|-tp)(?:=|\s+)(\d+)", envVar="VLLM_TENSOR_PARALLEL_SIZE"
	//   SGLang tp-size:        argRegex="(?:^|\s)--tp-size(?:=|\s+)(\d+)"
	Span IntentSourceSpec `json:"span,omitempty"`

	// Multiplier is an optional integer source multiplied onto Span before the
	// span is snapped to {1,2,4,8}. Omit it (or leave Default=1) when the
	// workload has no secondary parallel dimension.
	// +optional
	Multiplier *IntentSourceSpec `json:"multiplier,omitempty"`

	// CoLocate controls how a workload's span is spread across NUMA nodes:
	//   "" (default) — the placement engine chooses (bin-pack by live per-NUMA
	//     headroom);
	//   package — keep the whole span on one CPU package (lowest cross-NUMA latency);
	//   numa — spread the span across NUMA nodes (memory bandwidth).
	// +kubebuilder:validation:Enum=package;numa;""
	// +kubebuilder:default=""
	CoLocate string `json:"coLocate,omitempty"`
}

// ReservedSizingSpec sizes the reserved CPU pool for system/non-model
// components. Set at most one of Cpus, Percent and Measured; with none set (or
// the block omitted) the pool is 12 logical CPUs. Every mode then gets the same
// per-node guards: min 4 CPUs, max 25% of physical cores, leave ≥16 CPUs for
// workloads, round down to an even number.
type ReservedSizingSpec struct {
	// Cpus is an explicit logical-CPU count for the reserved pool.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Cpus *int `json:"cpus,omitempty"`

	// Percent is a per-node percentage of physical cores allocated to the
	// reserved pool.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	Percent *int `json:"percent,omitempty"`

	// Measured sizes the reserved pool per node from observed demand (the sum
	// of CPU requests of the node's non-pinned pods) plus headroom, returning
	// unused reserved cores to the pinning pool. See MeasuredSizingSpec.
	// +optional
	Measured *MeasuredSizingSpec `json:"measured,omitempty"`
}

// MeasuredSizingSpec sizes the reserved pool from observed demand:
// reserved = ceil(measuredUsageCPUs × (1 + HeadroomPercent/100)), raised to Min,
// then clamped by the standard per-node guards. Measured usage is the sum of CPU
// requests of the node's non-pinned pods, rounded up to whole CPUs. Before any
// usage is observed the pool is sized to Min.
type MeasuredSizingSpec struct {
	// HeadroomPercent is the slack added above measured usage, as a percentage.
	// 50 (default) reserves 1.5× observed demand. 0 tracks usage exactly (no
	// slack); larger values absorb bursts at the cost of pinning capacity.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=50
	// +optional
	HeadroomPercent *int `json:"headroomPercent,omitempty"`

	// Min is the floor for the measured result, in logical CPUs, and the
	// cold-start size before any usage is observed. Defaults to 4.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=4
	// +optional
	Min *int `json:"min,omitempty"`
}

// BalloonSpec holds the node-global balloon-generation settings, used both as
// the cluster default (spec.balloon) and as a per-node override block. The
// per-node fields (cpuAllocation, reserved) carry no CRD defaults: in an override
// an unset field means "inherit the cluster default", and a CRD default would
// silently override it. Empty values are normalized to the operator defaults in
// code. htSiblingPolicy is cluster-global and defaults to exclude.
//
// Per-node overrides honor only cpuAllocation and reserved; packing and
// reservedNamespaces are cluster-global and read from spec.balloon only.
// +kubebuilder:validation:XValidation:rule="!has(self.reserved) || ((has(self.reserved.cpus)?1:0) + (has(self.reserved.percent)?1:0) + (has(self.reserved.measured)?1:0)) <= 1",message="at most one of reserved.cpus, reserved.percent, reserved.measured may be set"
type BalloonSpec struct {
	// CPUAllocation selects the hyperthread handling mode:
	//   physical-cores (operator default) — hyperthread siblings are excluded
	//     from the pool (availableResources: exclude-cpuset); a request for N
	//     CPUs consumes N physical cores;
	//   hyperthreads — the full logical-CPU pool is used.
	// Unset means inherit: the operator default at the cluster level, or the
	// cluster default in a per-node override.
	// +kubebuilder:validation:Enum=physical-cores;hyperthreads
	// +optional
	CPUAllocation string `json:"cpuAllocation,omitempty"`

	// HTSiblingPolicy selects what happens to the hyperthread siblings of pinned
	// cores in physical-cores mode:
	//   exclude (default) — full isolation: siblings are removed from the
	//     allocatable pool (availableResources: exclude-cpuset), so no workload
	//     lands on the second thread of a pinned core;
	//   reserved — density mode: siblings are placed in the reserved pool's
	//     cpuset (reservedResources.cpu), so non-pinned work can use them while
	//     they stay out of the exclusive balloons. Recovers roughly half the
	//     cores exclude removes, at the cost of noisy-neighbor interference on
	//     the shared core. The reserved pool size is then the sibling count, so
	//     the reserved sizing spec is not applied.
	// No effect in hyperthreads mode.
	// +kubebuilder:validation:Enum=exclude;reserved
	// +kubebuilder:default=exclude
	HTSiblingPolicy string `json:"htSiblingPolicy,omitempty"`

	// Reserved sizes the reserved CPU pool (system / non-model components). If
	// omitted, defaults to 12 logical CPUs. See ReservedSizingSpec. Ignored in
	// density mode (htSiblingPolicy=reserved), where the reserved pool is the
	// HT-sibling set.
	// +optional
	Reserved *ReservedSizingSpec `json:"reserved,omitempty"`

	// Packing selects how the webhook steers an unscheduled pod among the nodes
	// that fit its request, when live headroom data is available:
	//   spread (default) — the node with the most free room. Smaller blast
	//     radius, but free room fragments over time so large spans stop fitting
	//     (see the cpu_pinning_cpu_fragmentation metric);
	//   binpack — the node left least fragmented after placement, filling
	//     partly-used nodes first. Keeps large-span capacity contiguous at the
	//     cost of a larger blast radius.
	// Never changes a pod's request or its reserved per-NUMA share. Cluster-global:
	// honored only on spec.balloon, not in a per-node override. Unset means spread.
	// +kubebuilder:validation:Enum=spread;binpack
	// +optional
	Packing string `json:"packing,omitempty"`

	// ReservedNamespaces lists namespace globs whose unpinned pods are routed to
	// the exclusive reserved CPU pool, emitted verbatim as the BalloonsPolicy's
	// reservedPoolNamespaces. The NRI plugin always routes "kube-system" to
	// reserved, and puts any other unmatched pod on its shared "default"
	// balloon, so scoping this list never strands a pod.
	//
	//   - Unset (nil) — ["*", "kube-system"]: every unpinned pod goes to
	//     reserved. Right for a dedicated inference node.
	//   - Explicit list (e.g. ["inference"]) — only those namespaces go to
	//     reserved; other unpinned pods land on the shared default balloon.
	//   - Explicit empty list ([]) — nothing beyond kube-system goes to reserved.
	//
	// On a shared/OpenShift node "*" funnels every platform pod into the
	// reserved pool and can exhaust it so pinned pods get no balloon; scope or
	// empty this list there. Cluster-global: honored only on spec.balloon, not in
	// a per-node override.
	// +optional
	// +listType=atomic
	ReservedNamespaces []string `json:"reservedNamespaces"`

	// No omitempty on ReservedNamespaces: it would collapse an explicit [] into
	// nil and re-default to "*". (Kept out of the godoc so it stays out of the CRD.)
}

// WebhookSpec holds the MutatingWebhookConfiguration knobs driven by this policy.
type WebhookSpec struct {
	// FailurePolicy controls whether admission fails open or closed when the webhook is unreachable.
	// +kubebuilder:validation:Enum=Ignore;Fail
	// +kubebuilder:default=Ignore
	FailurePolicy string `json:"failurePolicy,omitempty"`
}

// CapacityCheckSpec configures the capacity check applied to matched pods.
type CapacityCheckSpec struct {
	// Mode selects how a capacity-check failure (a request that cannot fit the
	// target node's per-NUMA capacity for the computed span) is handled:
	//   warn (default) — admit the pod but emit a Warning event;
	//   enforce — deny admission;
	//   off — skip the check entirely.
	// enforce also denies a pod whose reservation the leader could not commit;
	// warn and off fall back to accounting it in the local replica's store.
	// +kubebuilder:validation:Enum=enforce;warn;off
	// +kubebuilder:default=warn
	Mode string `json:"mode,omitempty"`
}

// SchedulingSpec holds per-workload node-selection knobs applied by the
// webhook as nodeAffinity. Node exclusion is cluster-global and lives on the
// BalloonsClusterConfig singleton instead.
type SchedulingSpec struct {
	// EnforcePinning, when true (default), injects a required nodeAffinity so
	// matched pods only schedule on balloons-enabled, non-excluded nodes (Pending
	// if none), and makes the webhook deny a matched pod it could not pin because
	// of a failure: no balloons-enabled node yet, or a failed Node/topology/capacity
	// read. The owning controller retries. A not-ready balloons plugin is not
	// denied under capacityCheck.mode warn/off: the pod is admitted pinned and held
	// by the wait-for-balloons init container; mode enforce denies it.
	//
	// Pods that are unpinnable by design are still admitted unpinned, with a
	// NotPinned event and a cpu_pinning_pods_skipped_total count: a fractional or
	// non-guaranteed CPU request, an ambiguous multi-container match, or a pod
	// pre-bound (spec.nodeName) to a node that is not balloons-enabled (so static
	// pods on excluded nodes are not blocked). A request the webhook cannot decode
	// is admitted unmutated.
	//
	// Pair with webhook.failurePolicy: Fail to also cover requests the webhook
	// never sees. Set false for best-effort: matched pods may schedule anywhere
	// and are admitted unpinned wherever the webhook cannot pin them.
	// +kubebuilder:default=true
	EnforcePinning *bool `json:"enforcePinning,omitempty"`

	// NodeFeatures maps a node label key to an affinity mode that steers
	// matched pods toward nodes carrying that label:
	//   prefer  — soft preference (weight 100 preferred nodeAffinity);
	//   require — hard requirement (required nodeAffinity; the pod stays
	//             Pending if no node carries the label);
	//   ignore  — no affinity emitted (useful to disable a chart default).
	// The chart default is {"cpu-pinning-operator.intel.com/amx": "prefer"}.
	// +optional
	NodeFeatures map[string]NodeFeatureMode `json:"nodeFeatures,omitempty"`
}

// NodeFeatureMode is how a single node-feature label steers pod placement.
// +kubebuilder:validation:Enum=prefer;require;ignore
type NodeFeatureMode string

const (
	// NodeFeaturePrefer emits a soft (preferred) nodeAffinity term for the label.
	NodeFeaturePrefer NodeFeatureMode = "prefer"
	// NodeFeatureRequire emits a hard (required) nodeAffinity term for the label.
	NodeFeatureRequire NodeFeatureMode = "require"
	// NodeFeatureIgnore emits no affinity for the label (explicit no-op).
	NodeFeatureIgnore NodeFeatureMode = "ignore"
)

// BalloonPinningPolicySpec defines the desired state of BalloonPinningPolicy.
// It holds only per-workload knobs; node-global settings (cpuAllocation,
// reserved pool, node exclusion, webhook failurePolicy) live on the
// BalloonsClusterConfig singleton.
type BalloonPinningPolicySpec struct {
	// Match defines which pods/containers are pinning targets.
	Match MatchSpec `json:"match,omitempty"`
	// Intent declares how the webhook derives a matched pod's placement intent
	// (span, multiplier, co-locate).
	Intent IntentSpec `json:"intent,omitempty"`
	// CapacityCheck configures the per-NUMA capacity check applied to matched pods.
	CapacityCheck CapacityCheckSpec `json:"capacityCheck,omitempty"`
	// Scheduling holds per-workload node-selection knobs (enforcePinning, nodeFeatures).
	Scheduling SchedulingSpec `json:"scheduling,omitempty"`
	// Namespaces lists exact names and/or glob patterns; empty = match none (safe default).
	Namespaces []string `json:"namespaces,omitempty"`

	// NamespaceSelector additionally targets every namespace whose labels match,
	// on top of Namespaces. nil (default) leaves targeting to Namespaces alone.
	// As in Kubernetes, an empty selector ({}) matches every namespace. The
	// selector is resolved to namespace names at reconcile time, and a namespace
	// relabel triggers a reconcile, so changes apply after a short lag.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// DriftPolicy controls what happens to running pinned pods when the pinning
	// config changes (the webhook only pins at CREATE). Every stale pod gets a
	// Warning "Drifted" event and counts on cpu_pinning_pods_drifted_total:
	//   eventsOnly (default) — report only; re-pinning needs a restart or rollout;
	//   restart — also evict drifted pods so they re-admit with the new config
	//     (PDB-aware and rate-paced).
	// +kubebuilder:validation:Enum=eventsOnly;restart
	// +kubebuilder:default=eventsOnly
	DriftPolicy string `json:"driftPolicy,omitempty"`

	// GuardActivePods enables a validating admission guard for this policy. A
	// pinned pod keeps its cpuset when its policy is deleted or narrowed, but
	// silently loses its guarantee. When true, the webhook rejects:
	//   delete — while status.boundPods is non-empty;
	//   update — when the new match/namespaces would stop covering a pod the
	//     current policy holds.
	// An update that turns the guard off is always allowed, so held pods can be
	// released deliberately before a delete or narrowing edit. nil or false
	// (default) disables the guard.
	// +optional
	GuardActivePods *bool `json:"guardActivePods,omitempty"`
}

// NodeStatus reports what the operator provisioned on one node.
type NodeStatus struct {
	// Name is the node name.
	Name string `json:"name"`
	// MaxSpan is the largest span the node supports (rounded to {1,2,4,8}).
	MaxSpan int `json:"maxSpan"`
	// NumaNodes is the node's NUMA node count.
	NumaNodes int `json:"numaNodes"`
	// PhysicalCPUs is the node's physical core count.
	PhysicalCPUs int `json:"physicalCPUs"`
	// ReservedCPUs is the reserved-pool size applied on the node, after any
	// per-node override and the per-node clamps.
	ReservedCPUs int `json:"reservedCPUs"`
	// CPUAllocation is the hyperthread-handling mode resolved for the node
	// ("physical-cores" | "hyperthreads"), reflecting any matching per-node
	// override. Empty while the node has published no mode.
	// +optional
	CPUAllocation string `json:"cpuAllocation,omitempty"`
	// Features is a copy of the node-feature labels the operator publishes on
	// this node (e.g. "cpu-pinning-operator.intel.com/amx": "true"). Used by
	// the scheduling nodeFeatures map to steer matched pods.
	// +optional
	Features map[string]string `json:"features,omitempty"`
}

// BoundPod identifies one pod a policy currently holds pinned and the balloon
// it is bound to.
type BoundPod struct {
	// Namespace is the pinned pod's namespace.
	Namespace string `json:"namespace"`
	// Name is the pinned pod's name.
	Name string `json:"name"`
	// Balloon is the balloon type the pod is bound to (e.g. "balloon-span2").
	Balloon string `json:"balloon"`
}

// BalloonPinningPolicyStatus defines the observed state of BalloonPinningPolicy.
// The pinning-pool inventory lives on the BalloonsClusterConfig singleton.
type BalloonPinningPolicyStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions follows the standard Kubernetes conditions convention. The
	// Ready condition reflects whether the pinning pool has any provisioned
	// nodes (from the cluster config) for this policy to place pods on.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// BoundPods lists the operator-pinned pods this policy holds (ns/name →
	// balloon), sorted by namespace then name. Capped at the first 100 entries
	// to keep the status object under etcd's per-object size limit;
	// BoundPodsTotal has the uncapped count.
	// +optional
	BoundPods []BoundPod `json:"boundPods,omitempty"`
	// BoundPodsTotal is the true count of operator-pinned pods this policy
	// holds, including any omitted from the capped BoundPods list.
	// +optional
	BoundPodsTotal int `json:"boundPodsTotal,omitempty"`
}

// BalloonPinningPolicy is the Schema for the balloonpinningpolicies API.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=bpp
// +kubebuilder:subresource:status
type BalloonPinningPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BalloonPinningPolicySpec   `json:"spec,omitempty"`
	Status BalloonPinningPolicyStatus `json:"status,omitempty"`
}

// BalloonPinningPolicyList contains a list of BalloonPinningPolicy.
// +kubebuilder:object:root=true
type BalloonPinningPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BalloonPinningPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BalloonPinningPolicy{}, &BalloonPinningPolicyList{})
}
