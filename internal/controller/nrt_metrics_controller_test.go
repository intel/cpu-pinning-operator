// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"

	"github.com/intel/cpu-pinning-operator/internal/balloons"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// twoNumaCPUInfo builds a 2-NUMA node with 4 single-thread cores per NUMA; core id
// equals CPU id, so free cores equal free CPUs.
func twoNumaCPUInfo() map[int]topology.CPUInfo {
	const perNUMA = 4
	info := map[int]topology.CPUInfo{}
	cpu := 0
	for numa := 0; numa < 2; numa++ {
		for i := 0; i < perNUMA; i++ {
			info[cpu] = topology.CPUInfo{CPU: cpu, NUMA: numa, Core: cpu}
			cpu++
		}
	}
	return info
}

// TestReconcile_MalformedZonesDropsSeries: a CR whose "zones" has the wrong type is
// unknown, so the node's stale series are dropped rather than reported all-free,
// matching the webhook's UNKNOWN.
func TestReconcile_MalformedZonesDropsSeries(t *testing.T) {
	metrics.CPUAssignment.Reset()
	metrics.CPUFragmentation.Reset()

	// "zones" is a string, not a slice.
	nrt := &unstructured.Unstructured{}
	nrt.SetGroupVersionKind(nrtpkg.GVK)
	nrt.SetName("n1")
	nrt.Object["zones"] = "not-a-slice"

	cache := topology.NewCache()
	cache.Set("n1", twoNumaCPUInfo()) // 8 CPUs: a valid CR would emit these as free
	r := &NRTMetricsReconciler{
		Client:    fake.NewClientBuilder().WithScheme(driftScheme()).WithRuntimeObjects(nrt).Build(),
		Log:       ctrl.Log.WithName("test"),
		TopoCache: cache,
	}

	metrics.CPUFragmentation.WithLabelValues("n1").Set(4)
	metrics.CPUAssignment.WithLabelValues("n1", "0", "0", "0", "reserved", "", "").Set(1)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "n1"}}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	if n := testutil.CollectAndCount(metrics.CPUAssignment); n != 0 {
		t.Errorf("cpu_assignment series count = %d, want 0 (malformed CR must not emit an all-free series)", n)
	}
	if n := testutil.CollectAndCount(metrics.CPUFragmentation); n != 0 {
		t.Errorf("cpu_fragmentation series count = %d, want 0 (malformed CR must drop the stale value)", n)
	}
}

// balloonZone builds a "balloon"-typed NRT zone, the shape of the reserved pool.
func balloonZone(name, cpuset string) map[string]interface{} {
	z := nrtZone(name, "", cpuset)
	z["type"] = balloonZoneType
	return z
}

