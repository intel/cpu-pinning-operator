// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"math"
	"reflect"
	"sort"
	"testing"
)

// twoNumaFour returns a 4-CPU node: NUMA0={cpu0,cpu1 on core0}, {cpu2 on core2};
// NUMA1={cpu3 on core3}. Two threads on core0 test HT sibling handling.
func twoNumaFour() map[int]CPUInfo {
	return map[int]CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0}, 1: {CPU: 1, NUMA: 0, Core: 0},
		2: {CPU: 2, NUMA: 0, Core: 2},
		3: {CPU: 3, NUMA: 1, Core: 3},
	}
}

func TestFreePerNUMAPhysicalCores(t *testing.T) {
	info := twoNumaFour()
	// Nothing occupied → 2 free cores on NUMA0 (core0, core2), 1 on NUMA1 (core3).
	if got := FreePerNUMA(info, map[int]bool{}, "physical-cores"); !reflect.DeepEqual(got, map[int]int{0: 2, 1: 1}) {
		t.Errorf("no occupancy: got %v, want {0:2, 1:1}", got)
	}
	// Occupy one sibling of core0 (cpu0). Core0 is now busy → NUMA0 has 1 free core.
	if got := FreePerNUMA(info, map[int]bool{0: true}, "physical-cores"); !reflect.DeepEqual(got, map[int]int{0: 1, 1: 1}) {
		t.Errorf("cpu0 occupied: got %v, want {0:1, 1:1}", got)
	}
	// Occupy both siblings of core0 — same result: NUMA0 has 1 free core.
	if got := FreePerNUMA(info, map[int]bool{0: true, 1: true}, "physical-cores"); !reflect.DeepEqual(got, map[int]int{0: 1, 1: 1}) {
		t.Errorf("core0 fully busy: got %v, want {0:1, 1:1}", got)
	}
	// Occupy core2 on NUMA0 and core3 on NUMA1 → NUMA0 free=1 (core0), NUMA1 free=0.
	if got := FreePerNUMA(info, map[int]bool{2: true, 3: true}, "physical-cores"); !reflect.DeepEqual(got, map[int]int{0: 1, 1: 0}) {
		t.Errorf("core2+core3 busy: got %v, want {0:1, 1:0}", got)
	}
}

// TestFreeCoresPerNUMADensity: on a density node the siblings ARE the reserved pool,
// so only the representative thread's occupancy takes a core out of the free count.
func TestFreeCoresPerNUMADensity(t *testing.T) {
	info := twoNumaFour()
	// cpu1 is core0's sibling (reserved): the density rule keeps core0 free.
	occupied := map[int]bool{1: true}
	if got := FreePerNUMA(info, occupied, "physical-cores"); !reflect.DeepEqual(got, map[int]int{0: 1, 1: 1}) {
		t.Errorf("exclude rule with the sibling occupied: got %v, want {0:1, 1:1}", got)
	}
	if got := FreeCoresPerNUMADensity(info, occupied); !reflect.DeepEqual(got, map[int]int{0: 2, 1: 1}) {
		t.Errorf("density rule with the sibling occupied: got %v, want {0:2, 1:1}", got)
	}
	// The representative thread of core0 occupied: the core is busy under either rule.
	if got := FreeCoresPerNUMADensity(info, map[int]bool{0: true}); !reflect.DeepEqual(got, map[int]int{0: 1, 1: 1}) {
		t.Errorf("density rule with the representative occupied: got %v, want {0:1, 1:1}", got)
	}
	// A fully occupied NUMA still appears, as with FreePerNUMA.
	got := FreeCoresPerNUMADensity(info, map[int]bool{3: true})
	if free, ok := got[1]; !ok || free != 0 {
		t.Errorf("fully-occupied NUMA1 must appear with free 0, got %v", got)
	}
	// The core reads busy when an unlisted representative is occupied.
	partial := map[int]CPUInfo{1: {CPU: 1, NUMA: 0, Core: 0}, 2: {CPU: 2, NUMA: 0, Core: 2}}
	if got := FreeCoresPerNUMADensity(partial, map[int]bool{0: true}); !reflect.DeepEqual(got, map[int]int{0: 1}) {
		t.Errorf("partial map, representative occupied: got %v, want {0:1}", got)
	}
}

func TestFreePerNUMAHyperthreads(t *testing.T) {
	info := twoNumaFour()
	// Nothing occupied → NUMA0 has 3 logical CPUs free (0,1,2), NUMA1 has 1 (cpu3).
	if got := FreePerNUMA(info, map[int]bool{}, "hyperthreads"); !reflect.DeepEqual(got, map[int]int{0: 3, 1: 1}) {
		t.Errorf("no occupancy (ht): got %v, want {0:3, 1:1}", got)
	}
	// Occupy cpu0 only — NUMA0 drops to 2 free (cpu1, cpu2), NUMA1 stays 1.
	if got := FreePerNUMA(info, map[int]bool{0: true}, "hyperthreads"); !reflect.DeepEqual(got, map[int]int{0: 2, 1: 1}) {
		t.Errorf("cpu0 occupied (ht): got %v, want {0:2, 1:1}", got)
	}
}

