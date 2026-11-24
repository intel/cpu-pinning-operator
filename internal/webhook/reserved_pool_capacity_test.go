// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/balloons"
	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/match"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// The fixture node: 64 physical cores on 2 NUMAs, 2 threads per core, reserved pool of
// 16 (ResolveReservedCPUs' 25% clamp), 8 per NUMA. Keep the pool a multiple of the NUMA
// count: the per-NUMA share below is a plain division while ReservedPerNUMA rounds up.
const (
	reservedFixturePhysical      = 64
	reservedFixtureNUMA          = 2
	reservedFixtureThreads       = 2
	reservedFixtureReserved      = 16
	reservedFixturePerNUMAShare  = reservedFixtureReserved / reservedFixtureNUMA // 8
	reservedFixtureHardwareCap   = reservedFixturePhysical / reservedFixtureNUMA // 32
	reservedFixtureUsableCap     = reservedFixtureHardwareCap - reservedFixturePerNUMAShare
	reservedFixtureHTHardwareCap = reservedFixtureHardwareCap * reservedFixtureThreads // 64
	reservedFixtureHTUsableCap   = reservedFixtureHTHardwareCap - reservedFixturePerNUMAShare
	// HT-sibling count stamped in density mode.
	reservedFixtureSiblings = reservedFixturePhysical * (reservedFixtureThreads - 1) // 64
)

// reservedPoolNodeLabels is the fixture node's labels. withReserved false omits the
// reserved-cpus label, as on a node labelled by an operator that did not stamp it.
func reservedPoolNodeLabels(withReserved bool) map[string]string {
	labels := map[string]string{
		controller.LabelBalloonsEnabled: "true",
		controller.LabelPhysicalCPUs:    strconv.Itoa(reservedFixturePhysical),
		controller.LabelNumaNodes:       strconv.Itoa(reservedFixtureNUMA),
		controller.LabelThreadsPerCore:  strconv.Itoa(reservedFixtureThreads),
		controller.LabelMaxSpan:         strconv.Itoa(reservedFixtureNUMA),
	}
	if withReserved {
		labels[controller.LabelReservedCPUs] = strconv.Itoa(reservedFixtureReserved)
	}
	return labels
}

func reservedPoolNode(name string, withReserved bool) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: reservedPoolNodeLabels(withReserved)}}
}

// TestNodeFitsSubtractsReservedPool proves the static capacity check subtracts the
// reserved pool; the no-label rows show raw hardware capacity for comparison.
func TestNodeFitsSubtractsReservedPool(t *testing.T) {
	cases := []struct {
		name          string
		withReserved  bool
		cpuAllocation string
		cores         int
		span          int
		wantFits      bool
		wantCapacity  int
	}{
		// 8 of the 32 cores on a NUMA are the reserved pool's.
		{"32c span1 denied once reserved counts", true, "physical-cores", 32, 1, false, reservedFixtureUsableCap},
		{"32c span1 fits without the reserved label", false, "physical-cores", 32, 1, true, reservedFixtureHardwareCap},

		{"24c span1 still fits", true, "physical-cores", 24, 1, true, reservedFixtureUsableCap},
		{"25c span1 does not", true, "physical-cores", 25, 1, false, reservedFixtureUsableCap},
		// Span divides the demand, not the capacity.
		{"48c span2 fits", true, "physical-cores", 48, 2, true, reservedFixtureUsableCap},
		{"50c span2 does not", true, "physical-cores", 50, 2, false, reservedFixtureUsableCap},
		// The reserved pool is already a logical-CPU count, subtracted unscaled: 64 - 8.
		{"hyperthreads capacity is scaled then reduced", true, "hyperthreads", reservedFixtureHTUsableCap, 1, true, reservedFixtureHTUsableCap},
		{"hyperthreads denies one above that", true, "hyperthreads", reservedFixtureHTUsableCap + 1, 1, false, reservedFixtureHTUsableCap},
		{"hyperthreads without the reserved label", false, "hyperthreads", reservedFixtureHTHardwareCap, 1, true, reservedFixtureHTHardwareCap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := reservedPoolNodeLabels(tc.withReserved)
			fits, capacity := nodeFits(labels, tc.cores, tc.span, tc.cpuAllocation)
			if fits != tc.wantFits || capacity != tc.wantCapacity {
				t.Errorf("nodeFits(%dc, span %d, %s) = (%v, %d), want (%v, %d)",
					tc.cores, tc.span, tc.cpuAllocation, fits, capacity, tc.wantFits, tc.wantCapacity)
			}
		})
	}
}

