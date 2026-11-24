// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// gridTopo is a node of numas NUMA nodes, coresPerNUMA physical cores each, two threads per core.
func gridTopo(numas, coresPerNUMA int) map[int]topology.CPUInfo {
	info := map[int]topology.CPUInfo{}
	total := numas * coresPerNUMA
	for core := 0; core < total; core++ {
		numa := core / coresPerNUMA
		info[core] = topology.CPUInfo{CPU: core, NUMA: numa, Core: core}
		info[core+total] = topology.CPUInfo{CPU: core + total, NUMA: numa, Core: core}
	}
	return info
}

// placementCoresPerNUMA is the physical cores per NUMA on every placementCluster node.
const placementCoresPerNUMA = 4

// placementCluster builds the node objects and topology cache for names, each a
// numas x placementCoresPerNUMA grid with memPerNUMA bytes per NUMA (0 for no memory label).
func placementCluster(names []string, numas int, memPerNUMA int64) (*topology.Cache, func() []client.Object) {
	topo := topology.NewCache()
	for _, n := range names {
		topo.Set(n, gridTopo(numas, placementCoresPerNUMA))
	}
	objs := func() []client.Object {
		var out []client.Object
		for _, n := range names {
			node := topoNode(n, numas*placementCoresPerNUMA, numas)
			if memPerNUMA > 0 {
				node.Labels[controller.LabelMemoryPerNUMABytes] = strconv.FormatInt(memPerNUMA, 10)
			}
			out = append(out, node, nrtObj(n))
		}
		return out
	}
	return topo, objs
}

// leaderMutator serves admissions on the lease holder: its store is the authority's.
func leaderMutator(topo *topology.Cache, objs []client.Object) *PodMutator {
	m := newMutatorWithTopo(topo, objs...)
	m.Reservations = NewReservationStore(0)
	return m
}

