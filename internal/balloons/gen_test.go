// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package balloons

import (
	"strconv"
	"testing"

	nriballoons "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/balloons"

	"github.com/intel/cpu-pinning-operator/internal/topology"
)

func findDef(defs []*nriballoons.BalloonDef, name string) *nriballoons.BalloonDef {
	for _, d := range defs {
		if d.Name == name {
			return d
		}
	}
	return nil
}

func componentNames(d *nriballoons.BalloonDef) []string {
	var names []string
	for _, c := range d.Components {
		names = append(names, c.DefName)
	}
	return names
}

func containsAll(got []string, want ...string) bool {
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return len(got) == len(want)
}

// TestGenerateBalloonDefs_TwoSocketFourNode pins the output for PKG_NODE={0:[0,1],1:[2,3]} MAX_TP=4.
func TestGenerateBalloonDefs_TwoSocketFourNode(t *testing.T) {
	pkgNode := map[int][]int{0: {0, 1}, 1: {2, 3}}
	defs := GenerateBalloonDefs(pkgNode, 4, "physical-cores")

	// Leaf balloons per NUMA node, with no HT options.
	for _, name := range []string{"pkg0node0", "pkg0node1", "pkg1node2", "pkg1node3"} {
		leaf := findDef(defs, name)
		if leaf == nil {
			t.Errorf("missing leaf balloon %s", name)
			continue
		}
		assertNoHTAttrs(t, leaf)
	}

	// Per-package balloons.
	pkg0nodes := findDef(defs, "pkg0nodes")
	if pkg0nodes == nil {
		t.Fatal("missing pkg0nodes")
	}
	if pkg0nodes.ComponentCreation != nriballoons.ComponentCreationAll {
		t.Errorf("pkg0nodes componentCreation = %s, want all", pkg0nodes.ComponentCreation)
	}
	if !containsAll(componentNames(pkg0nodes), "pkg0node0", "pkg0node1") {
		t.Errorf("pkg0nodes components = %v", componentNames(pkg0nodes))
	}

	pkg1nodes := findDef(defs, "pkg1nodes")
	if pkg1nodes == nil || !containsAll(componentNames(pkg1nodes), "pkg1node2", "pkg1node3") {
		t.Errorf("pkg1nodes components = %v", componentNames(pkg1nodes))
	}

	// tp1: balance-balloons over all 4 leaves, round-robin across packages: 0,2,1,3.
	tp1 := findDef(defs, "balloon-span1")
	if tp1 == nil {
		t.Fatal("missing balloon-span1")
	}
	if tp1.ComponentCreation != nriballoons.ComponentCreationBalanceBalloons {
		t.Errorf("tp1 componentCreation = %s, want balance-balloons", tp1.ComponentCreation)
	}
	wantOrder := []string{"pkg0node0", "pkg1node2", "pkg0node1", "pkg1node3"}
	gotOrder := componentNames(tp1)
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("tp1 components = %v, want %v", gotOrder, wantOrder)
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Errorf("tp1 component[%d] = %s, want %s (full: %v)", i, gotOrder[i], wantOrder[i], gotOrder)
		}
	}
	assertSpanAttrs(t, tp1)

	// tp2: two non-overlapping local-package pairs.
	comb0 := findDef(defs, "localpkgcomb0-span2")
	if comb0 == nil || !containsAll(componentNames(comb0), "pkg0node0", "pkg0node1") {
		t.Errorf("localpkgcomb0-span2 components = %v", componentNamesOrNil(comb0))
	}
	comb1 := findDef(defs, "localpkgcomb1-span2")
	if comb1 == nil || !containsAll(componentNames(comb1), "pkg1node2", "pkg1node3") {
		t.Errorf("localpkgcomb1-span2 components = %v", componentNamesOrNil(comb1))
	}
	tp2 := findDef(defs, "balloon-span2")
	if tp2 == nil {
		t.Fatal("missing balloon-span2")
	}
	if !containsAll(componentNames(tp2), "localpkgcomb0-span2", "localpkgcomb1-span2") {
		t.Errorf("tp2 components = %v", componentNames(tp2))
	}
	assertSpanAttrs(t, tp2)

	// tp4: two-socket cross-package combo of both pkgs' 2-node combos.
	tp4comb0 := findDef(defs, "crosspkgcomb0-span4")
	if tp4comb0 == nil {
		t.Fatal("missing crosspkgcomb0-span4")
	}
	if !containsAll(componentNames(tp4comb0), "pkg0node0", "pkg0node1", "pkg1node2", "pkg1node3") {
		t.Errorf("crosspkgcomb0-span4 components = %v", componentNames(tp4comb0))
	}
	tp4 := findDef(defs, "balloon-span4")
	if tp4 == nil {
		t.Fatal("missing balloon-span4")
	}
	if !containsAll(componentNames(tp4), "crosspkgcomb0-span4") {
		t.Errorf("tp4 components = %v", componentNames(tp4))
	}
	assertSpanAttrs(t, tp4)

	// tp8 not generated (only 4 NUMA nodes, maxSpan=4).
	if findDef(defs, "balloon-span8") != nil {
		t.Error("balloon-span8 should not be generated for a 4-node topology")
	}

	// reserved present, no catch-all default.
	if findDef(defs, reservedBalloonName) == nil {
		t.Error("missing reserved balloon")
	}
	if findDef(defs, "default") != nil {
		t.Error("unexpected catch-all default balloon")
	}
}

