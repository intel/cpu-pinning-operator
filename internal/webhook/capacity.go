// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	nodeutil "k8s.io/component-helpers/node/util"
	resourcehelper "k8s.io/component-helpers/resource"
	schedulinghelper "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/balloons"
	"github.com/intel/cpu-pinning-operator/internal/controller"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// Mirrored from internal/nrt, where they are unexported.
const (
	balloonZoneType     = "balloon"
	reservedBalloonName = "reserved"
)

// nodeMaxSpan reads a node's provisioning state and max span from its labels, and
// returns the labels so every later decision about the node uses this one read.
// labels is nil for an absent or non-balloons node.
func (m *PodMutator) nodeMaxSpan(ctx context.Context, nodeName string) (maxSpan int, enabled bool, labels map[string]string, err error) {
	node := &corev1.Node{}
	if err := m.nodeReader().Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		if errors.IsNotFound(err) {
			return 0, false, nil, nil
		}
		return 0, false, nil, err
	}
	if node.Labels[controller.LabelBalloonsEnabled] != "true" {
		return 0, false, nil, nil
	}
	return effectiveMaxSpan(node.Labels), true, node.Labels, nil
}

// clusterMaxSpan returns the largest max-span among balloons-enabled nodes, or 0 if
// none. It uses effectiveMaxSpan so the gate never sees the fleet wider than the affinity honors.
func (m *PodMutator) clusterMaxSpan(ctx context.Context) (int, error) {
	var nodes corev1.NodeList
	if err := m.nodeReader().List(ctx, &nodes); err != nil {
		return 0, err
	}
	max := 0
	for i := range nodes.Items {
		labels := nodes.Items[i].Labels
		if labels[controller.LabelBalloonsEnabled] != "true" {
			continue
		}
		if v := effectiveMaxSpan(labels); v > max {
			max = v
		}
	}
	return max, nil
}

