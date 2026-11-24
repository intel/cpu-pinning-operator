// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package nrt

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestParseCPUSet(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"", nil},
		{"0", []int{0}},
		{"0,10-12,102-105", []int{0, 10, 11, 12, 102, 103, 104, 105}},
		{" 3 , 5 ", []int{3, 5}},
		{"7-7", []int{7}},
		{"9-8", nil},      // invalid range dropped
		{"a,4", []int{4}}, // invalid token skipped
	}
	for _, tc := range cases {
		if got := ParseCPUSet(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseCPUSet(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestStripInstance(t *testing.T) {
	cases := map[string]string{
		"balloon-span2[0]": "balloon-span2",
		"reserved[0]":      "reserved",
		"default[1]":       "default",
		"balloon-span2":    "balloon-span2",
	}
	for in, want := range cases {
		if got := StripInstance(in); got != want {
			t.Errorf("StripInstance(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitContainerZoneName(t *testing.T) {
	cases := []struct {
		in                    string
		wNS, wPod, wContainer string
	}{
		{"inference/vllm-a/vllm", "inference", "vllm-a", "vllm"},
		{"ns/pod", "ns", "pod", ""},  // missing container
		{"weird", "", "", ""},        // not a zone name
		{"", "", "", ""},             // empty
		{"a/b/c/d", "a", "b", "c/d"}, // extra slashes stay in container
	}
	for _, tc := range cases {
		ns, pod, ctr := SplitContainerZoneName(tc.in)
		if ns != tc.wNS || pod != tc.wPod || ctr != tc.wContainer {
			t.Errorf("SplitContainerZoneName(%q) = (%q,%q,%q), want (%q,%q,%q)",
				tc.in, ns, pod, ctr, tc.wNS, tc.wPod, tc.wContainer)
		}
	}
}

func TestZoneAccessors(t *testing.T) {
	z := zone("balloon", "reserved", "", "0-3")
	if got := ZoneType(z); got != "balloon" {
		t.Errorf("ZoneType = %q, want %q", got, "balloon")
	}
	if got := ZoneName(z); got != "reserved" {
		t.Errorf("ZoneName = %q, want %q", got, "reserved")
	}
	if got := ZoneParent(zone("allocation for container", "ns/pod/ctr", "balloon-span2[0]", "0-3")); got != "balloon-span2[0]" {
		t.Errorf("ZoneParent = %q, want %q", got, "balloon-span2[0]")
	}

	// A wrong-typed or absent field reads as "", never an error.
	malformed := map[string]interface{}{"type": 42, "name": []interface{}{"x"}}
	if got := ZoneType(malformed); got != "" {
		t.Errorf("ZoneType(malformed) = %q, want empty", got)
	}
	if got := ZoneName(malformed); got != "" {
		t.Errorf("ZoneName(malformed) = %q, want empty", got)
	}
	if got := ZoneParent(map[string]interface{}{}); got != "" {
		t.Errorf("ZoneParent(absent) = %q, want empty", got)
	}
}

// zone builds an NRT zone map with a cpuset attribute for use in tests.
func zone(zoneType, name, parent, cpuset string) map[string]interface{} {
	return map[string]interface{}{
		"type":   zoneType,
		"name":   name,
		"parent": parent,
		"attributes": []interface{}{
			map[string]interface{}{"name": "cpuset", "value": cpuset},
		},
	}
}

func TestOccupiedCPUs(t *testing.T) {
	// Reserved container subzones and workload balloon parents must not be double-counted.
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"zones": []interface{}{
			zone(balloonZoneType, "reserved[0]", "", "0-1"),
			zone(balloonZoneType, "balloon-span2[0]", "", "10-13"),
			zone(containerZoneType, "inference/vllm-a/vllm", "balloon-span2[0]", "10-13"),
			zone(containerZoneType, "kube-system/coredns-x/coredns", "reserved[0]", "0-1"),
		},
	}}

	got, ok := OccupiedCPUs(obj)
	if !ok {
		t.Fatal("OccupiedCPUs(valid) ok = false, want true")
	}
	want := []int{0, 1, 10, 11, 12, 13}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OccupiedCPUs() = %v, want %v", got, want)
	}
}

func TestOccupiedCPUsEmpty(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	got, ok := OccupiedCPUs(obj)
	if !ok {
		t.Error("OccupiedCPUs(absent zones) ok = false, want true (absent is known-empty, not malformed)")
	}
	if len(got) != 0 {
		t.Errorf("OccupiedCPUs(empty) = %v, want empty", got)
	}
}

// zoneWithMem builds an NRT zone with a cpuset and a "memory set"; empty memSet pins no memory.
func zoneWithMem(zoneType, name, parent, cpuset, memSet string) map[string]interface{} {
	return map[string]interface{}{
		"type":   zoneType,
		"name":   name,
		"parent": parent,
		"attributes": []interface{}{
			map[string]interface{}{"name": "cpuset", "value": cpuset},
			map[string]interface{}{"name": "memory set", "value": memSet},
		},
	}
}

func TestZoneMemorySet(t *testing.T) {
	// Present + populated.
	z := zoneWithMem(containerZoneType, "inference/vllm/vllm", "balloon-span2[0]", "10-13", "0-1")
	if got := ZoneMemorySet(z); got != "0-1" {
		t.Errorf("ZoneMemorySet populated = %q, want %q", got, "0-1")
	}
	// Present but empty (balloon does not pin memory).
	if got := ZoneMemorySet(zoneWithMem(containerZoneType, "x", "y", "0", "")); got != "" {
		t.Errorf("ZoneMemorySet empty = %q, want \"\"", got)
	}
	// Absent entirely (only a cpuset attribute) → "".
	if got := ZoneMemorySet(zone(containerZoneType, "x", "y", "0")); got != "" {
		t.Errorf("ZoneMemorySet absent = %q, want \"\"", got)
	}
}

func TestPinnedMemoryContainers(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"zones": []interface{}{
			// Workload span-2 container pinning memory to NUMA 0-1 → counted.
			zoneWithMem(containerZoneType, "inference/vllm-a/vllm", "balloon-span2[0]", "10-13", "0-1"),
			// Workload span-1 container pinning memory to NUMA 0 → counted.
			zoneWithMem(containerZoneType, "inference/vllm-b/vllm", "balloon-span1[0]", "20", "0"),
			// Reserved-pool container subzone → skipped (reserved doesn't pin workload memory).
			zoneWithMem(containerZoneType, "kube-system/coredns/coredns", "reserved[0]", "0-1", ""),
			// Workload container with an EMPTY memory set → skipped (pins no memory).
			zoneWithMem(containerZoneType, "inference/nomem/app", "balloon-span1[1]", "21", ""),
			// A balloon parent zone → skipped (not a container subzone).
			zoneWithMem(balloonZoneType, "balloon-span2[0]", "", "10-13", "0-1"),
		},
	}}

	got, ok := PinnedMemoryContainers(obj)
	if !ok {
		t.Fatal("PinnedMemoryContainers(valid) ok = false, want true")
	}
	want := []PinnedMemoryContainer{
		{Zone: "inference/vllm-a/vllm", NUMAs: []int{0, 1}},
		{Zone: "inference/vllm-b/vllm", NUMAs: []int{0}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PinnedMemoryContainers() = %+v, want %+v", got, want)
	}
}

func TestContainerZoneNames(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"zones": []interface{}{
			// Two workload container zones → both named.
			zone(containerZoneType, "inference/vllm-a/vllm", "balloon-span1[0]", "3-18"),
			zone(containerZoneType, "inference/vllm-b/vllm", "balloon-span2[0]", "19-34"),
			// Duplicate zone (the plugin's leak signature) → preserved.
			zone(containerZoneType, "inference/vllm-a/vllm", "balloon-span1[0]", "35-50"),
			// Reserved-pool container subzone → skipped.
			zone(containerZoneType, "kube-system/coredns/coredns", "reserved[0]", "0-2"),
			// Empty-cpuset container zone → skipped (claims no capacity).
			zone(containerZoneType, "inference/empty/app", "balloon-span1[1]", ""),
			// A balloon parent zone → skipped (not a container subzone).
			zone(balloonZoneType, "balloon-span2[0]", "", "19-34"),
		},
	}}

	got, ok := ContainerZoneNames(obj)
	if !ok {
		t.Fatal("ContainerZoneNames(valid) ok = false, want true")
	}
	want := []string{
		"inference/vllm-a/vllm",
		"inference/vllm-b/vllm",
		"inference/vllm-a/vllm",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ContainerZoneNames() = %v, want %v", got, want)
	}
}

