// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// nrtZoneMem builds an NRT container zone carrying a cpuset and a "memory set" attribute.
func nrtZoneMem(name, parent, cpuset, memSet string) map[string]interface{} {
	return map[string]interface{}{
		"type":   testContainerZoneType,
		"name":   name,
		"parent": parent,
		"attributes": []interface{}{
			map[string]interface{}{"name": "cpuset", "value": cpuset},
			map[string]interface{}{"name": "memory set", "value": memSet},
		},
	}
}

// pinnedMemPod builds a running pod with one container requesting memGi GiB.
func pinnedMemPod(name string, memGi int64) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "inference"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "vllm",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceMemory: *resource.NewQuantity(memGi*gib, resource.BinarySI),
					},
				},
			}},
		},
	}
}

// memTopoNode is a balloons-enabled node with a max-span label; topology labels are
// not read on the live path.
func memTopoNode(name string, maxSpan int) *corev1.Node {
	return withMemoryFloor(withMaxSpan(topoNode(name, 8, 2), maxSpan), 250*gib)
}

func TestOccupiedMemPerNUMA(t *testing.T) {
	ctx := context.Background()
	// A span-2 300Gi pod on NUMA 0-1, a span-1 40Gi pod on NUMA 0, a reserved-pool
	// container and a zone whose pod is gone (both skipped).
	nrt := nrtObj("n1",
		nrtZoneMem("inference/big/vllm", "balloon-span2[0]", "0-3", "0-1"),
		nrtZoneMem("inference/small/vllm", "balloon-span1[0]", "4", "0"),
		nrtZoneMem("kube-system/dns/dns", "reserved[0]", "10-11", ""),
		nrtZoneMem("inference/ghost/vllm", "balloon-span1[1]", "5", "1"),
	)
	m := newMutatorWithTopo(topology.NewCache(),
		pinnedMemPod("big", 300),
		pinnedMemPod("small", 40),
		// "ghost" is absent, so its stale zone is skipped.
	)

	occ, ok := m.occupiedMemPerNUMA(ctx, nrt, nil)
	if !ok {
		t.Fatal("occupiedMemPerNUMA should succeed")
	}
	// NUMA0: 150 + 40 = 190Gi. NUMA1: 150Gi.
	if occ[0] != 190*gib {
		t.Errorf("NUMA0 occupied = %d, want %d (190Gi)", occ[0], 190*gib)
	}
	if occ[1] != 150*gib {
		t.Errorf("NUMA1 occupied = %d, want %d (150Gi)", occ[1], 150*gib)
	}
}

func TestOccupiedMemPerNUMANoDoubleCountPodScoped(t *testing.T) {
	ctx := context.Background()
	// Each container zone of a pod-scoped pod is charged its own container's request,
	// so the zones sum to the pod's total, not 2x.
	nrt := nrtObj("n1",
		nrtZoneMem("inference/multi/main", "balloon-span2[0]", "0-3", "0-1"),
		nrtZoneMem("inference/multi/side", "balloon-span2[0]", "0-3", "0-1"),
	)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "inference"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "main", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceMemory: *resource.NewQuantity(200*gib, resource.BinarySI)}}},
			{Name: "side", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceMemory: *resource.NewQuantity(40*gib, resource.BinarySI)}}},
		}},
	}
	m := newMutatorWithTopo(topology.NewCache(), pod)

	occ, ok := m.occupiedMemPerNUMA(ctx, nrt, nil)
	if !ok {
		t.Fatal("occupiedMemPerNUMA should succeed")
	}
	// 240Gi over span 2 is 120Gi per NUMA, not 240Gi.
	for _, numa := range []int{0, 1} {
		if occ[numa] != 120*gib {
			t.Errorf("NUMA%d occupied = %d, want %d (120Gi) — pod-scoped must not double-count", numa, occ[numa], 120*gib)
		}
	}
}

