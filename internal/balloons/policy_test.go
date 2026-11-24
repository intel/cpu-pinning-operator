// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package balloons

import (
	"sort"
	"strconv"
	"strings"
	"testing"

	nriv1alpha1 "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"
	nripolicy "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	nriballoons "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/balloons"

	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// htTopo builds a 2-NUMA, 2-core-per-NUMA, 2-thread topology. Representative
// (lowest-id) thread per core = 0,1,2,3; higher-id siblings = 4,5,6,7.
func htTopo() *topology.NodeTopology {
	return &topology.NodeTopology{
		PkgNode: map[int][]int{0: {0}, 1: {1}},
		CPUs: map[int]topology.CPUInfo{
			0: {CPU: 0, NUMA: 0, Core: 0},
			1: {CPU: 1, NUMA: 0, Core: 1},
			2: {CPU: 2, NUMA: 1, Core: 2},
			3: {CPU: 3, NUMA: 1, Core: 3},
			4: {CPU: 4, NUMA: 0, Core: 0},
			5: {CPU: 5, NUMA: 0, Core: 1},
			6: {CPU: 6, NUMA: 1, Core: 2},
			7: {CPU: 7, NUMA: 1, Core: 3},
		},
	}
}

func TestSiblingCPUs(t *testing.T) {
	// higher-id siblings 4,5,6,7 collapse to a single range.
	if got := formatCPUSet(siblingCPUs(htTopo().CPUs)); got != "4-7" {
		t.Errorf("siblingCPUs = %q, want %q", got, "4-7")
	}
	// no hyperthreading (cpu == core everywhere) -> nothing to exclude.
	noHT := map[int]topology.CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0},
		1: {CPU: 1, NUMA: 0, Core: 1},
	}
	if got := siblingCPUs(noHT); len(got) != 0 {
		t.Errorf("siblingCPUs(no HT) = %v, want empty", got)
	}
}

