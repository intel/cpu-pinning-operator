// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package balloons generates NRI balloons BalloonDefs (per-NUMA, per-package and
// balloon-span{1,2,4,8} composites) from a node topology.
package balloons

import (
	"fmt"
	"sort"
	"strconv"

	nriballoons "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/balloons"
)

const balloonNameBase = "balloon"

// spanBalloonPrefix is the name prefix of the composite span balloons.
const spanBalloonPrefix = balloonNameBase + "-span"

// generatedSpans are the spans generateSpanBalloonDefs emits, ascending.
var generatedSpans = []int{1, 2, 4, 8}

// reservedBalloonName is the built-in balloon carrying the reserved CPU pool.
const reservedBalloonName = "reserved"

// DeviceBalloonName is the balloon a pod requesting any device resource is routed
// to. Duplicated in internal/match to avoid an import cycle; a test keeps them equal.
const DeviceBalloonName = balloonNameBase + "-device"

// PCT (Priority Core Turbo) CPU classes: device workloads get high priority,
// CPU-only workloads low. A no-op without SST. nri-plugins allows at most one
// managed high and one managed low class per node.
const (
	gpuHighPriorityClass = "gpu-hp"
	cpuLowPriorityClass  = "cpu-lp"
	pctPriorityHigh      = "high"
	pctPriorityLow       = "low"
)

// deviceLocalityHint makes the plugin resolve, per container via the kubelet
// pod-resources API, the granted devices and pick CPUs close to them. "*/*"
// (path.Match) matches every vendor-domain/name resource.
const deviceLocalityHint = "podresourceapi:*/*"

// GenerateBalloonDefs generates a node's balloon types up to maxSpan. Physical-cores
// mode is enforced at the policy level (RenderNodePolicy), not on the defs.
func GenerateBalloonDefs(pkgNode map[int][]int, maxSpan int, cpuAllocation string) []*nriballoons.BalloonDef {
	normalized := normalizePkgNode(pkgNode)

	var defs []*nriballoons.BalloonDef
	defs = append(defs, generateNodeBalloonDefs(normalized, cpuAllocation)...)
	defs = append(defs, generateSpanBalloonDefs(normalized, maxSpan)...)
	defs = append(defs, generateDeviceBalloonDef())
	defs = append(defs, generateBuiltinBalloonDefs()...)
	return defs
}

// generateDeviceBalloonDef returns the device-locality balloon every node carries:
//   - PreferCloseToDevices: the pod-resource hint (needs the pod-resources API);
//   - PreferNewBalloons: a fresh balloon close to this container's device;
//   - PreferSpreadingPods: each container near its own device.
//
// It is a leaf balloon because the plugin rejects preferCloseToDevices on composites.
func generateDeviceBalloonDef() *nriballoons.BalloonDef {
	return &nriballoons.BalloonDef{
		Name:                 DeviceBalloonName,
		CpuClass:             gpuHighPriorityClass,
		PreferCloseToDevices: []string{deviceLocalityHint},
		PreferNewBalloons:    true,
		PreferSpreadingPods:  true,
	}
}

func normalizePkgNode(pkgNode map[int][]int) map[int][]int {
	out := make(map[int][]int, len(pkgNode))
	for pkg, nodes := range pkgNode {
		sorted := append([]int{}, nodes...)
		sort.Ints(sorted)
		out[pkg] = sorted
	}
	return out
}