// TestReconcile_ReservedContainersAreMembersNotAssignments: a container under the
// reserved balloon gets one pool_membership series and no cpu_assignment series;
// cpu_assignment stays one series per logical CPU, which consumers rely on.
func TestReconcile_ReservedContainersAreMembersNotAssignments(t *testing.T) {
	metrics.CPUAssignment.Reset()
	metrics.PoolMembership.Reset()

	// cpus 0-1: reserved, shared by two containers; 2-3: one workload balloon; 4-7 free.
	nrt := nrtObj(
		balloonZone("reserved[0]", "0-1"),
		balloonZone("balloon-span1[0]", "2-3"),
		nrtZone("kube-system/kube-proxy-abc/kube-proxy", "reserved[0]", "0-1"),
		nrtZone("default/busybox/busybox", "reserved[0]", "0-1"),
		nrtZone("inference/demo-abc/demo", "balloon-span1[0]", "2-3"),
	)

	cache := topology.NewCache()
	cache.Set("n1", twoNumaCPUInfo()) // 8 CPUs
	r := &NRTMetricsReconciler{
		Client:    fake.NewClientBuilder().WithScheme(driftScheme()).WithRuntimeObjects(nrt).Build(),
		Log:       ctrl.Log.WithName("test"),
		TopoCache: cache,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "n1"}}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	// One membership series per container zone, valued with its cpuset size.
	if n := testutil.CollectAndCount(metrics.PoolMembership); n != 3 {
		t.Errorf("pool_membership series count = %d, want 3 (one per container zone)", n)
	}
	for _, tc := range []struct {
		balloon, ns, pod, container string
		want                        float64
	}{
		{"reserved", "kube-system", "kube-proxy-abc", "kube-proxy", 2},
		{"reserved", "default", "busybox", "busybox", 2},
		{"balloon-span1", "inference", "demo-abc", "demo", 2},
	} {
		got := testutil.ToFloat64(metrics.PoolMembership.WithLabelValues(
			"n1", tc.balloon, tc.ns, tc.pod, tc.container))
		if got != tc.want {
			t.Errorf("pool_membership{balloon=%q,pod=%q} = %v, want %v", tc.balloon, tc.pod, got, tc.want)
		}
	}

	if n := testutil.CollectAndCount(metrics.CPUAssignment); n != 8 {
		t.Errorf("cpu_assignment series count = %d, want 8 (one per logical CPU); a reserved "+
			"container zone must not add a series", n)
	}
	// Reserved CPUs are attributed to the pool with an empty pod.
	for _, cpu := range []string{"0", "1"} {
		if got := testutil.ToFloat64(metrics.CPUAssignment.WithLabelValues(
			"n1", cpu, "0", cpu, "reserved", "", "")); got != 1 {
			t.Errorf("cpu_assignment{cpu=%s,balloon=reserved,workload_pod=\"\"} = %v, want 1", cpu, got)
		}
	}
	if got := testutil.ToFloat64(metrics.CPUAssignment.WithLabelValues(
		"n1", "2", "0", "2", "balloon-span1", "demo-abc", "inference")); got != 1 {
		t.Error("a workload container zone must still be attributed per pod in cpu_assignment")
	}
}

// TestReconcile_UnparseableZoneNameEmitsNoMembership: a zone name that does not split
// into namespace/pod/container is skipped, since such zones would collide on one series
// and hide each other's CPUs; cpu_assignment still attributes their CPUs.
func TestReconcile_UnparseableZoneNameEmitsNoMembership(t *testing.T) {
	metrics.CPUAssignment.Reset()
	metrics.PoolMembership.Reset()

	nrt := nrtObj(
		balloonZone("reserved[0]", "0-1"),
		balloonZone("balloon-span1[0]", "2-3"),
		// Two unnamed zones under reserved on one cpuset, plus one under a workload balloon.
		nrtZone("mangled", "reserved[0]", "0-1"),
		nrtZone("also-mangled", "reserved[0]", "0-1"),
		nrtZone("mangled-too", "balloon-span1[0]", "2-3"),
		// One zone per missing name part, since each guard checks only the others.
		nrtZone("/foo/bar", "reserved[0]", "0-1"),
		nrtZone("foo/bar", "reserved[0]", "0-1"),
	)
	cache := topology.NewCache()
	cache.Set("n1", twoNumaCPUInfo())
	r := &NRTMetricsReconciler{
		Client:    fake.NewClientBuilder().WithScheme(driftScheme()).WithRuntimeObjects(nrt).Build(),
		Log:       ctrl.Log.WithName("test"),
		TopoCache: cache,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "n1"}}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	if n := testutil.CollectAndCount(metrics.PoolMembership); n != 0 {
		t.Errorf("pool_membership series count = %d, want 0 (an unnamed zone must not claim "+
			"a series two of them would share)", n)
	}
	// cpu_assignment unchanged: one series per CPU, workload CPUs attributed with an empty pod.
	if n := testutil.CollectAndCount(metrics.CPUAssignment); n != 8 {
		t.Errorf("cpu_assignment series count = %d, want 8 (one per logical CPU)", n)
	}
	if got := testutil.ToFloat64(metrics.CPUAssignment.WithLabelValues(
		"n1", "2", "0", "2", "balloon-span1", "", "")); got != 1 {
		t.Error("an unnamed workload container zone must still have its CPUs attributed to " +
			"its balloon in cpu_assignment")
	}
}