// GenerateBalloonDefs leaves the reserved balloon's MinCpus/MaxCpus unset.
func TestGenerateBalloonDefs_ReservedUnsetBeforePin(t *testing.T) {
	defs := GenerateBalloonDefs(map[int][]int{0: {0, 1}, 1: {2, 3}}, 4, "physical-cores")
	reserved := findDef(defs, reservedBalloonName)
	if reserved == nil {
		t.Fatal("missing reserved balloon")
	}
	if reserved.MinCpus != 0 || reserved.MaxCpus != 0 {
		t.Errorf("reserved MinCpus/MaxCpus = %d/%d before pin, want 0/0",
			reserved.MinCpus, reserved.MaxCpus)
	}
}

// pinReservedBalloonSize pins only the reserved def, and is a no-op for size <= 0 or no reserved def.
func TestPinReservedBalloonSize(t *testing.T) {
	t.Run("pins only the reserved def", func(t *testing.T) {
		defs := GenerateBalloonDefs(map[int][]int{0: {0, 1}}, 2, "physical-cores")
		pinReservedBalloonSize(defs, 6)
		reserved := findDef(defs, reservedBalloonName)
		if reserved == nil {
			t.Fatal("missing reserved balloon")
		}
		if reserved.MinCpus != 6 || reserved.MaxCpus != 6 {
			t.Errorf("reserved MinCpus/MaxCpus = %d/%d, want 6/6", reserved.MinCpus, reserved.MaxCpus)
		}
		// Workload/composite balloons must NOT be pinned.
		for _, d := range defs {
			if d.Name == reservedBalloonName {
				continue
			}
			if d.MinCpus != 0 || d.MaxCpus != 0 {
				t.Errorf("%s MinCpus/MaxCpus = %d/%d, want 0/0 (only reserved is pinned)",
					d.Name, d.MinCpus, d.MaxCpus)
			}
		}
	})

	t.Run("non-positive size leaves reserved unset", func(t *testing.T) {
		for _, size := range []int{0, -1} {
			defs := []*nriballoons.BalloonDef{{Name: reservedBalloonName}}
			pinReservedBalloonSize(defs, size)
			if defs[0].MinCpus != 0 || defs[0].MaxCpus != 0 {
				t.Errorf("size %d pinned reserved to %d/%d, want left unset 0/0",
					size, defs[0].MinCpus, defs[0].MaxCpus)
			}
		}
	})

	t.Run("no reserved def is a no-op", func(t *testing.T) {
		defs := []*nriballoons.BalloonDef{{Name: "pkg0node0"}}
		pinReservedBalloonSize(defs, 4) // must not panic or mutate
		if defs[0].MinCpus != 0 || defs[0].MaxCpus != 0 {
			t.Errorf("non-reserved def mutated to %d/%d", defs[0].MinCpus, defs[0].MaxCpus)
		}
	})
}