// TestNodeStaticFreeAgreesWithNodeFits is a drift guard: with no outstanding
// reservations, nodeStaticFree must equal nodeFits' capacity, reserved pool included.
func TestNodeStaticFreeAgreesWithNodeFits(t *testing.T) {
	for _, cpuAllocation := range []string{"physical-cores", "hyperthreads"} {
		for _, withReserved := range []bool{true, false} {
			labels := reservedPoolNodeLabels(withReserved)
			_, wantCapacity := nodeFits(labels, 1, 1, cpuAllocation)
			free, ok := nodeStaticFree(labels, cpuAllocation, nil)
			if !ok {
				t.Fatalf("%s reserved=%v: nodeStaticFree reported no data for a fully labelled node", cpuAllocation, withReserved)
			}
			if len(free) != reservedFixtureNUMA {
				t.Errorf("%s reserved=%v: nodeStaticFree returned %d NUMA nodes, want %d",
					cpuAllocation, withReserved, len(free), reservedFixtureNUMA)
			}
			for numa, got := range free {
				if got != wantCapacity {
					t.Errorf("%s reserved=%v: nodeStaticFree NUMA %d = %d, nodeFits capacity = %d",
						cpuAllocation, withReserved, numa, got, wantCapacity)
				}
			}
		}
	}
}

// TestStaticFreeAgreesWithLiveFreeOnReservedPool proves the cold-start and live paths
// report the same room, for both a resolved pool and a density (sibling) pool. The live
// side reads a real reserved balloon zone through liveFreeFromNRT.
func TestStaticFreeAgreesWithLiveFreeOnReservedPool(t *testing.T) {
	const nodeName = "live-and-static"
	cases := []struct {
		name            string
		labels          map[string]string
		reservedCPUSet  string
		htSiblingPolicy string
		wantDensity     bool
		modes           []string
	}{
		{
			name: "resolved pool", labels: reservedPoolNodeLabels(true),
			reservedCPUSet: reservedPoolCPUSet(), htSiblingPolicy: balloons.HTSiblingPolicyExclude,
			modes: []string{"physical-cores", "hyperthreads"},
		},
		{
			name: "density pool (the HT siblings)", labels: densityNodeLabels(),
			reservedCPUSet: densitySiblingCPUSet(), htSiblingPolicy: balloons.HTSiblingPolicyReserved,
			wantDensity: true, modes: []string{"physical-cores"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			topo := topology.NewCache()
			topo.Set(nodeName, reservedPoolTopology())
			m := &PodMutator{TopoCache: topo, PolicyIndex: densityIndex(tc.htSiblingPolicy)}
			if got := m.densityNode(tc.labels); got != tc.wantDensity {
				t.Fatalf("densityNode = %v, want %v", got, tc.wantDensity)
			}
			// "reserved[0]" exercises OccupiedCPUs stripping the instance suffix.
			nrt := nrtObj(nodeName, nrtZone(testBalloonZoneType, "reserved[0]", "", tc.reservedCPUSet))

			for _, cpuAllocation := range tc.modes {
				live, ok := m.liveFreeFromNRT(nodeName, nrt, cpuAllocation, m.densityNode(tc.labels))
				if !ok {
					t.Fatalf("%s: liveFreeFromNRT reported no data", cpuAllocation)
				}
				static, ok := nodeStaticFree(tc.labels, cpuAllocation, nil)
				if !ok {
					t.Fatalf("%s: nodeStaticFree reported no data for a fully labelled node", cpuAllocation)
				}
				if len(live) != len(static) {
					t.Fatalf("%s: live free covers %d NUMA nodes, static %d", cpuAllocation, len(live), len(static))
				}
				for numa, staticFree := range static {
					if live[numa] != staticFree {
						t.Errorf("%s: NUMA %d live free %d != static free %d — a pod would be admitted at cold start and denied once NRT publishes",
							cpuAllocation, numa, live[numa], staticFree)
					}
				}
			}
		})
	}
}