// TestReconcile_DuplicateContainerZoneTakesTheLargest: two zones for one container
// (ghost signature) report the larger cpuset, in either zone order, so contention is
// not understated.
func TestReconcile_DuplicateContainerZoneTakesTheLargest(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"larger first", "0-3", "0-1"},
		{"larger second", "0-1", "0-3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics.CPUAssignment.Reset()
			metrics.PoolMembership.Reset()

			nrt := nrtObj(
				balloonZone("balloon-span1[0]", "0-3"),
				nrtZone("inference/demo-abc/demo", "balloon-span1[0]", tc.first),
				nrtZone("inference/demo-abc/demo", "balloon-span1[0]", tc.second),
			)
			cache := topology.NewCache()
			cache.Set("n1", twoNumaCPUInfo())
			r := &NRTMetricsReconciler{
				Client:    fake.NewClientBuilder().WithScheme(driftScheme()).WithRuntimeObjects(nrt).Build(),
				Log:       ctrl.Log.WithName("test"),
				TopoCache: cache,
			}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "n1"}}); err != nil {
				t.Fatalf("Reconcile returned error: %v", err)
			}

			if n := testutil.CollectAndCount(metrics.PoolMembership); n != 1 {
				t.Errorf("pool_membership series count = %d, want 1 (two zones for one "+
					"container are one member)", n)
			}
			if got := testutil.ToFloat64(metrics.PoolMembership.WithLabelValues(
				"n1", "balloon-span1", "inference", "demo-abc", "demo")); got != 4 {
				t.Errorf("pool_membership = %v, want 4 (the larger of the two cpusets, "+
					"whichever order they appear in)", got)
			}

			if n := testutil.CollectAndCount(metrics.CPUAssignment); n != 8 {
				t.Errorf("cpu_assignment series count = %d, want 8 (one per logical CPU)", n)
			}
		})
	}
}

// TestReconcile_DuplicateAcrossBalloonsStaysTwoSeries: a container with zones in two
// balloons is reported in both, since the CR cannot say which is live, and ghost
// counting skips reserved-parented zones.
func TestReconcile_DuplicateAcrossBalloonsStaysTwoSeries(t *testing.T) {
	metrics.CPUAssignment.Reset()
	metrics.PoolMembership.Reset()

	nrt := nrtObj(
		balloonZone("reserved[0]", "0-1"),
		balloonZone("balloon-span1[0]", "2-5"),
		// Stale zone in reserved, live one in the dedicated balloon.
		nrtZone("inference/demo-abc/demo", "reserved[0]", "0-1"),
		nrtZone("inference/demo-abc/demo", "balloon-span1[0]", "2-5"),
	)
	cache := topology.NewCache()
	cache.Set("n1", twoNumaCPUInfo())
	r := &NRTMetricsReconciler{
		Client:    fake.NewClientBuilder().WithScheme(driftScheme()).WithRuntimeObjects(nrt).Build(),
		Log:       ctrl.Log.WithName("test"),
		TopoCache: cache,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "n1"}}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	if n := testutil.CollectAndCount(metrics.PoolMembership); n != 2 {
		t.Errorf("pool_membership series count = %d, want 2 (one per balloon still holding "+
			"CPUs for the container)", n)
	}
	for _, tc := range []struct {
		balloon string
		want    float64
	}{
		{"reserved", 2},
		{"balloon-span1", 4},
	} {
		if got := testutil.ToFloat64(metrics.PoolMembership.WithLabelValues(
			"n1", tc.balloon, "inference", "demo-abc", "demo")); got != tc.want {
			t.Errorf("pool_membership{balloon=%q} = %v, want %v (that balloon's own cpuset, "+
				"not the other's)", tc.balloon, got, tc.want)
		}
	}

	if n := testutil.CollectAndCount(metrics.CPUAssignment); n != 8 {
		t.Errorf("cpu_assignment series count = %d, want 8 (one per logical CPU)", n)
	}
}