func TestFormatCPUSet(t *testing.T) {
	cases := []struct {
		in   []int
		want string
	}{
		{[]int{4, 5, 6, 7}, "4-7"},
		{[]int{3, 5, 7}, "3,5,7"},
		{[]int{1, 2, 3, 8, 9, 20}, "1-3,8-9,20"},
		{[]int{64}, "64"},
	}
	for _, c := range cases {
		if got := formatCPUSet(c.in); got != c.want {
			t.Errorf("formatCPUSet(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// wideTopo builds a homogeneous 2-NUMA, 4-core-per-NUMA, 2-thread topology.
// Representative (lowest-id) threads per core are 0-3 on NUMA 0 and 4-7 on
// NUMA 1; higher-id siblings are 8-11 (NUMA 0) and 12-15 (NUMA 1).
func wideTopo() *topology.NodeTopology {
	cpus := map[int]topology.CPUInfo{}
	// First threads (representatives): 0-3 on NUMA 0, 4-7 on NUMA 1.
	for cpu := 0; cpu < 8; cpu++ {
		numa := cpu / 4
		cpus[cpu] = topology.CPUInfo{CPU: cpu, NUMA: numa, Core: cpu}
	}
	// Sibling threads share the physical core of the representative 8 lower.
	for cpu := 8; cpu < 16; cpu++ {
		core := cpu - 8
		cpus[cpu] = topology.CPUInfo{CPU: cpu, NUMA: core / 4, Core: core}
	}
	return &topology.NodeTopology{
		PkgNode: map[int][]int{0: {0}, 1: {1}},
		CPUs:    cpus,
	}
}

// numaTopo builds numaNodes NUMA nodes of coresPerNUMA cores, no HT; NUMA n owns
// cpus [n*coresPerNUMA, (n+1)*coresPerNUMA).
func numaTopo(numaNodes int) *topology.NodeTopology {
	const coresPerNUMA = 4
	cpus := map[int]topology.CPUInfo{}
	pkg := map[int][]int{0: []int{}}
	for numa := 0; numa < numaNodes; numa++ {
		for c := 0; c < coresPerNUMA; c++ {
			cpu := numa*coresPerNUMA + c
			cpus[cpu] = topology.CPUInfo{CPU: cpu, NUMA: numa, Core: cpu}
		}
		pkg[0] = append(pkg[0], numa)
	}
	return &topology.NodeTopology{PkgNode: pkg, CPUs: cpus}
}

func TestReservedCPUSet(t *testing.T) {
	// singleNUMATopo builds a homogeneous single-NUMA node whose declared package
	// topology (PkgNode) matches the CPUINFO-populated CPUs.
	singleNUMATopo := func() *topology.NodeTopology {
		return &topology.NodeTopology{
			PkgNode: map[int][]int{0: {0}},
			CPUs: map[int]topology.CPUInfo{
				0: {CPU: 0, NUMA: 0, Core: 0},
				1: {CPU: 1, NUMA: 0, Core: 1},
				2: {CPU: 2, NUMA: 0, Core: 0},
				3: {CPU: 3, NUMA: 0, Core: 1},
			},
		}
	}
	// heteroTopo declares 2 NUMA nodes and populates both, but NUMA 0 exposes 2
	// physical cores and NUMA 1 only 1 → irregular per-NUMA core counts.
	heteroTopo := func() *topology.NodeTopology {
		return &topology.NodeTopology{
			PkgNode: map[int][]int{0: {0}, 1: {1}},
			CPUs: map[int]topology.CPUInfo{
				0: {CPU: 0, NUMA: 0, Core: 0},
				1: {CPU: 1, NUMA: 0, Core: 1},
				2: {CPU: 2, NUMA: 1, Core: 2},
			},
		}
	}
	// partialTopo declares two NUMA nodes but CPUINFO lists only NUMA 0: must fall back to the bare count.
	partialTopo := func() *topology.NodeTopology {
		return &topology.NodeTopology{
			NumaNodes: 2,
			PkgNode:   map[int][]int{0: {0}, 1: {1}},
			CPUs: map[int]topology.CPUInfo{
				0: {CPU: 0, NUMA: 0, Core: 0},
				1: {CPU: 1, NUMA: 0, Core: 1},
			},
		}
	}
	cases := []struct {
		name string
		topo *topology.NodeTopology
		// numaNodes is the NUMA count (0 for the bare-count fallback), for the rounding check.
		numaNodes int
		count     int
		want      string
	}{
		// 2-NUMA × 4-core: count=4 → 2 lowest reps per NUMA (0,1 and 4,5).
		{"balanced 4 across 2 NUMA", wideTopo(), 2, 4, "0-1,4-5"},
		// count=2 → one lowest rep from each NUMA.
		{"balanced 2 across 2 NUMA", wideTopo(), 2, 2, "0,4"},
		// An odd count rounds up to a NUMA multiple (3 → 4).
		{"odd count 3 rounds up on 2 NUMA", wideTopo(), 2, 3, "0-1,4-5"},
		// count equal to the full representative pool selects every rep.
		{"all 8 reps", wideTopo(), 2, 8, "0-7"},
		// Single-NUMA homogeneous node: lowest reps, siblings avoided.
		{"single NUMA count 2", singleNUMATopo(), 1, 2, "0-1"},
		// 4-NUMA, count=6: rounds up to 8 (2/2/2/2).
		{"count 6 rounds up to 2 per NUMA on 4 NUMA", numaTopo(4), 4, 6, "0-1,4-5,8-9,12-13"},
		// 4-NUMA, count=7: also rounds up to 8 (2 per NUMA).
		{"count 7 rounds up to 2 per NUMA on 4 NUMA", numaTopo(4), 4, 7, "0-1,4-5,8-9,12-13"},
		// 4-NUMA, count=8 already divides evenly → 2 per NUMA, footprint unchanged.
		{"count 8 divides evenly on 4 NUMA", numaTopo(4), 4, 8, "0-1,4-5,8-9,12-13"},
		// 4-NUMA, count=10: rounds up to 12 → a symmetric 3/3/3/3 split.
		{"count 10 rounds up to 3 per NUMA on 4 NUMA", numaTopo(4), 4, 10, "0-2,4-6,8-10,12-14"},
		// 3-NUMA, count=5: rounds up to 6 → 2 per NUMA.
		{"count 5 rounds up to 2 per NUMA on 3 NUMA", numaTopo(3), 3, 5, "0-1,4-5,8-9"},
		// Zero / negative count → no cpuset (caller keeps bare count).
		{"zero count", wideTopo(), 0, 0, ""},
		// Empty topology → unknown → bare count fallback.
		{"empty topology", &topology.NodeTopology{}, 0, 4, ""},
		// Heterogeneous: NUMA 0 has 2 reps, NUMA 1 has 1 rep → bare count.
		{"heterogeneous per-NUMA core counts", heteroTopo(), 0, 2, ""},
		// Partial CPUINFO → bare count.
		{"declared NUMA exceeds populated NUMA", partialTopo(), 0, 2, ""},
		// Not enough representative cores even after rounding up → bare count.
		// count=9 on a 2-NUMA×4-core node rounds to 10 (5 per NUMA) > 4 available.
		{"count exceeds available reps", wideTopo(), 0, 9, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reservedCPUSet(tc.topo, tc.count)
			if got != tc.want {
				t.Fatalf("reservedCPUSet(count=%d) = %q, want %q", tc.count, got, tc.want)
			}
			if got == "" {
				return
			}
			// The cpuset is count rounded up to the next NUMA multiple.
			n := countCPUSet(t, got)
			if tc.numaNodes > 0 && n%tc.numaNodes != 0 {
				t.Errorf("reservedCPUSet emitted %d CPUs, not a multiple of %d NUMA nodes (%q)", n, tc.numaNodes, got)
			}
			if n < tc.count || n >= tc.count+tc.numaNodes {
				t.Errorf("reservedCPUSet emitted %d CPUs, want count=%d rounded up to a multiple of %d NUMA nodes (%q)", n, tc.count, tc.numaNodes, got)
			}
		})
	}
}

// countCPUSet counts the CPU ids in a compact cpuset string.
func countCPUSet(t *testing.T, cset string) int {
	t.Helper()
	n := 0
	for _, part := range strings.Split(cset, ",") {
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			l, err1 := strconv.Atoi(lo)
			h, err2 := strconv.Atoi(hi)
			if err1 != nil || err2 != nil {
				t.Fatalf("bad cpuset range %q", part)
			}
			if l < 0 {
				t.Fatalf("negative cpuset range bound %q", part)
			}
			if h < l {
				t.Fatalf("inverted cpuset range %q (hi %d < lo %d)", part, h, l)
			}
			n += h - l + 1
		} else {
			id, err := strconv.Atoi(part)
			if err != nil {
				t.Fatalf("bad cpuset id %q", part)
			}
			if id < 0 {
				t.Fatalf("negative cpuset id %q", part)
			}
			n++
		}
	}
	return n
}

// On a homogeneous node the reserved pool is emitted as an explicit,
// NUMA-balanced cpuset rather than a bare count.
func TestRenderNodePolicy_ReservedHomogeneousCPUSet(t *testing.T) {
	topo := wideTopo()
	defs := GenerateBalloonDefs(topo.PkgNode, topo.MaxSpan, "physical-cores")
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, defs, 4, "physical-cores", "exclude", nil)

	amount, kind := policy.Spec.Config.ReservedResources.Get(nripolicy.CPU)
	if kind != nripolicy.AmountCPUSet {
		t.Fatalf("reservedResources CPU kind = %v, want AmountCPUSet", kind)
	}
	if string(amount) != "0-1,4-5" {
		t.Errorf("reserved cpuset = %q, want %q", amount, "0-1,4-5")
	}
}

// The CR uses the caller's namespace and the nri-plugins name node.<nodeName>.
func TestRenderNodePolicy_NamespaceIsCallerSupplied(t *testing.T) {
	topo := wideTopo()
	for _, ns := range []string{DefaultBalloonsNamespace, "cpu-pinning-operator-system"} {
		policy := RenderNodePolicy("worker-1", ns, topo, nil, 4, "physical-cores", "exclude", nil)
		if policy.Namespace != ns {
			t.Errorf("policy namespace = %q, want %q", policy.Namespace, ns)
		}
		if policy.Name != "node.worker-1" {
			t.Errorf("policy name = %q, want %q", policy.Name, "node.worker-1")
		}
	}
}

// An evenly dividing count is not inflated: 6 on 2 NUMA nodes is 3 each.
func TestRenderNodePolicy_ReservedCPUSetDivisibleCount(t *testing.T) {
	topo := wideTopo()
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 6, "physical-cores", "exclude", nil)

	amount, kind := policy.Spec.Config.ReservedResources.Get(nripolicy.CPU)
	if kind != nripolicy.AmountCPUSet {
		t.Fatalf("reservedResources CPU kind = %v, want AmountCPUSet", kind)
	}
	if n := countCPUSet(t, string(amount)); n != 6 {
		t.Errorf("reserved cpuset %q names %d CPUs, want exactly 6", amount, n)
	}
}