// followerMutator serves admissions on a follower that forwards commits over mTLS to an
// authority holding the returned store.
func followerMutator(t *testing.T, topo *topology.Cache, objs []client.Object) (follower *PodMutator, authority *ReservationStore) {
	t.Helper()
	certDir := writeReserveCerts(t)
	authority = NewReservationStore(0)
	addr := startAuthority(t, certDir, authority)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	m := newMutatorWithTopo(topo, objs...)
	m.Reservations = NewReservationStore(0)
	c, err := newLeaderReserveClient(certDir, atoiOrFatal(t, portStr), m.Reservations,
		func(context.Context) (string, bool, error) { return host, false, nil }, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	m.SetReserver(c)
	return m, authority
}

// placed is one pod's outcome: admitted, and if so the node and NUMAs recorded.
type placed struct {
	fits  bool
	node  string
	numas []int
}

func (a placed) String() string {
	if !a.fits {
		return "denied"
	}
	return fmt.Sprintf("%s%v", a.node, a.numas)
}

// admitThrough runs one pod through admitWithHeadroom and reads the reservation it recorded in
// store (the authority's ledger).
func admitThrough(t *testing.T, m *PodMutator, store *ReservationStore, i int, cores, span int, memBytes int64, alloc, packing string) placed {
	t.Helper()
	pod := unscheduledPod(fmt.Sprintf("p%d", i), strconv.Itoa(cores))
	if memBytes > 0 {
		pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = *resource.NewQuantity(memBytes, resource.BinarySI)
	}
	before := reservationCounts(store)
	rp := reservePolicy{key: fmt.Sprintf("pod-%d/adm-%d", i, i), mode: "enforce", failClosed: true}
	v := m.admitWithHeadroom(context.Background(), pod, cores, span, memBytes, memBytes > 0, "", alloc, packing, nil, rp, logr.Discard())
	if !v.sawLive || v.unaccountable {
		t.Fatalf("pod %d: verdict %+v, want a live, accountable verdict", i, v)
	}
	if !v.fits {
		return placed{}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for node, rs := range store.byND {
		if len(rs) > before[node] {
			return placed{fits: true, node: node, numas: slices.Clone(rs[len(rs)-1].numas)}
		}
	}
	t.Fatalf("pod %d admitted but the authority recorded no reservation", i)
	return placed{}
}

func reservationCounts(s *ReservationStore) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for node, rs := range s.byND {
		out[node] = len(rs)
	}
	return out
}

// TestFollowerAdmitsFullNUMAPodOnSecondNUMA: two full-NUMA pods through a follower on a
// 2-NUMA node. The follower's local store is empty, so it cannot pick the NUMA; the second
// pod must land on NUMA 1, and a third is denied.
func TestFollowerAdmitsFullNUMAPodOnSecondNUMA(t *testing.T) {
	topo, objs := placementCluster([]string{"n1"}, 2, 0)
	m, auth := followerMutator(t, topo, objs())

	want := []string{"n1[0]", "n1[1]", "denied"}
	for i, w := range want {
		if got := admitThrough(t, m, auth, i, 4, 1, 0, defaultCPUAllocation, packingSpread).String(); got != w {
			t.Errorf("pod %d: %s, want %s", i, got, w)
		}
	}
	if r := reservedCores(m.Reservations, "n1"); len(r) != 0 {
		t.Errorf("follower local store = %v, want empty: commits go to the authority", r)
	}
}

// TestFollowerSpanOneBurstAdmitsExactlyCapacity: a span-1 burst through a follower admits
// exactly the node's capacity across both NUMAs and denies the next pod.
func TestFollowerSpanOneBurstAdmitsExactlyCapacity(t *testing.T) {
	const numas, perNUMA = 2, placementCoresPerNUMA
	topo, objs := placementCluster([]string{"n1"}, numas, 0)
	m, auth := followerMutator(t, topo, objs())

	for i := 0; i < numas*perNUMA; i++ {
		if a := admitThrough(t, m, auth, i, 1, 1, 0, defaultCPUAllocation, packingSpread); !a.fits {
			t.Fatalf("pod %d of a %d-core burst denied; the node had room on another NUMA", i, numas*perNUMA)
		}
	}
	if a := admitThrough(t, m, auth, numas*perNUMA, 1, 1, 0, defaultCPUAllocation, packingSpread); a.fits {
		t.Fatalf("pod %d past capacity admitted on %s", numas*perNUMA, a)
	}
	if r := reservedCores(auth, "n1"); r[0] != perNUMA || r[1] != perNUMA {
		t.Errorf("authority ledger = %v, want %d on each NUMA", r, perNUMA)
	}
}

// TestFollowerMemoryPodOnSecondNUMA: memory picks follow the authority's NUMA choice too. Each
// pod takes a whole NUMA's memory and one core.
func TestFollowerMemoryPodOnSecondNUMA(t *testing.T) {
	const memPerNUMA = 8 * gib
	topo, objs := placementCluster([]string{"n1"}, 2, memPerNUMA)
	m, auth := followerMutator(t, topo, objs())

	want := []string{"n1[0]", "n1[1]", "denied"}
	for i, w := range want {
		if got := admitThrough(t, m, auth, i, 1, 1, memPerNUMA, defaultCPUAllocation, packingSpread).String(); got != w {
			t.Errorf("pod %d: %s, want %s", i, got, w)
		}
	}
}

// TestLeaderFollowerPlacementParity: for the same nodes and pod sequence, a leader admitting
// against its own store and a follower committing to the authority make the same picks.
func TestLeaderFollowerPlacementParity(t *testing.T) {
	// Per-NUMA shares; the pod asks for share*span CPUs. Four rounds overfill every case.
	var shares []int
	for range 4 {
		shares = append(shares, 3, 1, 2, 2, 4, 1, 3, 1, 1, 2, 1, 1, 4, 1)
	}
	for _, numas := range []int{1, 2, 4} {
		for _, span := range []int{1, 2} {
			if span > numas {
				continue
			}
			for _, alloc := range []string{defaultCPUAllocation, hyperthreadsAllocation} {
				for _, packing := range []string{packingSpread, packingBinpack} {
					t.Run(fmt.Sprintf("numa%d/span%d/%s/%s", numas, span, alloc, packing), func(t *testing.T) {
						topo, objs := placementCluster([]string{"a", "b"}, numas, 0)
						leader := leaderMutator(topo, objs())
						follower, auth := followerMutator(t, topo, objs())
						admitted := 0
						for i, share := range shares {
							l := admitThrough(t, leader, leader.Reservations, i, share*span, span, 0, alloc, packing)
							f := admitThrough(t, follower, auth, i, share*span, span, 0, alloc, packing)
							if l.String() != f.String() {
								t.Fatalf("pod %d (%d CPUs): leader %s, follower %s", i, share*span, l, f)
							}
							if l.fits {
								admitted++
							}
						}
						if admitted == 0 || admitted == len(shares) {
							t.Fatalf("%d of %d pods admitted; the sequence must reach both outcomes", admitted, len(shares))
						}
					})
				}
			}
		}
	}
}

// TestLeaderFollowerParityAsymmetricMemory: NUMA 0 has too little memory for the second pod,
// and only the authority knows the first pod took CPUs there. The follower must not drop the
// node on its own memory check; the authority places the pod on NUMA 1.
func TestLeaderFollowerParityAsymmetricMemory(t *testing.T) {
	topo, objs := placementCluster([]string{"n1"}, 2, 0)
	topo.SetMemory("n1", map[int]int64{0: 1 * gib, 1: 8 * gib})
	leader := leaderMutator(topo, objs())
	follower, auth := followerMutator(t, topo, objs())

	pods := []struct {
		cores int
		mem   int64
		want  string
	}{{2, 0, "n1[0]"}, {1, 8 * gib, "n1[1]"}, {1, 8 * gib, "denied"}}
	for i, p := range pods {
		l := admitThrough(t, leader, leader.Reservations, i, p.cores, 1, p.mem, defaultCPUAllocation, packingSpread)
		f := admitThrough(t, follower, auth, i, p.cores, 1, p.mem, defaultCPUAllocation, packingSpread)
		if l.String() != p.want || f.String() != p.want {
			t.Errorf("pod %d: leader %s, follower %s, want %s", i, l, f, p.want)
		}
	}
}
