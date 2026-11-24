// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package balloons

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	nriv1alpha1 "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"
	nripolicy "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	nriballoons "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/balloons"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// DefaultBalloonsNamespace is used when BALLOONS_NAMESPACE is unset. The chart
// overrides it with the release namespace so a self-installed plugin sees the CR.
const DefaultBalloonsNamespace = "kube-system"

// defaultReservedPoolNamespaces routes every unpinned pod onto the reserved pool.
// "*" is a node-exhaustion hazard on shared/OpenShift nodes — scope
// spec.balloon.reservedNamespaces there. Cloned on every render; never mutate.
var defaultReservedPoolNamespaces = []string{"*", "kube-system"}

// CPUAllocationPhysicalCores means a request for N CPUs consumes N whole cores.
const CPUAllocationPhysicalCores = "physical-cores"

// HTSiblingPolicyExclude removes hyperthread siblings from the allocatable pool.
const HTSiblingPolicyExclude = "exclude"

// HTSiblingPolicyReserved hands siblings to the reserved pool instead of excluding
// them. Workload balloons stay isolated; reserved work can use the siblings.
const HTSiblingPolicyReserved = "reserved"

// The four reported (never configured) outcomes for a machine's hyperthread siblings.
const (
	// SiblingsHeldBack means the siblings are out of the allocatable pool.
	SiblingsHeldBack = "held-back"
	// SiblingsReservedPool means the siblings are the reserved pool.
	SiblingsReservedPool = "reserved-pool"
	// SiblingsAllocatable means the siblings are ordinary allocatable CPUs.
	SiblingsAllocatable = "allocatable"
	// SiblingsNone means the machine runs one thread per core.
	SiblingsNone = "no-siblings"
)

// SiblingOutcomes lists every state SiblingOutcome can report. The indicator gauge
// publishes one series per entry so a reader can tell "0" from "not measured".
var SiblingOutcomes = []string{
	SiblingsHeldBack,
	SiblingsReservedPool,
	SiblingsAllocatable,
	SiblingsNone,
}

// SiblingOutcome names what became of a machine's siblings from ResolveSiblings'
// results, so it cannot contradict the rendered policy. topo only separates
// SiblingsNone from SiblingsAllocatable.
func SiblingOutcome(topo *topology.NodeTopology, resolved []int, density bool) string {
	switch {
	case density:
		return SiblingsReservedPool
	case len(resolved) > 0:
		return SiblingsHeldBack
	case hasSiblingThreads(topo):
		return SiblingsAllocatable
	}
	return SiblingsNone
}

// hasSiblingThreads reports whether the per-CPU topology or the thread count shows more than one thread per core.
func hasSiblingThreads(topo *topology.NodeTopology) bool {
	return topo != nil && (topo.ThreadsPerCore > 1 || SiblingCPUCount(topo) > 0)
}

func ptr[T any](v T) *T { return &v }

// siblingCPUs lists, ascending, the CPUs that are not their core's representative thread.
func siblingCPUs(cpus map[int]topology.CPUInfo) []int {
	var siblings []int
	for cpu, info := range cpus {
		if cpu != info.Core {
			siblings = append(siblings, cpu)
		}
	}
	sort.Ints(siblings)
	return siblings
}

// SiblingCPUCount is the number of hyperthread-sibling CPUs.
func SiblingCPUCount(topo *topology.NodeTopology) int {
	if topo == nil {
		return 0
	}
	return len(siblingCPUs(topo.CPUs))
}

// ResolveSiblings is the single decision on hyperthread siblings (the webhook
// re-derives it from node labels in internal/webhook/capacity.go):
//
//   - siblings empty: nothing is excluded;
//   - density false: siblings are excluded from the allocatable pool;
//   - density true: siblings are the reserved pool.
func ResolveSiblings(cpus map[int]topology.CPUInfo, cpuAllocation, htSiblingPolicy string) (siblings []int, density bool) {
	if cpuAllocation == CPUAllocationPhysicalCores {
		siblings = siblingCPUs(cpus)
	}
	density = len(siblings) > 0 && htSiblingPolicy == HTSiblingPolicyReserved
	return siblings, density
}