// TestMaxAdvertisableSpan_MatchesEmittedDefs proves the advertised span is always
// servable, without under-advertising shapes the span-4 search covers.
func TestMaxAdvertisableSpan_MatchesEmittedDefs(t *testing.T) {
	cases := []struct {
		label string
		// counts is the per-package NUMA-node count.
		counts             []int
		wantSpan           int
		wantLargestEmitted int
	}{
		// Shapes with no span-4 layout: the NUMA count alone would advertise 4 or 8.
		{"3-socket 2+2+2", []int{2, 2, 2}, 2, 2},
		{"3-socket 2+2+3", []int{2, 2, 3}, 2, 2},
		{"3-socket 2+3+3 (span-8 emitted, span-4 missing)", []int{2, 3, 3}, 2, 8},
		{"2-socket 1+3", []int{1, 3}, 2, 2},
		{"2-socket 1+5", []int{1, 5}, 2, 2},
		{"4 packages 1+1+2+2", []int{1, 1, 2, 2}, 2, 2},
		{"6 single-NUMA packages", []int{1, 1, 1, 1, 1, 1}, 2, 2},
		{"8 single-NUMA packages (span-8 emitted, span-4 missing)", []int{1, 1, 1, 1, 1, 1, 1, 1}, 2, 8},
		{"6 packages 2+2+2+2+2+2", []int{2, 2, 2, 2, 2, 2}, 2, 8},
		// Shapes the span-4 / span-8 searches do cover: unchanged.
		{"single package, 1 NUMA", []int{1}, 1, 1},
		{"2-socket 1+1", []int{1, 1}, 2, 2},
		{"2-socket 2+2", []int{2, 2}, 4, 4},
		{"2-socket 2+3 (one CPU-less NUMA joins package 0)", []int{2, 3}, 4, 4},
		{"2-socket 3+3", []int{3, 3}, 4, 4},
		{"single package, 4 NUMA", []int{4}, 4, 4},
		{"4 single-NUMA packages", []int{1, 1, 1, 1}, 4, 4},
		{"single package, 8 NUMA", []int{8}, 8, 8},
		{"4 packages 2+2+2+2", []int{2, 2, 2, 2}, 8, 8},
		// The span-4 cross-quadrant fallback first fires at 13 NUMA nodes.
		{"8 packages 2 each", []int{2, 2, 2, 2, 2, 2, 2, 2}, 8, 8},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			total := 0
			for _, c := range tc.counts {
				total += c
			}
			// Invoked as the Node reconciler does, capped by topology.MaxSpanForNUMA.
			genCap := topology.MaxSpanForNUMA(total)
			defs := GenerateBalloonDefs(syntheticPkgNode(tc.counts), genCap, "physical-cores")

			got := MaxAdvertisableSpan(defs)
			if got != tc.wantSpan {
				t.Errorf("MaxAdvertisableSpan = %d, want %d (%d NUMA nodes, generator cap %d)",
					got, tc.wantSpan, total, genCap)
			}
			// Every span the label admits must have its balloon.
			for _, span := range generatedSpans {
				if span > got {
					continue
				}
				name := spanBalloonPrefix + strconv.Itoa(span)
				if findDef(defs, name) == nil {
					t.Errorf("label advertises span %d but %s is not emitted", got, name)
				}
			}
			// And it may never exceed the largest span that IS emitted.
			largest := 0
			for _, span := range generatedSpans {
				if findDef(defs, spanBalloonPrefix+strconv.Itoa(span)) != nil {
					largest = span
				}
			}
			if largest != tc.wantLargestEmitted {
				t.Errorf("largest emitted span = %d, want %d", largest, tc.wantLargestEmitted)
			}
			if got > largest {
				t.Errorf("label advertises span %d, above the largest emitted span %d", got, largest)
			}
		})
	}
}

