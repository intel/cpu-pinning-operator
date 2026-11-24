// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"math"
	"slices"
	"sync"
	"time"

	"github.com/intel/cpu-pinning-operator/internal/metrics"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// DefaultReservationTTL must outlive NRT publish + informer lag or the admission race
// reopens; bounded so a never-scheduled pod cannot hold headroom forever. Overlap with
// NRT double-subtracts on purpose.
const DefaultReservationTTL = 90 * time.Second

// reservation is a pod steered to a node before NRT reflects the pin. cores and memBytes
// are per-NUMA shares; the NUMA ids are a prediction.
type reservation struct {
	numas    []int
	cores    int   // per-NUMA CPU share
	memBytes int64 // per-NUMA memory share (0 for CPU-only pods)
	expires  time.Time
}

// ReservationStore is per-process. In HA the lease holder is the authority; a follower
// writes locally only under capacityCheck.mode warn/off.
type ReservationStore struct {
	mu   sync.Mutex
	byND map[string][]reservation // node name -> live reservations
	now  func() time.Time         // injectable clock for tests
	ttl  time.Duration            // how long a reservation lives; see DefaultReservationTTL
	// committed maps an idempotency key to its reservation; entries expire with it.
	committed map[string]committedReserve
	// committedOrder is insertion order, which is expiry order under a monotonic clock.
	committedOrder []string
}

// committedReserve is the answer given for one idempotency key. Only accepts are recorded,
// so a rejection is re-evaluated on retry.
type committedReserve struct {
	node    string
	numas   []int
	expires time.Time
}

// NewReservationStore returns an empty store; ttl <= 0 means DefaultReservationTTL.
func NewReservationStore(ttl time.Duration) *ReservationStore {
	if ttl <= 0 {
		ttl = DefaultReservationTTL
	}
	return &ReservationStore{
		byND:      map[string][]reservation{},
		committed: map[string]committedReserve{},
		now:       time.Now,
		ttl:       ttl,
	}
}

// pruneLocked drops expired reservations and returns the live set. Caller holds s.mu.
func (s *ReservationStore) pruneLocked(node string) []reservation {
	now := s.now()
	before := len(s.byND[node])
	live := s.byND[node][:0]
	for _, r := range s.byND[node] {
		if now.Before(r.expires) {
			live = append(live, r)
		}
	}
	if len(live) == 0 {
		delete(s.byND, node)
	} else {
		s.byND[node] = live
	}
	// Only on change: re-Setting unchanged gauges adds fleet-sized work under this lock.
	if len(live) != before {
		metrics.ReservationsLive.WithLabelValues(node).Set(float64(len(live)))
	}
	return live
}

// republishLive publishes even when unchanged, so warm-up writes an explicit 0. Takes s.mu.
func (s *ReservationStore) republishLive(node string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	metrics.ReservationsLive.WithLabelValues(node).Set(float64(len(s.pruneLocked(node))))
}

func (s *ReservationStore) reservedBothLocked(node string) (cores map[int]int, mem map[int]int64) {
	cores = map[int]int{}
	mem = map[int]int64{}
	for _, r := range s.pruneLocked(node) {
		for _, numa := range r.numas {
			// Saturate: a wrapped sum would go negative and read as extra headroom.
			if cores[numa] > math.MaxInt-r.cores {
				cores[numa] = math.MaxInt
			} else {
				cores[numa] += r.cores
			}
			if r.memBytes != 0 {
				if mem[numa] > math.MaxInt64-r.memBytes {
					mem[numa] = math.MaxInt64
				} else {
					mem[numa] += r.memBytes
				}
			}
		}
	}
	return cores, mem
}