// effectiveMaxSpan parses the max-span label, defaulting to 1 for missing/invalid labels.
// The topology gate and the candidacy filter both use it so they stay consistent.
func effectiveMaxSpan(labels map[string]string) int {
	if v, ok := labels[controller.LabelMaxSpan]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

// nodeSupportsSpan mirrors the injected `max-span Gt (span-1)` affinity exactly, so the
// live-scan filter and the scheduler agree on candidacy.
func nodeSupportsSpan(labels map[string]string, span int) bool {
	return effectiveMaxSpan(labels) >= span
}

// variantSuffix extracts the coLocate variant suffix from a stamped balloon name
// ("balloon-span2-colocate" -> "-colocate", "balloon-span2-spread" -> "-spread",
// "balloon-span2" -> ""). Only these two suffixes exist; anything else yields "".
func variantSuffix(balloonName string) string {
	switch {
	case strings.HasSuffix(balloonName, "-colocate"):
		return "-colocate"
	case strings.HasSuffix(balloonName, "-spread"):
		return "-spread"
	default:
		return ""
	}
}

// requiredFeatureLabels returns the nodeFeatures keys in require mode. They become
// required affinity terms, so headroom candidacy must apply them too.
func requiredFeatureLabels(nodeFeatures map[string]cpopv1alpha1.NodeFeatureMode) []string {
	var keys []string
	for key, mode := range nodeFeatures {
		if mode == cpopv1alpha1.NodeFeatureRequire {
			keys = append(keys, key)
		}
	}
	return keys
}

// nodeHasFeatures reports whether a node carries every required feature label.
func nodeHasFeatures(labels map[string]string, required []string) bool {
	for _, key := range required {
		if labels[key] != "true" {
			return false
		}
	}
	return true
}

// podRequiredNodeMatcher compiles the pod's nodeSelector and required nodeAffinity with
// the scheduler's helper; the scheduler ANDs them with the injected hostname pin.
func podRequiredNodeMatcher(pod *corev1.Pod) nodeaffinity.RequiredNodeAffinity {
	return nodeaffinity.GetRequiredNodeAffinity(pod)
}

// nodeMatchesPodRequirements reports whether a node satisfies those constraints.
// An unparseable selector excludes the node, as the scheduler does.
func nodeMatchesPodRequirements(matcher nodeaffinity.RequiredNodeAffinity, node *corev1.Node) bool {
	ok, err := matcher.Match(node)
	return err == nil && ok
}

// nodeTaintsTolerated reports whether the pod tolerates every scheduling-blocking taint
// on the node, state taints included, using the scheduler's own taint helper.
func nodeTaintsTolerated(tolerations []corev1.Toleration, node *corev1.Node) bool {
	state := nodeStateTaints(node)
	if len(node.Spec.Taints) == 0 && len(state) == 0 {
		return true
	}
	// Fresh slice: appending to node.Spec.Taints would write into the cached Node.
	taints := make([]corev1.Taint, 0, len(node.Spec.Taints)+len(state))
	taints = append(taints, node.Spec.Taints...)
	taints = append(taints, state...)
	// Comparison operators off (alpha gate): stricter than a gate-on scheduler, which only
	// costs a candidate. The logger is unreachable with them off.
	_, untolerated := schedulinghelper.FindMatchingUntoleratedTaint(
		logr.Discard(), taints, tolerations, taintBlocksScheduling, false)
	return !untolerated
}

// nodeStateTaints returns the NoSchedule cordon and not-ready/unreachable taints the
// node's state implies, mirroring node-lifecycle. The NoExecute twins are tolerated by
// every pod via DefaultTolerationSeconds, so they would filter nothing.
// A missing Ready condition is unknown: don't block.
func nodeStateTaints(node *corev1.Node) []corev1.Taint {
	var taints []corev1.Taint
	if node.Spec.Unschedulable {
		taints = append(taints, corev1.Taint{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule})
	}
	if _, ready := nodeutil.GetNodeCondition(&node.Status, corev1.NodeReady); ready != nil {
		switch ready.Status {
		case corev1.ConditionFalse:
			taints = append(taints, corev1.Taint{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoSchedule})
		case corev1.ConditionUnknown:
			taints = append(taints, corev1.Taint{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoSchedule})
		}
	}
	return taints
}

// taintBlocksScheduling selects hard taint effects; PreferNoSchedule is soft and excluded.
func taintBlocksScheduling(taint *corev1.Taint) bool {
	return taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute
}

// podExtendedRequests returns the pod's positive extended-resource requests, aggregated
// with the scheduler's helper. Defaulting has already copied limits into requests.
func podExtendedRequests(pod *corev1.Pod) corev1.ResourceList {
	var out corev1.ResourceList
	for name, qty := range resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{}) {
		if qty.Sign() <= 0 || !extendedResourceName(name) {
			continue
		}
		if out == nil {
			out = corev1.ResourceList{}
		}
		out[name] = qty
	}
	return out
}

// extendedResourceName reports whether a resource name is fully qualified and outside the
// kubernetes.io domain. Prefix, not upstream's substring test: vendor.kubernetes.io/gpu
// is a scalar resource the scheduler checks. cpu, memory and hugepages-* are out of scope.
func extendedResourceName(name corev1.ResourceName) bool {
	return strings.Contains(string(name), "/") &&
		!strings.HasPrefix(string(name), corev1.ResourceDefaultNamespacePrefix)
}

// nodeAdvertisesResources reports whether the node's allocatable covers the requests.
// Allocatable is a ceiling, so a busy node stays a candidate.
func nodeAdvertisesResources(requests corev1.ResourceList, node *corev1.Node) bool {
	for name, qty := range requests {
		allocatable := node.Status.Allocatable[name]
		if allocatable.Cmp(qty) < 0 {
			return false
		}
	}
	return true
}

