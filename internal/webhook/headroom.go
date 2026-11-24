// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// freePerNUMA returns free room per NUMA id in the cpuAllocation unit, via internal/topology
// (the same definition as the cpu_fragmentation gauge). density counts a core free iff its
// representative thread is free; the gauge keeps the sibling rule, so on density nodes it
// reads 0 where admission sees room.
func freePerNUMA(cpuInfo map[int]topology.CPUInfo, occupied map[int]bool, cpuAllocation string, density bool) map[int]int {
	if density && cpuAllocation != hyperthreadsAllocation {
		return topology.FreeCoresPerNUMADensity(cpuInfo, occupied)
	}
	return topology.FreePerNUMA(cpuInfo, occupied, cpuAllocation)
}

// subtractReservations subtracts per-NUMA reservations (pods admitted but not yet in NRT)
// from free, clamped at zero.
func subtractReservations(free, reserved map[int]int) map[int]int {
	if len(reserved) == 0 {
		return free
	}
	out := make(map[int]int, len(free))
	for numa, n := range free {
		n -= reserved[numa]
		if n < 0 {
			n = 0
		}
		out[numa] = n
	}
	return out
}

// spanMinFree returns the free room of the least-free NUMA among the span most-free ones.
func spanMinFree(free map[int]int, span int) (minFree int, ok bool) {
	return topology.SpanMinFree(free, span)
}

// topSpanNUMAs returns the span most-free NUMA ids, or nil with no per-NUMA data (a normal
// cold-start state; callers skip the reservation).
func topSpanNUMAs(free map[int]int, span int) []int {
	ids := topology.NUMAFreeDesc(free)
	if len(ids) == 0 {
		return nil
	}

	if span < 1 {
		span = 1
	}
	if span > len(ids) {
		span = len(ids)
	}
	return ids[:span]
}