// An uneven count rounds up: 6 on 4 NUMA nodes becomes 8 (2/2/2/2).
func TestRenderNodePolicy_ReservedCPUSetRoundsUpForNUMABalance(t *testing.T) {
	topo := numaTopo(4)
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 6, "physical-cores", "exclude", nil)

	amount, kind := policy.Spec.Config.ReservedResources.Get(nripolicy.CPU)
	if kind != nripolicy.AmountCPUSet {
		t.Fatalf("reservedResources CPU kind = %v, want AmountCPUSet", kind)
	}
	if string(amount) != "0-1,4-5,8-9,12-13" {
		t.Errorf("reserved cpuset = %q, want %q (2 per NUMA, rounded up from 6)", amount, "0-1,4-5,8-9,12-13")
	}
}

// On a heterogeneous or unknown topology the reserved pool stays a bare count.
func TestRenderNodePolicy_ReservedHeterogeneousBareCount(t *testing.T) {
	topo := &topology.NodeTopology{
		PkgNode: map[int][]int{0: {0}, 1: {1}},
		// NUMA 0 exposes 2 physical cores, NUMA 1 only 1 → irregular.
		CPUs: map[int]topology.CPUInfo{
			0: {CPU: 0, NUMA: 0, Core: 0},
			1: {CPU: 1, NUMA: 0, Core: 1},
			2: {CPU: 2, NUMA: 1, Core: 2},
		},
	}
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 2, "physical-cores", "exclude", nil)

	amount, kind := policy.Spec.Config.ReservedResources.Get(nripolicy.CPU)
	if kind != nripolicy.AmountQuantity {
		t.Fatalf("reservedResources CPU kind = %v, want AmountQuantity (bare count)", kind)
	}
	if string(amount) != "2" {
		t.Errorf("reserved bare count = %q, want %q", amount, "2")
	}
}

// Hyperthreads mode also gets the NUMA-balanced reserved cpuset.
func TestRenderNodePolicy_ReservedCPUSetInHyperthreadsMode(t *testing.T) {
	topo := wideTopo()
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 2, "hyperthreads", "exclude", nil)

	amount, kind := policy.Spec.Config.ReservedResources.Get(nripolicy.CPU)
	if kind != nripolicy.AmountCPUSet {
		t.Fatalf("reservedResources CPU kind = %v, want AmountCPUSet", kind)
	}
	if string(amount) != "0,4" {
		t.Errorf("reserved cpuset = %q, want %q", amount, "0,4")
	}
}

// physical-cores mode must set availableResources to exclude the HT siblings.
func TestRenderNodePolicy_PhysicalCoresExcludesSiblings(t *testing.T) {
	topo := htTopo()
	defs := GenerateBalloonDefs(topo.PkgNode, topo.MaxSpan, "physical-cores")
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, defs, 4, "physical-cores", "exclude", nil)

	amount, kind := policy.Spec.Config.AvailableResources.Get(nripolicy.CPU)
	if kind != nripolicy.AmountExcludeCPUSet {
		t.Fatalf("availableResources CPU kind = %v, want AmountExcludeCPUSet", kind)
	}
	if string(amount) != "4-7" {
		t.Errorf("excluded cpuset = %q, want %q", amount, "4-7")
	}
}

// TestRenderNodePolicy_DeviceLocality proves the pod-resources API is enabled and
// the device balloon survives rendering intact.
func TestRenderNodePolicy_DeviceLocality(t *testing.T) {
	topo := wideTopo()
	defs := GenerateBalloonDefs(topo.PkgNode, topo.MaxSpan, "physical-cores")
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, defs, 4, "physical-cores", "exclude", nil)

	// The pod-resources API must be enabled so the podresourceapi: hint resolves.
	if !policy.Spec.Agent.PodResourceAPI {
		t.Errorf("Agent.PodResourceAPI = false, want true (required to resolve the device-locality hint)")
	}

	// The device balloon keeps its hint and flags.
	dev := findDef(policy.Spec.Config.BalloonDefs, DeviceBalloonName)
	if dev == nil {
		t.Fatalf("rendered policy is missing the %q balloon (have: %d defs)",
			DeviceBalloonName, len(policy.Spec.Config.BalloonDefs))
	}
	if len(dev.PreferCloseToDevices) != 1 || dev.PreferCloseToDevices[0] != "podresourceapi:*/*" {
		t.Errorf("%s PreferCloseToDevices = %v, want [podresourceapi:*/*]",
			DeviceBalloonName, dev.PreferCloseToDevices)
	}
	if !dev.PreferNewBalloons {
		t.Errorf("%s PreferNewBalloons = false, want true", DeviceBalloonName)
	}
	if !dev.PreferSpreadingPods {
		t.Errorf("%s PreferSpreadingPods = false, want true", DeviceBalloonName)
	}
}

// Hyperthreads mode sets no availableResources (full pool).
func TestRenderNodePolicy_HyperthreadsLeavesFullPool(t *testing.T) {
	topo := htTopo()
	defs := GenerateBalloonDefs(topo.PkgNode, topo.MaxSpan, "hyperthreads")
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, defs, 4, "hyperthreads", "exclude", nil)

	if policy.Spec.Config.AvailableResources != nil {
		t.Errorf("hyperthreads mode set availableResources = %v, want nil (full pool)",
			policy.Spec.Config.AvailableResources)
	}
}

// Even in physical-cores mode, a node with no HT (cpu == core) must not get an
// empty/invalid exclude-cpuset.
func TestRenderNodePolicy_PhysicalCoresNoHTNoExclusion(t *testing.T) {
	topo := &topology.NodeTopology{
		PkgNode: map[int][]int{0: {0}},
		CPUs: map[int]topology.CPUInfo{
			0: {CPU: 0, NUMA: 0, Core: 0},
			1: {CPU: 1, NUMA: 0, Core: 1},
		},
	}
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 4, "physical-cores", "exclude", nil)
	if policy.Spec.Config.AvailableResources != nil {
		t.Errorf("no-HT node set availableResources = %v, want nil",
			policy.Spec.Config.AvailableResources)
	}
}