// TestReconcile_PoolMembershipClearsDepartedPods: a pod that left the node disappears
// from the membership gauge on the next reconcile.
func TestReconcile_PoolMembershipClearsDepartedPods(t *testing.T) {
	metrics.CPUAssignment.Reset()
	metrics.PoolMembership.Reset()

	nrt := nrtObj(
		balloonZone("reserved[0]", "0-1"),
		nrtZone("default/busybox/busybox", "reserved[0]", "0-1"),
	)
	cache := topology.NewCache()
	cache.Set("n1", twoNumaCPUInfo())
	r := &NRTMetricsReconciler{
		Client:    fake.NewClientBuilder().WithScheme(driftScheme()).WithRuntimeObjects(nrt).Build(),
		Log:       ctrl.Log.WithName("test"),
		TopoCache: cache,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "n1"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if n := testutil.CollectAndCount(metrics.PoolMembership); n != 1 {
		t.Fatalf("precondition: pool_membership series count = %d, want 1", n)
	}

	nrt.Object["zones"] = []interface{}{balloonZone("reserved[0]", "0-1")}
	if err := r.Update(context.Background(), nrt); err != nil {
		t.Fatalf("update NRT: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if n := testutil.CollectAndCount(metrics.PoolMembership); n != 0 {
		t.Errorf("pool_membership series count = %d, want 0 (a departed pod must not go stale)", n)
	}
}

// TestEmitFragmentation_Imbalance: with NUMA1 fully occupied, a span-2 balloon strands
// all 4 free cores on NUMA0.
func TestEmitFragmentation_Imbalance(t *testing.T) {
	metrics.CPUFragmentation.Reset()
	r := &NRTMetricsReconciler{}
	info := twoNumaCPUInfo()

	occupied := map[int]bool{4: true, 5: true, 6: true, 7: true}

	r.emitFragmentation("n1", info, occupied, balloons.CPUAllocationPhysicalCores)

	if got := testutil.ToFloat64(metrics.CPUFragmentation.WithLabelValues("n1")); got != 4 {
		t.Errorf("cpu_fragmentation = %v, want 4 (NUMA0's 4 free cores stranded by full NUMA1)", got)
	}
}

// TestEmitFragmentation_Balanced: 4 free cores per NUMA strands nothing.
func TestEmitFragmentation_Balanced(t *testing.T) {
	metrics.CPUFragmentation.Reset()
	r := &NRTMetricsReconciler{}
	info := twoNumaCPUInfo()

	r.emitFragmentation("n1", info, map[int]bool{}, balloons.CPUAllocationPhysicalCores)

	if got := testutil.ToFloat64(metrics.CPUFragmentation.WithLabelValues("n1")); got != 0 {
		t.Errorf("cpu_fragmentation = %v, want 0 (balanced free room packs into a max-span balloon)", got)
	}
}

// TestEmitFragmentation_ModeSetsTheUnit: one idle HT pair on a full NUMA strands 1 core
// under physical-cores and 2 threads under hyperthreads.
func TestEmitFragmentation_ModeSetsTheUnit(t *testing.T) {
	// NUMA0: core0 (cpu0, cpu1) free; NUMA1: core2 (cpu2) busy.
	info := map[int]topology.CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0}, 1: {CPU: 1, NUMA: 0, Core: 0},
		2: {CPU: 2, NUMA: 1, Core: 2},
	}
	occupied := map[int]bool{2: true} // NUMA1 fully busy

	for _, tc := range []struct {
		mode string
		want float64
	}{
		{balloons.CPUAllocationPhysicalCores, 1},
		{cpuAllocationHyperthreads, 2},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			metrics.CPUFragmentation.Reset()
			r := &NRTMetricsReconciler{}

			r.emitFragmentation("worker-1", info, occupied, tc.mode)

			if got := testutil.ToFloat64(metrics.CPUFragmentation.WithLabelValues("worker-1")); got != tc.want {
				t.Errorf("cpu_fragmentation = %v, want %v stranded in %s", got, tc.want, tc.mode)
			}
		})
	}
}

