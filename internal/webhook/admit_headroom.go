// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"fmt"
	"sort"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
)

// headroomVerdict is a signal, not a decision: admitWithHeadroom steers and reserves when a
// node fits, and the caller applies enforce/warn/off.
type headroomVerdict struct {
	// sawLive false means fall back to the static check.
	sawLive bool

	fits bool

	detail string
	// reason is "capacity" or "memory".
	reason string
	// cpuAllocation is the resolved mode of the committed node, "" when none.
	cpuAllocation string
	// unaccountable is true when the commit got no answer and reservePolicy.failClosed refused
	// the local fallback. Not the same as !fits.
	unaccountable bool
}

// admitWithHeadroom does lock-free NRT reads, ranks under a short lock, then commits after
// release; holding the lock across Gets pushes p99 past the webhook timeout. dryRun records
// nothing. filterMem=false still accounts memBytes. Each candidate is counted in its own
// mode; clusterAllocation only covers nodes that published none.
func (m *PodMutator) admitWithHeadroom(ctx context.Context, pod *corev1.Pod, cores, span int, memBytes int64, filterMem bool, variant, clusterAllocation, packing string, requiredFeatures []string, rp reservePolicy, log logr.Logger) headroomVerdict {
	if m.Reservations == nil || m.TopoCache == nil || cores == 0 {
		return headroomVerdict{}
	}

	var nodes corev1.NodeList
	if err := m.nodeReader().List(ctx, &nodes); err != nil {
		log.Error(err, "headroom admit: listing nodes failed; falling back to static capacity check")
		return headroomVerdict{}
	}

	type nodeSnapshot struct {
		node *corev1.Node

		allocation string
		rawFree    map[int]int
		rawMemFree map[int]int64
		memLiveOK  bool
	}
	var snapshots []nodeSnapshot
	enabled := 0
	candidates := 0
	sawLive := false
	supportsVariant := variantChecker(span, variant)
	podRequires := podRequiredNodeMatcher(pod)
	podDevices := podExtendedRequests(pod)
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Labels[controller.LabelBalloonsEnabled] != "true" {
			continue
		}
		enabled++
		if !nodeSupportsSpan(node.Labels, span) {
			continue
		}
		// Honor the pod's other required affinity: affinity is immutable, so a bad pin stays Pending.
		if !nodeHasFeatures(node.Labels, requiredFeatures) || !nodeMatchesPodRequirements(podRequires, node) {
			continue
		}
		if !supportsVariant(node.Labels) {
			continue
		}
		if !nodeAdvertisesResources(podDevices, node) {
			continue
		}
		if !nodeTaintsTolerated(pod.Spec.Tolerations, node) {
			continue
		}
		candidates++
		// Count each candidate in its own mode; the sibling rule differs between modes.
		allocation := m.nodeCPUAllocation(node.Labels, clusterAllocation)
		rawFree, ok := m.nodeLiveFreeRaw(ctx, node.Name, allocation, m.densityNode(node.Labels))
		if !ok {
			continue // no live data for this node → don't steer on/against it
		}
		sawLive = true
		snap := nodeSnapshot{node: node, allocation: allocation, rawFree: rawFree}
		if filterMem && memBytes > 0 {
			if rawMin, ok := spanMinFree(rawFree, span); ok && perNUMACPUShare(cores, span) <= rawMin {
				if rawMemFree, ok := m.nodeLiveMemFreeRaw(ctx, node.Name); ok {
					snap.rawMemFree = rawMemFree
					snap.memLiveOK = true
				}
			}
		}
		snapshots = append(snapshots, snap)
	}

	if !sawLive {
		return headroomVerdict{}
	}

	// The local lock guards only the ranking; the authoritative commit is atomic on its own.
	m.Reservations.mu.Lock()

	type cand struct {
		name    string
		place   nodePlacement
		snap    *nodeSnapshot
		memFree map[int]int64 // raw per-NUMA free memory when checked, else nil
	}
	var fitting []cand
	// Roomiest node and its mode, for the error when nothing fits.
	var bestAvailable int
	var bestAvailableNode string
	bestAvailableMode := clusterAllocation
	// Roomiest node rejected only on memory, for the error.
	var memBlockedNode string
	var memBlockedPerNUMA int64
	memBlockedHere := 0 // CPU-fitting candidates forwarded despite a local memory miss
	noteMemBlocked := func(node string, perNUMA int64) {
		if memBlockedNode == "" || perNUMA > memBlockedPerNUMA {
			memBlockedPerNUMA = perNUMA
			memBlockedNode = node
		}
	}
	// This replica's reservations only: a pre-filter and the dry-run pick. The commit
	// re-places every candidate against the authority's ledger.
	for si := range snapshots {
		snap := &snapshots[si]
		node := snap.node
		var memFree map[int]int64
		if filterMem && memBytes > 0 {
			if snap.memLiveOK {
				memFree = snap.rawMemFree
			} else if rawMem, ok := nodeStaticMemFree(node.Labels, nil); ok {
				memFree = rawMem
			}
		}
		reserved, reservedMem := m.Reservations.reservedBothLocked(node.Name)
		place, ok := placeOnNode(snap.rawFree, reserved, memFree, reservedMem, perNUMACPUShare(cores, span), span, memBytes, packing)
		if !ok {
			continue
		}
		if bestAvailableNode == "" || place.minFree > bestAvailable {
			bestAvailable = place.minFree
			bestAvailableNode = node.Name
			bestAvailableMode = snap.allocation
		}
		if !place.cpuFits {
			continue // CPU doesn't fit → not a candidate
		}
		if !place.memFits {
			noteMemBlocked(node.Name, place.memMin)
			// The authority sees more reservations and may place on other NUMAs, so a
			// local memory miss is not final; dry-run has no authority to ask.
			if rp.dryRun {
				continue
			}
			memBlockedHere++
		}
		fitting = append(fitting, cand{name: node.Name, place: place, snap: snap, memFree: memFree})
	}
	memDenied := func() headroomVerdict {
		detail := fmt.Sprintf("memory request of %s (%s per NUMA at span %d) exceeds the per-NUMA memory on every CPU-fitting node (best %s: %s per NUMA)",
			humanBytes(memBytes), humanBytes(perNUMAMemShare(memBytes, span)), span, memBlockedNode, humanBytes(memBlockedPerNUMA))
		log.Info("headroom admit: CPU fits but no node has per-NUMA memory for span",
			"memBytes", memBytes, "memPerNUMAShare", perNUMAMemShare(memBytes, span), "span", span, "bestMemNode", memBlockedNode, "bestMemPerNUMA", memBlockedPerNUMA)
		return headroomVerdict{sawLive: true, fits: false, detail: detail, reason: "memory"}
	}

	if len(fitting) == 0 {
		m.Reservations.mu.Unlock()
		if memBlockedNode != "" {
			return memDenied()
		}
		detail := fmt.Sprintf("request of %d cores (%d per NUMA at span %d) exceeds the live per-NUMA free room on every candidate node (%s; best %s: %d free, %s)",
			cores, perNUMACPUShare(cores, span), span, liveCandidateScope(candidates, enabled, len(snapshots)), bestAvailableNode, bestAvailable, bestAvailableMode)
		log.Info("headroom admit: no node has live headroom for span",
			"cores", cores, "perNUMAShare", perNUMACPUShare(cores, span), "span", span, "bestNode", bestAvailableNode, "bestFree", bestAvailable,
			"candidates", candidates, "withLiveData", len(snapshots), "balloonsEnabled", enabled)
		return headroomVerdict{sawLive: true, fits: false, detail: detail, reason: "capacity"}
	}

	sort.SliceStable(fitting, func(i, j int) bool {
		return rankedBefore(packing, fitting[i].place, fitting[j].place, fitting[i].name, fitting[j].name)
	})

	perNUMA := perNUMACPUShare(cores, span)
	memPerNUMA := perNUMAMemShare(memBytes, span)

	names := make([]string, 0, len(fitting))
	reqs := make([]ReserveRequest, 0, len(fitting))
	modes := make(map[string]string, len(fitting))
	for _, c := range fitting {
		names = append(names, c.name)
		modes[c.name] = c.snap.allocation
		reqs = append(reqs, ReserveRequest{
			Node:         c.name,
			PerNUMACores: perNUMA,
			PerNUMAMem:   memPerNUMA,
			Span:         span,
			MemBytes:     memBytes,
			RawFree:      c.snap.rawFree,
			MemChecked:   c.memFree != nil,
			MemFree:      c.memFree,
			Packing:      packing,
		})
	}

	if rp.dryRun {
		m.Reservations.mu.Unlock()
		best := fitting[0]
		injectHeadroomAffinity(pod, names, best.name)
		log.Info("headroom admit (dry-run): restricted to fitting nodes without reserving",
			"fittingNodes", names, "bestNode", best.name, "bestFree", best.place.minFree,
			"cores", cores, "perNUMAShare", perNUMA, "memBytes", memBytes, "memPerNUMAShare", memPerNUMA, "span", span)
		return headroomVerdict{sawLive: true, fits: true}
	}

	// Release before commit: the RPC must run unlocked, and a local commit takes this
	// same non-reentrant mutex.
	m.Reservations.mu.Unlock()

	bestNode, numas, accepted, unaccountable := m.commitReserve(ctx, pod, rp, reqs, log)
	if unaccountable {
		return headroomVerdict{sawLive: true, unaccountable: true}
	}
	if !accepted {
		if memBlockedHere == len(fitting) {
			return memDenied()
		}
		detail := fmt.Sprintf("request of %d cores (%d per NUMA at span %d) exceeds the live per-NUMA free room on every candidate node once outstanding reservations are counted (%s%s)",
			cores, perNUMA, span, liveCandidateScope(candidates, enabled, len(snapshots)), sharedMode(modes))
		log.Info("headroom admit: all fitting nodes contended on the authoritative store",
			"cores", cores, "perNUMAShare", perNUMA, "span", span, "fittingNodes", names,
			"candidates", candidates, "withLiveData", len(snapshots), "balloonsEnabled", enabled)
		return headroomVerdict{sawLive: true, fits: false, detail: detail, reason: "capacity"}
	}

	// Pin to the committed node, which may differ from the local top pick.
	injectReservedNodeAffinity(pod, bestNode)
	metrics.ReservationsRecorded.WithLabelValues(bestNode).Inc()
	log.Info("headroom admit: reserved on best and restricted to it",
		"fittingNodes", names, "bestNode", bestNode, "numas", numas,
		"cores", cores, "perNUMAReserved", perNUMA, "memBytes", memBytes, "memPerNUMAReserved", memPerNUMA, "span", span)
	// A committed node that was never a candidate here leaves the mode unresolved.
	return headroomVerdict{sawLive: true, fits: true, cpuAllocation: modes[bestNode]}
}