// TestRenderNodePolicy_ReservedCpusetDisjointFromExcludeCpuset proves the reserved
// cpuset never overlaps the exclude-cpuset; the plugin rejects the config if it does.
func TestRenderNodePolicy_ReservedCpusetDisjointFromExcludeCpuset(t *testing.T) {
	topo := wideTopo() // 2 NUMA x 4 cores x 2 threads; representatives 0-7, siblings 8-15
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 4, "physical-cores", "exclude", nil)

	exclAmount, exclKind := policy.Spec.Config.AvailableResources.Get(nripolicy.CPU)
	if exclKind != nripolicy.AmountExcludeCPUSet {
		t.Fatalf("availableResources CPU kind = %v, want AmountExcludeCPUSet", exclKind)
	}
	resAmount, resKind := policy.Spec.Config.ReservedResources.Get(nripolicy.CPU)
	if resKind != nripolicy.AmountCPUSet {
		t.Fatalf("reservedResources CPU kind = %v, want AmountCPUSet (homogeneous node)", resKind)
	}

	excluded := parseCPUSetString(t, string(exclAmount))
	reserved := parseCPUSetString(t, string(resAmount))
	if len(reserved) == 0 {
		t.Fatal("reserved cpuset is empty; expected an explicit NUMA-balanced cpuset on a homogeneous node")
	}
	for cpu := range reserved {
		if excluded[cpu] {
			t.Errorf("reserved cpuset names CPU %d which is in the exclude-cpuset (%q); "+
				"the NRI plugin would reject this config", cpu, exclAmount)
		}
	}
}

// smallHTTopo builds a 1-NUMA, 2-core, 2-thread node with siblings 2,3, a
// different range than htTopo/wideTopo.
func smallHTTopo() *topology.NodeTopology {
	return &topology.NodeTopology{
		PkgNode: map[int][]int{0: {0}},
		CPUs: map[int]topology.CPUInfo{
			0: {CPU: 0, NUMA: 0, Core: 0},
			1: {CPU: 1, NUMA: 0, Core: 1},
			2: {CPU: 2, NUMA: 0, Core: 0},
			3: {CPU: 3, NUMA: 0, Core: 1},
		},
	}
}

// Density mode emits no exclude-cpuset and reserves all per-node siblings, ignoring reservedCount.
func TestRenderNodePolicy_DensityModeReservesSiblings(t *testing.T) {
	cases := []struct {
		name string
		topo *topology.NodeTopology
		// reservedCount differs from the sibling count on purpose.
		reservedCount int
		wantSiblings  string
	}{
		{"2-NUMA 2-core", htTopo(), 2, "4-7"},
		{"2-NUMA 4-core", wideTopo(), 3, "8-15"},
		{"single-NUMA differing sibling range", smallHTTopo(), 7, "2-3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, tc.topo, nil, tc.reservedCount, "physical-cores", HTSiblingPolicyReserved, nil)

			// Density mode leaves the siblings in the pool: no exclude-cpuset.
			if policy.Spec.Config.AvailableResources != nil {
				t.Errorf("density mode set availableResources = %v, want nil (siblings stay in pool)",
					policy.Spec.Config.AvailableResources)
			}

			amount, kind := policy.Spec.Config.ReservedResources.Get(nripolicy.CPU)
			if kind != nripolicy.AmountCPUSet {
				t.Fatalf("reservedResources CPU kind = %v, want AmountCPUSet", kind)
			}
			if string(amount) != tc.wantSiblings {
				t.Errorf("reserved cpuset = %q, want sibling set %q", amount, tc.wantSiblings)
			}
			// The reserved cpuset is the full sibling set.
			if got := countCPUSet(t, string(amount)); got != SiblingCPUCount(tc.topo) {
				t.Errorf("reserved cpuset names %d CPUs, want all %d siblings (count arg %d ignored)",
					got, SiblingCPUCount(tc.topo), tc.reservedCount)
			}
		})
	}
}

// In density mode there is no exclude-cpuset and every reserved CPU is a real CPU.
func TestRenderNodePolicy_DensityModeReservedWithinAllocatablePool(t *testing.T) {
	topo := wideTopo() // representatives 0-7, siblings 8-15
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 4, "physical-cores", HTSiblingPolicyReserved, nil)

	if policy.Spec.Config.AvailableResources != nil {
		t.Fatalf("density mode set availableResources = %v, want nil (no exclude-cpuset)",
			policy.Spec.Config.AvailableResources)
	}
	resAmount, resKind := policy.Spec.Config.ReservedResources.Get(nripolicy.CPU)
	if resKind != nripolicy.AmountCPUSet {
		t.Fatalf("reservedResources CPU kind = %v, want AmountCPUSet", resKind)
	}
	reserved := parseCPUSetString(t, string(resAmount))
	if len(reserved) == 0 {
		t.Fatal("reserved cpuset is empty; expected the HT-sibling set")
	}
	for cpu := range reserved {
		if _, ok := topo.CPUs[cpu]; !ok {
			t.Errorf("reserved cpuset names CPU %d which is not in the allocatable pool (topo.CPUs)", cpu)
		}
	}
}

// On the same node, htSiblingPolicy alone flips exclude mode and density mode.
func TestRenderNodePolicy_ExcludeModeUnchangedVersusDensity(t *testing.T) {
	topo := wideTopo() // representatives 0-7, siblings 8-15

	excl := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 4, "physical-cores", HTSiblingPolicyExclude, nil)
	exAmount, exKind := excl.Spec.Config.AvailableResources.Get(nripolicy.CPU)
	if exKind != nripolicy.AmountExcludeCPUSet {
		t.Fatalf("exclude mode availableResources CPU kind = %v, want AmountExcludeCPUSet", exKind)
	}
	if string(exAmount) != "8-15" {
		t.Errorf("exclude-cpuset = %q, want %q", exAmount, "8-15")
	}
	// Reserved pool is sized normally: NUMA-balanced cpuset of reservedCount=4.
	resAmount, resKind := excl.Spec.Config.ReservedResources.Get(nripolicy.CPU)
	if resKind != nripolicy.AmountCPUSet {
		t.Fatalf("exclude mode reservedResources CPU kind = %v, want AmountCPUSet", resKind)
	}
	if string(resAmount) != "0-1,4-5" {
		t.Errorf("exclude mode reserved cpuset = %q, want NUMA-balanced %q", resAmount, "0-1,4-5")
	}

	// Density mode on the same node: no exclude-cpuset, reserved = siblings.
	dens := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 4, "physical-cores", HTSiblingPolicyReserved, nil)
	if dens.Spec.Config.AvailableResources != nil {
		t.Errorf("density mode set availableResources = %v, want nil", dens.Spec.Config.AvailableResources)
	}
	dAmount, _ := dens.Spec.Config.ReservedResources.Get(nripolicy.CPU)
	if string(dAmount) != "8-15" {
		t.Errorf("density mode reserved cpuset = %q, want sibling set %q", dAmount, "8-15")
	}
}