// TestEmitFragmentation_UnresolvedModeNoEmit: no resolved mode, no series.
func TestEmitFragmentation_UnresolvedModeNoEmit(t *testing.T) {
	metrics.CPUFragmentation.Reset()
	r := &NRTMetricsReconciler{}

	r.emitFragmentation("worker-1", twoNumaCPUInfo(), map[int]bool{4: true, 5: true, 6: true, 7: true}, "")

	if n := testutil.CollectAndCount(metrics.CPUFragmentation); n != 0 {
		t.Errorf("cpu_fragmentation series count = %d, want 0 (an unresolved mode emits nothing)", n)
	}
}

// TestEmitFragmentation_NotProbedNoEmit: no cached topology, no series.
func TestEmitFragmentation_NotProbedNoEmit(t *testing.T) {
	metrics.CPUFragmentation.Reset()
	r := &NRTMetricsReconciler{}

	r.emitFragmentation("n1", nil, map[int]bool{}, balloons.CPUAllocationPhysicalCores)

	if n := testutil.CollectAndCount(metrics.CPUFragmentation); n != 0 {
		t.Errorf("cpu_fragmentation series count = %d, want 0 (unprobed node emits nothing)", n)
	}
}

// TestEmitFragmentation_LostTopologyClearsStale: a pruned topology clears the prior value.
func TestEmitFragmentation_LostTopologyClearsStale(t *testing.T) {
	metrics.CPUFragmentation.Reset()
	r := &NRTMetricsReconciler{}

	info := twoNumaCPUInfo()
	r.emitFragmentation("n1", info, map[int]bool{4: true, 5: true, 6: true, 7: true}, balloons.CPUAllocationPhysicalCores)
	if got := testutil.ToFloat64(metrics.CPUFragmentation.WithLabelValues("n1")); got != 4 {
		t.Fatalf("precondition: cpu_fragmentation = %v, want 4", got)
	}

	// Topology pruned: the stale value must be cleared.
	r.emitFragmentation("n1", nil, map[int]bool{}, balloons.CPUAllocationPhysicalCores)
	if n := testutil.CollectAndCount(metrics.CPUFragmentation); n != 0 {
		t.Errorf("cpu_fragmentation series count = %d, want 0 (stale value must clear on lost topology)", n)
	}
}

// twoNumaHTCPUInfo builds a 2-NUMA node with 2 HT cores per NUMA: representatives
// 0,1 (NUMA 0) and 2,3 (NUMA 1), siblings 4,5 and 6,7.
func twoNumaHTCPUInfo() map[int]topology.CPUInfo {
	return map[int]topology.CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0},
		1: {CPU: 1, NUMA: 0, Core: 1},
		2: {CPU: 2, NUMA: 1, Core: 2},
		3: {CPU: 3, NUMA: 1, Core: 3},
		4: {CPU: 4, NUMA: 0, Core: 0},
		5: {CPU: 5, NUMA: 0, Core: 1},
		6: {CPU: 6, NUMA: 1, Core: 2},
		7: {CPU: 7, NUMA: 1, Core: 3},
	}
}

