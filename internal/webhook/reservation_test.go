// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a monotonic, race-safe clock for TTL tests.
type fakeClock struct{ now atomic.Int64 } // stores UnixNano

func (c *fakeClock) At(t time.Time) *fakeClock { c.now.Store(t.UnixNano()); return c }
func (c *fakeClock) Now() time.Time            { return time.Unix(0, c.now.Load()) }
func (c *fakeClock) Advance(d time.Duration)   { c.now.Add(int64(d)) }

func TestReservationStoreAggregatesAndPrunes(t *testing.T) {
	clk := (&fakeClock{}).At(time.Unix(1_000_000, 0))
	s := NewReservationStore(0)
	s.now = clk.Now

	reserve(s, "n1", []int{0, 1}, 4, 10*gib)
	reserve(s, "n1", []int{0}, 2, 5*gib)
	reserve(s, "n2", []int{1}, 1, 0) // CPU-only reservation (memBytes 0)

	if got := reservedCores(s, "n1"); !reflect.DeepEqual(got, map[int]int{0: 6, 1: 4}) {
		t.Errorf("n1 reserved cores = %v, want {0:6, 1:4}", got)
	}

	if got := reservedMem(s, "n1"); !reflect.DeepEqual(got, map[int]int64{0: 15 * gib, 1: 10 * gib}) {
		t.Errorf("n1 reserved mem = %v, want {0:15Gi, 1:10Gi}", got)
	}
	// n2's CPU-only reservation contributes no memory.
	if got := reservedCores(s, "n2"); !reflect.DeepEqual(got, map[int]int{1: 1}) {
		t.Errorf("n2 reserved cores = %v, want {1:1}", got)
	}
	if got := reservedMem(s, "n2"); len(got) != 0 {
		t.Errorf("n2 reserved mem = %v, want empty (CPU-only reservation)", got)
	}

	clk.Advance(DefaultReservationTTL - time.Second)
	if got := reservedCores(s, "n1"); got[0] != 6 {
		t.Errorf("pre-TTL: n1 NUMA0 = %d, want 6", got[0])
	}

	clk.Advance(2 * time.Second)
	if got := reservedCores(s, "n1"); len(got) != 0 {
		t.Errorf("post-TTL: n1 reserved = %v, want empty", got)
	}
	if got := reservedCores(s, "n2"); len(got) != 0 {
		t.Errorf("post-TTL: n2 reserved = %v, want empty", got)
	}
}

// TestReservationStoreConfigurableTTL: a custom TTL is honored, and a non-positive TTL
// falls back to the default instead of expiring reservations immediately.
func TestReservationStoreConfigurableTTL(t *testing.T) {
	t.Run("custom TTL is honored", func(t *testing.T) {
		const customTTL = 5 * time.Minute
		clk := (&fakeClock{}).At(time.Unix(2_000_000, 0))
		s := NewReservationStore(customTTL)
		s.now = clk.Now

		reserve(s, "n1", []int{0}, 2, 0)

		clk.Advance(DefaultReservationTTL + time.Second)
		if got := reservedCores(s, "n1"); got[0] != 2 {
			t.Errorf("within custom TTL: n1 NUMA0 = %d, want 2 (reservation must outlive the default TTL)", got[0])
		}

		clk.Advance(customTTL)
		if got := reservedCores(s, "n1"); len(got) != 0 {
			t.Errorf("past custom TTL: n1 reserved = %v, want empty", got)
		}
	})

	t.Run("non-positive TTL falls back to the default", func(t *testing.T) {
		for _, ttl := range []time.Duration{0, -time.Second} {
			clk := (&fakeClock{}).At(time.Unix(3_000_000, 0))
			s := NewReservationStore(ttl)
			s.now = clk.Now

			reserve(s, "n1", []int{0}, 1, 0)

			clk.Advance(DefaultReservationTTL - time.Second)
			if got := reservedCores(s, "n1"); got[0] != 1 {
				t.Errorf("ttl=%v: reservation expired before the default TTL (NUMA0 = %d, want 1)", ttl, got[0])
			}
			clk.Advance(2 * time.Second)
			if got := reservedCores(s, "n1"); len(got) != 0 {
				t.Errorf("ttl=%v: reservation not pruned past the default TTL: %v", ttl, got)
			}
		}
	})
}

// reserve takes the store lock and calls reserveLocked; production already holds the lock.
func reserve(s *ReservationStore, node string, numas []int, cores int, memBytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserveLocked(node, numas, cores, memBytes)
}

// reservedCores is the cores half of reservedBothLocked, under the store's lock.
func reservedCores(s *ReservationStore, node string) map[int]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cores, _ := s.reservedBothLocked(node)
	return cores
}

// reservedMem is the memory half of reservedBothLocked, under the store's lock.
func reservedMem(s *ReservationStore, node string) map[int]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, mem := s.reservedBothLocked(node)
	return mem
}

func TestReservationStoreNoOpOnEmptyInputs(t *testing.T) {
	s := NewReservationStore(0)
	// Empty numas and non-positive cores add no entries.
	reserve(s, "n1", nil, 4, 0)
	reserve(s, "n1", []int{}, 4, 8<<30)
	reserve(s, "n1", []int{0}, 0, 8<<30) // cores<=0 guards out even with memBytes
	reserve(s, "n1", []int{0}, -1, 0)

	reserve(s, "n1", []int{0}, 1, -1*gib)
	if got := reservedCores(s, "n1"); len(got) != 0 {
		t.Errorf("empty inputs leaked: got %v, want empty", got)
	}
	if got := reservedMem(s, "n1"); len(got) != 0 {
		t.Errorf("empty inputs leaked mem: got %v, want empty", got)
	}
}

// TestReservationStoreConcurrent checks for lost updates and races under go test -race.
func TestReservationStoreConcurrent(t *testing.T) {
	s := NewReservationStore(0)
	const goroutines = 32
	const perG = 25
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				reserve(s, "n1", []int{i % 2}, 1, 1<<30)
				_ = reservedCores(s, "n1") // interleave reads
			}
		}()
	}
	wg.Wait()

	got := reservedCores(s, "n1")
	total := got[0] + got[1]
	want := goroutines * perG
	if total != want {
		t.Errorf("concurrent total = %d, want %d (got per-NUMA %v)", total, want, got)
	}
}