// htSiblingPolicy=reserved is a no-op in hyperthreads mode.
func TestRenderNodePolicy_DensityNoOpInHyperthreadsMode(t *testing.T) {
	topo := wideTopo()
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 2, "hyperthreads", HTSiblingPolicyReserved, nil)

	if policy.Spec.Config.AvailableResources != nil {
		t.Errorf("hyperthreads mode set availableResources = %v, want nil", policy.Spec.Config.AvailableResources)
	}
	amount, kind := policy.Spec.Config.ReservedResources.Get(nripolicy.CPU)
	if kind != nripolicy.AmountCPUSet {
		t.Fatalf("reservedResources CPU kind = %v, want AmountCPUSet", kind)
	}
	// Normal NUMA-balanced reserved cpuset for count=2, NOT the sibling set 8-15.
	if string(amount) != "0,4" {
		t.Errorf("reserved cpuset = %q, want normal NUMA-balanced %q (not sibling set)", amount, "0,4")
	}
}

// A non-HT node with irregular NUMA data gets the bare reservedCount in density mode, never an empty cpuset.
func TestRenderNodePolicy_DensityModeNonHTFallsBackToBareCount(t *testing.T) {
	// Declared and populated NUMA disagree, so reservedCPUSet declines.
	topo := &topology.NodeTopology{
		PkgNode: map[int][]int{0: {0}, 1: {1}},
		CPUs: map[int]topology.CPUInfo{
			0: {CPU: 0, NUMA: 0, Core: 0},
			1: {CPU: 1, NUMA: 0, Core: 1},
			2: {CPU: 2, NUMA: 1, Core: 2},
		},
	}
	if SiblingCPUCount(topo) != 0 {
		t.Fatalf("test setup: expected a non-HT node, got %d siblings", SiblingCPUCount(topo))
	}
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 5, "physical-cores", HTSiblingPolicyReserved, nil)

	if policy.Spec.Config.AvailableResources != nil {
		t.Errorf("non-HT node set availableResources = %v, want nil", policy.Spec.Config.AvailableResources)
	}
	amount, kind := policy.Spec.Config.ReservedResources.Get(nripolicy.CPU)
	if kind != nripolicy.AmountQuantity {
		t.Fatalf("reservedResources CPU kind = %v, want AmountQuantity (bare count fallback)", kind)
	}
	if string(amount) != "5" {
		t.Errorf("reserved bare count = %q, want %q", amount, "5")
	}
}

// On a homogeneous non-HT node, density mode renders identically to exclude mode.
func TestRenderNodePolicy_DensityModeNonHTMatchesExclude(t *testing.T) {
	// Homogeneous 2-NUMA node, 2 physical cores each, NO hyperthreads.
	nonHT := func() *topology.NodeTopology {
		return &topology.NodeTopology{
			PkgNode: map[int][]int{0: {0}, 1: {1}},
			CPUs: map[int]topology.CPUInfo{
				0: {CPU: 0, NUMA: 0, Core: 0},
				1: {CPU: 1, NUMA: 0, Core: 1},
				2: {CPU: 2, NUMA: 1, Core: 2},
				3: {CPU: 3, NUMA: 1, Core: 3},
			},
		}
	}
	if n := SiblingCPUCount(nonHT()); n != 0 {
		t.Fatalf("test setup: expected a non-HT node, got %d siblings", n)
	}

	reservedPolicy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, nonHT(), nil, 2, "physical-cores", HTSiblingPolicyReserved, nil)
	excludePolicy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, nonHT(), nil, 2, "physical-cores", HTSiblingPolicyExclude, nil)

	// No exclusion either way (no siblings to exclude).
	if reservedPolicy.Spec.Config.AvailableResources != nil {
		t.Errorf("density mode on non-HT node set availableResources = %v, want nil",
			reservedPolicy.Spec.Config.AvailableResources)
	}
	// Both must emit the SAME deterministic NUMA-balanced reserved cpuset.
	rAmount, rKind := reservedPolicy.Spec.Config.ReservedResources.Get(nripolicy.CPU)
	eAmount, eKind := excludePolicy.Spec.Config.ReservedResources.Get(nripolicy.CPU)
	if rKind != nripolicy.AmountCPUSet {
		t.Fatalf("density-mode reserved kind = %v, want AmountCPUSet (deterministic cpuset, not bare quantity)", rKind)
	}
	if rKind != eKind || string(rAmount) != string(eAmount) {
		t.Errorf("density mode reserved = %q (%v), want identical to exclude mode %q (%v) on a non-HT node",
			rAmount, rKind, eAmount, eKind)
	}
	// count=2 across 2 NUMA nodes → one lowest rep from each NUMA (0 and 2).
	if string(rAmount) != "0,2" {
		t.Errorf("reserved cpuset = %q, want NUMA-balanced %q", rAmount, "0,2")
	}
}

func TestCPUSetSize(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"0", 1},
		{"0-1,4-5", 4},
		{"8-15", 8},
		{"3,5,7", 3},
		{"1-3,8-9,20", 6},
	}
	for _, c := range cases {
		if got := cpuSetSize(c.in); got != c.want {
			t.Errorf("cpuSetSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// cpuSetSize panics on malformed input rather than under-count the reserved pin.
func TestCPUSetSize_MalformedPanics(t *testing.T) {
	bad := []string{
		"-1",     // negative single id
		"-1-1",   // negative range low bound
		"0--1",   // negative range high bound
		"x",      // non-numeric id
		"1-x",    // non-numeric range bound
		"1-",     // empty range bound
		"3-1",    // descending range
		"0,,1",   // empty field
		"0-1,-2", // negative id in a later field
	}
	for _, in := range bad {
		in := in
		t.Run(in, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("cpuSetSize(%q) did not panic on malformed input", in)
				}
			}()
			_ = cpuSetSize(in)
		})
	}
}