func TestFreePerNUMAFullyOccupiedNUMAStillPresent(t *testing.T) {
	// A fully occupied NUMA still appears with free 0.
	info := twoNumaFour()
	got := FreePerNUMA(info, map[int]bool{3: true}, "physical-cores")
	if _, ok := got[1]; !ok {
		t.Errorf("fully-occupied NUMA1 must appear in the map, got %v", got)
	}
	if got[1] != 0 {
		t.Errorf("NUMA1 free = %d, want 0", got[1])
	}
}

func TestSpanMinFree(t *testing.T) {
	free := map[int]int{0: 5, 1: 2, 2: 8}
	// span 1 → pick the most-free NUMA (id 2, free 8).
	if got, ok := SpanMinFree(free, 1); !ok || got != 8 {
		t.Errorf("span 1: got (%d,%v), want (8,true)", got, ok)
	}
	// span 2 → top-2 are {2:8, 0:5}; least-free of those is 5.
	if got, ok := SpanMinFree(free, 2); !ok || got != 5 {
		t.Errorf("span 2: got (%d,%v), want (5,true)", got, ok)
	}
	// span > NUMA count → clamped; least-free overall is 2.
	if got, ok := SpanMinFree(free, 10); !ok || got != 2 {
		t.Errorf("span 10 (clamp): got (%d,%v), want (2,true)", got, ok)
	}
	// Empty map → ok=false.
	if _, ok := SpanMinFree(map[int]int{}, 1); ok {
		t.Error("empty free must return ok=false")
	}
}

func TestNUMAFreeDescDeterministic(t *testing.T) {
	// Ties broken by NUMA id ascending, so ordering is stable across calls.
	free := map[int]int{2: 4, 0: 4, 1: 4}
	if got := NUMAFreeDesc(free); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Errorf("tie-break: got %v, want [0 1 2]", got)
	}
	// Non-tie: most-free first, descending by value.
	got := NUMAFreeDesc(map[int]int{0: 1, 1: 3, 2: 2})
	if !reflect.DeepEqual(got, []int{1, 2, 0}) {
		t.Errorf("desc: got %v, want [1 2 0]", got)
	}
	// Sanity: the returned ids are exactly the map keys.
	sortedGot := append([]int(nil), got...)
	sort.Ints(sortedGot)
	if !reflect.DeepEqual(sortedGot, []int{0, 1, 2}) {
		t.Errorf("ids: got %v, want all keys", sortedGot)
	}
}

func TestStrandedFreeCores(t *testing.T) {
	// Perfectly balanced: 4 free on each of 2 NUMAs, maxSpan 2 → span-2 balloon
	// absorbs 4×2=8 = total → 0 stranded.
	if got := StrandedFreeCores(map[int]int{0: 4, 1: 4}, 2); got != 0 {
		t.Errorf("balanced: got %d, want 0", got)
	}
	// Imbalanced: 4 free on NUMA0, 0 on NUMA1. A span-2 balloon is bounded by the
	// least-free (0), absorbing 0×2=0 → all 4 free cores are stranded.
	if got := StrandedFreeCores(map[int]int{0: 4, 1: 0}, 2); got != 4 {
		t.Errorf("fully imbalanced: got %d, want 4", got)
	}
	// Partial imbalance: {0:5, 1:2}. span-2 minFree=2 → absorbs 2×2=4, total 7,
	// stranded = 3.
	if got := StrandedFreeCores(map[int]int{0: 5, 1: 2}, 2); got != 3 {
		t.Errorf("partial: got %d, want 3", got)
	}
	// maxSpan 4 over 4 NUMAs {0:3,1:3,2:3,3:1}: minFree of top-4 = 1, absorbs
	// 1×4=4, total 10, stranded = 6.
	if got := StrandedFreeCores(map[int]int{0: 3, 1: 3, 2: 3, 3: 1}, 4); got != 6 {
		t.Errorf("span4: got %d, want 6", got)
	}
	// maxSpan < 2 → single-NUMA node has no cross-NUMA imbalance, always 0.
	if got := StrandedFreeCores(map[int]int{0: 9}, 1); got != 0 {
		t.Errorf("maxSpan 1: got %d, want 0", got)
	}
	// maxSpan clamped to NUMA count: maxSpan 4 but only 2 NUMAs {0:4,1:4} →
	// clamps to span 2, absorbs 4×2=8 = total → 0 (not over-subtracted).
	if got := StrandedFreeCores(map[int]int{0: 4, 1: 4}, 4); got != 0 {
		t.Errorf("clamp maxSpan: got %d, want 0", got)
	}
	// Empty free → 0 (nothing to reason about).
	if got := StrandedFreeCores(map[int]int{}, 2); got != 0 {
		t.Errorf("empty: got %d, want 0", got)
	}
}