// densityNodeLabels is the fixture node as labelled in density mode: reserved-cpus
// carries the HT-sibling count.
func densityNodeLabels() map[string]string {
	labels := reservedPoolNodeLabels(false)
	labels[controller.LabelReservedCPUs] = strconv.Itoa(reservedFixtureSiblings)
	labels[controller.LabelCPUAllocation] = balloons.CPUAllocationPhysicalCores
	return labels
}

// densitySiblingCPUSet is the reserved cpuset in density mode: threads 64-127.
func densitySiblingCPUSet() string {
	return fmt.Sprintf("%d-%d", reservedFixturePhysical, reservedFixturePhysical*reservedFixtureThreads-1)
}

// densityIndex is a match Index carrying only the cluster-global htSiblingPolicy.
func densityIndex(htSiblingPolicy string) *match.Index {
	idx := match.NewIndex()
	idx.SetClusterConfig(match.ClusterConfig{HTSiblingPolicy: htSiblingPolicy})
	return idx
}

// TestDensityModeLiveFreeReportsRepresentativeThreads proves a density node's live free
// count is its representative threads, not the 0 the sibling-exclude rule reads.
func TestDensityModeLiveFreeReportsRepresentativeThreads(t *testing.T) {
	const nodeName = "density-live"
	topo := topology.NewCache()
	topo.Set(nodeName, reservedPoolTopology())
	m := &PodMutator{TopoCache: topo, PolicyIndex: densityIndex(balloons.HTSiblingPolicyReserved)}
	nrt := nrtObj(nodeName, nrtZone(testBalloonZoneType, "reserved[0]", "", densitySiblingCPUSet()))

	for _, tc := range []struct {
		density bool
		want    int
	}{
		{density: false, want: 0},                         // the defect: the exclude rule on a density node
		{density: true, want: reservedFixtureHardwareCap}, // what the node offers
	} {
		free, ok := m.liveFreeFromNRT(nodeName, nrt, "physical-cores", tc.density)
		if !ok {
			t.Fatalf("density=%v: liveFreeFromNRT reported no data", tc.density)
		}
		if len(free) != reservedFixtureNUMA {
			t.Fatalf("density=%v: live free covers %d NUMA nodes, want %d", tc.density, len(free), reservedFixtureNUMA)
		}
		for numa, got := range free {
			if got != tc.want {
				t.Errorf("density=%v: NUMA %d live free %d, want %d", tc.density, numa, got, tc.want)
			}
		}
	}
}

// TestDensityRuleRequiresTheReservedPoolToBeTheSiblings proves the density reading is
// used only when the reserved balloon holds exactly the siblings; otherwise the sibling
// rule applies rather than over-stating room.
func TestDensityRuleRequiresTheReservedPoolToBeTheSiblings(t *testing.T) {
	const nodeName = "density-unconfirmed"
	topo := topology.NewCache()
	topo.Set(nodeName, reservedPoolTopology())
	m := &PodMutator{TopoCache: topo, PolicyIndex: densityIndex(balloons.HTSiblingPolicyReserved)}
	// NUMA 0's siblings only: cores 0..31 keep an occupied sibling under the sibling rule.
	halfTheSiblings := fmt.Sprintf("%d-%d", reservedFixturePhysical,
		reservedFixturePhysical+reservedFixtureHardwareCap-1)

	for _, tc := range []struct {
		name   string
		cpuset string
		want   map[int]int
	}{
		{"the sibling set", densitySiblingCPUSet(),
			map[int]int{0: reservedFixtureHardwareCap, 1: reservedFixtureHardwareCap}},
		{"a resolved pool of representative threads", reservedPoolCPUSet(),
			map[int]int{0: reservedFixtureUsableCap, 1: reservedFixtureUsableCap}},
		{"a partial claim", halfTheSiblings,
			map[int]int{0: 0, 1: reservedFixtureHardwareCap}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nrt := nrtObj(nodeName, nrtZone(testBalloonZoneType, "reserved[0]", "", tc.cpuset))
			free, ok := m.liveFreeFromNRT(nodeName, nrt, "physical-cores", true)
			if !ok {
				t.Fatal("liveFreeFromNRT reported no data")
			}
			if !reflect.DeepEqual(free, tc.want) {
				t.Errorf("live free %v, want %v", free, tc.want)
			}
		})
	}
}