// findReservedDef returns the reserved balloon from the generated policy's defs.
func findReservedDef(t *testing.T, policy *nriv1alpha1.BalloonsPolicy) *nriballoons.BalloonDef {
	t.Helper()
	for _, d := range policy.Spec.Config.BalloonDefs {
		if d.Name == reservedBalloonName {
			return d
		}
	}
	t.Fatal("generated policy has no reserved balloon def")
	return nil
}

// bigHTTopo builds a 2-NUMA, 2-thread node with coresPerNUMA cores each; siblings
// follow the 2*coresPerNUMA representatives.
func bigHTTopo(coresPerNUMA int) *topology.NodeTopology {
	cpus := map[int]topology.CPUInfo{}
	reps := 2 * coresPerNUMA // representatives across both NUMA nodes
	for cpu := 0; cpu < reps; cpu++ {
		cpus[cpu] = topology.CPUInfo{CPU: cpu, NUMA: cpu / coresPerNUMA, Core: cpu}
	}
	for i := 0; i < reps; i++ {
		cpu := reps + i
		core := i
		cpus[cpu] = topology.CPUInfo{CPU: cpu, NUMA: core / coresPerNUMA, Core: core}
	}
	return &topology.NodeTopology{
		PkgNode: map[int][]int{0: {0}, 1: {1}},
		CPUs:    cpus,
	}
}

// The reserved def's MinCpus == MaxCpus == the emitted cpuset size in every sizing form.
func TestRenderNodePolicy_ReservedBalloonPinnedToReservedSize(t *testing.T) {
	// 2 NUMA × 32 cores × 2 threads → 64 siblings.
	big := bigHTTopo(32)
	// wantPin derives from the same topology instance, so fixtures cannot desync.
	wide := wideTopo()

	cases := []struct {
		name          string
		topo          *topology.NodeTopology
		reservedCount int
		cpuAllocation string
		htPolicy      string
		wantPin       int
	}{
		// An evenly dividing count names exactly reservedCount.
		{"small explicit cpuset physical-cores", wideTopo(), 4, "physical-cores", HTSiblingPolicyExclude, 4},
		// hyperthreads mode is still eligible for the NUMA-balanced cpuset.
		{"small explicit cpuset hyperthreads", wideTopo(), 2, "hyperthreads", HTSiblingPolicyExclude, 2},
		// count=3 rounds to 4; the pin follows the emitted size.
		{"cpuset rounded up to NUMA multiple", wideTopo(), 3, "physical-cores", HTSiblingPolicyExclude, 4},
		// Heterogeneous topology → bare-count fallback; pin equals the bare count.
		{"heterogeneous bare count", heteroReservedTopo(), 2, "physical-cores", HTSiblingPolicyExclude, 2},
		// Density mode: pin to the sibling-set size, not reservedCount.
		{"density small sibling set", wide, 3, "physical-cores", HTSiblingPolicyReserved, SiblingCPUCount(wide)},
		{"density large sibling set (50+)", big, 8, "physical-cores", HTSiblingPolicyReserved, SiblingCPUCount(big)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defs := GenerateBalloonDefs(tc.topo.PkgNode, tc.topo.MaxSpan, tc.cpuAllocation)
			policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, tc.topo, defs, tc.reservedCount, tc.cpuAllocation, tc.htPolicy, nil)

			reserved := findReservedDef(t, policy)
			if reserved.MinCpus != tc.wantPin || reserved.MaxCpus != tc.wantPin {
				t.Errorf("reserved MinCpus/MaxCpus = %d/%d, want %d/%d",
					reserved.MinCpus, reserved.MaxCpus, tc.wantPin, tc.wantPin)
			}
			// The pin equals the emitted reservedResources.cpu size.
			amount, kind := policy.Spec.Config.ReservedResources.Get(nripolicy.CPU)
			var emitted int
			switch kind {
			case nripolicy.AmountCPUSet:
				emitted = countCPUSet(t, string(amount))
			case nripolicy.AmountQuantity:
				n, err := strconv.Atoi(string(amount))
				if err != nil {
					t.Fatalf("bare reserved count %q not an integer: %v", amount, err)
				}
				emitted = n
			default:
				t.Fatalf("unexpected reservedResources CPU kind %v", kind)
			}
			if reserved.MinCpus != emitted {
				t.Errorf("reserved pin %d != emitted reservedResources.cpu size %d (%q)",
					reserved.MinCpus, emitted, amount)
			}
		})
	}
}

// A non-positive reserved size leaves the reserved balloon unpinned (plugin default).
func TestRenderNodePolicy_ReservedBalloonUnpinnedWhenZero(t *testing.T) {
	topo := wideTopo()
	defs := GenerateBalloonDefs(topo.PkgNode, topo.MaxSpan, "physical-cores")
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, defs, 0, "physical-cores", HTSiblingPolicyExclude, nil)

	reserved := findReservedDef(t, policy)
	if reserved.MinCpus != 0 || reserved.MaxCpus != 0 {
		t.Errorf("reserved MinCpus/MaxCpus = %d/%d, want 0/0 (unpinned, plugin default)",
			reserved.MinCpus, reserved.MaxCpus)
	}
}

// heteroReservedTopo declares 2 NUMA nodes with irregular per-NUMA core counts,
// forcing the reserved pool to the bare-count fallback (not a cpuset).
func heteroReservedTopo() *topology.NodeTopology {
	return &topology.NodeTopology{
		PkgNode: map[int][]int{0: {0}, 1: {1}},
		CPUs: map[int]topology.CPUInfo{
			0: {CPU: 0, NUMA: 0, Core: 0},
			1: {CPU: 1, NUMA: 0, Core: 1},
			2: {CPU: 2, NUMA: 1, Core: 2},
		},
	}
}