func TestContainerZoneNamesEmpty(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	got, ok := ContainerZoneNames(obj)
	if !ok {
		t.Error("ContainerZoneNames(absent zones) ok = false, want true")
	}
	if len(got) != 0 {
		t.Errorf("ContainerZoneNames(empty) = %v, want empty", got)
	}
}

func TestPinnedMemoryContainersEmpty(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	got, ok := PinnedMemoryContainers(obj)
	if !ok {
		t.Error("PinnedMemoryContainers(absent zones) ok = false, want true")
	}
	if len(got) != 0 {
		t.Errorf("PinnedMemoryContainers(empty) = %v, want empty", got)
	}
}

// TestMalformedZonesUnknownNotEmpty proves a wrong-typed "zones" reads as unknown (ok=false), not empty.
func TestMalformedZonesUnknownNotEmpty(t *testing.T) {
	// "zones" is a string, not a []interface{} — NestedSlice returns a type error.
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"zones": "not-a-slice",
	}}

	if cpus, ok := OccupiedCPUs(obj); ok {
		t.Errorf("OccupiedCPUs(malformed) ok = true (cpus=%v), want false (unknown, not empty)", cpus)
	}
	if c, ok := PinnedMemoryContainers(obj); ok {
		t.Errorf("PinnedMemoryContainers(malformed) ok = true (containers=%v), want false", c)
	}
	if names, ok := ContainerZoneNames(obj); ok {
		t.Errorf("ContainerZoneNames(malformed) ok = true (names=%v), want false", names)
	}
	if HasZones(obj) {
		t.Error("HasZones(malformed) = true, want false (unknown, not a claim of zones)")
	}
	if balloon, found := ContainerBalloon(obj, "inference/vllm/vllm"); found {
		t.Errorf("ContainerBalloon(malformed) found = true (balloon=%q), want false", balloon)
	}
}