// TestDensityModeHyperthreadsUnitReadsRepresentativeThreads pins the helpers' readings for
// a density node in the hyperthreads unit: live reports representative threads, static
// the full logical complement. No caller pairs them, so they are paired here directly.
func TestDensityModeHyperthreadsUnitReadsRepresentativeThreads(t *testing.T) {
	const nodeName = "density-hyperthreads"
	topo := topology.NewCache()
	topo.Set(nodeName, reservedPoolTopology())
	m := &PodMutator{TopoCache: topo, PolicyIndex: densityIndex(balloons.HTSiblingPolicyReserved)}
	nrt := nrtObj(nodeName, nrtZone(testBalloonZoneType, "reserved[0]", "", densitySiblingCPUSet()))
	labels := densityNodeLabels()

	live, ok := m.liveFreeFromNRT(nodeName, nrt, "hyperthreads", m.densityNode(labels))
	if !ok {
		t.Fatal("liveFreeFromNRT reported no data")
	}
	if len(live) != reservedFixtureNUMA {
		t.Fatalf("live free covers %d NUMA nodes, want %d", len(live), reservedFixtureNUMA)
	}
	for numa, got := range live {
		if got != reservedFixtureHardwareCap {
			t.Errorf("NUMA %d live free %d logical CPUs, want the %d representative threads",
				numa, got, reservedFixtureHardwareCap)
		}
	}
	static, ok := nodeStaticFree(labels, "hyperthreads", nil)
	if !ok {
		t.Fatal("nodeStaticFree reported no data for a fully labelled node")
	}
	for numa, got := range static {
		if got != reservedFixtureHTHardwareCap {
			t.Errorf("NUMA %d static free %d, want the full logical complement %d (the unit mismatch, unchanged here)",
				numa, got, reservedFixtureHTHardwareCap)
		}
	}
}

// TestEnforceAdmitsPinnedPodOnDensityNode proves enforce admits a pinned pod on a density
// node with live NRT data, while the second row still denies and names the live reading.
func TestEnforceAdmitsPinnedPodOnDensityNode(t *testing.T) {
	const nodeName = "density-enforce"
	for _, tc := range []struct {
		cores      int
		wantAllow  bool
		wantDetail string
	}{
		{cores: reservedFixtureHardwareCap, wantAllow: true},
		{cores: reservedFixtureHardwareCap + 1, wantAllow: false,
			wantDetail: fmt.Sprintf("live per-NUMA free of %d", reservedFixtureHardwareCap)},
	} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: densityNodeLabels()}}
		nrt := nrtObj(nodeName, nrtZone(testBalloonZoneType, "reserved[0]", "", densitySiblingCPUSet()))
		topo := topology.NewCache()
		topo.Set(nodeName, reservedPoolTopology())
		m, rec := newHandleMutatorFromClient(t, "enforce", nrtClient(node, nrt), topo)
		m.PolicyIndex.SetClusterConfig(match.ClusterConfig{HTSiblingPolicy: balloons.HTSiblingPolicyReserved})

		resp := admitCustomPod(t, m, matchedPod("vllm-density", nodeName, strconv.Itoa(tc.cores), "vllm"))
		if resp.Allowed != tc.wantAllow {
			t.Fatalf("%d cores on a density node: allowed=%v, want %v (message %q)",
				tc.cores, resp.Allowed, tc.wantAllow, resp.Result.Message)
		}
		// An admitted-but-unpinned pod carries no patch.
		if tc.wantAllow && len(resp.Patches) == 0 {
			t.Errorf("%d cores: admitted without a pinning patch: %+v", tc.cores, resp.Result)
		}
		if tc.wantDetail != "" && !eventsContain(drainEvents(rec), "CapacityCheckFailed", tc.wantDetail) {
			t.Errorf("%d cores: no CapacityCheckFailed event reporting %q", tc.cores, tc.wantDetail)
		}
	}
}

