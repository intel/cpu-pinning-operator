// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"math"
	"reflect"
	"testing"
)

const gib = int64(1) << 30

func TestPerNUMAMemShare(t *testing.T) {
	cases := []struct {
		bytes int64
		span  int
		want  int64
	}{
		{0, 1, 0},
		{100 * gib, 1, 100 * gib}, // span 1 → full request
		{100 * gib, 2, 50 * gib},  // even split
		{300 * gib, 2, 150 * gib}, // the vllm TP=2 example
		{100 * gib, 0, 100 * gib}, // span clamps to 1, never divide by zero
		{100 * gib, -4, 100 * gib},
		// A remainder rounds the per-NUMA share up.
		{5, 2, 3},    // 5/2 = 2.5 → 3
		{7, 3, 3},    // 7/3 = 2.33 → 3
		{100, 3, 34}, // 100/3 = 33.3 → 34
		{100, 4, 25}, // exact
		// MaxInt64 must not overflow: divide first.
		{math.MaxInt64, 2, math.MaxInt64/2 + 1}, // odd → ceil bumps
		{math.MaxInt64, 1, math.MaxInt64},       // span 1 → unchanged, no wrap
	}
	for _, c := range cases {
		if got := perNUMAMemShare(c.bytes, c.span); got != c.want {
			t.Errorf("perNUMAMemShare(%d, %d) = %d, want %d", c.bytes, c.span, got, c.want)
		}
	}
}

func TestFreeMemPerNUMA(t *testing.T) {
	capacity := map[int]int64{0: 250 * gib, 1: 260 * gib}

	got := freeMemPerNUMA(capacity, map[int]int64{0: 100 * gib})
	if !reflect.DeepEqual(got, map[int]int64{0: 150 * gib, 1: 260 * gib}) {
		t.Errorf("freeMemPerNUMA = %v, want {0:150Gi, 1:260Gi}", got)
	}
	// Over-occupancy clamps to 0.
	got = freeMemPerNUMA(map[int]int64{0: 10 * gib}, map[int]int64{0: 99 * gib})
	if got[0] != 0 {
		t.Errorf("over-occupancy NUMA0 free = %d, want 0", got[0])
	}
	// A fully occupied NUMA still appears, with free 0.
	got = freeMemPerNUMA(map[int]int64{0: 5 * gib, 1: 5 * gib}, map[int]int64{0: 5 * gib})
	if _, ok := got[0]; !ok || got[0] != 0 {
		t.Errorf("fully-occupied NUMA0 must appear as 0, got %v", got)
	}

	got = freeMemPerNUMA(map[int]int64{0: 5 * gib}, map[int]int64{9: 100 * gib})
	if !reflect.DeepEqual(got, map[int]int64{0: 5 * gib}) {
		t.Errorf("unknown-NUMA occupancy leaked: got %v, want {0:5Gi}", got)
	}
}

// TestSubtractMemReservations: the byte-valued twin of subtractReservations.
func TestSubtractMemReservations(t *testing.T) {
	free := map[int]int64{0: 100 * gib, 1: 40 * gib}
	// Nothing reserved returns the same map, not a copy; compare map pointers via reflect.
	got := subtractMemReservations(free, nil)
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(free).Pointer() {
		t.Errorf("empty reservations returned a copy, want the same map passed through")
	}

	got = subtractMemReservations(free, map[int]int64{0: 60 * gib})
	if !reflect.DeepEqual(got, map[int]int64{0: 40 * gib, 1: 40 * gib}) {
		t.Errorf("subtractMemReservations = %v, want {0:40Gi, 1:40Gi}", got)
	}

	got = subtractMemReservations(free, map[int]int64{1: 99 * gib})
	if got[1] != 0 {
		t.Errorf("over-reserved NUMA1 free = %d, want 0", got[1])
	}
	// The input map must not be mutated.
	if free[0] != 100*gib || free[1] != 40*gib {
		t.Errorf("input free mutated: %v", free)
	}
}

func TestMemSpanMinFree(t *testing.T) {
	free := map[int]int64{0: 5 * gib, 1: 2 * gib, 2: 8 * gib}

	if got, ok := memSpanMinFree(free, 1); !ok || got != 8*gib {
		t.Errorf("span1 => (%d,%v), want (8Gi,true)", got, ok)
	}

	if got, ok := memSpanMinFree(free, 2); !ok || got != 5*gib {
		t.Errorf("span2 => (%d,%v), want (5Gi,true)", got, ok)
	}
	// span > NUMA count is clamped.
	if got, ok := memSpanMinFree(free, 10); !ok || got != 2*gib {
		t.Errorf("span10 clamp => (%d,%v), want (2Gi,true)", got, ok)
	}

	if _, ok := memSpanMinFree(map[int]int64{}, 1); ok {
		t.Error("empty free must return ok=false")
	}
}

func TestMemoryLiveFits(t *testing.T) {
	free := map[int]int64{0: 150 * gib, 1: 150 * gib}
	// span 2, 300Gi: 150Gi per NUMA fits.
	if fits, mf, ok := memoryLiveFits(free, 300*gib, 2); !ok || !fits || mf != 150*gib {
		t.Errorf("span2 300Gi => (fits=%v,minFree=%d,ok=%v), want (true,150Gi,true)", fits, mf, ok)
	}

	if fits, _, ok := memoryLiveFits(free, 301*gib, 2); !ok || fits {
		t.Errorf("span2 301Gi should not fit (share > 150Gi free)")
	}

	if fits, _, ok := memoryLiveFits(free, 300*gib, 1); !ok || fits {
		t.Errorf("span1 300Gi should not fit (full request > 150Gi free)")
	}
	// No per-NUMA data: ok=false, the memory dimension is skipped.
	if _, _, ok := memoryLiveFits(map[int]int64{}, 1*gib, 1); ok {
		t.Error("empty free must signal ok=false")
	}
}