func TestHasZones(t *testing.T) {
	empty := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if HasZones(empty) {
		t.Error("HasZones(no zones key) = true, want false")
	}
	emptyList := &unstructured.Unstructured{Object: map[string]interface{}{"zones": []interface{}{}}}
	if HasZones(emptyList) {
		t.Error("HasZones(empty zones list) = true, want false")
	}
	populated := &unstructured.Unstructured{Object: map[string]interface{}{
		"zones": []interface{}{zone(balloonZoneType, "reserved[0]", "", "0-1")},
	}}
	if !HasZones(populated) {
		t.Error("HasZones(populated) = false, want true")
	}
}

func TestContainerBalloon(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"zones": []interface{}{
			zone(balloonZoneType, "balloon-span2[0]", "", "10-13"),
			// A workload container bound to a dedicated balloon (actually pinned).
			zone(containerZoneType, "inference/vllm-a/vllm", "balloon-span2[0]", "10-13"),
			// A container bound to the reserved/shared pool (running unpinned).
			zone(containerZoneType, "inference/shared/app", "reserved[0]", "0-1"),
			// A container subzone with no cpuset (no placement → treated as absent).
			zone(containerZoneType, "inference/nocpu/app", "balloon-span2[0]", ""),
			// A cpuset but no parent balloon: must read as absent, or it masks a lost pin.
			zone(containerZoneType, "inference/noparent/app", "", "10-13"),
		},
	}}

	cases := []struct {
		name        string
		zoneName    string
		wantBalloon string
		wantFound   bool
	}{
		// Confined to a dedicated workload balloon (instance suffix stripped).
		{"pinned", "inference/vllm-a/vllm", "balloon-span2", true},
		// Bound to the reserved pool → found, but the reserved balloon name.
		{"reserved", "inference/shared/app", reservedBalloonName, true},
		// Empty cpuset → treated as not found (no real placement).
		{"empty cpuset", "inference/nocpu/app", "", false},
		// Cpuset but empty parent balloon → not found.
		{"empty parent", "inference/noparent/app", "", false},
		// No subzone at all → not found.
		{"absent", "inference/ghost/app", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			balloon, found := ContainerBalloon(obj, tc.zoneName)
			if balloon != tc.wantBalloon || found != tc.wantFound {
				t.Errorf("ContainerBalloon(%q) = (%q, %v), want (%q, %v)",
					tc.zoneName, balloon, found, tc.wantBalloon, tc.wantFound)
			}
		})
	}
}
