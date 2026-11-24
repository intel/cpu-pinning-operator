// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestAuthoritativeReserveAcceptsThenRejects: one store accepts the last headroom once and
// rejects the identical second request.
func TestAuthoritativeReserveAcceptsThenRejects(t *testing.T) {
	s := NewReservationStore(0)
	// 2 free cores on NUMA 0 and 1; a span-2 pod at 2 cores/NUMA fills both.
	req := ReserveRequest{
		Node:         "n1",
		PerNUMACores: 2,
		Span:         2,
		RawFree:      map[int]int{0: 2, 1: 2},
	}
	if _, _, ok := s.authoritativeReserveBest("", []ReserveRequest{req}); !ok {
		t.Fatal("first reserve must be accepted (2 <= 2 on both NUMAs)")
	}

	if _, _, ok := s.authoritativeReserveBest("", []ReserveRequest{req}); ok {
		t.Fatal("second reserve for the same headroom must be REJECTED (single authoritative store)")
	}
}

// TestAuthoritativeReserveMemoryRejects: CPU fits but the second memory share does not.
func TestAuthoritativeReserveMemoryRejects(t *testing.T) {
	s := NewReservationStore(0)

	mk := func() ReserveRequest {
		return ReserveRequest{
			Node:         "n1",
			PerNUMACores: 1,
			PerNUMAMem:   10 * gib,
			Span:         1,
			MemBytes:     10 * gib,
			RawFree:      map[int]int{0: 10},
			MemChecked:   true,
			MemFree:      map[int]int64{0: 10 * gib},
		}
	}
	if _, _, ok := s.authoritativeReserveBest("", []ReserveRequest{mk()}); !ok {
		t.Fatal("first memory reserve must be accepted (10 GiB <= 10 GiB)")
	}
	if _, _, ok := s.authoritativeReserveBest("", []ReserveRequest{mk()}); ok {
		t.Fatal("second memory reserve must be REJECTED — the first claimed NUMA 0's memory")
	}
}

// TestAuthoritativeReserveSkipsMemoryWhenUnchecked: with MemChecked=false the authority
// skips the memory re-check; the demand exceeds the node so a re-check would reject.
func TestAuthoritativeReserveSkipsMemoryWhenUnchecked(t *testing.T) {
	s := NewReservationStore(0)
	const demand = 100 * gib
	req := ReserveRequest{
		Node:         "n1",
		PerNUMACores: 1,
		Span:         1,
		PerNUMAMem:   perNUMAMemShare(demand, 1),
		MemBytes:     demand,
		RawFree:      map[int]int{0: 4},
		MemChecked:   false,
	}
	if _, _, ok := s.authoritativeReserveBest("", []ReserveRequest{req}); !ok {
		t.Fatal("reserve must be accepted when the memory dimension is unchecked (don't block on absent data)")
	}
}

// TestAuthoritativeReserveRecordsUncheckedMemory: an unchecked request with a share (the
// normal shape when live memory could not be read) still records that share.
func TestAuthoritativeReserveRecordsUncheckedMemory(t *testing.T) {
	s := NewReservationStore(0)
	const demand = 100 * gib
	req := ReserveRequest{
		Node:         "n1",
		PerNUMACores: 1,
		PerNUMAMem:   perNUMAMemShare(demand, 1),
		Span:         1,
		MemBytes:     demand,
		RawFree:      map[int]int{0: 4},
		MemChecked:   false,
	}
	if _, _, ok := s.authoritativeReserveBest("", []ReserveRequest{req}); !ok {
		t.Fatal("reserve must be accepted when the memory dimension is unchecked")
	}
	if _, mem := s.reservedBothLocked("n1"); mem[0] != demand {
		t.Errorf("held memory on NUMA 0 = %d, want the shipped share %d", mem[0], demand)
	}
}