func TestStrandedAfterPlacing(t *testing.T) {
	// Node {0:4, 1:4}, max-span 2. Placing a span-2 pod taking 2 per NUMA leaves
	// {0:2, 1:2} → balanced → 0 stranded.
	if got := StrandedAfterPlacing(map[int]int{0: 4, 1: 4}, []int{0, 1}, 2); got != 0 {
		t.Errorf("balanced placement: got %d, want 0", got)
	}
	// Node {0:4, 1:2}. Placing 2 per NUMA on {0,1} leaves {0:2, 1:0} → span-2
	// bounded by 0, total 2 → 2 stranded.
	if got := StrandedAfterPlacing(map[int]int{0: 4, 1: 2}, []int{0, 1}, 2); got != 2 {
		t.Errorf("imbalanced placement: got %d, want 2", got)
	}
	// {0:2, 1:2}: placing 2 per NUMA fills it → 0 stranded.
	if got := StrandedAfterPlacing(map[int]int{0: 2, 1: 2}, []int{0, 1}, 2); got != 0 {
		t.Errorf("fills partially-used node: got %d, want 0", got)
	}
	// {0:6, 1:6}: leaves {0:4, 1:4} → 0 stranded too.
	if got := StrandedAfterPlacing(map[int]int{0: 6, 1: 6}, []int{0, 1}, 2); got != 0 {
		t.Errorf("emptier node: got %d, want 0", got)
	}
	// Clamp: a share larger than a NUMA's free never drives free negative.
	if got := StrandedAfterPlacing(map[int]int{0: 1, 1: 1}, []int{0, 1}, 5); got != 0 {
		t.Errorf("over-large share must clamp: got %d, want 0", got)
	}
}

// TestBinpackDoesNotFragmentMoreThanSpread replays the webhook's spread and binpack
// rankings and proves binpack never ends more fragmented.
func TestBinpackDoesNotFragmentMoreThanSpread(t *testing.T) {
	// Each node starts with 4 free cores per NUMA (2 NUMAs, max-span 2).
	newFleet := func() map[string]map[int]int {
		return map[string]map[int]int{
			"n1": {0: 4, 1: 4},
			"n2": {0: 4, 1: 4},
		}
	}
	// place subtracts one core from both NUMAs of the chosen node.
	place := func(free map[int]int) {
		free[0]--
		free[1]--
	}
	// A sequence of six span-2 pods, each taking 1 core per NUMA.
	const pods, share, span = 6, 1, 2

	names := []string{"n1", "n2"}

	// spread: most free on the tightest NUMA, then most total free, then name.
	spread := newFleet()
	for i := 0; i < pods; i++ {
		best := ""
		for _, name := range names {
			if best == "" {
				best = name
				continue
			}
			mf, _ := SpanMinFree(spread[name], span)
			bmf, _ := SpanMinFree(spread[best], span)
			switch {
			case mf != bmf:
				if mf > bmf {
					best = name
				}
			case total(spread[name]) != total(spread[best]):
				if total(spread[name]) > total(spread[best]) {
					best = name
				}
			case name < best:
				best = name
			}
		}
		place(spread[best])
	}

	// binpack: least stranded after placing, then least total free, then name.
	binpack := newFleet()
	for i := 0; i < pods; i++ {
		best := ""
		for _, name := range names {
			if best == "" {
				best = name
				continue
			}
			s, bs := StrandedAfterPlacing(binpack[name], []int{0, 1}, share), StrandedAfterPlacing(binpack[best], []int{0, 1}, share)
			switch {
			case s != bs:
				if s < bs {
					best = name
				}
			case total(binpack[name]) != total(binpack[best]):
				if total(binpack[name]) < total(binpack[best]) {
					best = name
				}
			case name < best:
				best = name
			}
		}
		place(binpack[best])
	}

	fragOf := func(fleet map[string]map[int]int) int {
		sum := 0
		for _, free := range fleet {
			sum += StrandedFreeCores(free, MaxSpanForNUMA(len(free)))
		}
		return sum
	}
	spreadFrag, binpackFrag := fragOf(spread), fragOf(binpack)
	if binpackFrag > spreadFrag {
		t.Errorf("binpack fragmented MORE than spread: binpack=%d spread=%d (fleets: binpack=%v spread=%v)",
			binpackFrag, spreadFrag, binpack, spread)
	}
}