// A nil reservedNamespaces becomes the default ["*", "kube-system"].
func TestRenderNodePolicy_ReservedNamespacesDefaultsWhenNil(t *testing.T) {
	topo := wideTopo()
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 4, "physical-cores", "exclude", nil)

	got := policy.Spec.Config.ReservedPoolNamespaces
	want := []string{"*", "kube-system"}
	if !equalStrings(got, want) {
		t.Errorf("reservedPoolNamespaces = %v, want default %v", got, want)
	}
}

// A scoped list is emitted verbatim.
func TestRenderNodePolicy_ReservedNamespacesScoped(t *testing.T) {
	topo := wideTopo()
	scoped := []string{"inference", "kube-system"}
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 4, "physical-cores", "exclude", scoped)

	got := policy.Spec.Config.ReservedPoolNamespaces
	if !equalStrings(got, scoped) {
		t.Errorf("reservedPoolNamespaces = %v, want scoped %v", got, scoped)
	}
	for _, ns := range got {
		if ns == "*" {
			t.Errorf("scoped reservedPoolNamespaces %v still contains the wildcard; the shared-node fix would be defeated", got)
		}
	}
}

// An explicit empty list means "route nothing extra" and is not re-defaulted.
func TestRenderNodePolicy_ReservedNamespacesExplicitEmptyHonored(t *testing.T) {
	topo := wideTopo()
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, nil, 4, "physical-cores", "exclude", []string{})

	got := policy.Spec.Config.ReservedPoolNamespaces
	if len(got) != 0 {
		t.Errorf("reservedPoolNamespaces = %v, want empty (explicit [] must not be re-defaulted to the wildcard)", got)
	}
}