// reconcileMetrics runs one metrics reconcile over cr with cpuInfo cached and,
// when non-nil, the node's pinnable pool.
func reconcileMetrics(t *testing.T, nrt *unstructured.Unstructured, cpuInfo map[int]topology.CPUInfo, pinnable map[int]bool) {
	t.Helper()
	metrics.CPUAssignment.Reset()
	metrics.PoolMembership.Reset()
	metrics.NodeLogicalCPUs.Reset()
	metrics.NodePinnableCPUs.Reset()

	cache := topology.NewCache()
	cache.Set("n1", cpuInfo)
	if pinnable != nil {
		cache.SetPinnable("n1", pinnable)
	}
	r := &NRTMetricsReconciler{
		Client:    fake.NewClientBuilder().WithScheme(driftScheme()).WithRuntimeObjects(nrt).Build(),
		Log:       ctrl.Log.WithName("test"),
		TopoCache: cache,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "n1"}}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
}

// TestReconcile_ExcludedSiblingsAreNotFreeCapacity: under htSiblingPolicy=exclude with
// every representative claimed, free reads nothing, and the webhook's headroom helpers
// (nrt.OccupiedCPUs, topology.FreePerNUMA) agree there is no room.
func TestReconcile_ExcludedSiblingsAreNotFreeCapacity(t *testing.T) {
	cpuInfo := twoNumaHTCPUInfo()
	// Reserved holds CPU 0, workloads 1-3; siblings 4-7 are excluded.
	nrt := nrtObj(
		balloonZone("reserved[0]", "0"),
		balloonZone("balloon-span1[0]", "1"),
		nrtZone("inference/load-tp1-abc/vllm", "balloon-span1[0]", "1"),
		balloonZone("balloon-span2[0]", "2-3"),
		nrtZone("inference/load-tp2-def/vllm", "balloon-span2[0]", "2-3"),
	)
	pinnable := map[int]bool{0: true, 1: true, 2: true, 3: true}

	reconcileMetrics(t, nrt, cpuInfo, pinnable)

	// Total series count, so any free series for an excluded sibling fails.
	if n := testutil.CollectAndCount(metrics.CPUAssignment); n != 4 {
		t.Errorf("cpu_assignment series count = %d, want 4 (the claimed pool only; an "+
			"excluded sibling is claimed by nobody and can be taken by nobody, so it is "+
			"not free capacity)", n)
	}

	// Free is counted against the pool, not the hardware.
	for _, numa := range []string{"0", "1"} {
		if got := testutil.ToFloat64(metrics.NodePinnableCPUs.WithLabelValues("n1", numa)); got != 2 {
			t.Errorf("node_pinnable_cpus{numa=%s} = %v, want 2 (the representative threads)", numa, got)
		}

		if got := testutil.ToFloat64(metrics.NodeLogicalCPUs.WithLabelValues("n1", numa)); got != 4 {
			t.Errorf("node_logical_cpus{numa=%s} = %v, want 4 (hardware inventory must not change meaning)", numa, got)
		}
	}

	// Admission must agree: zero room.
	occupied, ok := nrtpkg.OccupiedCPUs(nrt)
	if !ok {
		t.Fatal("nrt.OccupiedCPUs reported the CR unreadable; the fixture is wrong")
	}
	occ := map[int]bool{}
	for _, cpu := range occupied {
		occ[cpu] = true
	}
	free := topology.FreePerNUMA(cpuInfo, occ, balloons.CPUAllocationPhysicalCores)
	if n := topology.SumFree(free); n != 0 {
		t.Errorf("the headroom definition admission refuses on reports %d free (%v), want 0; "+
			"the metrics and admission must not disagree", n, free)
	}
}