// PinnableCPUs is the plugin's allocatable pool, the denominator for free capacity.
// It includes the reserved pool, which already counts as claimed. nil (never
// empty) means unknown topology; readers fall back to the hardware total.
func PinnableCPUs(cpus map[int]topology.CPUInfo, cpuAllocation, htSiblingPolicy string) map[int]bool {
	if len(cpus) == 0 {
		return nil
	}
	pinnable := make(map[int]bool, len(cpus))
	for cpu := range cpus {
		pinnable[cpu] = true
	}
	siblings, density := ResolveSiblings(cpus, cpuAllocation, htSiblingPolicy)
	if density {
		return pinnable
	}
	for _, cpu := range siblings {
		delete(pinnable, cpu)
	}
	return pinnable
}

// reservedCPUSet names reserved CPUs, NUMA-balanced, one thread per core; "" when
// topology is unknown, incomplete or heterogeneous. count rounds up to a multiple
// of the NUMA count, since an uneven split shrinks multi-NUMA balloons.
func reservedCPUSet(topo *topology.NodeTopology, count int) string {
	cpus := topo.CPUs
	if count <= 0 || len(cpus) == 0 {
		return ""
	}
	// PKGNODE is the completeness yardstick: CPUINFO can omit NUMA nodes.
	expectedNUMA := map[int]bool{}
	for _, nodes := range topo.PkgNode {
		for _, n := range nodes {
			expectedNUMA[n] = true
		}
	}
	if len(expectedNUMA) == 0 {
		return ""
	}
	if topo.NumaNodes > 0 && len(expectedNUMA) != topo.NumaNodes {
		return ""
	}
	byNUMA := map[int][]int{}
	for cpu, info := range cpus {
		if cpu == info.Core {
			byNUMA[info.NUMA] = append(byNUMA[info.NUMA], cpu)
		}
	}
	if len(byNUMA) != len(expectedNUMA) {
		return ""
	}
	for numa := range expectedNUMA {
		if _, ok := byNUMA[numa]; !ok {
			return ""
		}
	}
	numaIDs := make([]int, 0, len(byNUMA))
	perNUMA := -1
	for numa, cores := range byNUMA {
		sort.Ints(cores)
		numaIDs = append(numaIDs, numa)
		switch {
		case perNUMA == -1:
			perNUMA = len(cores)
		case perNUMA != len(cores):
			perNUMA = 0 // irregular: NUMA nodes differ in physical-core count
		}
	}
	numaNodes := len(numaIDs)
	perNUMASelect := (count + numaNodes - 1) / numaNodes
	if perNUMA <= 0 || perNUMASelect > perNUMA {
		return ""
	}
	sort.Ints(numaIDs)
	selected := make([]int, 0, perNUMASelect*numaNodes)
	for round := 0; round < perNUMASelect; round++ {
		for _, numa := range numaIDs {
			selected = append(selected, byNUMA[numa][round])
		}
	}
	sort.Ints(selected)
	return formatCPUSet(selected)
}

// AppliedReservedCPUs is the reserved pool a rendered policy actually hands the
// plugin: the size of its reserved cpuset, or the bare count when it names none.
// It can exceed the resolved size, because reservedCPUSet rounds up per NUMA node.
func AppliedReservedCPUs(p *nriv1alpha1.BalloonsPolicy) int {
	amount := string(p.Spec.Config.ReservedResources[nripolicy.CPU])
	if cset, ok := strings.CutPrefix(amount, "cpuset:"); ok {
		return cpuSetSize(cset)
	}
	n, _ := strconv.Atoi(amount)
	return n
}