// nrtClient is a fake client that also serves NodeResourceTopology CRs.
func nrtClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// reservedPoolTopology builds the fixture topology: representative threads 0..63 (NUMA 0
// the lower half), siblings 64..127.
func reservedPoolTopology() map[int]topology.CPUInfo {
	const coresPerNUMA = reservedFixturePhysical / reservedFixtureNUMA
	cpuInfo := make(map[int]topology.CPUInfo, reservedFixturePhysical*reservedFixtureThreads)
	for core := 0; core < reservedFixturePhysical; core++ {
		numa := core / coresPerNUMA
		cpuInfo[core] = topology.CPUInfo{CPU: core, NUMA: numa, Core: core}
		sibling := core + reservedFixturePhysical
		cpuInfo[sibling] = topology.CPUInfo{CPU: sibling, NUMA: numa, Core: core}
	}
	return cpuInfo
}

// reservedPoolCPUSet is the reserved balloon's cpuset: the lowest representative threads
// of each NUMA, an equal number per NUMA.
func reservedPoolCPUSet() string {
	const coresPerNUMA = reservedFixturePhysical / reservedFixtureNUMA
	var parts []string
	for numa := 0; numa < reservedFixtureNUMA; numa++ {
		first := numa * coresPerNUMA
		parts = append(parts, fmt.Sprintf("%d-%d", first, first+reservedFixturePerNUMAShare-1))
	}
	return strings.Join(parts, ",")
}

// TestEnforceDeniesRequestThatOnlyFitsTheReservedPool proves enforce denies a request that
// fits the hardware share but not what the reserved pool leaves, and allows it without
// the label.
func TestEnforceDeniesRequestThatOnlyFitsTheReservedPool(t *testing.T) {
	// One NUMA's whole hardware share.
	const cores = "32"

	m, rec := newHandleMutator(t, "enforce", reservedPoolNode("with-pool", true))
	resp := admitCustomPod(t, m, matchedPod("vllm-denied", "", cores, "vllm"))
	if resp.Allowed {
		t.Errorf("a %s-core request must be DENIED on a node whose reserved pool holds 8 of the 32 cores per NUMA node", cores)
	}
	// The response withholds node detail, so the capacity gate is proven via the event.
	wantDetail := "exceeds every node's per-NUMA capacity (best " + strconv.Itoa(reservedFixtureUsableCap)
	events := drainEvents(rec)
	if !eventsContain(events, "CapacityCheckFailed", wantDetail) {
		t.Errorf("no CapacityCheckFailed event reporting %q; events: %v", wantDetail, events)
	}

	mNoLabel, _ := newHandleMutator(t, "enforce", reservedPoolNode("no-pool-label", false))
	respNoLabel := admitCustomPod(t, mNoLabel, matchedPod("vllm-admitted", "", cores, "vllm"))
	if !respNoLabel.Allowed {
		t.Fatalf("the no-reserved-label control must be ADMITTED, got denied: %q", respNoLabel.Result.Message)
	}
}

// TestEnforceStillAdmitsWhatTheReservedPoolLeaves proves a request fitting the room the
// pool leaves is still admitted.
func TestEnforceStillAdmitsWhatTheReservedPoolLeaves(t *testing.T) {
	m, _ := newHandleMutator(t, "enforce", reservedPoolNode("with-pool", true))
	resp := admitCustomPod(t, m, matchedPod("vllm-fits", "", strconv.Itoa(reservedFixtureUsableCap), "vllm"))
	if !resp.Allowed {
		t.Fatalf("a %d-core request fits what the reserved pool leaves and must be admitted, got: %q",
			reservedFixtureUsableCap, resp.Result.Message)
	}
}