// TestReconcile_DensityModeKeepsSiblingsInThePool: with htSiblingPolicy=reserved the
// siblings are the reserved pool, so they stay in the denominator as claimed.
func TestReconcile_DensityModeKeepsSiblingsInThePool(t *testing.T) {
	cpuInfo := twoNumaHTCPUInfo()
	// Reserved holds the siblings; 0 and 2 are pinned; 1 and 3 are free.
	nrt := nrtObj(
		balloonZone("reserved[0]", "4-7"),
		balloonZone("balloon-span1[0]", "0"),
		nrtZone("inference/demo-abc/vllm", "balloon-span1[0]", "0"),
		balloonZone("balloon-span1[1]", "2"),
		nrtZone("inference/demo-def/vllm", "balloon-span1[1]", "2"),
	)
	pinnable := map[int]bool{}
	for cpu := range cpuInfo {
		pinnable[cpu] = true
	}

	reconcileMetrics(t, nrt, cpuInfo, pinnable)

	if n := testutil.CollectAndCount(metrics.CPUAssignment); n != 8 {
		t.Errorf("cpu_assignment series count = %d, want 8 (6 claimed + 2 free)", n)
	}
	for _, cpu := range []struct{ id, numa, core string }{{"1", "0", "1"}, {"3", "1", "3"}} {
		if got := testutil.ToFloat64(metrics.CPUAssignment.WithLabelValues(
			"n1", cpu.id, cpu.numa, cpu.core, freeBalloonName, "", "")); got != 1 {
			t.Errorf("CPU %s is unclaimed and inside the pool, so it must read free", cpu.id)
		}
	}

	for _, numa := range []string{"0", "1"} {
		pool := testutil.ToFloat64(metrics.NodePinnableCPUs.WithLabelValues("n1", numa))
		logical := testutil.ToFloat64(metrics.NodeLogicalCPUs.WithLabelValues("n1", numa))
		if pool != logical || pool != 4 {
			t.Errorf("numa %s: node_pinnable_cpus = %v, node_logical_cpus = %v, want both 4 "+
				"(density mode excludes nothing)", numa, pool, logical)
		}
	}
}

// TestReconcile_HyperthreadsPoolEqualsLogicalCPUs: in hyperthreads mode the pinnable
// total equals the logical total and every unclaimed CPU reads free.
func TestReconcile_HyperthreadsPoolEqualsLogicalCPUs(t *testing.T) {
	cpuInfo := twoNumaHTCPUInfo()
	nrt := nrtObj(
		balloonZone("balloon-span1[0]", "0"),
		nrtZone("inference/demo-abc/vllm", "balloon-span1[0]", "0"),
	)
	pinnable := map[int]bool{}
	for cpu := range cpuInfo {
		pinnable[cpu] = true
	}

	reconcileMetrics(t, nrt, cpuInfo, pinnable)

	if n := testutil.CollectAndCount(metrics.CPUAssignment); n != 8 {
		t.Errorf("cpu_assignment series count = %d, want 8 (1 claimed + 7 free)", n)
	}
	for _, cpu := range []struct{ id, numa, core string }{{"4", "0", "0"}, {"7", "1", "3"}} {
		if got := testutil.ToFloat64(metrics.CPUAssignment.WithLabelValues(
			"n1", cpu.id, cpu.numa, cpu.core, freeBalloonName, "", "")); got != 1 {
			t.Errorf("CPU %s is allocatable in hyperthreads mode, so it must read free", cpu.id)
		}
	}
	for _, numa := range []string{"0", "1"} {
		pool := testutil.ToFloat64(metrics.NodePinnableCPUs.WithLabelValues("n1", numa))
		logical := testutil.ToFloat64(metrics.NodeLogicalCPUs.WithLabelValues("n1", numa))
		if pool != logical {
			t.Errorf("numa %s: node_pinnable_cpus = %v, want the logical total %v", numa, pool, logical)
		}
	}
}