// equalStrings reports whether two string slices have the same elements in the
// same order (nil and empty are treated as equal-length here via len checks).
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSiblingCPUCount(t *testing.T) {
	cases := []struct {
		name string
		topo *topology.NodeTopology
		want int
	}{
		{"2-NUMA 2-core HT", htTopo(), 4},
		{"2-NUMA 4-core HT", wideTopo(), 8},
		{"single-NUMA HT", smallHTTopo(), 2},
		{"no HT (cpu == core)", &topology.NodeTopology{
			CPUs: map[int]topology.CPUInfo{
				0: {CPU: 0, NUMA: 0, Core: 0},
				1: {CPU: 1, NUMA: 0, Core: 1},
			},
		}, 0},
		{"empty topology", &topology.NodeTopology{}, 0},
		{"nil topology", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SiblingCPUCount(tc.topo); got != tc.want {
				t.Errorf("SiblingCPUCount = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSiblingOutcome_SeparatesTheMachineFromTheMode proves SiblingsAllocatable and
// SiblingsNone are told apart, including for a summary-only topology.
func TestSiblingOutcome_SeparatesTheMachineFromTheMode(t *testing.T) {
	summaryOnlyHT := &topology.NodeTopology{ThreadsPerCore: 2}
	cases := []struct {
		name          string
		topo          *topology.NodeTopology
		cpuAllocation string
		policy        string
		want          string
	}{
		{"physical cores holds the siblings back", wideTopo(),
			CPUAllocationPhysicalCores, HTSiblingPolicyExclude, SiblingsHeldBack},
		{"physical cores hands them to the reserved pool", wideTopo(),
			CPUAllocationPhysicalCores, HTSiblingPolicyReserved, SiblingsReservedPool},
		{"hyperthreads leaves them allocatable", wideTopo(),
			"hyperthreads", HTSiblingPolicyExclude, SiblingsAllocatable},
		{"hyperthreading off in firmware has no sibling", numaTopo(2),
			CPUAllocationPhysicalCores, HTSiblingPolicyExclude, SiblingsNone},
		{"hyperthreading off, siblings would be the reserved pool", numaTopo(2),
			CPUAllocationPhysicalCores, HTSiblingPolicyReserved, SiblingsNone},
		{"summary-only topology excludes nothing", summaryOnlyHT,
			CPUAllocationPhysicalCores, HTSiblingPolicyExclude, SiblingsAllocatable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, density := ResolveSiblings(tc.topo.CPUs, tc.cpuAllocation, tc.policy)
			if got := SiblingOutcome(tc.topo, resolved, density); got != tc.want {
				t.Errorf("SiblingOutcome = %q, want %q (cpuAllocation=%q, htSiblingPolicy=%q)",
					got, tc.want, tc.cpuAllocation, tc.policy)
			}
		})
	}
}

// pctTopo builds a 2-package, 4-NUMA node so every def family is generated.
func pctTopo() *topology.NodeTopology {
	cpus := map[int]topology.CPUInfo{}
	for cpu := 0; cpu < 8; cpu++ {
		cpus[cpu] = topology.CPUInfo{CPU: cpu, NUMA: cpu / 2, Core: cpu}
	}
	return &topology.NodeTopology{
		PkgNode: map[int][]int{0: {0, 1}, 1: {2, 3}},
		CPUs:    cpus,
	}
}

// The policy carries exactly gpu-hp/high and cpu-lp/low; the plugin rejects extra managed classes.
func TestRenderNodePolicy_CPUClassesPCTPriority(t *testing.T) {
	topo := pctTopo()
	defs := GenerateBalloonDefs(topo.PkgNode, 4, "physical-cores")
	policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, topo, defs, 4, "physical-cores", "exclude", nil)

	classes := policy.Spec.Config.CPUClasses
	if len(classes) != 2 {
		t.Fatalf("Config.CPUClasses has %d entries, want exactly 2 (%+v)", len(classes), classes)
	}
	got := map[string]string{}
	for _, c := range classes {
		if _, dup := got[c.Name]; dup {
			t.Errorf("duplicate CPU class %q", c.Name)
		}
		got[c.Name] = c.PctPriority
	}
	want := map[string]string{"gpu-hp": "high", "cpu-lp": "low"}
	for name, pri := range want {
		if got[name] != pri {
			t.Errorf("CPU class %q PctPriority = %q, want %q", name, got[name], pri)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected CPU class %q (only gpu-hp and cpu-lp are allowed)", name)
		}
	}
}

// CpuClass wiring: device → gpu-hp, leaves → cpu-lp, composites and reserved unset.
func TestGenerateBalloonDefs_CpuClassPCTWiring(t *testing.T) {
	topo := pctTopo()
	defs := GenerateBalloonDefs(topo.PkgNode, 4, "physical-cores")

	// (b) device def -> high-priority GPU class.
	if dev := findDef(defs, DeviceBalloonName); dev == nil {
		t.Fatalf("missing %q def", DeviceBalloonName)
	} else if dev.CpuClass != "gpu-hp" {
		t.Errorf("%s CpuClass = %q, want %q", DeviceBalloonName, dev.CpuClass, "gpu-hp")
	}

	// (c) a leaf -> low-priority CPU class.
	if leaf := findDef(defs, "pkg0node0"); leaf == nil {
		t.Fatalf("missing %q def (have %d defs)", "pkg0node0", len(defs))
	} else if leaf.CpuClass != "cpu-lp" {
		t.Errorf("pkg0node0 CpuClass = %q, want %q", leaf.CpuClass, "cpu-lp")
	}

	// (d) reserved builtin keeps CpuClass unset.
	if res := findDef(defs, "reserved"); res == nil {
		t.Fatal(`missing "reserved" def`)
	} else if res.CpuClass != "" {
		t.Errorf("reserved CpuClass = %q, want empty", res.CpuClass)
	}

	// Composites carry no CpuClass; every other CPU-only def is cpu-lp.
	composites := 0
	for _, d := range defs {
		switch {
		case d.Name == DeviceBalloonName, d.Name == "reserved":
			// asserted above.
		case len(d.Components) > 0:
			composites++
			if d.CpuClass != "" {
				t.Errorf("composite def %q CpuClass = %q, want empty (plugin rejects the policy)", d.Name, d.CpuClass)
			}
		default:
			if d.CpuClass != "cpu-lp" {
				t.Errorf("def %q CpuClass = %q, want %q", d.Name, d.CpuClass, "cpu-lp")
			}
		}
	}
	if composites == 0 {
		t.Fatal("topology generated no composite defs; the composite check above ran on nothing")
	}
}

// parseCPUSetString parses a compact cpuset into a set, failing on malformed input.
func parseCPUSetString(t *testing.T, s string) map[int]bool {
	t.Helper()
	out := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			l, err := strconv.Atoi(strings.TrimSpace(lo))
			if err != nil {
				t.Fatalf("parseCPUSetString: bad range lower bound %q in %q: %v", lo, s, err)
			}
			h, err := strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				t.Fatalf("parseCPUSetString: bad range upper bound %q in %q: %v", hi, s, err)
			}
			if h < l {
				t.Fatalf("parseCPUSetString: inverted range %q in %q", part, s)
			}
			for i := l; i <= h; i++ {
				out[i] = true
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			t.Fatalf("parseCPUSetString: bad CPU id %q in %q: %v", part, s, err)
		}
		out[n] = true
	}
	return out
}

// TestPinnableCPUs_FollowsTheRenderedPolicy proves, for every sibling outcome,
// PinnableCPUs == all CPUs minus the rendered exclude-cpuset.
func TestPinnableCPUs_FollowsTheRenderedPolicy(t *testing.T) {
	cases := []struct {
		name            string
		topo            *topology.NodeTopology
		cpuAllocation   string
		htSiblingPolicy string
		// wantAll means every logical CPU.
		want    []int
		wantAll bool
	}{
		// Excluded: only representative threads remain.
		{
			name: "physical-cores with siblings excluded",
			topo: wideTopo(), cpuAllocation: CPUAllocationPhysicalCores,
			htSiblingPolicy: HTSiblingPolicyExclude,
			want:            []int{0, 1, 2, 3, 4, 5, 6, 7},
		},
		// Density: the siblings stay in the pool as the reserved pool.
		{
			name: "physical-cores with siblings reserved (density)",
			topo: wideTopo(), cpuAllocation: CPUAllocationPhysicalCores,
			htSiblingPolicy: HTSiblingPolicyReserved,
			wantAll:         true,
		},
		// Hyperthreads mode excludes nothing.
		{
			name: "hyperthreads excludes nothing",
			topo: wideTopo(), cpuAllocation: "hyperthreads",
			htSiblingPolicy: HTSiblingPolicyExclude,
			wantAll:         true,
		},
		// No HT: nothing to exclude.
		{
			name: "physical-cores on a node without hyperthreading",
			topo: numaTopo(2), cpuAllocation: CPUAllocationPhysicalCores,
			htSiblingPolicy: HTSiblingPolicyExclude,
			wantAll:         true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PinnableCPUs(tc.topo.CPUs, tc.cpuAllocation, tc.htSiblingPolicy)

			want := map[int]bool{}
			if tc.wantAll {
				for cpu := range tc.topo.CPUs {
					want[cpu] = true
				}
			} else {
				for _, cpu := range tc.want {
					want[cpu] = true
				}
			}
			if len(got) != len(want) {
				t.Errorf("PinnableCPUs returned %d CPUs, want %d (%v vs %v)",
					len(got), len(want), sortedKeys(got), sortedKeys(want))
			}
			for cpu := range want {
				if !got[cpu] {
					t.Errorf("CPU %d missing from the pinnable pool", cpu)
				}
			}
			for cpu := range got {
				if !want[cpu] {
					t.Errorf("CPU %d is in the pinnable pool but should not be", cpu)
				}
			}
			if tc.wantAll && len(got) != len(tc.topo.CPUs) {
				t.Errorf("pinnable total = %d, want the logical total %d",
					len(got), len(tc.topo.CPUs))
			}

			// pinnable == topology minus the rendered exclude-cpuset.
			defs := GenerateBalloonDefs(tc.topo.PkgNode, tc.topo.MaxSpan, tc.cpuAllocation)
			policy := RenderNodePolicy("worker-1", DefaultBalloonsNamespace, tc.topo, defs, 2,
				tc.cpuAllocation, tc.htSiblingPolicy, nil)
			excluded := map[int]bool{}
			if amount, kind := policy.Spec.Config.AvailableResources.Get(nripolicy.CPU); kind == nripolicy.AmountExcludeCPUSet {
				excluded = parseCPUSetString(t, string(amount))
			}
			for cpu := range tc.topo.CPUs {
				wantPinnable := !excluded[cpu]
				if got[cpu] != wantPinnable {
					t.Errorf("CPU %d: pinnable = %v, but the rendered policy says pinnable = %v",
						cpu, got[cpu], wantPinnable)
				}
			}
		})
	}
}

// sortedKeys renders a CPU set in a stable order for failure messages.
func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