// TestPreScheduledDeniedAgainstReservedPool proves the pre-scheduled path
// (nodeStaticFree) reaches the same verdict as nodeFits.
func TestPreScheduledDeniedAgainstReservedPool(t *testing.T) {
	m, rec := newHandleMutator(t, "enforce", reservedPoolNode("bound-node", true))
	resp := admitCustomPod(t, m, matchedPod("vllm-bound", "bound-node", "32", "vllm"))
	if resp.Allowed {
		t.Error("a pre-scheduled 32-core request must be denied against the same reserved-pool-aware capacity")
	}
	// This wording is nodeStaticFree's, proving which caller produced the verdict.
	wantDetail := "per-NUMA capacity of " + strconv.Itoa(reservedFixtureUsableCap) +
		" once outstanding reservations are counted"
	if !eventsContain(drainEvents(rec), "CapacityCheckFailed", wantDetail) {
		t.Errorf("no CapacityCheckFailed event from nodeStaticFree reporting %q", wantDetail)
	}
}

// eventsContain reports whether any event carries both substrings.
func eventsContain(events []string, reason, detail string) bool {
	for _, e := range events {
		if strings.Contains(e, reason) && strings.Contains(e, detail) {
			return true
		}
	}
	return false
}

// TestDensityModeReservedPoolIsNotSubtracted proves a density-mode reserved-cpus label
// (the HT-sibling count) is discarded rather than subtracted, which would zero capacity.
// A uniform node stamps exactly its core count, hence the `>=` discard.
func TestDensityModeReservedPoolIsNotSubtracted(t *testing.T) {
	// Also swept in hyperthreads mode, the only mode that reads threads-per-core.
	for _, siblings := range []int{reservedFixturePhysical, reservedFixturePhysical - 2} {
		for _, threads := range []string{strconv.Itoa(reservedFixtureThreads), "", "garbage"} {
			labels := reservedPoolNodeLabels(false)
			labels[controller.LabelReservedCPUs] = strconv.Itoa(siblings)
			if threads == "" {
				delete(labels, controller.LabelThreadsPerCore)
			} else {
				labels[controller.LabelThreadsPerCore] = threads
			}

			wantHT := reservedFixtureHardwareCap
			if threads == strconv.Itoa(reservedFixtureThreads) {
				wantHT = reservedFixtureHTHardwareCap
			}

			for cpuAllocation, want := range map[string]int{
				"physical-cores": reservedFixtureHardwareCap,
				"hyperthreads":   wantHT,
			} {
				fits, capacity := nodeFits(labels, want, 1, cpuAllocation)
				if !fits || capacity != want {
					t.Errorf("density-mode node (siblings %d, threads-per-core %q, %s) => (fits=%v, cap=%d), want (true, %d): the sibling pool must not erase capacity",
						siblings, threads, cpuAllocation, fits, capacity, want)
				}
			}
		}
	}
}

// TestReservedShareIsJudgedTheSameInBothModes proves a reserved label's credibility does
// not depend on allocation mode (found by FuzzNodeCapacityLabels).
func TestReservedShareIsJudgedTheSameInBothModes(t *testing.T) {
	labels := map[string]string{
		controller.LabelPhysicalCPUs:   "140", // 70 cores per NUMA node
		controller.LabelNumaNodes:      "2",
		controller.LabelThreadsPerCore: "2",
		controller.LabelReservedCPUs:   "200", // a 100-CPU share: more cores than a NUMA node has
	}
	_, physCap := nodeFits(labels, 1, 1, "physical-cores")
	_, htCap := nodeFits(labels, 1, 1, "hyperthreads")
	if physCap != 70 || htCap != 140 {
		t.Errorf("capacity = (physical-cores %d, hyperthreads %d), want (70, 140): an implausible share must be discarded in both modes",
			physCap, htCap)
	}
	if htCap < physCap {
		t.Errorf("hyperthreads capacity %d is below physical-cores capacity %d for one node", htCap, physCap)
	}
}