// TestOccupiedMemPerNUMAPodGetCoalesced proves a pod with several zones is fetched
// once per scan.
func TestOccupiedMemPerNUMAPodGetCoalesced(t *testing.T) {
	ctx := context.Background()
	// Three container zones for the same pod.
	nrt := nrtObj("n1",
		nrtZoneMem("inference/multi/c0", "balloon-span2[0]", "0-3", "0-1"),
		nrtZoneMem("inference/multi/c1", "balloon-span2[0]", "0-3", "0-1"),
		nrtZoneMem("inference/multi/c2", "balloon-span2[0]", "0-3", "0-1"),
	)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "inference"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "c0", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(10*gib, resource.BinarySI)}}},
			{Name: "c1", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(10*gib, resource.BinarySI)}}},
			{Name: "c2", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(10*gib, resource.BinarySI)}}},
		}},
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	var podGets int32
	c := interceptor.NewClient(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build(),
		interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					atomic.AddInt32(&podGets, 1)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		},
	)
	m := &PodMutator{Client: c, TopoCache: topology.NewCache()}

	if _, ok := m.occupiedMemPerNUMA(ctx, nrt, nil); !ok {
		t.Fatal("occupiedMemPerNUMA should succeed")
	}
	if got := atomic.LoadInt32(&podGets); got != 1 {
		t.Errorf("pod Gets = %d, want 1 (three container zones must coalesce to one Pod Get)", got)
	}
}

func TestNodeLiveMemFree(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.SetMemory("n1", map[int]int64{0: 250 * gib, 1: 260 * gib})
	nrt := nrtObj("n1",
		nrtZoneMem("inference/big/vllm", "balloon-span2[0]", "0-3", "0-1"),
	)
	m := newMutatorWithTopo(topo, nrt, pinnedMemPod("big", 300))

	// 300Gi span-2 is 150Gi per NUMA; free = cap - occ.
	free, ok := m.nodeLiveMemFree(ctx, "n1", nil)
	if !ok {
		t.Fatal("nodeLiveMemFree should have data")
	}
	if free[0] != 100*gib { // 250 − 150
		t.Errorf("NUMA0 free = %d, want %d (100Gi)", free[0], 100*gib)
	}
	if free[1] != 110*gib { // 260 − 150
		t.Errorf("NUMA1 free = %d, want %d (110Gi)", free[1], 110*gib)
	}

	free, ok = m.nodeLiveMemFree(ctx, "n1", map[int]int64{0: 60 * gib})
	if !ok {
		t.Fatal("nodeLiveMemFree should have data")
	}
	if free[0] != 40*gib { // 100 − 60
		t.Errorf("NUMA0 free with reservation = %d, want %d (40Gi)", free[0], 40*gib)
	}

	// No cached memory map: ok=false, caller falls back to static.
	if _, ok := m.nodeLiveMemFree(ctx, "uncached", nil); ok {
		t.Error("uncached node should signal ok=false")
	}
}

// TestMemoryCapacityCheckScheduledLive proves live per-NUMA free memory, not the
// static floor x span, decides for a scheduled pod.
func TestMemoryCapacityCheckScheduledLive(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.SetMemory("n1", map[int]int64{0: 250 * gib, 1: 250 * gib})
	// 150Gi per NUMA already pinned, leaving 100Gi.
	nrt := nrtObj("n1",
		nrtZoneMem("inference/incumbent/vllm", "balloon-span2[0]", "0-3", "0-1"),
	)
	m := newMutatorWithTopo(topo,
		memTopoNode("n1", 2), nrt,
		pinnedMemPod("incumbent", 300),
	)

	// 75Gi per NUMA fits.
	pod := scheduledPod("n1", "2")
	if fits, _, err := m.memoryCapacityCheck(ctx, pod, 2, 150*gib); err != nil || !fits {
		t.Errorf("150Gi span2 should fit live (share 75Gi ≤ 100Gi free): fits=%v err=%v", fits, err)
	}

	// 130Gi per NUMA does not, though the static floor (500Gi) would admit it.
	fits, detail, err := m.memoryCapacityCheck(ctx, pod, 2, 260*gib)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fits {
		t.Error("260Gi span2 must NOT fit live (share 130Gi > 100Gi free)")
	}
	if detail == "" {
		t.Error("live rejection must carry a detail for the event")
	}
}

