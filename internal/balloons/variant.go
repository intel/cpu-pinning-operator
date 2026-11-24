// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package balloons

import (
	"sort"
	"strconv"
	"strings"

	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// PerPackageNUMACounts returns the NUMA-node count per package, sorted: variant
// emittability depends only on these counts. It feeds the package-topology label.
func PerPackageNUMACounts(pkgNode map[int][]int) []int {
	counts := make([]int, 0, len(pkgNode))
	for _, nodes := range pkgNode {
		counts = append(counts, len(nodes))
	}
	sort.Ints(counts)
	return counts
}

// packageTopologySep is "_" because label values cannot contain ",".
const packageTopologySep = "_"

// FormatPackageTopology renders per-package NUMA counts as sorted, "_"-joined
// ({2,1} -> "1_2"); empty renders "". Round-trips through ParsePackageTopology.
func FormatPackageTopology(counts []int) string {
	if len(counts) == 0 {
		return ""
	}
	sorted := append([]int(nil), counts...)
	sort.Ints(sorted)
	parts := make([]string, len(sorted))
	for i, c := range sorted {
		parts[i] = strconv.Itoa(c)
	}
	return strings.Join(parts, packageTopologySep)
}

// maxLabelNUMAsPerPackage and maxLabelNUMAs bound a package-topology label, since
// VariantEmittable runs the generator on the admission path. They derive from the
// probe parser's bounds; a label past them reads as unknown topology.
const (
	maxLabelNUMAsPerPackage = topology.MaxNUMAsPerPackage
	maxLabelNUMAs           = topology.MaxNUMAID + 1
)

// ParsePackageTopology parses a package-topology label into sorted counts. Any
// bad or out-of-bounds field makes the whole value unknown (nil).
func ParsePackageTopology(v string) []int {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	fields := strings.Split(v, packageTopologySep)
	counts := make([]int, 0, len(fields))
	total := 0
	for _, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n <= 0 || n > maxLabelNUMAsPerPackage {
			return nil // any bad field → unknown, don't trust a partial shape
		}
		total += n
		if total > maxLabelNUMAs {
			return nil
		}
		counts = append(counts, n)
	}
	sort.Ints(counts)
	return counts
}

// syntheticPkgNode builds a package->NUMA-node map with sequential ids from counts.
func syntheticPkgNode(counts []int) map[int][]int {
	pkgNode := make(map[int][]int, len(counts))
	next := 0
	for pkg, n := range counts {
		nodes := make([]int, n)
		for i := 0; i < n; i++ {
			nodes[i] = next
			next++
		}
		pkgNode[pkg] = nodes
	}
	return pkgNode
}

// VariantEmittable reports whether a node with these counts emits
// balloon-span<span><suffix>, by running the real generator on a synthetic
// topology so the webhook and generator cannot drift. Empty counts → false.
func VariantEmittable(counts []int, span int, suffix string) bool {
	if len(counts) == 0 || span < 1 {
		return false
	}
	pkgNode := syntheticPkgNode(counts)
	// maxSpan = span; the node's max-span gate is checked separately.
	want := balloonNameBase + "-span" + strconv.Itoa(span) + suffix
	for _, def := range GenerateBalloonDefs(pkgNode, span, defaultCPUAllocationForGen) {
		if def.Name == want {
			return true
		}
	}
	return false
}

// defaultCPUAllocationForGen is any valid cpuAllocation; it does not affect span variants.
const defaultCPUAllocationForGen = "physical-cores"