// TestUnusableReservedLabelFailsOpen proves an unreadable reserved-cpus label keeps the
// node's hardware capacity instead of zeroing it.
func TestUnusableReservedLabelFailsOpen(t *testing.T) {
	cases := []struct {
		reserved string
		branch   string
	}{
		{"", "no label at all — intLabel reads 0, nothing is subtracted"},
		{"-16", "non-positive — intLabel reads 0"},
		{"not-a-number", "unparseable — intLabel reads 0"},
		{"1000", "parseable but far above the clamp — the share is discarded"},
		{"9223372036854775807", "saturating — the share must not wrap into capacity"},
	}
	for _, tc := range cases {
		labels := reservedPoolNodeLabels(false)
		labels[controller.LabelThreadsPerCore] = "1" // no hyperthreads to hide behind
		if tc.reserved != "" {
			labels[controller.LabelReservedCPUs] = tc.reserved
		}

		fits, capacity := nodeFits(labels, reservedFixtureHardwareCap, 1, "physical-cores")
		if !fits || capacity != reservedFixtureHardwareCap {
			t.Errorf("reserved-cpus=%q (%s) => (fits=%v, cap=%d), want (true, %d)",
				tc.reserved, tc.branch, fits, capacity, reservedFixtureHardwareCap)
		}
	}
}

// TestEveryResolvableReservedPoolLeavesRoom proves every pool ResolveReservedCPUs can
// produce leaves per-NUMA capacity above zero and below the hardware share, across
// resolve and read modes, which may differ. With one core per NUMA the pool claims the
// only core and is discarded, so the "subtracted" check skips those shapes.
func TestEveryResolvableReservedPoolLeavesRoom(t *testing.T) {
	specs := map[string]*cpopv1alpha1.ReservedSizingSpec{
		"default":     nil,
		"cpus 12":     {Cpus: ptrInt(12)},
		"cpus 64":     {Cpus: ptrInt(64)},
		"percent 25":  {Percent: ptrInt(25)},
		"percent 100": {Percent: ptrInt(100)},
		// Small enough to stay under the clamp.
		"measured light": {Measured: &cpopv1alpha1.MeasuredSizingSpec{}},
		"measured heavy": {Measured: &cpopv1alpha1.MeasuredSizingSpec{}},
	}
	for _, physical := range []int{18, 32, 64, 96, 128, 256} {
		for _, numa := range []int{1, 2, 4, 8, 16} {
			for _, threads := range []int{1, 2, 4} {
				for _, resolvedPhysCores := range []bool{true, false} {
					for _, cpuAllocation := range []string{"physical-cores", "hyperthreads"} {
						for name, spec := range specs {
							usage := physical / 8
							if name == "measured heavy" {
								usage = physical
							}
							reserved := balloons.ResolveReservedCPUs(spec, physical, threads, usage, resolvedPhysCores)
							labels := map[string]string{
								controller.LabelPhysicalCPUs:   strconv.Itoa(physical),
								controller.LabelNumaNodes:      strconv.Itoa(numa),
								controller.LabelThreadsPerCore: strconv.Itoa(threads),
								controller.LabelReservedCPUs:   strconv.Itoa(reserved),
							}
							_, hardware := nodeFits(reservedPoolLabelsWithout(labels, controller.LabelReservedCPUs), 1, 1, cpuAllocation)
							fits, capacity := nodeFits(labels, 1, 1, cpuAllocation)
							where := fmt.Sprintf("%s, phys=%d numa=%d threads=%d resolved-physcores=%v read-as-%s",
								name, physical, numa, threads, resolvedPhysCores, cpuAllocation)
							switch {
							case capacity <= 0 || !fits:
								t.Errorf("%s: pool of %d left per-NUMA capacity %d", where, reserved, capacity)
							case reserved > 0 && physical/numa > 1 && capacity >= hardware:
								t.Errorf("%s: pool of %d was not subtracted (capacity %d, hardware %d)",
									where, reserved, capacity, hardware)
							}
						}
					}
				}
			}
		}
	}
}

func ptrInt(n int) *int { return &n }

// reservedPoolLabelsWithout copies labels less one key.
func reservedPoolLabelsWithout(labels map[string]string, drop string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if k != drop {
			out[k] = v
		}
	}
	return out
}