// TestMaxAdvertisableSpan_Gaps covers the def sets the layout table cannot
// produce: no span balloon at all, and a run broken at its first step.
func TestMaxAdvertisableSpan_Gaps(t *testing.T) {
	named := func(names ...string) []*nriballoons.BalloonDef {
		defs := make([]*nriballoons.BalloonDef, 0, len(names))
		for _, n := range names {
			defs = append(defs, &nriballoons.BalloonDef{Name: n})
		}
		return defs
	}
	cases := []struct {
		label string
		defs  []*nriballoons.BalloonDef
		want  int
	}{
		{"no defs at all", nil, 0},
		{"leaves only", named("pkg0node0", "pkg0nodes", reservedBalloonName), 0},
		{"span1 missing", named("balloon-span2", "balloon-span4"), 0},
		{"run stops at 2", named("balloon-span1", "balloon-span2", "balloon-span8"), 2},
		{"variants do not extend the run", named("balloon-span1", "balloon-span2", "balloon-span4-colocate"), 2},
		{"full run", named("balloon-span1", "balloon-span2", "balloon-span4", "balloon-span8"), 8},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			if got := MaxAdvertisableSpan(tc.defs); got != tc.want {
				t.Errorf("MaxAdvertisableSpan = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestOptimizeGNR3Tile pins balloon-span1's leaf order: middle node first with three NUMA nodes per socket, else round-robin.
func TestOptimizeGNR3Tile(t *testing.T) {
	t.Run("1-socket 3-node", func(t *testing.T) {
		got := optimizeGNR3Tile(map[int][]int{0: {0, 1, 2}})
		want := []pkgNodePair{{0, 1}, {0, 2}, {0, 0}}
		assertPairOrder(t, got, want)
	})

	t.Run("2-socket 6-node", func(t *testing.T) {
		pkgNode := map[int][]int{0: {0, 1, 2}, 1: {3, 4, 5}}
		want := []pkgNodePair{{1, 4}, {0, 1}, {1, 5}, {0, 2}, {1, 3}, {0, 0}}
		assertPairOrder(t, optimizeGNR3Tile(pkgNode), want)

		// The order must reach the balloon-span1 components.
		tp1 := findDef(GenerateBalloonDefs(pkgNode, 4, "physical-cores"), "balloon-span1")
		if tp1 == nil {
			t.Fatal("missing balloon-span1")
		}
		var wantNames []string
		for _, p := range want {
			wantNames = append(wantNames, leafName(p.Pkg, p.Node))
		}
		got := componentNames(tp1)
		if len(got) != len(wantNames) {
			t.Fatalf("balloon-span1 components = %v, want %v", got, wantNames)
		}
		for i := range wantNames {
			if got[i] != wantNames[i] {
				t.Errorf("balloon-span1 component[%d] = %s, want %s (full: %v)", i, got[i], wantNames[i], got)
			}
		}
	})

	t.Run("3-socket 6-node falls back to round-robin", func(t *testing.T) {
		got := optimizeGNR3Tile(map[int][]int{0: {0, 1}, 1: {2, 3}, 2: {4, 5}})
		want := []pkgNodePair{{0, 0}, {1, 2}, {2, 4}, {0, 1}, {1, 3}, {2, 5}}
		assertPairOrder(t, got, want)
	})

	t.Run("uneven packages interleave zip-longest", func(t *testing.T) {
		got := optimizeGNR3Tile(map[int][]int{0: {0}, 1: {1, 2}})
		want := []pkgNodePair{{0, 0}, {1, 1}, {1, 2}}
		assertPairOrder(t, got, want)
	})
}

func assertPairOrder(t *testing.T, got, want []pkgNodePair) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("order[%d] = %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}
}

func componentNamesOrNil(d *nriballoons.BalloonDef) []string {
	if d == nil {
		return nil
	}
	return componentNames(d)
}

// assertSpanAttrs verifies a composite carries no CPU-allocation options.
func assertSpanAttrs(t *testing.T, d *nriballoons.BalloonDef) {
	t.Helper()
	if !d.PreferNewBalloons {
		t.Errorf("%s: PreferNewBalloons = false, want true", d.Name)
	}
	if d.PinMemory == nil || !*d.PinMemory {
		t.Errorf("%s: PinMemory = %v, want true", d.Name, d.PinMemory)
	}
	if d.HideHyperthreads != nil {
		t.Errorf("%s: HideHyperthreads = %v, want nil (forbidden on composite balloons)", d.Name, *d.HideHyperthreads)
	}
	if d.PreferSpreadOnPhysicalCores != nil {
		t.Errorf("%s: PreferSpreadOnPhysicalCores = %v, want nil (forbidden on composite balloons)", d.Name, *d.PreferSpreadOnPhysicalCores)
	}
	if d.CpuClass != "" {
		t.Errorf("%s: CpuClass = %q, want empty (forbidden on composite balloons)", d.Name, d.CpuClass)
	}
}

// assertNoHTAttrs verifies a leaf carries no hyperthread options (physical-cores mode).
func assertNoHTAttrs(t *testing.T, d *nriballoons.BalloonDef) {
	t.Helper()
	if d.HideHyperthreads != nil {
		t.Errorf("%s: HideHyperthreads = %v, want nil (physical-cores uses exclude-cpuset)", d.Name, *d.HideHyperthreads)
	}
	if d.PreferSpreadOnPhysicalCores != nil {
		t.Errorf("%s: PreferSpreadOnPhysicalCores = %v, want nil (physical-cores uses exclude-cpuset)", d.Name, *d.PreferSpreadOnPhysicalCores)
	}
}

// assertHyperthreadsLeafAttrs verifies a hyperthreads-mode leaf carries both options explicitly false.
func assertHyperthreadsLeafAttrs(t *testing.T, d *nriballoons.BalloonDef) {
	t.Helper()
	if d.HideHyperthreads == nil || *d.HideHyperthreads {
		t.Errorf("%s: HideHyperthreads = %v, want false", d.Name, d.HideHyperthreads)
	}
	if d.PreferSpreadOnPhysicalCores == nil || *d.PreferSpreadOnPhysicalCores {
		t.Errorf("%s: PreferSpreadOnPhysicalCores = %v, want false", d.Name, d.PreferSpreadOnPhysicalCores)
	}
}

func TestGenerateBalloonDefs_HyperthreadsMode(t *testing.T) {
	pkgNode := map[int][]int{0: {0, 1}}
	defs := GenerateBalloonDefs(pkgNode, 2, "hyperthreads")

	// hyperthreads mode is unchanged: leaves carry both options explicitly false.
	leaf := findDef(defs, "pkg0node0")
	if leaf == nil {
		t.Fatal("missing pkg0node0")
	}
	assertHyperthreadsLeafAttrs(t, leaf)

	// Composite still carries no CPU-allocation options.
	tp1 := findDef(defs, "balloon-span1")
	if tp1 == nil {
		t.Fatal("missing balloon-span1")
	}
	assertSpanAttrs(t, tp1)
}

func TestGenerateBalloonDefs_SingleNode(t *testing.T) {
	pkgNode := map[int][]int{0: {0}}
	defs := GenerateBalloonDefs(pkgNode, 1, "physical-cores")

	if findDef(defs, "pkg0node0") == nil {
		t.Error("missing pkg0node0")
	}
	tp1 := findDef(defs, "balloon-span1")
	if tp1 == nil {
		t.Fatal("missing balloon-span1")
	}
	if !containsAll(componentNames(tp1), "pkg0node0") {
		t.Errorf("tp1 components = %v, want [pkg0node0]", componentNames(tp1))
	}
	if findDef(defs, "balloon-span2") != nil {
		t.Error("tp2 should not be generated for a single-node topology")
	}
}

func TestGenerateBalloonDefs_EightNode(t *testing.T) {
	pkgNode := map[int][]int{
		0: {0, 1}, 1: {2, 3}, 2: {4, 5}, 3: {6, 7},
	}
	defs := GenerateBalloonDefs(pkgNode, 8, "physical-cores")

	tp8 := findDef(defs, "balloon-span8")
	if tp8 == nil {
		t.Fatal("missing balloon-span8")
	}
	if len(tp8.Components) == 0 {
		t.Error("balloon-span8 has no components")
	}
	// Every referenced intermediate combo must exist and reference 8 distinct leaves.
	for _, comp := range tp8.Components {
		combo := findDef(defs, comp.DefName)
		if combo == nil {
			t.Errorf("tp8 references missing combo %s", comp.DefName)
			continue
		}
		if len(combo.Components) != 8 {
			t.Errorf("%s has %d components, want 8", comp.DefName, len(combo.Components))
		}
	}
}

// TestGenerateBalloonDefs_DeviceBalloon proves every topology gets exactly one
// leaf device balloon with PreferCloseToDevices ["podresourceapi:*/*"],
// PreferNewBalloons and PreferSpreadingPods.
func TestGenerateBalloonDefs_DeviceBalloon(t *testing.T) {
	topos := []struct {
		label   string
		pkgNode map[int][]int
		maxSpan int
	}{
		{"single-NUMA single-package", map[int][]int{0: {0}}, 1},
		{"2-socket / 4-NUMA", map[int][]int{0: {0, 1}, 1: {2, 3}}, 4},
	}
	for _, topo := range topos {
		t.Run(topo.label, func(t *testing.T) {
			defs := GenerateBalloonDefs(topo.pkgNode, topo.maxSpan, "physical-cores")

			// Exactly one device balloon, regardless of topology.
			count := 0
			for _, d := range defs {
				if d.Name == DeviceBalloonName {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("found %d %q balloons, want exactly 1", count, DeviceBalloonName)
			}

			dev := findDef(defs, DeviceBalloonName)
			if dev == nil {
				t.Fatalf("missing %q balloon", DeviceBalloonName)
			}

			wantHint := []string{"podresourceapi:*/*"}
			if !containsAll(dev.PreferCloseToDevices, wantHint...) {
				t.Errorf("%s PreferCloseToDevices = %v, want %v",
					DeviceBalloonName, dev.PreferCloseToDevices, wantHint)
			}
			if !dev.PreferNewBalloons {
				t.Errorf("%s PreferNewBalloons = false, want true", DeviceBalloonName)
			}
			if !dev.PreferSpreadingPods {
				t.Errorf("%s PreferSpreadingPods = false, want true", DeviceBalloonName)
			}
			// Must be a leaf: only leaves may carry preferCloseToDevices.
			if len(dev.Components) != 0 {
				t.Errorf("%s has %d components, want 0 (must be a leaf to carry preferCloseToDevices)",
					DeviceBalloonName, len(dev.Components))
			}
			if dev.ComponentCreation != "" {
				t.Errorf("%s ComponentCreation = %q, want empty (leaf)", DeviceBalloonName, dev.ComponentCreation)
			}
		})
	}
}

func TestRotcovCombinations_CoversAllEqually(t *testing.T) {
	elts := []int{0, 1, 2, 3, 4, 5, 6, 7}
	combos := rotcovCombinations(elts, 8)
	if len(combos) != 1 {
		t.Fatalf("8-choose-8 rotation should yield exactly 1 combination, got %d", len(combos))
	}

	elts2 := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	combos2 := rotcovCombinations(elts2, 8)
	count := map[int]int{}
	for _, c := range combos2 {
		for _, e := range c {
			count[e]++
		}
	}
	ref := count[elts2[0]]
	for _, e := range elts2 {
		if count[e] != ref {
			t.Errorf("element %d used %d times, want %d (uneven coverage)", e, count[e], ref)
		}
	}
}

func TestCombinations_MatchesExpectedCount(t *testing.T) {
	elts := []int{0, 1, 2, 3}
	combos := combinations(elts, 2)
	// C(4,2) = 6
	if len(combos) != 6 {
		t.Fatalf("combinations(4,2) = %d combos, want 6", len(combos))
	}
	want := [][]int{{0, 1}, {0, 2}, {0, 3}, {1, 2}, {1, 3}, {2, 3}}
	for i, w := range want {
		if combos[i][0] != w[0] || combos[i][1] != w[1] {
			t.Errorf("combos[%d] = %v, want %v", i, combos[i], w)
		}
	}
}