// TestAuthoritativeReserveBestFallsThroughContended: a contended top candidate falls
// through to the next one that fits, not a reject.
func TestAuthoritativeReserveBestFallsThroughContended(t *testing.T) {
	s := NewReservationStore(0)

	if _, _, ok := s.authoritativeReserveBest("", []ReserveRequest{{Node: "a", PerNUMACores: 2, Span: 2, RawFree: map[int]int{0: 2, 1: 2}}}); !ok {
		t.Fatal("setup: reserving all of node a must succeed")
	}

	reqs := []ReserveRequest{
		{Node: "a", PerNUMACores: 2, Span: 2, RawFree: map[int]int{0: 2, 1: 2}},
		{Node: "b", PerNUMACores: 2, Span: 2, RawFree: map[int]int{0: 2, 1: 2}},
	}
	node, _, accepted := s.authoritativeReserveBest("", reqs)
	if !accepted || node != "b" {
		t.Fatalf("first-fit = (%q, %v), want (b, true) — must fall through the contended top choice to b", node, accepted)
	}

	if r := reservedCores(s, "b"); r[0] != 2 || r[1] != 2 {
		t.Fatalf("node b reserved = %v, want 2 on each NUMA", r)
	}
}

// TestAuthoritativeReserveBestRejectsWhenAllContended: reject only when every candidate is contended.
func TestAuthoritativeReserveBestRejectsWhenAllContended(t *testing.T) {
	s := NewReservationStore(0)
	for _, n := range []string{"a", "b"} {
		if _, _, ok := s.authoritativeReserveBest("", []ReserveRequest{{Node: n, PerNUMACores: 2, Span: 1, RawFree: map[int]int{0: 2}}}); !ok {
			t.Fatalf("setup: reserving all of node %s must succeed", n)
		}
	}
	reqs := []ReserveRequest{
		{Node: "a", PerNUMACores: 2, Span: 1, RawFree: map[int]int{0: 2}},
		{Node: "b", PerNUMACores: 2, Span: 1, RawFree: map[int]int{0: 2}},
	}
	if node, _, accepted := s.authoritativeReserveBest("", reqs); accepted || node != "" {
		t.Fatalf("first-fit = (%q, %v), want (\"\", false) — every candidate contended", node, accepted)
	}
}

// TestAuthoritativeReserveConcurrentNoDoubleReserve: racing reserves against one store
// accept exactly the capacity, never more.
func TestAuthoritativeReserveConcurrentNoDoubleReserve(t *testing.T) {
	s := NewReservationStore(0)
	// One NUMA with 8 free cores, 1 core per pod: exactly 8 accepted.
	const capacity = 8
	const goroutines = 64
	var accepted int32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			_, _, ok := s.authoritativeReserveBest("", []ReserveRequest{{
				Node:         "n1",
				PerNUMACores: 1,
				Span:         1,
				RawFree:      map[int]int{0: capacity},
			}})
			if ok {
				atomic.AddInt32(&accepted, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&accepted); got != capacity {
		t.Fatalf("accepted = %d, want exactly %d — the authoritative store must not double-reserve a node's headroom", got, capacity)
	}

	if r := reservedCores(s, "n1"); r[0] != capacity {
		t.Fatalf("reserved on NUMA 0 = %d, want %d", r[0], capacity)
	}
}

// TestAuthoritativeReserveRejectsSpanPastNUMAs: a candidate with fewer NUMAs than its span
// is skipped; clamping the span would record only part of the pod's memory and CPUs.
func TestAuthoritativeReserveRejectsSpanPastNUMAs(t *testing.T) {
	req := ReserveRequest{
		Node: "n1", PerNUMACores: 1, Span: 2, RawFree: map[int]int{0: 8},
		MemBytes: 8, PerNUMAMem: 4, MemChecked: true, MemFree: map[int]int64{0: 8},
	}
	if node, numas, ok := NewReservationStore(0).authoritativeReserveBest("", []ReserveRequest{req}); ok {
		t.Errorf("accepted a span-2 request on a 1-NUMA node: %s%v", node, numas)
	}
}

// TestAuthoritativeReserveRejectsMemoryWithoutShare: MemBytes without PerNUMAMem is
// rejected (it would record zero memory); CPU-only requests are still accepted.
func TestAuthoritativeReserveRejectsMemoryWithoutShare(t *testing.T) {
	base := func() ReserveRequest {
		return ReserveRequest{
			Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4},
		}
	}
	memOnly := base()
	memOnly.MemBytes = 8 * gib // no PerNUMAMem
	if _, _, ok := NewReservationStore(0).authoritativeReserveBest("", []ReserveRequest{memOnly}); ok {
		t.Error("accepted a request declaring MemBytes with no per-NUMA share; it would record zero memory")
	}
	if _, _, ok := NewReservationStore(0).authoritativeReserveBest("", []ReserveRequest{base()}); !ok {
		t.Error("rejected a CPU-only request, which carries no memory demand at all")
	}
}