// candidateScope reports how many balloons-enabled nodes survived candidacy, so a no-fit
// event does not imply the whole fleet is full.
func candidateScope(candidates, enabled int) string {
	return fmt.Sprintf("%d of %d balloons-enabled nodes were candidates for this pod", candidates, enabled)
}

// liveCandidateScope also counts candidates with live per-NUMA data, separating "no room"
// from "NRT not published yet".
func liveCandidateScope(candidates, enabled, withLiveData int) string {
	return fmt.Sprintf("%s, %d with live topology data", candidateScope(candidates, enabled), withLiveData)
}

// variantChecker returns a predicate for whether a node can host the coLocate variant,
// memoized on topology label.
func variantChecker(span int, suffix string) func(labels map[string]string) bool {
	if suffix == "" {
		return func(map[string]string) bool { return true }
	}
	cache := map[string]bool{}
	return func(labels map[string]string) bool {
		key := labels[controller.LabelPackageTopology]
		if v, ok := cache[key]; ok {
			return v
		}
		v := balloons.VariantEmittable(balloons.ParsePackageTopology(key), span, suffix)
		cache[key] = v
		return v
	}
}

// podCPURequestCores returns whole cores to size the cpuset for, rounded up; pod-scoped
// sums across containers since they share one balloon.
func podCPURequestCores(pod *corev1.Pod, container *corev1.Container, podScoped bool) int {
	if !podScoped && container != nil {
		q := container.Resources.Requests[corev1.ResourceCPU]
		return int(cpuMilliCores(q) / 1000)
	}
	totalMilli := int64(0)
	for i := range pod.Spec.Containers {
		milli := cpuMilliCores(pod.Spec.Containers[i].Resources.Requests[corev1.ResourceCPU])
		if totalMilli > math.MaxInt64-milli {
			totalMilli = math.MaxInt64
			break
		}
		totalMilli += milli
	}
	// Overflow-free ceil.
	cores := totalMilli / 1000
	if totalMilli%1000 != 0 {
		cores++
	}
	return int(cores)
}

// maxInt64Quantity gates saturation: above it Value() wraps negative. Reachable because
// mutating admission runs before apiserver validation.
var maxInt64Quantity = *resource.NewQuantity(math.MaxInt64, resource.DecimalSI)

// cpuMilliCores saturates at max int64; MilliValue scales by 1000 unchecked.
func cpuMilliCores(q resource.Quantity) int64 {
	if q.Sign() <= 0 {
		return 0
	}
	if q.Cmp(maxInt64Quantity) >= 0 {
		return math.MaxInt64
	}
	if v := q.Value(); v > math.MaxInt64/1000 {
		return math.MaxInt64
	}
	return q.MilliValue()
}

// memoryBytes saturates instead of wrapping past int64.
func memoryBytes(q resource.Quantity) int64 {
	if q.Sign() <= 0 {
		return 0
	}
	if q.Cmp(maxInt64Quantity) >= 0 {
		return math.MaxInt64
	}
	return q.Value()
}

// podMemoryRequestBytes returns the memory request for balloon sizing; pod-scoped sums
// across containers.
func podMemoryRequestBytes(pod *corev1.Pod, container *corev1.Container, podScoped bool) int64 {
	if !podScoped && container != nil {
		return memoryBytes(container.Resources.Requests[corev1.ResourceMemory])
	}
	total := int64(0)
	for i := range pod.Spec.Containers {
		// Saturate: a wrapped sum would under-state demand and pass the gate.
		v := memoryBytes(pod.Spec.Containers[i].Resources.Requests[corev1.ResourceMemory])
		if total > math.MaxInt64-v {
			return math.MaxInt64
		}
		total += v
	}
	return total
}