// ReserveRequest is one candidate node's raw capacity and the pod's demand. RawFree and
// MemFree cover every NUMA of the node and are never reservation-adjusted: the authority
// picks the NUMA set itself from them minus its own ledger (placeOnNode), so a follower's
// local store cannot steer the pick.
type ReserveRequest struct {
	Node         string `json:"node"`
	PerNUMACores int    `json:"perNumaCores"`
	PerNUMAMem   int64  `json:"perNumaMem"`
	Span         int    `json:"span"`
	MemBytes     int64  `json:"memBytes"`
	// RawFree is the per-NUMA CPU free room, never reservation-adjusted.
	RawFree map[int]int `json:"rawFree"`
	// MemChecked is true when the follower resolved per-NUMA free memory.
	MemChecked bool          `json:"memChecked"`
	MemFree    map[int]int64 `json:"memFree,omitempty"`
	// Packing ranks the fitting candidates; every candidate of one admission carries the same.
	Packing string `json:"packing,omitempty"`
}

// nodePlacement is one candidate node scored against a ledger.
type nodePlacement struct {
	// numas is the span most-free NUMAs net of reservations.
	numas     []int
	minFree   int
	totalFree int
	stranded  int
	cpuFits   bool
	// memFits is true when memory is unchecked; memMin is the least free memory on numas.
	memFits bool
	memMin  int64
}

// placeOnNode is the one NUMA selection the local admission paths and the authority share:
// the span most-free NUMAs of rawFree minus reserved, and whether the pod's per-NUMA CPU and
// (when memFree is non-nil) memory shares fit them. ok is false without per-NUMA data.
func placeOnNode(rawFree, reserved map[int]int, memFree, reservedMem map[int]int64, perNUMACores, span int, memBytes int64, packing string) (p nodePlacement, ok bool) {
	free := subtractReservations(rawFree, reserved)
	minFree, ok := spanMinFree(free, span)
	if !ok {
		return nodePlacement{}, false
	}
	p = nodePlacement{numas: topSpanNUMAs(free, span), minFree: minFree, totalFree: topology.SumFree(free), memFits: true}
	p.cpuFits = perNUMACores <= minFree
	if packing == packingBinpack {
		p.stranded = topology.StrandedAfterPlacing(free, p.numas, perNUMACores)
	}
	if memFree != nil && memBytes > 0 {
		mf := subtractMemReservations(memFree, reservedMem)
		p.memFits = memFitsOnNUMAs(mf, p.numas, memBytes, span)
		p.memMin = minMemOnNUMAs(mf, p.numas)
	}
	return p, true
}

// rankedBefore orders fitting candidates: binpack leaves the least stranded room, spread
// the most per-NUMA room; ties go by name.
func rankedBefore(packing string, a, b nodePlacement, aName, bName string) bool {
	if packing == packingBinpack {
		if a.stranded != b.stranded {
			return a.stranded < b.stranded
		}
		if a.totalFree != b.totalFree {
			return a.totalFree < b.totalFree
		}
		return aName < bName
	}
	if a.minFree != b.minFree {
		return a.minFree > b.minFree
	}
	if a.totalFree != b.totalFree {
		return a.totalFree > b.totalFree
	}
	return aName < bName
}

// authoritativeReserveBest places every valid candidate against this store, ranks the
// fitting ones and records the best, holding s.mu so place, rank and record are atomic. A
// repeated non-empty key replays the committed node and NUMAs instead of booking again.
func (s *ReservationStore) authoritativeReserveBest(key string, reqs []ReserveRequest) (node string, numas []int, accepted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key != "" {
		s.expireCommittedLocked()
		// Replay unconditionally; re-committing would book a second reservation.
		if c, ok := s.committed[key]; ok {
			return c.node, c.numas, true
		}
	}
	var (
		req       ReserveRequest
		bestPlace nodePlacement
		found     bool
	)
	for _, r := range reqs {
		p, ok := s.placeLocked(r)
		if !ok {
			continue
		}
		if !found || rankedBefore(r.Packing, p, bestPlace, r.Node, req.Node) {
			req, bestPlace, found = r, p, true
		}
	}
	if !found {
		return "", nil, false
	}
	s.reserveLocked(req.Node, bestPlace.numas, req.PerNUMACores, req.PerNUMAMem)
	if key != "" {
		s.committed[key] = committedReserve{node: req.Node, numas: bestPlace.numas, expires: s.now().Add(s.ttl)}
		s.committedOrder = append(s.committedOrder, key)
	}
	return req.Node, bestPlace.numas, true
}