// TestMemoryCapacityCheckScheduledFallsBackToStatic proves the scheduled path uses
// floor x span when no memory map is cached.
func TestMemoryCapacityCheckScheduledFallsBackToStatic(t *testing.T) {
	ctx := context.Background()
	// No memory map for n1: static fallback.
	node := withMemoryFloor(withMaxSpan(topoNode("n1", 8, 2), 2), 100*gib)
	m := newMutatorWithTopo(topology.NewCache(), node, nrtObj("n1"))

	pod := scheduledPod("n1", "2")
	// 200Gi static capacity: 150Gi fits, 300Gi does not.
	if fits, _, err := m.memoryCapacityCheck(ctx, pod, 2, 150*gib); err != nil || !fits {
		t.Errorf("static fallback: 150Gi should fit (cap 200Gi): fits=%v err=%v", fits, err)
	}
	if fits, _, _ := m.memoryCapacityCheck(ctx, pod, 2, 300*gib); fits {
		t.Error("static fallback: 300Gi must NOT fit (cap 200Gi)")
	}
}

// TestAdmitWithHeadroomLiveMemorySteersAndReserves proves steering requires the node
// with live memory room, where the static floor accepts both, and records a memory
// reservation there.
func TestAdmitWithHeadroomLiveMemorySteersAndReserves(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	// Both nodes: 4 CPUs / 2 NUMA and 250Gi/NUMA memory.
	topo.Set("full", simpleTopo())
	topo.Set("free", simpleTopo())
	topo.SetMemory("full", map[int]int64{0: 250 * gib, 1: 250 * gib})
	topo.SetMemory("free", map[int]int64{0: 250 * gib, 1: 250 * gib})
	// "full" has 10Gi free per NUMA; its incumbent uses only cpu0, so memory is the sole
	// discriminator.
	nrtFull := nrtObj("full",
		nrtZoneMem("inference/incumbent/vllm", "balloon-span2[0]", "0", "0-1"))
	nrtFree := nrtObj("free")
	m := newMutatorWithTopo(topo,
		withMemoryFloor(withMaxSpan(topoNode("full", 4, 2), 2), 250*gib),
		withMemoryFloor(withMaxSpan(topoNode("free", 4, 2), 2), 250*gib),
		nrtFull, nrtFree,
		pinnedMemPod("incumbent", 480),
	)
	m.Reservations = NewReservationStore(0)

	// 25Gi per NUMA fits only "free".
	pod := unscheduledPod("p", "2")
	verdict := m.admitWithHeadroom(ctx, pod, 2, 2, 50*gib, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawLive || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=true", verdict)
	}
	req := affinityRequiredValues(pod)
	if len(req) != 1 || req[0] != "free" {
		t.Errorf("REQUIRED hostnames = %v, want [free] (full is memory-saturated live)", req)
	}
	// 50Gi span-2 reserves 25Gi/NUMA on "free".
	memRes := reservedMem(m.Reservations, "free")
	if len(memRes) == 0 {
		t.Fatal("expected a memory reservation on the chosen node")
	}
	for numa, b := range memRes {
		if b != 25*gib {
			t.Errorf("NUMA %d memory reserved = %d, want %d (25Gi = 50Gi/span2)", numa, b, 25*gib)
		}
	}
}

