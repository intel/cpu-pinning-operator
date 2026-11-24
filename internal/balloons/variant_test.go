// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package balloons

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestPerPackageNUMACounts(t *testing.T) {
	cases := []struct {
		pkgNode map[int][]int
		want    []int
	}{
		{map[int][]int{0: {0}, 1: {1}}, []int{1, 1}},       // dual-socket, 1 NUMA per pkg
		{map[int][]int{0: {0, 1}, 1: {2, 3}}, []int{2, 2}}, // 2 pkgs × 2 NUMA
		{map[int][]int{0: {0, 1, 2}, 1: {3}}, []int{1, 3}}, // uneven → sorted ascending
		{map[int][]int{0: {0, 1, 2, 3}}, []int{4}},         // single package, 4 NUMA
		{map[int][]int{}, []int{}},                         // empty
	}
	for _, c := range cases {
		if got := PerPackageNUMACounts(c.pkgNode); !reflect.DeepEqual(got, c.want) {
			t.Errorf("PerPackageNUMACounts(%v) = %v, want %v", c.pkgNode, got, c.want)
		}
	}
}

func TestFormatParsePackageTopologyRoundTrip(t *testing.T) {
	cases := []struct {
		counts []int
		label  string
	}{
		{[]int{1, 1}, "1_1"},
		{[]int{2, 2}, "2_2"},
		{[]int{1, 3}, "1_3"},
		{[]int{4}, "4"},
	}
	for _, c := range cases {
		if got := FormatPackageTopology(c.counts); got != c.label {
			t.Errorf("FormatPackageTopology(%v) = %q, want %q", c.counts, got, c.label)
		}
		if got := ParsePackageTopology(c.label); !reflect.DeepEqual(got, c.counts) {
			t.Errorf("ParsePackageTopology(%q) = %v, want %v", c.label, got, c.counts)
		}
	}
	// Every formatted value must be a legal Kubernetes label value (no comma).
	for _, c := range cases {
		if strings.ContainsAny(c.label, ",") {
			t.Errorf("label %q contains a comma (illegal in a k8s label value)", c.label)
		}
	}
	// Empty / garbage / non-positive → nil (unknown), never a partial shape.
	if FormatPackageTopology(nil) != "" {
		t.Error("FormatPackageTopology(nil) should be empty")
	}
	for _, bad := range []string{"", "  ", "1_x", "2_0", "-1", "1__2"} {
		if got := ParsePackageTopology(bad); got != nil {
			t.Errorf("ParsePackageTopology(%q) = %v, want nil (unknown)", bad, got)
		}
	}
	// Parse normalizes order: "3_1" → sorted {1,3}.
	if got := ParsePackageTopology("3_1"); !reflect.DeepEqual(got, []int{1, 3}) {
		t.Errorf("ParsePackageTopology(\"3_1\") = %v, want [1 3]", got)
	}
	// Unsorted input yields the sorted encoding without mutating the caller's slice.
	unsorted := []int{3, 1, 2}
	if got := FormatPackageTopology(unsorted); got != "1_2_3" {
		t.Errorf("FormatPackageTopology(%v) = %q, want %q (sorts internally)", unsorted, got, "1_2_3")
	}
	if !reflect.DeepEqual(unsorted, []int{3, 1, 2}) {
		t.Errorf("FormatPackageTopology mutated its input: %v", unsorted)
	}
}

func TestVariantEmittableUnknownTopology(t *testing.T) {
	if VariantEmittable(nil, 2, "-colocate") {
		t.Error("unknown topology (nil counts) must not support any variant")
	}
	if VariantEmittable([]int{2, 2}, 0, "") {
		t.Error("span < 1 must be unsupported")
	}
}

// TestVariantEmittableMatchesGenerator proves VariantEmittable agrees exactly with GenerateBalloonDefs.
func TestVariantEmittableMatchesGenerator(t *testing.T) {
	topologies := []map[int][]int{
		{0: {0}},               // 1 pkg × 1  → counts {1}
		{0: {0}, 1: {1}},       // dual-socket, 1 NUMA per pkg → {1,1}
		{0: {0, 1}},            // 1 pkg × 2  → {2}
		{0: {0, 1}, 1: {2, 3}}, // 2 pkgs × 2 → {2,2}
		{0: {0, 1, 2, 3}},      // 1 pkg × 4  → {4}
		{0: {0, 1, 2}, 1: {3}}, // uneven {1,3}
		{0: {0, 1}, 1: {2, 3}, 2: {4, 5}, 3: {6, 7}}, // 4 pkgs × 2 → {2,2,2,2}
		{0: {0}, 1: {1}, 2: {2}, 3: {3}},             // 4 pkgs × 1 → {1,1,1,1}
	}
	suffixes := []string{"", "-colocate", "-spread"}
	for _, pkgNode := range topologies {
		counts := PerPackageNUMACounts(pkgNode)
		for _, span := range []int{1, 2, 4, 8} {
			// The generator is invoked with maxSpan == span (matching VariantEmittable).
			emitted := map[string]bool{}
			for _, def := range GenerateBalloonDefs(pkgNode, span, "physical-cores") {
				emitted[def.Name] = true
			}
			for _, suffix := range suffixes {
				name := balloonNameBase + "-span" + strconv.Itoa(span) + suffix
				want := emitted[name]
				got := VariantEmittable(counts, span, suffix)
				if got != want {
					t.Errorf("topo %v span %d suffix %q: VariantEmittable=%v but generator emits=%v",
						pkgNode, span, suffix, got, want)
				}
			}
		}
	}
}

// TestVariantEmittableHardwareFacts proves "1_1" emits span2-spread but not span2-colocate; 2×2 emits both.
func TestVariantEmittableHardwareFacts(t *testing.T) {
	oneOne := []int{1, 1} // dual-socket, 1 NUMA per package
	if VariantEmittable(oneOne, 2, "-colocate") {
		t.Error(`"1,1" must NOT support span2-colocate (no package has ≥2 NUMA)`)
	}
	if !VariantEmittable(oneOne, 2, "-spread") {
		t.Error(`"1,1" must support span2-spread (2 packages)`)
	}
	twoTwo := []int{2, 2}
	if !VariantEmittable(twoTwo, 2, "-colocate") {
		t.Error(`"2,2" must support span2-colocate (each package has 2 NUMA)`)
	}
	if !VariantEmittable(twoTwo, 2, "-spread") {
		t.Error(`"2,2" must support span2-spread`)
	}
	// A single-package node can't spread anything.
	if VariantEmittable([]int{2}, 2, "-spread") {
		t.Error(`"2" (one package) must NOT support span2-spread`)
	}
}
