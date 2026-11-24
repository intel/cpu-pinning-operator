// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"math"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/intel/cpu-pinning-operator/internal/controller"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
)

// perNUMAMemShare returns ceil(bytes/span), a span-N workload's per-NUMA memory share. Span
// adds capacity, it never multiplies the request. Mirrors perNUMACPUShare.
func perNUMAMemShare(bytes int64, span int) int64 {
	if span < 1 {
		span = 1
	}
	// Divide first: adding span-1 can overflow int64.
	s := int64(span)
	share := bytes / s
	if bytes%s != 0 {
		share++
	}
	return share
}

// freeMemPerNUMA returns capacity minus occupied per NUMA, never negative. Every capacity
// NUMA appears; occupancy on unknown NUMAs is ignored.
func freeMemPerNUMA(capacity, occupied map[int]int64) map[int]int64 {
	free := make(map[int]int64, len(capacity))
	for numa, cap := range capacity {
		f := cap - occupied[numa]
		if f < 0 {
			f = 0
		}
		free[numa] = f
	}
	return free
}

// subtractMemReservations returns free minus reservedMem per NUMA, clamped at 0. With no
// reservations it returns free itself (aliased), safe only because consumers are read-only.
func subtractMemReservations(free, reservedMem map[int]int64) map[int]int64 {
	if len(reservedMem) == 0 {
		return free
	}
	out := make(map[int]int64, len(free))
	for numa, f := range free {
		f -= reservedMem[numa]
		if f < 0 {
			f = 0
		}
		out[numa] = f
	}
	return out
}