// memoryFits checks memBytes against the node's floor × span. Missing labels: don't block.
func memoryFits(labels map[string]string, memBytes int64, span int) (bool, int64) {
	floor := int64Label(labels, controller.LabelMemoryPerNUMABytes)
	if floor == 0 {
		return true, 0
	}
	// Floor span to 1 so capacity is never 0.
	if span < 1 {
		span = 1
	}
	if floor > math.MaxInt64/int64(span) {
		return true, 0 // floor×span would overflow → unknown, don't block
	}
	capacity := floor * int64(span)
	return memBytes <= capacity, capacity
}

// int64Label parses a positive node label, 0 meaning unknown (don't block).
func int64Label(labels map[string]string, key string) int64 {
	if v, ok := labels[key]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// memoryCapacityCheck is the per-NUMA memory fit gate, returning (fits, detail, err).
// Caller guarantees memBytes > 0. On the pre-scheduled path it folds in the bound node's
// outstanding reservations itself, so two pods inside one NRT publish lag can't both pass.
func (m *PodMutator) memoryCapacityCheck(ctx context.Context, pod *corev1.Pod, span int, memBytes int64) (bool, string, error) {
	if pod.Spec.NodeName != "" {
		node := &corev1.Node{}
		if err := m.nodeReader().Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
			if errors.IsNotFound(err) {
				return true, "", nil
			}
			return false, "", err
		}
		_, reservedMem := m.nodeReservations(pod.Spec.NodeName)
		// Prefer live free memory, fall back to the static floor; both net of reservations.
		if free, ok := m.nodeLiveMemFree(ctx, pod.Spec.NodeName, reservedMem); ok {
			fits, minFree, _ := memoryLiveFits(free, memBytes, span)
			if fits {
				return true, "", nil
			}
			return false, fmt.Sprintf("memory request of %s (%s per NUMA at span %d) exceeds node %q live per-NUMA free memory of %s",
				humanBytes(memBytes), humanBytes(perNUMAMemShare(memBytes, span)), span, pod.Spec.NodeName, humanBytes(minFree)), nil
		}

		if free, ok := nodeStaticMemFree(node.Labels, reservedMem); ok {
			fits, minFree, dataOK := memoryLiveFits(free, memBytes, span)
			if dataOK && !fits {
				return false, fmt.Sprintf("memory request of %s (%s per NUMA at span %d) exceeds node %q static per-NUMA memory floor of %s once outstanding reservations are counted",
					humanBytes(memBytes), humanBytes(perNUMAMemShare(memBytes, span)), span, pod.Spec.NodeName, humanBytes(minFree)), nil
			}
			if dataOK {
				return true, "", nil
			}
		}
		fits, capacity := memoryFits(node.Labels, memBytes, span)
		if fits {
			return true, "", nil
		}
		return false, fmt.Sprintf("memory request of %s exceeds node %q per-span memory capacity of %s (span %d)",
			humanBytes(memBytes), pod.Spec.NodeName, humanBytes(capacity), span), nil
	}

	var nodes corev1.NodeList
	if err := m.nodeReader().List(ctx, &nodes); err != nil {
		return false, "", err
	}

	var bestPerNUMA int64
	sawMemBound := false
	for i := range nodes.Items {
		node := &nodes.Items[i]
		labels := node.Labels
		if labels[controller.LabelBalloonsEnabled] != "true" {
			continue
		}

		if !nodeSupportsSpan(labels, span) {
			continue
		}
		// Reservations are not checked here.
		if free, ok := m.nodeLiveMemFree(ctx, node.Name, nil); ok {
			sawMemBound = true
			if fits, minFree, _ := memoryLiveFits(free, memBytes, span); fits {
				return true, "", nil
			} else if minFree > bestPerNUMA {
				bestPerNUMA = minFree
			}
			continue
		}
		// No floor label: don't block.
		floor := int64Label(labels, controller.LabelMemoryPerNUMABytes)
		if floor == 0 {
			return true, "", nil
		}
		sawMemBound = true
		if fits, _ := memoryFits(labels, memBytes, span); fits {
			return true, "", nil
		} else if floor > bestPerNUMA {
			bestPerNUMA = floor // per-NUMA floor, same unit as the live branch's minFree
		}
	}
	if !sawMemBound {
		return true, "", nil // no balloons-enabled node with live data or a memory floor → don't block
	}
	return false, fmt.Sprintf("memory request of %s (%s per NUMA at span %d) exceeds every balloons-enabled node's per-NUMA memory (best per-NUMA %s)",
		humanBytes(memBytes), humanBytes(perNUMAMemShare(memBytes, span)), span, humanBytes(bestPerNUMA)), nil
}