func sortedPkgKeys(pkgNode map[int][]int) []int {
	keys := make([]int, 0, len(pkgNode))
	for k := range pkgNode {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

func allPkgsSatisfy(pkgNode map[int][]int, pred func([]int) bool) bool {
	for _, nodes := range pkgNode {
		if !pred(nodes) {
			return false
		}
	}
	return true
}

func leafName(pkg, node int) string {
	return fmt.Sprintf("pkg%dnode%d", pkg, node)
}

// generateNodeBalloonDefs generates one leaf balloon per NUMA node ("pkg<P>node<N>")
// and one per package combining its leaves ("pkg<P>nodes").
func generateNodeBalloonDefs(pkgNode map[int][]int, cpuAllocation string) []*nriballoons.BalloonDef {
	var defs []*nriballoons.BalloonDef
	for _, pkg := range sortedPkgKeys(pkgNode) {
		for _, node := range pkgNode[pkg] {
			leaf := &nriballoons.BalloonDef{
				Name:                 leafName(pkg, node),
				CpuClass:             cpuLowPriorityClass,
				PreferCloseToDevices: []string{fmt.Sprintf("/sys/devices/system/node/node%d", node)},
			}
			if cpuAllocation == "hyperthreads" {
				// Explicit false, not nil, keeps the hyperthreads-mode output stable.
				leaf.HideHyperthreads = ptrBool(false)
				leaf.PreferSpreadOnPhysicalCores = ptrBool(false)
			}
			defs = append(defs, leaf)
		}
		var comps []nriballoons.BalloonDefComponent
		for _, node := range pkgNode[pkg] {
			comps = append(comps, nriballoons.BalloonDefComponent{DefName: leafName(pkg, node)})
		}
		defs = append(defs, &nriballoons.BalloonDef{
			Name:              fmt.Sprintf("pkg%dnodes", pkg),
			ComponentCreation: nriballoons.ComponentCreationAll,
			Components:        comps,
		})
	}
	return defs
}

// generateBuiltinBalloonDefs returns the "reserved" placeholder; the plugin adds
// its own implicit "default" balloon. Sizing is pinned later by pinReservedBalloonSize.
func generateBuiltinBalloonDefs() []*nriballoons.BalloonDef {
	return []*nriballoons.BalloonDef{{Name: reservedBalloonName}}
}

// pinReservedBalloonSize sets the reserved balloon's MinCpus = MaxCpus = size.
// Unset, the plugin starts it at 0 CPUs and workload balloons can borrow reserved
// cores under pressure. size <= 0 leaves the plugin default.
func pinReservedBalloonSize(defs []*nriballoons.BalloonDef, size int) {
	if size <= 0 {
		return
	}
	for _, d := range defs {
		if d.Name == reservedBalloonName {
			d.MinCpus = size
			d.MaxCpus = size
			// A single reserved def is expected; stop once it is pinned.
			break
		}
	}
}

func ptrBool(v bool) *bool { return &v }

// newSpanBalloonDef builds a composite balloon-spanN definition. Composites must
// not carry CPU-allocation options or a CpuClass, or the plugin rejects the policy.
func newSpanBalloonDef(name string) *nriballoons.BalloonDef {
	pinMemory := true
	return &nriballoons.BalloonDef{
		Name:              name,
		ComponentCreation: nriballoons.ComponentCreationBalanceBalloons,
		PreferNewBalloons: true,
		PinMemory:         &pinMemory,
	}
}

// generateSpanBalloonDefs generates balloon-span{1,2,4,8} and their intermediate
// node-set balloons, capped by maxSpan and by the NUMA node count.
func generateSpanBalloonDefs(pkgNode map[int][]int, maxSpan int) []*nriballoons.BalloonDef {
	nodePkg := map[int]int{}
	var nodeOrder []int
	for _, pkg := range sortedPkgKeys(pkgNode) {
		for _, node := range pkgNode[pkg] {
			nodePkg[node] = pkg
			nodeOrder = append(nodeOrder, node)
		}
	}

	var defs []*nriballoons.BalloonDef

	if maxSpan >= 1 {
		span1 := newSpanBalloonDef(balloonNameBase + "-span1")
		for _, pair := range optimizeGNR3Tile(pkgNode) {
			span1.Components = append(span1.Components, nriballoons.BalloonDefComponent{DefName: leafName(pair.Pkg, pair.Node)})
		}
		defs = append(defs, span1)
	}

	if maxSpan >= 2 && len(nodePkg) >= 2 {
		defs = append(defs, generateSpan2Balloons(pkgNode, nodePkg, nodeOrder)...)
	}

	if maxSpan >= 4 && len(nodePkg) >= 4 {
		defs = append(defs, generateSpan4Balloons(pkgNode, nodePkg)...)
	}

	if maxSpan >= 8 && len(nodePkg) >= 8 {
		defs = append(defs, generateSpan8Balloons(pkgNode, nodePkg)...)
	}

	return defs
}

// MaxAdvertisableSpan returns the largest N with balloon-span1 … balloon-spanN all
// present (0 without span1): a gap would let a pod be stamped with a missing balloon.
func MaxAdvertisableSpan(defs []*nriballoons.BalloonDef) int {
	present := make(map[string]bool, len(defs))
	for _, d := range defs {
		present[d.Name] = true
	}
	advertisable := 0
	for _, span := range generatedSpans {
		if !present[spanBalloonPrefix+strconv.Itoa(span)] {
			break
		}
		advertisable = span
	}
	return advertisable
}

// span2ColocatePairs returns same-package 2-node sets, or nil when no package has ≥ 2 NUMA nodes.
func span2ColocatePairs(pkgNode map[int][]int) [][]int {
	// Non-overlapping pairs when every package has an even node count ≥ 2, else all pairs.
	allEvenGE2 := allPkgsSatisfy(pkgNode, func(nodes []int) bool { return len(nodes) >= 2 && len(nodes)%2 == 0 })
	allGE2 := allPkgsSatisfy(pkgNode, func(nodes []int) bool { return len(nodes) >= 2 })

	var out [][]int
	switch {
	case allEvenGE2:
		for _, pkg := range sortedPkgKeys(pkgNode) {
			selected := map[int]bool{}
			for _, comb := range combinations(pkgNode[pkg], 2) {
				if !selected[comb[0]] && !selected[comb[1]] {
					out = append(out, comb)
					selected[comb[0]] = true
					selected[comb[1]] = true
				}
			}
		}
	case allGE2:
		for _, pkg := range sortedPkgKeys(pkgNode) {
			out = append(out, combinations(pkgNode[pkg], 2)...)
		}
	}
	// If no package has ≥ 2 NUMAs, out stays nil (topology can't co-locate).
	return out
}

// span2SpreadPairs returns cross-package 2-node sets, or nil with one package.
func span2SpreadPairs(nodePkg map[int]int, nodeOrder []int) [][]int {
	var out [][]int
	for _, comb := range combinations(nodeOrder, 2) {
		if nodePkg[comb[0]] != nodePkg[comb[1]] {
			out = append(out, comb)
		}
	}
	return out
}

// span2DefaultPairs co-locates when possible, else falls back to cross-package.
func span2DefaultPairs(pkgNode map[int][]int, nodePkg map[int]int, nodeOrder []int) (pairs [][]int, localityLabel string) {
	if p := span2ColocatePairs(pkgNode); len(p) > 0 {
		return p, "localpkg"
	}
	return span2SpreadPairs(nodePkg, nodeOrder), "crosspkg"
}

// span2Variant emits the combo defs and the balloon-span2[-suffix] composite for
// pairs, or nil to skip the variant; a pod stamped with a skipped variant fails at
// CreateContainer (strict intent, no silent fallback).
func span2Variant(pairs [][]int, nodePkg map[int]int, comboLocalityLabel, spanBalloonSuffix string) []*nriballoons.BalloonDef {
	if len(pairs) == 0 {
		return nil
	}
	var defs []*nriballoons.BalloonDef
	var names []string
	for i, nodeset := range pairs {
		name := fmt.Sprintf("%scomb%d%s-span2", comboLocalityLabel, i, spanBalloonSuffix)
		names = append(names, name)
		defs = append(defs, &nriballoons.BalloonDef{
			Name:              name,
			ComponentCreation: nriballoons.ComponentCreationAll,
			Components: []nriballoons.BalloonDefComponent{
				{DefName: leafName(nodePkg[nodeset[0]], nodeset[0])},
				{DefName: leafName(nodePkg[nodeset[1]], nodeset[1])},
			},
		})
	}
	span2 := newSpanBalloonDef(balloonNameBase + "-span2" + spanBalloonSuffix)
	for _, name := range names {
		span2.Components = append(span2.Components, nriballoons.BalloonDefComponent{DefName: name})
	}
	defs = append(defs, span2)
	return defs
}

func generateSpan2Balloons(pkgNode map[int][]int, nodePkg map[int]int, nodeOrder []int) []*nriballoons.BalloonDef {
	var defs []*nriballoons.BalloonDef

	// balloon-span2 (default, no suffix).
	defaultPairs, defaultLocality := span2DefaultPairs(pkgNode, nodePkg, nodeOrder)
	defs = append(defs, span2Variant(defaultPairs, nodePkg, defaultLocality, "")...)

	// balloon-span2-colocate: the pair in one package, skipped if impossible.
	if colocatePairs := span2ColocatePairs(pkgNode); len(colocatePairs) > 0 {
		defs = append(defs, span2Variant(colocatePairs, nodePkg, "localpkg", "-colocate")...)
	}

	// balloon-span2-spread: a cross-package pair, skipped with one package.
	if spreadPairs := span2SpreadPairs(nodePkg, nodeOrder); len(spreadPairs) > 0 {
		defs = append(defs, span2Variant(spreadPairs, nodePkg, "crosspkg", "-spread")...)
	}

	return defs
}

// span4LocalityChoice selects span4NodeSets branches: default, colocate (local
// package only) or spread (cross-package only).
type span4LocalityChoice int

const (
	span4Default span4LocalityChoice = iota
	span4Colocate
	span4Spread
)

// span4NodeSets returns the 4-node sets for choice and their locality label
// ("localpkg"/"crosspkg"), or (nil, "") when the topology cannot support it.
// The default prefers local-package layouts, falling back to cross-package.
func span4NodeSets(pkgNode map[int][]int, nodePkg map[int]int, choice span4LocalityChoice) ([][]int, string) {
	pkgs := sortedPkgKeys(pkgNode)

	allDiv4GE4 := allPkgsSatisfy(pkgNode, func(nodes []int) bool { return len(nodes) >= 4 && len(nodes)%4 == 0 })
	allDiv2GE4 := allPkgsSatisfy(pkgNode, func(nodes []int) bool { return len(nodes) >= 4 && len(nodes)%2 == 0 })
	allGE4 := allPkgsSatisfy(pkgNode, func(nodes []int) bool { return len(nodes) >= 4 })

	// Local-package branches: at least one package has ≥ 4 NUMA nodes, so a
	// 4-node span can fit entirely inside a single package.
	localBranch := func() [][]int {
		switch {
		case allDiv4GE4:
			var out [][]int
			for _, pkg := range pkgs {
				selected := map[int]bool{}
				for _, comb := range combinations(pkgNode[pkg], 4) {
					if noneSelected(comb, selected) {
						out = append(out, comb)
						markSelected(comb, selected)
					}
				}
			}
			return out
		case allDiv2GE4:
			var out [][]int
			for _, pkg := range pkgs {
				selected := map[int]bool{}
				var nonOverlapTwoNodes [][]int
				for _, comb := range combinations(pkgNode[pkg], 2) {
					if !selected[comb[0]] && !selected[comb[1]] {
						nonOverlapTwoNodes = append(nonOverlapTwoNodes, comb)
						selected[comb[0]] = true
						selected[comb[1]] = true
					}
				}
				for _, idxs := range combinationIndices(len(nonOverlapTwoNodes), 2) {
					p0, p1 := nonOverlapTwoNodes[idxs[0]], nonOverlapTwoNodes[idxs[1]]
					out = append(out, concatInts(p0, p1))
				}
			}
			return out
		case allGE4:
			var out [][]int
			for _, pkg := range pkgs {
				out = append(out, combinations(pkgNode[pkg], 4)...)
			}
			return out
		}
		return nil
	}

	// Cross-package branches: mix nodes from multiple packages. Requires enough
	// packages/nodes to construct a 4-node cross-package combo.
	crossBranch := func() [][]int {
		twoPkgsGE2 := len(pkgs) == 2 && allPkgsSatisfy(pkgNode, func(nodes []int) bool { return len(nodes) >= 2 })
		switch {
		case twoPkgsGE2:
			pkg0, pkg1 := pkgs[0], pkgs[1]
			return zipConcat(combinations(pkgNode[pkg0], 2), combinations(pkgNode[pkg1], 2))
		case len(pkgs) == 4:
			allOne := allPkgsSatisfy(pkgNode, func(nodes []int) bool { return len(nodes) == 1 })
			allGE2Local := allPkgsSatisfy(pkgNode, func(nodes []int) bool { return len(nodes) >= 2 })
			if allOne {
				return [][]int{sortedNodeIDs(nodePkg)}
			}
			if allGE2Local {
				pkg0, pkg1, pkg2, pkg3 := pkgs[0], pkgs[1], pkgs[2], pkgs[3]
				pkgs01 := zipConcat(combinations(pkgNode[pkg0], 2), combinations(pkgNode[pkg1], 2))
				pkgs23 := zipConcat(combinations(pkgNode[pkg2], 2), combinations(pkgNode[pkg3], 2))
				return append(pkgs01, pkgs23...)
			}
		default:
			// Cross-quadrant fallback when no other layout matches.
			sortedNodes := sortedNodeIDs(nodePkg)
			nodeQuadrant := map[int]int{}
			for idx, n := range sortedNodes {
				nodeQuadrant[n] = idx / 4
			}
			var out [][]int
			for _, comb := range combinations(sortedNodes, 4) {
				if nodeQuadrant[comb[0]] < nodeQuadrant[comb[1]] &&
					nodeQuadrant[comb[1]] < nodeQuadrant[comb[2]] &&
					nodeQuadrant[comb[2]] < nodeQuadrant[comb[3]] {
					out = append(out, comb)
				}
			}
			return out
		}
		return nil
	}

	switch choice {
	case span4Colocate:
		if sets := localBranch(); len(sets) > 0 {
			return sets, "localpkg"
		}
		return nil, ""
	case span4Spread:
		if sets := crossBranch(); len(sets) > 0 {
			return sets, "crosspkg"
		}
		return nil, ""
	default: // span4Default: one package when it fits, else cross-package.
		if sets := localBranch(); len(sets) > 0 {
			return sets, "localpkg"
		}
		return crossBranch(), "crosspkg"
	}
}

// span4Variant emits the combo defs and the balloon-span4[-suffix] composite, or nil to skip.
func span4Variant(nodeSets [][]int, nodePkg map[int]int, comboLocalityLabel, spanBalloonSuffix string) []*nriballoons.BalloonDef {
	if len(nodeSets) == 0 {
		return nil
	}
	var defs []*nriballoons.BalloonDef
	var names []string
	for i, nodeset := range nodeSets {
		name := fmt.Sprintf("%scomb%d%s-span4", comboLocalityLabel, i, spanBalloonSuffix)
		names = append(names, name)
		var comps []nriballoons.BalloonDefComponent
		for _, node := range nodeset {
			comps = append(comps, nriballoons.BalloonDefComponent{DefName: leafName(nodePkg[node], node)})
		}
		defs = append(defs, &nriballoons.BalloonDef{
			Name:              name,
			ComponentCreation: nriballoons.ComponentCreationAll,
			Components:        comps,
		})
	}
	span4 := newSpanBalloonDef(balloonNameBase + "-span4" + spanBalloonSuffix)
	for _, name := range names {
		span4.Components = append(span4.Components, nriballoons.BalloonDefComponent{DefName: name})
	}
	defs = append(defs, span4)
	return defs
}

func generateSpan4Balloons(pkgNode map[int][]int, nodePkg map[int]int) []*nriballoons.BalloonDef {
	var defs []*nriballoons.BalloonDef

	// balloon-span4 (default).
	defaultSets, defaultLocality := span4NodeSets(pkgNode, nodePkg, span4Default)
	defs = append(defs, span4Variant(defaultSets, nodePkg, defaultLocality, "")...)

	// balloon-span4-colocate — 4-node span within a single package.
	if sets, locality := span4NodeSets(pkgNode, nodePkg, span4Colocate); len(sets) > 0 {
		defs = append(defs, span4Variant(sets, nodePkg, locality, "-colocate")...)
	}

	// balloon-span4-spread — 4-node span across packages.
	if sets, locality := span4NodeSets(pkgNode, nodePkg, span4Spread); len(sets) > 0 {
		defs = append(defs, span4Variant(sets, nodePkg, locality, "-spread")...)
	}

	return defs
}

// span8ColocateSets returns 8-node sets inside one package (needs ≥ 8 NUMA nodes per package).
func span8ColocateSets(pkgNode map[int][]int) [][]int {
	var out [][]int
	for _, pkg := range sortedPkgKeys(pkgNode) {
		if len(pkgNode[pkg]) >= 8 {
			out = append(out, combinations(pkgNode[pkg], 8)...)
		}
	}
	return out
}

// span8DefaultSets returns the rotcov cross-quadrant combinations, capped at combLimit.
func span8DefaultSets(nodePkg map[int]int) [][]int {
	sortedNodes := sortedNodeIDs(nodePkg)
	const combLimit = 16
	combos := rotcovCombinations(sortedNodes, 8)
	if len(combos) > combLimit {
		combos = combos[:combLimit]
	}
	return combos
}

// span8Variant emits the combo defs and the balloon-span8[-suffix] composite.
func span8Variant(nodeSets [][]int, nodePkg map[int]int, comboLocalityLabel, spanBalloonSuffix string) []*nriballoons.BalloonDef {
	if len(nodeSets) == 0 {
		return nil
	}
	var defs []*nriballoons.BalloonDef
	var names []string
	for i, nodeset := range nodeSets {
		name := fmt.Sprintf("%scomb%d%s-span8", comboLocalityLabel, i, spanBalloonSuffix)
		names = append(names, name)
		var comps []nriballoons.BalloonDefComponent
		for _, node := range nodeset {
			comps = append(comps, nriballoons.BalloonDefComponent{DefName: leafName(nodePkg[node], node)})
		}
		defs = append(defs, &nriballoons.BalloonDef{
			Name:              name,
			ComponentCreation: nriballoons.ComponentCreationAll,
			Components:        comps,
		})
	}
	span8 := newSpanBalloonDef(balloonNameBase + "-span8" + spanBalloonSuffix)
	for _, name := range names {
		span8.Components = append(span8.Components, nriballoons.BalloonDefComponent{DefName: name})
	}
	defs = append(defs, span8)
	return defs
}

func generateSpan8Balloons(pkgNode map[int][]int, nodePkg map[int]int) []*nriballoons.BalloonDef {
	var defs []*nriballoons.BalloonDef

	// balloon-span8 (default).
	defs = append(defs, span8Variant(span8DefaultSets(nodePkg), nodePkg, "crosspkg", "")...)

	// balloon-span8-colocate: only with ≥ 8 NUMA nodes per package.
	if sets := span8ColocateSets(pkgNode); len(sets) > 0 {
		defs = append(defs, span8Variant(sets, nodePkg, "localpkg", "-colocate")...)
	}

	// balloon-span8-spread: needs ≥ 2 packages, where the default output is
	// already cross-package and is re-emitted under this name.
	if len(pkgNode) >= 2 {
		defs = append(defs, span8Variant(span8DefaultSets(nodePkg), nodePkg, "crosspkg", "-spread")...)
	}

	return defs
}

type pkgNodePair struct {
	Pkg  int
	Node int
}

// optimizeGNR3Tile orders (pkg, node) leaves: systems with three NUMA nodes per
// socket (one or two sockets) prefer the middle node first; other multi-package systems interleave round-robin.
func optimizeGNR3Tile(pkgNode map[int][]int) []pkgNodePair {
	nodePkg := map[int]int{}
	for pkg, nodes := range pkgNode {
		for _, n := range nodes {
			nodePkg[n] = pkg
		}
	}
	nodes := sortedNodeIDs(nodePkg)

	var nodeOrder []int
	switch {
	case len(nodePkg) == 3 && len(pkgNode) == 1:
		nodeOrder = []int{1, 2, 0}
	case len(nodePkg) == 6 && len(pkgNode) == 2:
		nodeOrder = []int{4, 1, 5, 2, 3, 0}
	}

	if len(nodeOrder) > 0 {
		result := make([]pkgNodePair, 0, len(nodeOrder))
		for _, idx := range nodeOrder {
			node := nodes[idx]
			result = append(result, pkgNodePair{Pkg: nodePkg[node], Node: node})
		}
		return result
	}

	pkgs := sortedPkgKeys(pkgNode)
	maxLen := 0
	for _, pkg := range pkgs {
		if len(pkgNode[pkg]) > maxLen {
			maxLen = len(pkgNode[pkg])
		}
	}
	var result []pkgNodePair
	for i := 0; i < maxLen; i++ {
		for _, pkg := range pkgs {
			ns := pkgNode[pkg]
			if i < len(ns) {
				result = append(result, pkgNodePair{Pkg: pkg, Node: ns[i]})
			}
		}
	}
	return result
}

// rotcovCombinations yields n-sized combinations of elts, rotating the start
// index each time, until every element has been used equally often.
func rotcovCombinations(elts []int, n int) [][]int {
	if n > len(elts) {
		return nil
	}
	idxCount := make([]int, len(elts))
	nextStart := 0
	var result [][]int
	for {
		comb := make([]int, n)
		for i := 0; i < n; i++ {
			idx := (nextStart + i) % len(elts)
			comb[i] = elts[idx]
			idxCount[idx]++
		}
		result = append(result, comb)
		nextStart = (nextStart + n) % len(elts)
		refCount := idxCount[0]
		allEqual := true
		for _, c := range idxCount {
			if c != refCount {
				allEqual = false
				break
			}
		}
		if allEqual {
			break
		}
	}
	return result
}

// combinationIndices returns all k-sized index combinations of [0,n) in the
// same lexicographic order as Python's itertools.combinations.
func combinationIndices(n, k int) [][]int {
	if k > n || k < 0 {
		return nil
	}
	if k == 0 {
		return [][]int{{}}
	}
	var result [][]int
	indices := make([]int, k)
	for i := range indices {
		indices[i] = i
	}
	for {
		result = append(result, append([]int{}, indices...))
		i := k - 1
		for i >= 0 && indices[i] == n-k+i {
			i--
		}
		if i < 0 {
			break
		}
		indices[i]++
		for j := i + 1; j < k; j++ {
			indices[j] = indices[j-1] + 1
		}
	}
	return result
}

func combinations(elts []int, k int) [][]int {
	var result [][]int
	for _, idxs := range combinationIndices(len(elts), k) {
		comb := make([]int, len(idxs))
		for i, idx := range idxs {
			comb[i] = elts[idx]
		}
		result = append(result, comb)
	}
	return result
}

func noneSelected(comb []int, selected map[int]bool) bool {
	for _, n := range comb {
		if selected[n] {
			return false
		}
	}
	return true
}

func markSelected(comb []int, selected map[int]bool) {
	for _, n := range comb {
		selected[n] = true
	}
}

func concatInts(a, b []int) []int {
	out := make([]int, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

// zipConcat pairwise-concatenates a and b like Python's
// [x+y for x,y in zip(a,b)], stopping at the shorter slice.
func zipConcat(a, b [][]int) [][]int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	result := make([][]int, 0, n)
	for i := 0; i < n; i++ {
		result = append(result, concatInts(a[i], b[i]))
	}
	return result
}

func sortedNodeIDs(nodePkg map[int]int) []int {
	nodes := make([]int, 0, len(nodePkg))
	for n := range nodePkg {
		nodes = append(nodes, n)
	}
	sort.Ints(nodes)
	return nodes
}