// memNUMAFreeDesc returns NUMA ids by free memory descending, ties by ascending id.
func memNUMAFreeDesc(free map[int]int64) []int {
	ids := make([]int, 0, len(free))
	for numa := range free {
		ids = append(ids, numa)
	}
	sort.Slice(ids, func(i, j int) bool {
		if free[ids[i]] != free[ids[j]] {
			return free[ids[i]] > free[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids
}

// memSpanMinFree returns the least free memory among the span most-free NUMAs. ok=false
// with no data; span is clamped to [1, NUMA count].
func memSpanMinFree(free map[int]int64, span int) (minFree int64, ok bool) {
	if len(free) == 0 {
		return 0, false
	}
	ids := memNUMAFreeDesc(free)
	if span > len(ids) {
		span = len(ids)
	}
	if span < 1 {
		span = 1
	}
	return free[ids[span-1]], true
}

// memoryLiveFits reports whether the per-NUMA share fits memSpanMinFree. With empty free,
// ok=false and fits=false, which callers ignoring ok read as a rejection.
func memoryLiveFits(free map[int]int64, bytes int64, span int) (fits bool, minFree int64, ok bool) {
	minFree, ok = memSpanMinFree(free, span)
	if !ok {
		return false, 0, false
	}
	return perNUMAMemShare(bytes, span) <= minFree, minFree, true
}

// memFitsOnNUMAs reports whether the per-NUMA share fits every NUMA in the exact set CPU
// headroom chose, not just some combination. Absent NUMAs count as 0.
func memFitsOnNUMAs(memFree map[int]int64, numas []int, bytes int64, span int) bool {
	share := perNUMAMemShare(bytes, span)
	for _, n := range numas {
		if memFree[n] < share {
			return false
		}
	}
	return true
}

// minMemOnNUMAs returns the least free memory in the set (absent counts as 0), for
// reporting a memory shortfall.
func minMemOnNUMAs(memFree map[int]int64, numas []int) int64 {
	if len(numas) == 0 {
		return 0
	}
	min := memFree[numas[0]]
	for _, n := range numas[1:] {
		if memFree[n] < min {
			min = memFree[n]
		}
	}
	return min
}

// occupiedMemPerNUMA reconstructs per-NUMA occupied memory from the NRT CR and pods. NRT
// gives zones and NUMA sets but no bytes, so each container's requests.memory is split
// ceil-wise over its memory set. capacity filters out NUMAs the node lacks (nil keeps all).
func (m *PodMutator) occupiedMemPerNUMA(ctx context.Context, nrt *unstructured.Unstructured, capacity map[int]int64) (map[int]int64, bool) {
	occ := map[int]int64{}
	// Ghost zones (failed CreateContainer, no RemoveContainer) duplicate a zone on other NUMAs.
	// Coalesce per zone at the largest share per NUMA: never double charge, never miss.
	perZone := map[string]map[int]int64{}
	// Cache pod Gets per pod; nil marks NotFound.
	pods := map[string]*corev1.Pod{}
	pinned, ok := nrtpkg.PinnedMemoryContainers(nrt)
	if !ok {
		return nil, false // malformed NRT → occupancy unknown, fall back rather than under-count
	}
	for _, c := range pinned {
		ns, podName, container := nrtpkg.SplitContainerZoneName(c.Zone)
		if ns == "" || podName == "" || container == "" {
			continue // unparseable zone name → can't attribute; skip
		}
		key := ns + "/" + podName
		pod, cached := pods[key]
		if !cached {
			pod = &corev1.Pod{}
			if err := m.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: podName}, pod); err != nil {
				if errors.IsNotFound(err) {
					pods[key] = nil // stale; don't re-Get for its other zones
					continue
				}
				return nil, false // real read error → fall back rather than under-count
			}
			pods[key] = pod
		}
		if pod == nil {
			continue // known-stale pod from a prior zone
		}
		bytes := containerMemoryRequestBytes(pod, container)
		if bytes == 0 {
			continue // container declares no memory request → nothing to attribute
		}
		// A missing NUMA would spread the request too thin and undercharge real NUMAs.
		numas := c.NUMAs
		if len(capacity) > 0 {
			numas = make([]int, 0, len(c.NUMAs))
			for _, n := range c.NUMAs {
				if _, real := capacity[n]; real {
					numas = append(numas, n)
				}
			}
			if len(numas) == 0 {
				continue
			}
		}
		share := perNUMAMemShare(bytes, len(numas))
		shares := perZone[c.Zone]
		if shares == nil {
			shares = map[int]int64{}
			perZone[c.Zone] = shares
		}
		for _, numa := range numas {
			if share > shares[numa] {
				shares[numa] = share
			}
		}
	}
	for _, shares := range perZone {
		for numa, share := range shares {
			// Saturate: a wrapped sum would read as extra free.
			if occ[numa] > math.MaxInt64-share {
				occ[numa] = math.MaxInt64
				continue
			}
			occ[numa] += share
		}
	}
	return occ, true
}

// containerMemoryRequestBytes returns the named container's requests.memory, or 0.
func containerMemoryRequestBytes(pod *corev1.Pod, container string) int64 {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == container {
			return memoryBytes(pod.Spec.Containers[i].Resources.Requests[corev1.ResourceMemory])
		}
	}
	return 0
}

// nodeLiveMemFreeRaw returns live per-NUMA free memory without reservations. ok=false (use
// static) when the cached map or NRT CR is missing. Lock-free: it does Gets, so call it
// outside ReservationStore.mu and fold reservations in-lock.
func (m *PodMutator) nodeLiveMemFreeRaw(ctx context.Context, nodeName string) (free map[int]int64, ok bool) {
	if m.TopoCache == nil {
		return nil, false
	}
	capacity, cached := m.TopoCache.GetMemory(nodeName)
	if !cached || len(capacity) == 0 {
		return nil, false
	}
	nrt := &unstructured.Unstructured{}
	nrt.SetGroupVersionKind(nrtpkg.GVK)
	if err := m.Client.Get(ctx, client.ObjectKey{Name: nodeName}, nrt); err != nil {
		return nil, false
	}
	occ, ok := m.occupiedMemPerNUMA(ctx, nrt, capacity)
	if !ok {
		return nil, false
	}
	return freeMemPerNUMA(capacity, occ), true
}

// nodeLiveMemFree is nodeLiveMemFreeRaw minus reservedMem. The unscheduled path passes nil,
// since admitWithHeadroom subtracts reservations itself. It does Gets, so never call it
// while holding ReservationStore.mu.
func (m *PodMutator) nodeLiveMemFree(ctx context.Context, nodeName string, reservedMem map[int]int64) (free map[int]int64, ok bool) {
	f, ok := m.nodeLiveMemFreeRaw(ctx, nodeName)
	if !ok {
		return nil, false
	}
	return subtractMemReservations(f, reservedMem), true
}

// nodeStaticMemFree lays the memory-floor label on each NUMA minus reservations, for
// cold start with no live NRT data. It agrees with memoryFits' static gate, not with the
// live path (the floor is the per-NUMA minimum). ok=false when the labels are missing.
func nodeStaticMemFree(labels map[string]string, reserved map[int]int64) (free map[int]int64, ok bool) {
	floor := int64Label(labels, controller.LabelMemoryPerNUMABytes)
	numa := intLabel(labels, controller.LabelNumaNodes)
	// Same bound as nodeStaticFree.
	if floor == 0 || numa == 0 || numa > maxStaticNUMANodes {
		return nil, false
	}
	f := make(map[int]int64, numa)
	for id := 0; id < numa; id++ {
		f[id] = floor
	}
	if len(reserved) > 0 {
		for id := range f {
			f[id] -= reserved[id]
			if f[id] < 0 {
				f[id] = 0
			}
		}
	}
	return f, true
}