// humanBytes renders a byte count as a compact GiB/MiB string for event detail.
func humanBytes(b int64) string {
	const (
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1fGiB", float64(b)/gib)
	case b >= mib:
		return fmt.Sprintf("%.1fMiB", float64(b)/mib)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// perNUMACPUShare returns ceil(cores/span), the per-NUMA CPU demand shared by nodeFits,
// liveFits and ReservationStore. Every NUMA is assumed to hold the ceiling, since NRI picks
// where the remainder goes.
func perNUMACPUShare(cores, span int) int {
	if span < 1 {
		span = 1
	}
	// Overflow-free ceil.
	share := cores / span
	if cores%span != 0 {
		share++
	}
	return share
}

// maxStaticNUMANodes bounds the label NUMA count; matches the probe ceiling.
const maxStaticNUMANodes = topology.MaxNUMAID + 1

// staticPerNUMACapacity is the label-derived per-NUMA capacity shared by nodeFits and
// nodeStaticFree. ok false means topology unknown: callers must not block.
// It is net of the reserved CPU pool (LabelPhysicalCPUs is raw hardware), subtracted
// after hyperthread scaling. A reserved share is ignored, so bad data can't deny, when it
// exceeds balloons.MaxCredibleReservedPerNUMA or is >= the NUMA's core count, both in
// cores. The test is `>=` because on a density node the share is exactly the core count
// and `>` would deny every pinned pod. A missing label subtracts nothing.
func staticPerNUMACapacity(labels map[string]string, cpuAllocation string) (perNUMA, numa int, ok bool) {
	physical := intLabel(labels, controller.LabelPhysicalCPUs)
	numa = intLabel(labels, controller.LabelNumaNodes)
	if physical == 0 || numa == 0 || numa > maxStaticNUMANodes {
		return 0, 0, false
	}
	coresPerNUMA := physical / numa
	reservedShare := balloons.ReservedPerNUMA(intLabel(labels, controller.LabelReservedCPUs), numa)
	if reservedShare > balloons.MaxCredibleReservedPerNUMA(physical, numa) || reservedShare >= coresPerNUMA {
		reservedShare = 0
	}
	// Capacity stays >= coresPerNUMA - reservedShare, never negative.
	return hyperthreadCapacity(coresPerNUMA, labels, cpuAllocation) - reservedShare, numa, true
}

// nodeFits reports if the pod's per-NUMA share fits one NUMA's static label capacity.
// liveFits uses the same demand model but subtracts live occupancy.
func nodeFits(labels map[string]string, cores, span int, cpuAllocation string) (bool, int) {
	perNUMA, _, ok := staticPerNUMACapacity(labels, cpuAllocation)
	if !ok {
		return true, 0
	}
	return perNUMACPUShare(cores, span) <= perNUMA, perNUMA
}

// hyperthreadCapacity scales per-NUMA capacity by threads-per-core in hyperthreads mode, saturating.
func hyperthreadCapacity(perNUMA int, labels map[string]string, cpuAllocation string) int {
	if cpuAllocation != hyperthreadsAllocation || perNUMA <= 0 {
		return perNUMA
	}
	threads := intLabel(labels, controller.LabelThreadsPerCore)
	if threads < 1 {
		threads = 1
	}
	if perNUMA > math.MaxInt/threads {
		return math.MaxInt
	}
	return perNUMA * threads
}

// intLabel parses a positive integer node label, returning 0 for missing/unparseable/non-positive values.
// Callers treat 0 as "unknown → don't block".
func intLabel(labels map[string]string, key string) int {
	if v, ok := labels[key]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// capacityFits checks the pod's CPU request against per-NUMA capacity for the span,
// returning (fits, detail, err). Bound pods use their node (live, then static); unbound pods
// pass if any enabled node fits. boundLabels nil means unknown: skip the check.
// Each node is counted in its own nodeCPUAllocation mode; clusterAllocation is only the fallback.
func (m *PodMutator) capacityFits(ctx context.Context, pod *corev1.Pod, container *corev1.Container, podScoped bool, span int, variant, clusterAllocation string, boundLabels map[string]string, requiredFeatures []string) (bool, string, error) {
	cores := podCPURequestCores(pod, container, podScoped)
	if cores == 0 {
		return true, "", nil // nothing to size (guarded earlier); don't block
	}

	if pod.Spec.NodeName != "" {
		if boundLabels == nil {
			return true, "", nil
		}
		cpuAllocation := m.nodeCPUAllocation(boundLabels, clusterAllocation)
		// Fold in outstanding reservations so a second pod inside one NRT publish lag is gated
		// against what's actually left.
		reserved, _ := m.nodeReservations(pod.Spec.NodeName)

		if fits, free, ok := m.liveFits(ctx, pod.Spec.NodeName, cores, span, cpuAllocation, m.densityNode(boundLabels), reserved); ok {
			if fits {
				return true, "", nil
			}
			return false, fmt.Sprintf("request of %d cores (%d per NUMA at span %d) exceeds node %q live per-NUMA free of %d (%s)",
				cores, perNUMACPUShare(cores, span), span, pod.Spec.NodeName, free, cpuAllocation), nil
		}

		if free, ok := nodeStaticFree(boundLabels, cpuAllocation, reserved); ok {
			if minFree, dataOK := spanMinFree(free, span); dataOK {
				if perNUMACPUShare(cores, span) <= minFree {
					return true, "", nil
				}
				return false, fmt.Sprintf("request of %d cores (%d per NUMA at span %d) exceeds node %q per-NUMA capacity of %d once outstanding reservations are counted (%s)",
					cores, perNUMACPUShare(cores, span), span, pod.Spec.NodeName, minFree, cpuAllocation), nil
			}
		}
		fits, perNUMACap := nodeFits(boundLabels, cores, span, cpuAllocation)
		if fits {
			return true, "", nil
		}
		return false, fmt.Sprintf("request of %d cores (%d per NUMA at span %d) exceeds node %q per-NUMA capacity of %d (%s)",
			cores, perNUMACPUShare(cores, span), span, pod.Spec.NodeName, perNUMACap, cpuAllocation), nil
	}

	var nodes corev1.NodeList
	if err := m.nodeReader().List(ctx, &nodes); err != nil {
		return false, "", err
	}
	bestCap := 0
	bestMode := clusterAllocation
	supportsVariant := variantChecker(span, variant) // memoized per topology label
	podRequires := podRequiredNodeMatcher(pod)
	for i := range nodes.Items {
		labels := nodes.Items[i].Labels
		if labels[controller.LabelBalloonsEnabled] != "true" ||
			!nodeHasFeatures(labels, requiredFeatures) || !nodeMatchesPodRequirements(podRequires, &nodes.Items[i]) {
			continue
		}

		if !nodeSupportsSpan(labels, span) {
			continue
		}

		if !supportsVariant(labels) {
			continue
		}
		// Each candidate is counted in its own mode.
		mode := m.nodeCPUAllocation(labels, clusterAllocation)
		if fits, perNUMACap := nodeFits(labels, cores, span, mode); fits {
			return true, "", nil
		} else if perNUMACap > bestCap {
			bestCap = perNUMACap
			bestMode = mode
		}
	}
	return false, fmt.Sprintf("request of %d cores (%d per NUMA at span %d) exceeds every node's per-NUMA capacity (best %d, %s)",
		cores, perNUMACPUShare(cores, span), span, bestCap, bestMode), nil
}

// nodeCPUAllocation returns the cpuAllocation mode the reconciler published on the node,
// which an override can make differ from the cluster default. All capacity decisions read
// it here. clusterAllocation stands in for a node with no valid mode label.
func (m *PodMutator) nodeCPUAllocation(labels map[string]string, clusterAllocation string) string {
	if mode := controller.ResolvedCPUAllocation(labels); mode != "" {
		return mode
	}
	return clusterAllocation
}

// fleetCPUAllocation returns the mode every balloons-enabled node the pod can land on
// allocates in, or "" if they differ. "Can land on" is the pod's nodeSelector and
// required nodeAffinity plus the policy's required feature labels.
func (m *PodMutator) fleetCPUAllocation(ctx context.Context, pod *corev1.Pod, requiredFeatures []string, clusterAllocation string) string {
	var nodes corev1.NodeList
	if err := m.nodeReader().List(ctx, &nodes); err != nil {
		return ""
	}
	podRequires := podRequiredNodeMatcher(pod)
	mode := ""
	for i := range nodes.Items {
		labels := nodes.Items[i].Labels
		if labels[controller.LabelBalloonsEnabled] != "true" ||
			!nodeHasFeatures(labels, requiredFeatures) || !nodeMatchesPodRequirements(podRequires, &nodes.Items[i]) {
			continue
		}
		nodeMode := m.nodeCPUAllocation(labels, clusterAllocation)
		if mode != "" && nodeMode != mode {
			return ""
		}
		mode = nodeMode
	}
	return mode
}

// densityNode reports whether the node's reserved pool is its hyperthread-sibling set,
// from the cluster htSiblingPolicy and the node's cpu-allocation label. A node without the
// label is not treated as density; there is no cluster fallback.
func (m *PodMutator) densityNode(labels map[string]string) bool {
	if m.PolicyIndex == nil {
		return false
	}
	return m.PolicyIndex.HTSiblingPolicy() == balloons.HTSiblingPolicyReserved &&
		controller.ResolvedCPUAllocation(labels) == balloons.CPUAllocationPhysicalCores
}

// densityReservedIsSiblings confirms against the NRT CR that the reserved balloon holds
// exactly the HT siblings; the reconciler re-renders the policy before re-labelling the
// node, so densityNode alone can be stale.
func densityReservedIsSiblings(nrt *unstructured.Unstructured, cpuInfo map[int]topology.CPUInfo) bool {
	zones, ok := nrtpkg.Zones(nrt)
	if !ok {
		return false
	}
	reserved := map[int]bool{}
	for _, z := range zones {
		zone, ok := z.(map[string]interface{})
		if !ok {
			continue
		}
		if nrtpkg.ZoneType(zone) != balloonZoneType || nrtpkg.StripInstance(nrtpkg.ZoneName(zone)) != reservedBalloonName {
			continue
		}
		for _, cpu := range nrtpkg.ParseCPUSet(nrtpkg.ZoneCpuset(zone)) {
			reserved[cpu] = true
		}
	}
	siblings := 0
	for cpu, info := range cpuInfo {
		if cpu == info.Core {
			continue
		}
		siblings++
		if !reserved[cpu] {
			return false // a sibling the pool does not hold is allocatable
		}
	}
	// Equality, not containment.
	return siblings > 0 && len(reserved) == siblings
}

// liveFits reports if the pod's per-NUMA share fits the node's live free per-NUMA room.
func (m *PodMutator) liveFits(ctx context.Context, nodeName string, cores, span int, cpuAllocation string, density bool, reserved map[int]int) (fits bool, free int, ok bool) {
	if m.TopoCache == nil {
		return false, 0, false
	}
	cpuInfo, cached := m.TopoCache.Get(nodeName)
	if !cached || len(cpuInfo) == 0 {
		return false, 0, false
	}

	nrt := &unstructured.Unstructured{}
	nrt.SetGroupVersionKind(nrtpkg.GVK)
	if err := m.Client.Get(ctx, client.ObjectKey{Name: nodeName}, nrt); err != nil {
		return false, 0, false // NotFound or any read error → static fallback
	}

	cpus, occOK := nrtpkg.OccupiedCPUs(nrt)
	if !occOK {
		return false, 0, false // malformed NRT → occupancy unknown, static fallback
	}
	occupied := map[int]bool{}
	for _, c := range cpus {
		occupied[c] = true
	}

	// Place the span on the most-free NUMAs; it fits when the least-free of those holds the share.
	density = density && densityReservedIsSiblings(nrt, cpuInfo)
	fpn := subtractReservations(freePerNUMA(cpuInfo, occupied, cpuAllocation, density), reserved)
	minFree, dataOK := spanMinFree(fpn, span)
	if !dataOK {
		return false, 0, false // no per-NUMA data to reason about → static fallback
	}
	return perNUMACPUShare(cores, span) <= minFree, minFree, true
}

// nodeLiveFreeRaw returns live per-NUMA free room without subtracting reservations.
// The NRT Get runs outside the ReservationStore lock.
func (m *PodMutator) nodeLiveFreeRaw(ctx context.Context, nodeName string, cpuAllocation string, density bool) (free map[int]int, ok bool) {
	if m.TopoCache == nil {
		return nil, false
	}
	cpuInfo, cached := m.TopoCache.Get(nodeName)
	if !cached || len(cpuInfo) == 0 {
		return nil, false
	}
	nrt := &unstructured.Unstructured{}
	nrt.SetGroupVersionKind(nrtpkg.GVK)
	if err := m.Client.Get(ctx, client.ObjectKey{Name: nodeName}, nrt); err != nil {
		return nil, false
	}
	return m.liveFreeFromNRT(nodeName, nrt, cpuAllocation, density)
}

// liveFreeFromNRT is nodeLiveFreeRaw over an already-read CR, for callers that need free
// room and the zone set from one version.
func (m *PodMutator) liveFreeFromNRT(nodeName string, nrt *unstructured.Unstructured, cpuAllocation string, density bool) (free map[int]int, ok bool) {
	if m.TopoCache == nil {
		return nil, false
	}
	cpuInfo, cached := m.TopoCache.Get(nodeName)
	if !cached || len(cpuInfo) == 0 {
		return nil, false
	}
	cpus, occOK := nrtpkg.OccupiedCPUs(nrt)
	if !occOK {
		return nil, false // malformed NRT → occupancy unknown, static fallback
	}
	occupied := map[int]bool{}
	for _, c := range cpus {
		occupied[c] = true
	}
	f := freePerNUMA(cpuInfo, occupied, cpuAllocation, density && densityReservedIsSiblings(nrt, cpuInfo))
	if len(f) == 0 {
		return nil, false
	}
	return f, true
}

// nodeStaticFree is the label-derived twin of nodeLiveFreeRaw, minus outstanding
// reservations (the webhook's own admissions, not the reserved CPU pool). Capacity comes
// from staticPerNUMACapacity, as in nodeFits.
func nodeStaticFree(labels map[string]string, cpuAllocation string, reserved map[int]int) (free map[int]int, ok bool) {
	perNUMA, numa, ok := staticPerNUMACapacity(labels, cpuAllocation)
	if !ok {
		return nil, false
	}
	f := make(map[int]int, numa)
	for id := 0; id < numa; id++ {
		f[id] = perNUMA
	}
	return subtractReservations(f, reserved), true
}