// cpuSetSize counts CPU ids in a compact cpuset ("0-1,4-5" = 4). Panics on a
// malformed string: skipping a field would under-count and leak reserved cores
// into workload balloons via pinReservedBalloonSize.
func cpuSetSize(cset string) int {
	if cset == "" {
		return 0
	}
	n := 0
	for _, part := range strings.Split(cset, ",") {
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			l, err1 := strconv.Atoi(lo)
			h, err2 := strconv.Atoi(hi)
			if err1 != nil || err2 != nil || l < 0 || h < 0 || h < l {
				panic(fmt.Sprintf("cpuSetSize: malformed cpuset range %q in %q", part, cset))
			}
			n += h - l + 1
		} else {
			id, err := strconv.Atoi(part)
			if err != nil || id < 0 {
				panic(fmt.Sprintf("cpuSetSize: malformed cpuset id %q in %q", part, cset))
			}
			n++
		}
	}
	return n
}

// formatCPUSet renders sorted CPU ids as a compact Linux cpuset string.
func formatCPUSet(sorted []int) string {
	var parts []string
	for i := 0; i < len(sorted); {
		j := i
		for j+1 < len(sorted) && sorted[j+1] == sorted[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(sorted[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", sorted[i], sorted[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// RenderNodePolicy assembles the per-node BalloonsPolicy CR. nil reservedNamespaces
// takes the default; an empty slice routes nothing extra.
func RenderNodePolicy(
	nodeName string,
	namespace string,
	topo *topology.NodeTopology,
	defs []*nriballoons.BalloonDef,
	reservedCount int,
	cpuAllocation string,
	htSiblingPolicy string,
	reservedNamespaces []string,
) *nriv1alpha1.BalloonsPolicy {
	if reservedNamespaces == nil {
		// Clone: a later append on the policy slice must not rewrite the global.
		reservedNamespaces = slices.Clone(defaultReservedPoolNamespaces)
	}
	var availableResources nripolicy.Constraints
	siblingIDs, densityMode := ResolveSiblings(topo.CPUs, cpuAllocation, htSiblingPolicy)
	siblings := formatCPUSet(siblingIDs)

	if len(siblingIDs) > 0 && !densityMode {
		availableResources = nripolicy.Constraints{
			nripolicy.CPU: nripolicy.Amount("exclude-cpuset:" + siblings),
		}
	}

	reservedAmount := nripolicy.Amount(strconv.Itoa(reservedCount))
	reservedSize := reservedCount
	if densityMode {
		reservedAmount = nripolicy.Amount("cpuset:" + siblings)
		reservedSize = cpuSetSize(siblings)
	} else if cset := reservedCPUSet(topo, reservedCount); cset != "" {
		reservedAmount = nripolicy.Amount("cpuset:" + cset)
		// Pin to the cpuset's actual size: reservedCPUSet may round the count up.
		reservedSize = cpuSetSize(cset)
	}

	// Without this pin the plugin leaves reserved at MinCpus 0 and cores leak.
	pinReservedBalloonSize(defs, reservedSize)

	return &nriv1alpha1.BalloonsPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "config.nri/v1alpha1",
			Kind:       "BalloonsPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("node.%s", nodeName),
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":    "cpu-pinning-operator",
				cpopv1alpha1.LabelPrefix + "node": nodeName,
			},
		},
		Spec: nriv1alpha1.BalloonsPolicySpec{
			Agent: nriv1alpha1.AgentConfig{
				NodeResourceTopology: true,
				// Device balloons need the pod-resources API; disabling it also kills List.
				PodResourceAPI: true,
			},
			Config: nriballoons.Config{
				PinCPU:                     ptr(true),
				PinMemory:                  ptr(false),
				AllocatorTopologyBalancing: true,
				// Per-container NRT subzones: without this, cpu_assignment cannot name a pod.
				ShowContainersInNrt:    ptr(true),
				ReservedPoolNamespaces: reservedNamespaces,
				ReservedResources: nripolicy.Constraints{
					nripolicy.CPU: reservedAmount,
				},
				AvailableResources: availableResources,
				BalloonDefs:        defs,
				// PCT classes referenced by the balloon defs' CpuClass; a no-op without SST-CP.
				CPUClasses: []*nripolicy.CPUClass{
					{Name: gpuHighPriorityClass, PctPriority: pctPriorityHigh},
					{Name: cpuLowPriorityClass, PctPriority: pctPriorityLow},
				},
			},
		},
	}
}
