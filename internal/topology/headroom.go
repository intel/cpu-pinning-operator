// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"math"
	"sort"
)

// SumFree saturates instead of wrapping. A wrapped total ranks the node as the
// emptiest or fullest in the fleet.
func SumFree(free map[int]int) int {
	total := 0
	for _, n := range free {
		switch {
		case n > 0 && total > math.MaxInt-n:
			return math.MaxInt
		case n < 0 && total < math.MinInt-n:
			return math.MinInt
		}
		total += n
	}
	return total
}

// FreePerNUMA is per-NUMA free room in the cpuAllocation unit, shared by webhook
// and metrics. In physical-cores mode a core is free only when no sibling is
// occupied; density nodes need FreeCoresPerNUMADensity.
func FreePerNUMA(cpuInfo map[int]CPUInfo, occupied map[int]bool, cpuAllocation string) map[int]int {
	if cpuAllocation == "hyperthreads" {
		free := map[int]int{}
		for cpu, info := range cpuInfo {
			// NUMA must appear even when fully occupied (free 0 ≠ missing).
			if _, ok := free[info.NUMA]; !ok {
				free[info.NUMA] = 0
			}
			if !occupied[cpu] {
				free[info.NUMA]++
			}
		}
		return free
	}
	return freeCoresPerNUMA(cpuInfo, occupied, false)
}

// FreeCoresPerNUMADensity counts a core free when its representative thread is free;
// under density the siblings are the reserved pool, so FreePerNUMA would read 0.
func FreeCoresPerNUMADensity(cpuInfo map[int]CPUInfo, occupied map[int]bool) map[int]int {
	return freeCoresPerNUMA(cpuInfo, occupied, true)
}

// freeCoresPerNUMA counts free physical cores per NUMA node. siblingsReserved takes a
// core busy only from its representative thread — the CPU whose id is the core id.
func freeCoresPerNUMA(cpuInfo map[int]CPUInfo, occupied map[int]bool, siblingsReserved bool) map[int]int {
	coreOccupied := map[int]bool{} // core id -> occupied by the rule in force
	coreNUMA := map[int]int{}      // core id -> its NUMA
	numaSeen := map[int]bool{}
	for cpu, info := range cpuInfo {
		numaSeen[info.NUMA] = true
		coreNUMA[info.Core] = info.NUMA
		if siblingsReserved {
			// Key on the representative itself: a partial probe may omit it.
			if occupied[info.Core] {
				coreOccupied[info.Core] = true
			}
			continue
		}
		if occupied[cpu] {
			coreOccupied[info.Core] = true
		}
	}
	free := map[int]int{}
	for numa := range numaSeen {
		free[numa] = 0 // NUMA present even if fully occupied
	}
	for core, numa := range coreNUMA {
		if !coreOccupied[core] {
			free[numa]++
		}
	}
	return free
}

// NUMAFreeDesc returns NUMA IDs by descending free capacity, ties by ascending ID.
func NUMAFreeDesc(free map[int]int) []int {
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

// SpanMinFree is the least-free of the `span` most-free NUMAs. ok is false when free is empty.
func SpanMinFree(free map[int]int, span int) (minFree int, ok bool) {
	if len(free) == 0 {
		return 0, false
	}
	ids := NUMAFreeDesc(free)
	if span > len(ids) {
		span = len(ids)
	}
	if span < 1 {
		span = 1
	}
	return free[ids[span-1]], true
}

// StrandedFreeCores is free capacity a max-span balloon cannot absorb. Unit
// follows cpuAllocation. maxSpan < 2 is 0: one NUMA cannot be imbalanced.
func StrandedFreeCores(free map[int]int, maxSpan int) int {
	if maxSpan < 2 || len(free) == 0 {
		return 0
	}
	span := maxSpan
	if span > len(free) {
		span = len(free)
	}
	ids := NUMAFreeDesc(free)
	// Floored: a negative reading is not usable room and would inflate an excess.
	minFree := max(free[ids[span-1]], 0)

	// Sum excesses, not total-minFree*span: saturation would read an imbalanced node as balanced.
	stranded := 0
	add := func(n int) {
		if n <= 0 {
			return // not usable room, so not stranded either
		}
		if stranded > math.MaxInt-n {
			stranded = math.MaxInt
			return
		}
		stranded += n
	}
	for i, id := range ids {
		if i < span {
			add(free[id] - minFree) // excess a span-wide workload cannot use
			continue
		}
		add(free[id]) // outside the span entirely
	}
	return stranded
}

// StrandedAfterPlacing is the binpack score: among nodes that fit, pick the one
// left least fragmented. The span bound is the free-map NUMA count, not the
// node's max-span label, so the score matches post-placement cpu_fragmentation.
func StrandedAfterPlacing(free map[int]int, numas []int, perNUMAShare int) int {
	post := make(map[int]int, len(free))
	for numa, n := range free {
		post[numa] = n
	}
	for _, numa := range numas {
		post[numa] -= perNUMAShare
		if post[numa] < 0 {
			post[numa] = 0
		}
	}
	return StrandedFreeCores(post, MaxSpanForNUMA(len(post)))
}