// TestReconcile_UnresolvedPoolFallsBackToTheHardwareSet: before the first provisioning
// pass free counts every unclaimed CPU and node_pinnable_cpus is absent, so consumers
// fall back to node_logical_cpus.
func TestReconcile_UnresolvedPoolFallsBackToTheHardwareSet(t *testing.T) {
	cpuInfo := twoNumaHTCPUInfo()
	nrt := nrtObj(
		balloonZone("balloon-span1[0]", "0"),
		nrtZone("inference/demo-abc/vllm", "balloon-span1[0]", "0"),
	)

	reconcileMetrics(t, nrt, cpuInfo, nil)

	if n := testutil.CollectAndCount(metrics.CPUAssignment); n != 8 {
		t.Errorf("cpu_assignment series count = %d, want 8 (1 claimed + 7 free)", n)
	}
	if n := testutil.CollectAndCount(metrics.NodePinnableCPUs); n != 0 {
		t.Errorf("node_pinnable_cpus series count = %d, want 0 while the pool is unresolved", n)
	}
}

// TestReconcile_FragmentationFollowsEachNodesOwnMode: identical nodes (one NUMA busy,
// one idle) strand by their own mode: physical-cores 2 cores, hyperthreads 4 threads,
// unresolved no series.
func TestReconcile_FragmentationFollowsEachNodesOwnMode(t *testing.T) {
	metrics.CPUFragmentation.Reset()

	physicalCoresPool := map[int]bool{0: true, 1: true, 2: true, 3: true}
	cases := []struct {
		node     string
		mode     string
		claimed  string // the busy NUMA node's CPUs, in the unit the mode allocates
		pinnable map[int]bool
		series   bool
		want     float64
	}{
		{
			node:    "worker-1",
			mode:    balloons.CPUAllocationPhysicalCores,
			claimed: "2-3",

			pinnable: physicalCoresPool,
			series:   true,
			want:     2,
		},
		{
			node:     "worker-2",
			mode:     cpuAllocationHyperthreads,
			claimed:  "2-3,6-7",
			pinnable: map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true},
			series:   true,
			want:     4,
		},
		{
			node:     "worker-3",
			mode:     "", // no cpu-allocation label: not resolved yet
			claimed:  "2-3",
			pinnable: physicalCoresPool,
		},
		{
			node:     "worker-4",
			mode:     "densest-possible", // labelled with a mode nothing allocates in
			claimed:  "2-3",
			pinnable: physicalCoresPool,
		},
	}

	cache := topology.NewCache()
	var objs []client.Object
	wantSeries := 0
	for _, tc := range cases {
		cache.Set(tc.node, twoNumaHTCPUInfo())
		cache.SetPinnable(tc.node, tc.pinnable)

		node := poolNode(tc.node)
		if tc.mode != "" {
			node.Labels = map[string]string{LabelCPUAllocation: tc.mode}
		}
		if tc.series {
			wantSeries++
		}

		nrt := nrtObj(
			balloonZone("balloon-span1[0]", tc.claimed),
			nrtZone("inference/demo-abc/vllm", "balloon-span1[0]", tc.claimed),
		)
		nrt.SetName(tc.node)
		objs = append(objs, node, nrt)
	}

	r := &NRTMetricsReconciler{
		Client:    fake.NewClientBuilder().WithScheme(driftScheme()).WithObjects(objs...).Build(),
		Log:       ctrl.Log.WithName("test"),
		TopoCache: cache,
	}
	for _, tc := range cases {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: tc.node}}); err != nil {
			t.Fatalf("reconcile %s: %v", tc.node, err)
		}
	}

	// Counted before any per-node read, because reading a series creates it.
	if n := testutil.CollectAndCount(metrics.CPUFragmentation); n != wantSeries {
		t.Errorf("cpu_fragmentation series count = %d, want %d (a node whose mode is not one this "+
			"operator allocates in reports nothing, so the reading stays absent instead of "+
			"asserting a guess)", n, wantSeries)
	}
	for _, tc := range cases {
		if !tc.series {
			continue
		}
		if got := testutil.ToFloat64(metrics.CPUFragmentation.WithLabelValues(tc.node)); got != tc.want {
			t.Errorf("cpu_fragmentation{node=%q} = %v, want %v (stranded in %s)", tc.node, got, tc.want, tc.mode)
		}
	}
}
