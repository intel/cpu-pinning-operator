// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"reflect"
	"sort"
	"testing"
)

// Topology helpers are tested in internal/topology; only reservation helpers here.

func TestSubtractReservations(t *testing.T) {
	free := map[int]int{0: 4, 1: 2}

	if got := subtractReservations(free, nil); !reflect.DeepEqual(got, free) {
		t.Errorf("nil reservations: got %v, want %v", got, free)
	}

	if got := subtractReservations(free, map[int]int{0: 1, 1: 1}); !reflect.DeepEqual(got, map[int]int{0: 3, 1: 1}) {
		t.Errorf("subtract: got %v, want {0:3, 1:1}", got)
	}
	// Clamp at zero.
	if got := subtractReservations(free, map[int]int{0: 10, 1: 100}); !reflect.DeepEqual(got, map[int]int{0: 0, 1: 0}) {
		t.Errorf("clamp: got %v, want {0:0, 1:0}", got)
	}
	// A reservation for an unknown NUMA is ignored.
	if got := subtractReservations(map[int]int{0: 2}, map[int]int{99: 5}); !reflect.DeepEqual(got, map[int]int{0: 2}) {
		t.Errorf("unknown numa: got %v, want {0:2}", got)
	}
}

func TestTopSpanNUMAsDeterministic(t *testing.T) {
	// Ties break by NUMA id ascending, so concurrent reservations pick the same order.
	free := map[int]int{2: 4, 0: 4, 1: 4}
	got := topSpanNUMAs(free, 2)

	if !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("tie-break: got %v, want [0 1]", got)
	}

	got = topSpanNUMAs(map[int]int{0: 1, 1: 3, 2: 2}, 2)
	sortedGot := append([]int(nil), got...)
	sort.Ints(sortedGot)
	if !reflect.DeepEqual(got, []int{1, 2}) || !reflect.DeepEqual(sortedGot, []int{1, 2}) {
		t.Errorf("desc: got %v, want [1 2]", got)
	}
}

// TestTopSpanNUMAsNoDataReturnsNil: an empty free map returns nil instead of panicking.
func TestTopSpanNUMAsNoDataReturnsNil(t *testing.T) {
	for _, span := range []int{-1, 0, 1, 2, 8} {
		if got := topSpanNUMAs(nil, span); len(got) != 0 {
			t.Errorf("topSpanNUMAs(nil, %d) = %v, want empty", span, got)
		}
		if got := topSpanNUMAs(map[int]int{}, span); len(got) != 0 {
			t.Errorf("topSpanNUMAs(empty, %d) = %v, want empty", span, got)
		}
	}
	// A span below 1 still returns one NUMA when data exists.
	if got := topSpanNUMAs(map[int]int{3: 7}, 0); !reflect.DeepEqual(got, []int{3}) {
		t.Errorf("topSpanNUMAs({3:7}, 0) = %v, want [3]", got)
	}
}