// total sums a per-NUMA free map (test helper).
func total(free map[int]int) int {
	sum := 0
	for _, n := range free {
		sum += n
	}
	return sum
}

func TestMaxSpanForNUMA(t *testing.T) {
	cases := map[int]int{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 7: 4, 8: 8, 16: 8}
	for numa, want := range cases {
		if got := MaxSpanForNUMA(numa); got != want {
			t.Errorf("MaxSpanForNUMA(%d) = %d, want %d", numa, got, want)
		}
	}
}

// TestStrandedFreeCoresSaturates proves saturated free values never wrap to zero
// stranded, which binpack would prefer.
func TestStrandedFreeCoresSaturates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		free    map[int]int
		maxSpan int
	}{
		{"two saturated NUMAs, two empty", map[int]int{0: math.MaxInt, 1: math.MaxInt, 2: 0, 3: 0}, 4},
		{"one saturated NUMA, span 2", map[int]int{0: math.MaxInt, 1: 4}, 2},
		{"three saturated, one small", map[int]int{0: math.MaxInt, 1: math.MaxInt, 2: math.MaxInt, 3: 8}, 4},
		{
			"eight NUMAs, four saturated and four exhausted",
			map[int]int{0: math.MaxInt, 1: math.MaxInt, 2: math.MaxInt, 3: math.MaxInt, 4: 0, 5: 0, 6: 0, 7: 0},
			8,
		},
		// Both aggregates saturate here.
		{"saturated against half-saturated", map[int]int{0: math.MaxInt, 1: math.MaxInt/2 + 1}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := StrandedFreeCores(tc.free, tc.maxSpan)
			if got < 0 {
				t.Fatalf("StrandedFreeCores = %d, negative", got)
			}
			// Saturated imbalance must score large, not zero.
			if got == 0 {
				t.Errorf("StrandedFreeCores = 0 for %v, which reads as unfragmented; "+
					"a wrapped sum clamps to exactly this and makes binpack prefer the node", tc.free)
			}
		})
	}
}

// TestStrandedFreeCoresKeepsImbalanceMagnitude pins the score's size under saturation.
func TestStrandedFreeCoresKeepsImbalanceMagnitude(t *testing.T) {
	// NUMA 1 bounds a span-2 placement, so NUMA 0's excess over it is stranded.
	free := map[int]int{0: math.MaxInt, 1: math.MaxInt/2 + 1}
	if want, got := math.MaxInt-(math.MaxInt/2+1), StrandedFreeCores(free, 2); got != want {
		t.Errorf("StrandedFreeCores(%v) = %d, want the imbalance %d", free, got, want)
	}
	// The imbalance is outside the span: the whole third NUMA is stranded.
	free = map[int]int{0: math.MaxInt, 1: math.MaxInt, 2: math.MaxInt / 2}
	if want, got := math.MaxInt/2, StrandedFreeCores(free, 2); got != want {
		t.Errorf("StrandedFreeCores(%v) = %d, want the unselected NUMA's %d", free, got, want)
	}
}

// TestStrandedAfterPlacingSaturates covers the same through StrandedAfterPlacing.
func TestStrandedAfterPlacingSaturates(t *testing.T) {
	free := map[int]int{
		0: math.MaxInt, 1: math.MaxInt, 2: math.MaxInt, 3: math.MaxInt,
		4: 0, 5: 0, 6: 0, 7: 0,
	}
	if got := StrandedAfterPlacing(free, []int{0, 1}, 1); got <= 0 {
		t.Errorf("StrandedAfterPlacing = %d for a half-exhausted node; binpack picks the "+
			"LEAST stranded candidate, so this would make it prefer the most fragmented one", got)
	}
}

// TestStrandedFreeCoresOrdinary proves ordinary values score total minus span-wide room.
func TestStrandedFreeCoresOrdinary(t *testing.T) {
	// 4 NUMAs, free 8/8/2/2, span 4 → min over the 4 most-free is 2, so a span-4
	// workload can use 2 per NUMA = 8, leaving 20-8 = 12 stranded.
	if got, want := StrandedFreeCores(map[int]int{0: 8, 1: 8, 2: 2, 3: 2}, 4), 12; got != want {
		t.Errorf("StrandedFreeCores = %d, want %d", got, want)
	}
	// Perfectly even free room strands nothing.
	if got := StrandedFreeCores(map[int]int{0: 4, 1: 4}, 2); got != 0 {
		t.Errorf("StrandedFreeCores on an even node = %d, want 0", got)
	}
	// Uniformly saturated is still even: nothing stranded.
	if got := StrandedFreeCores(map[int]int{0: math.MaxInt, 1: math.MaxInt}, 2); got != 0 {
		t.Errorf("StrandedFreeCores on a uniformly saturated node = %d, want 0", got)
	}
}