// TestAdmitWithHeadroomCPUOnlyReservesNoMemory proves a pod without a memory request
// reserves memBytes 0.
func TestAdmitWithHeadroomCPUOnlyReservesNoMemory(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("n1", simpleTopo())
	m := newMutatorWithTopo(topo, withMaxSpan(topoNode("n1", 4, 2), 2), nrtObj("n1"))
	m.Reservations = NewReservationStore(0)

	pod := unscheduledPod("p", "1")
	if v := m.admitWithHeadroom(ctx, pod, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard()); !v.fits {
		t.Fatalf("CPU-only pod should fit, verdict=%+v", v)
	}
	if got := reservedCores(m.Reservations, "n1"); len(got) == 0 {
		t.Error("CPU reservation should be recorded")
	}
	if got := reservedMem(m.Reservations, "n1"); len(got) != 0 {
		t.Errorf("CPU-only pod must not record a memory reservation, got %v", got)
	}
}

// TestAdmitWithHeadroomMemoryCheckedOnCPUChosenNUMAs proves memory is checked on the
// NUMAs CPU picks: CPU picks {0,1} by tie-break, memory is free only on {2,3}, so the
// pod must not fit.
func TestAdmitWithHeadroomMemoryCheckedOnCPUChosenNUMAs(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("n1", wideTopo()) // 4 NUMA, 1 core each, all CPUs free
	// NUMA 0-1 have 10Gi free, NUMA 2-3 have 250Gi.
	topo.SetMemory("n1", map[int]int64{0: 250 * gib, 1: 250 * gib, 2: 250 * gib, 3: 250 * gib})
	nrt := nrtObj("n1",
		nrtZoneMem("inference/incumbent/vllm", "balloon-span2[0]", "0", "0-1"))
	m := newMutatorWithTopo(topo,
		withMemoryFloor(withMaxSpan(topoNode("n1", 4, 4), 4), 250*gib),
		nrt, pinnedMemPod("incumbent", 480),
	)
	m.Reservations = NewReservationStore(0)

	// 25Gi/NUMA does not fit the CPU-chosen {0,1}.
	pod := unscheduledPod("p", "2")
	v := m.admitWithHeadroom(ctx, pod, 2, 2, 50*gib, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !v.sawLive {
		t.Fatalf("expected sawLive=true, verdict=%+v", v)
	}
	if v.fits {
		t.Error("must NOT fit: CPU-chosen NUMAs {0,1} lack memory (only {2,3} have it); the check must use the CPU-chosen set")
	}
	if pod.Spec.Affinity != nil {
		t.Error("no fitting node must not inject affinity")
	}
	if len(reservedMem(m.Reservations, "n1")) != 0 {
		t.Error("no fit → no memory reservation")
	}
	// The failure must be attributed to memory (reason and detail).
	if v.reason != "memory" {
		t.Errorf("reason = %q, want %q (CPU fit, rejected on memory)", v.reason, "memory")
	}
	if !strings.Contains(v.detail, "memory request of") {
		t.Errorf("detail should be memory-worded, got %q", v.detail)
	}
}

// TestAdmitWithHeadroomStaticMemoryBlockedAttributedToMemory proves a static-fallback
// memory rejection is still attributed to memory.
func TestAdmitWithHeadroomStaticMemoryBlockedAttributedToMemory(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("n1", simpleTopo()) // live CPU data present (empty NRT → CPUs free)
	// No live memory map; static 2Gi capacity cannot fit 50Gi.
	m := newMutatorWithTopo(topo,
		withMemoryFloor(withMaxSpan(topoNode("n1", 4, 2), 2), 1*gib),
		nrtObj("n1"),
	)
	m.Reservations = NewReservationStore(0)

	pod := unscheduledPod("p", "2")
	v := m.admitWithHeadroom(ctx, pod, 2, 2, 50*gib, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !v.sawLive || v.fits {
		t.Fatalf("expected sawLive=true, fits=false, got %+v", v)
	}
	if v.reason != "memory" {
		t.Errorf("reason = %q, want %q (CPU fit, rejected on the STATIC memory floor)", v.reason, "memory")
	}
	if !strings.Contains(v.detail, "memory request of") {
		t.Errorf("detail should be memory-worded, got %q", v.detail)
	}
}