// sharedMode returns a detail-message clause naming the mode all candidates were counted
// in, or "" when they differ.
func sharedMode(modes map[string]string) string {
	shared := ""
	for _, mode := range modes {
		if shared != "" && mode != shared {
			return ""
		}
		shared = mode
	}
	if shared == "" {
		return ""
	}
	return ", " + shared
}

// staticVerdict is the cold-start twin of headroomVerdict. sawStatic false means fall back
// to capacityFits.
type staticVerdict struct {
	sawStatic bool
	fits      bool
	detail    string
	reason    string
	// cpuAllocation is the committed node's resolved mode, as in headroomVerdict.
	cpuAllocation string
	unaccountable bool
}

// admitStaticHeadroom is the cold-start twin of admitWithHeadroom for when no node has live
// NRT data: free room comes from topology labels. sawStatic=false when labels cannot
// support a decision (none usable, or no known candidate fits while an eligible candidate
// has unknown capacity); the caller then uses capacityFits. dryRun, filterMem and clusterAllocation behave as in
// admitWithHeadroom.
func (m *PodMutator) admitStaticHeadroom(ctx context.Context, pod *corev1.Pod, cores, span int, memBytes int64, filterMem bool, variant, clusterAllocation, packing string, requiredFeatures []string, rp reservePolicy, log logr.Logger) staticVerdict {
	if m.Reservations == nil || cores == 0 {
		return staticVerdict{}
	}

	var nodes corev1.NodeList
	if err := m.nodeReader().List(ctx, &nodes); err != nil {
		log.Error(err, "static headroom admit: listing nodes failed; falling back to read-only static check")
		return staticVerdict{}
	}

	supportsVariant := variantChecker(span, variant)
	podRequires := podRequiredNodeMatcher(pod)
	podDevices := podExtendedRequests(pod)

	m.Reservations.mu.Lock()

	type cand struct {
		name       string
		allocation string
		place      nodePlacement
		rawFree    map[int]int
		memFree    map[int]int64 // per-NUMA memory capacity when checked, else nil
	}
	var fitting []cand
	var bestAvailable int
	var bestAvailableNode string
	bestAvailableMode := clusterAllocation
	var memBlockedNode string
	var memBlockedPerNUMA int64
	noteMemBlocked := func(node string, perNUMA int64) {
		if memBlockedNode == "" || perNUMA > memBlockedPerNUMA {
			memBlockedPerNUMA = perNUMA
			memBlockedNode = node
		}
	}
	enabled := 0
	candidates := 0
	sawStatic := false
	// skippedUnknown: a deny would rest on partial labels.
	skippedUnknown := false
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Labels[controller.LabelBalloonsEnabled] != "true" {
			continue
		}
		enabled++
		if !nodeSupportsSpan(node.Labels, span) {
			continue
		}
		if !supportsVariant(node.Labels) {
			continue
		}
		if !nodeHasFeatures(node.Labels, requiredFeatures) || !nodeMatchesPodRequirements(podRequires, node) {
			continue
		}
		if !nodeAdvertisesResources(podDevices, node) {
			continue
		}
		if !nodeTaintsTolerated(pod.Spec.Tolerations, node) {
			continue
		}
		candidates++
		reserved, reservedMem := m.Reservations.reservedBothLocked(node.Name)

		allocation := m.nodeCPUAllocation(node.Labels, clusterAllocation)
		rawFree, ok := nodeStaticFree(node.Labels, allocation, nil)
		if !ok {
			skippedUnknown = true
			continue
		}
		sawStatic = true
		var memFree map[int]int64
		if filterMem && memBytes > 0 {
			if rawMem, ok := nodeStaticMemFree(node.Labels, nil); ok {
				memFree = rawMem
			}
		}
		place, ok := placeOnNode(rawFree, reserved, memFree, reservedMem, perNUMACPUShare(cores, span), span, memBytes, packing)
		if !ok {
			continue
		}
		if bestAvailableNode == "" || place.minFree > bestAvailable {
			bestAvailable = place.minFree
			bestAvailableNode = node.Name
			bestAvailableMode = allocation
		}
		if !place.cpuFits {
			continue // CPU doesn't fit → not a candidate
		}
		if !place.memFits {
			noteMemBlocked(node.Name, place.memMin)
			continue
		}
		fitting = append(fitting, cand{name: node.Name, allocation: allocation, place: place, rawFree: rawFree, memFree: memFree})
	}

	if !sawStatic {
		m.Reservations.mu.Unlock()
		return staticVerdict{}
	}
	if len(fitting) == 0 && skippedUnknown {
		m.Reservations.mu.Unlock()
		return staticVerdict{}
	}
	if len(fitting) == 0 {
		m.Reservations.mu.Unlock()
		if memBlockedNode != "" {
			detail := fmt.Sprintf("memory request of %s (%s per NUMA at span %d) exceeds the per-NUMA memory on every CPU-fitting node (best %s: %s per NUMA)",
				humanBytes(memBytes), humanBytes(perNUMAMemShare(memBytes, span)), span, memBlockedNode, humanBytes(memBlockedPerNUMA))
			log.Info("static headroom admit: CPU fits but no node has per-NUMA memory for span",
				"memBytes", memBytes, "memPerNUMAShare", perNUMAMemShare(memBytes, span), "span", span, "bestMemNode", memBlockedNode, "bestMemPerNUMA", memBlockedPerNUMA)
			return staticVerdict{sawStatic: true, fits: false, detail: detail, reason: "memory"}
		}
		detail := fmt.Sprintf("request of %d cores (%d per NUMA at span %d) exceeds the per-NUMA capacity minus outstanding reservations on every candidate node (%s; best %s: %d free, %s)",
			cores, perNUMACPUShare(cores, span), span, candidateScope(candidates, enabled), bestAvailableNode, bestAvailable, bestAvailableMode)
		log.Info("static headroom admit: no node has static headroom for span",
			"cores", cores, "perNUMAShare", perNUMACPUShare(cores, span), "span", span, "bestNode", bestAvailableNode, "bestFree", bestAvailable,
			"candidates", candidates, "balloonsEnabled", enabled)
		return staticVerdict{sawStatic: true, fits: false, detail: detail, reason: "capacity"}
	}

	sort.SliceStable(fitting, func(i, j int) bool {
		return rankedBefore(packing, fitting[i].place, fitting[j].place, fitting[i].name, fitting[j].name)
	})

	perNUMA := perNUMACPUShare(cores, span)
	memPerNUMA := perNUMAMemShare(memBytes, span)

	names := make([]string, 0, len(fitting))
	reqs := make([]ReserveRequest, 0, len(fitting))
	modes := make(map[string]string, len(fitting))
	for _, c := range fitting {
		names = append(names, c.name)
		modes[c.name] = c.allocation
		reqs = append(reqs, ReserveRequest{
			Node:         c.name,
			PerNUMACores: perNUMA,
			PerNUMAMem:   memPerNUMA,
			Span:         span,
			MemBytes:     memBytes,
			RawFree:      c.rawFree,
			MemChecked:   c.memFree != nil,
			MemFree:      c.memFree,
			Packing:      packing,
		})
	}

	if rp.dryRun {
		m.Reservations.mu.Unlock()
		best := fitting[0]
		injectHeadroomAffinity(pod, names, best.name)
		log.Info("static headroom admit (dry-run): restricted to fitting nodes without reserving",
			"fittingNodes", names, "bestNode", best.name, "bestFree", best.place.minFree,
			"cores", cores, "perNUMAShare", perNUMA, "memBytes", memBytes, "memPerNUMAShare", memPerNUMA, "span", span)
		return staticVerdict{sawStatic: true, fits: true}
	}

	// Release before commit: the RPC must run unlocked, and a local commit takes this
	// same non-reentrant mutex.
	m.Reservations.mu.Unlock()

	bestNode, numas, accepted, unaccountable := m.commitReserve(ctx, pod, rp, reqs, log)
	if unaccountable {
		return staticVerdict{sawStatic: true, unaccountable: true}
	}
	if !accepted {
		detail := fmt.Sprintf("request of %d cores (%d per NUMA at span %d) exceeds the per-NUMA capacity minus outstanding reservations on every candidate node (%s%s)",
			cores, perNUMA, span, candidateScope(candidates, enabled), sharedMode(modes))
		log.Info("static headroom admit: all fitting nodes contended on the authoritative store",
			"cores", cores, "perNUMAShare", perNUMA, "span", span, "fittingNodes", names,
			"candidates", candidates, "balloonsEnabled", enabled)
		return staticVerdict{sawStatic: true, fits: false, detail: detail, reason: "capacity"}
	}

	injectReservedNodeAffinity(pod, bestNode)
	metrics.ReservationsRecorded.WithLabelValues(bestNode).Inc()
	log.Info("static headroom admit: reserved on best and restricted to it (cold start)",
		"fittingNodes", names, "bestNode", bestNode, "numas", numas,
		"cores", cores, "perNUMAReserved", perNUMA, "memBytes", memBytes, "memPerNUMAReserved", memPerNUMA, "span", span)
	return staticVerdict{sawStatic: true, fits: true, cpuAllocation: modes[bestNode]}
}