// expireCommittedLocked drops expired idempotency keys. Caller holds s.mu.
func (s *ReservationStore) expireCommittedLocked() {
	now := s.now()
	dropped := 0
	for _, k := range s.committedOrder {
		if c, ok := s.committed[k]; ok && now.Before(c.expires) {
			break
		}
		delete(s.committed, k)
		dropped++
	}
	s.committedOrder = s.committedOrder[dropped:]
}

// placeLocked validates one candidate and places it against this store; ok is true only
// when it fits. Caller holds s.mu.
func (s *ReservationStore) placeLocked(req ReserveRequest) (nodePlacement, bool) {
	// An empty node, NUMA map or core count would accept without a real record.
	if req.Node == "" || len(req.RawFree) == 0 || req.PerNUMACores <= 0 {
		return nodePlacement{}, false
	}
	// Span must be a generated width, or ceil(MemBytes/span) understates per-NUMA demand.
	if !slices.Contains([]int{1, 2, 4, 8}, req.Span) {
		return nodePlacement{}, false
	}
	// A node with fewer NUMAs than the span cannot host it; placement would clamp the span
	// and record less than the pod needs.
	if len(req.RawFree) < req.Span {
		return nodePlacement{}, false
	}
	// Bounds the maps walked under the store lock. Negative free overflows into headroom.
	if len(req.RawFree) > topology.MaxNUMAID+1 || len(req.MemFree) > topology.MaxNUMAID+1 {
		return nodePlacement{}, false
	}
	for n, free := range req.RawFree {
		if n < 0 || n > topology.MaxNUMAID || free < 0 {
			return nodePlacement{}, false
		}
	}
	for n, free := range req.MemFree {
		if n < 0 || n > topology.MaxNUMAID || free < 0 {
			return nodePlacement{}, false
		}
	}
	// Negative memory would add free room.
	if req.MemBytes < 0 || req.PerNUMAMem < 0 {
		return nodePlacement{}, false
	}
	// Memory is checked as MemBytes/Span but recorded as PerNUMAMem, so they must agree.
	// CPU-only (both zero) skips this; MemChecked=false still records the share.
	if req.MemChecked || req.PerNUMAMem != 0 || req.MemBytes != 0 {
		if req.PerNUMAMem != perNUMAMemShare(req.MemBytes, req.Span) {
			return nodePlacement{}, false
		}
	}
	var memFree map[int]int64
	if req.MemChecked {
		memFree = req.MemFree
		if memFree == nil {
			memFree = map[int]int64{} // checked but empty: no memory anywhere
		}
	}
	reserved, reservedMem := s.reservedBothLocked(req.Node)
	p, ok := placeOnNode(req.RawFree, reserved, memFree, reservedMem, req.PerNUMACores, req.Span, req.MemBytes, req.Packing)
	return p, ok && p.cpuFits && p.memFits
}

// reserveLocked records a reservation at the store TTL. Caller holds s.mu.
func (s *ReservationStore) reserveLocked(node string, numas []int, cores int, memBytes int64) {
	s.recordLocked(node, numas, cores, memBytes, s.now().Add(s.ttl))
}

// recordLocked is the shared write path for both reserve entry points. Caller holds s.mu.
func (s *ReservationStore) recordLocked(node string, numas []int, cores int, memBytes int64, expires time.Time) bool {
	if len(numas) == 0 || cores <= 0 || memBytes < 0 || !s.now().Before(expires) {
		return false
	}
	dup := make([]int, len(numas))
	copy(dup, numas)
	s.byND[node] = append(s.byND[node], reservation{
		numas:    dup,
		cores:    cores,
		memBytes: memBytes,
		expires:  expires,
	})
	metrics.ReservationsLive.WithLabelValues(node).Set(float64(len(s.byND[node])))
	return true
}

// seed records a reservation for a pod admitted before this store existed, without a fit
// check: the pod is already pinned.
func (s *ReservationStore) seed(node string, numas []int, cores int, memBytes int64, expires time.Time) bool {
	if node == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordLocked(node, numas, cores, memBytes, expires)
}
