// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package nrt

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// NRT zone attributes are written by a node-side plugin and read on every
// admission, so they are untrusted input; these targets fuzz that path.

// wantCPUSet is the naive oracle for ParseCPUSet; it shares cpuSetRanges so only
// the expansion is under test.
func wantCPUSet(s string) []int {
	var seen [maxCPUID + 1]bool
	cpuSetRanges(s, func(l, h int) {
		for i := l; i <= h; i++ {
			seen[i] = true
		}
	})
	var ids []int
	for id, marked := range seen {
		if marked {
			ids = append(ids, id)
		}
	}
	return ids
}

// FuzzParseCPUSet requires exact equality with wantCPUSet; a dropped id would read as a free CPU.
func FuzzParseCPUSet(f *testing.F) {
	for _, seed := range []string{
		"",
		"0",
		"0-3",
		"0,10-12,102-105",
		" 3 , 5 ",
		"7-7",
		"9-8",
		"a,4",
		"0-1,1-2",
		"3,3,3",
		"5,1",
		"-1",
		"-0",
		"0-",
		"-",
		"1-2-3",
		"0--1",
		",,,",
		"99999999999999999999",
		"0-99999999999999999999",
		"0-4294967296",
		"8191",
		"8192",
		strings.Repeat("0,", 70000) + "1",
		strings.Repeat("0-4095,", 8) + "4096-8191",
		strings.Repeat("0-0,", 50000) + "7",
		strings.Repeat("0-0,", 32768) + "1-2",
		strings.Repeat("63-64,", 4096) + "128-128",
		// The second never covers id 0, so a "stop once full" shortcut would miss it.
		strings.Repeat("0-8191,", 200) + "7",
		strings.Repeat("1-8191,", 200) + "0",
		"0-8191",
		"64-127,4-5",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := ParseCPUSet(s)

		// The oracle is quadratic; past a work budget, skip only the comparison
		// (TestParseCPUSetPathologicalShapes covers those shapes).
		work := 0
		cpuSetRanges(s, func(l, h int) { work += h - l + 1 })
		if work > 1<<20 {
			return
		}
		if want := wantCPUSet(s); !slices.Equal(got, want) {
			t.Fatalf("ParseCPUSet(%q) = %v, want %v", s, got, want)
		}
	})
}

// TestParseCPUSetPathologicalShapes covers inputs too wide for the fuzz oracle:
// repeated wide ranges, ranges that skip id 0, and many repeated single ids.
func TestParseCPUSetPathologicalShapes(t *testing.T) {
	all := make([]int, maxCPUID+1)
	for i := range all {
		all[i] = i
	}
	for name, tc := range map[string]struct {
		in   string
		want []int
	}{
		"widest range repeated":       {strings.Repeat("0-8191,", 40000) + "7", all},
		"widest range never covers 0": {strings.Repeat("1-8191,", 40000) + "0", all},
		"single id repeated":          {strings.Repeat("0,", 500000) + "1", []int{0, 1}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ParseCPUSet(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("ParseCPUSet returned %d ids, want %d", len(got), len(tc.want))
			}
		})
	}
}

// TestParseCPUSetCostFollowsTheIDSpace proves allocation depends on the id space,
// not the fragment count. testing.Benchmark averages out background allocations.
func TestParseCPUSetCostFollowsTheIDSpace(t *testing.T) {
	short := strings.TrimSuffix(strings.Repeat("0,", 100), ",")
	long := strings.TrimSuffix(strings.Repeat("0,", 10000), ",")

	bytesPerOp := func(s string) int64 {
		return testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				ParseCPUSet(s)
			}
		}).AllocedBytesPerOp()
	}
	base := bytesPerOp(short)
	if got := bytesPerOp(long); got > 4*base+1024 {
		t.Errorf("100x the fragments allocated %d bytes/op against %d for the same set: "+
			"the cost is following the fragment count, not the id space", got, base)
	}
}

// FuzzZoneReaders drives the zone readers with an arbitrary zone and checks the invariants callers rely on.
func FuzzZoneReaders(f *testing.F) {
	f.Add("allocation for container", "inference/vllm-a/vllm", "balloon-span2[0]", "cpuset", "0-3", "0")
	f.Add("balloon", "reserved[0]", "", "cpuset", "0,1", "")
	f.Add("allocation for container", "ns/pod/c", "reserved[0]", "memory set", "1-2", "1-2")
	f.Add("", "", "", "", "", "")

	f.Fuzz(func(t *testing.T, zoneType, name, parent, attrName, cpuset, memSet string) {
		z := map[string]interface{}{
			"type":   zoneType,
			"name":   name,
			"parent": parent,
			"attributes": []interface{}{
				map[string]interface{}{"name": attrName, "value": cpuset},
				map[string]interface{}{"name": cpusetAttribute, "value": cpuset},
				map[string]interface{}{"name": memorySetAttribute, "value": memSet},
			},
		}
		nrt := &unstructured.Unstructured{Object: map[string]interface{}{
			// The same zone twice, as the plugin leaks on a failed CreateContainer.
			"zones": []interface{}{z, z},
		}}
		checkZoneReaders(t, nrt)
	})
}

// FuzzNRTDocument proves arbitrary JSON CRs cannot panic the zone readers.
func FuzzNRTDocument(f *testing.F) {
	f.Add(`{"zones":[{"type":"balloon","name":"reserved[0]","attributes":[{"name":"cpuset","value":"0-3"}]}]}`)
	f.Add(`{"zones":[]}`)
	f.Add(`{"zones":{"type":"balloon"}}`)
	f.Add(`{"zones":[[],1,"x",null]}`)
	f.Add(`{"zones":[{"attributes":{"name":"cpuset"}}]}`)
	f.Add(`{}`)

	f.Fuzz(func(t *testing.T, doc string) {
		// Deep nesting only exercises encoding/json; the apiserver schema bounds real CRs.
		if len(doc) > 4096 {
			return
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(doc), &obj); err != nil {
			return
		}
		checkZoneReaders(t, &unstructured.Unstructured{Object: obj})
	})
}

// checkZoneReaders runs every zone reader over one CR and asserts the invariants
// their callers depend on.
func checkZoneReaders(t *testing.T, nrt *unstructured.Unstructured) {
	t.Helper()

	occupied, _ := OccupiedCPUs(nrt)
	if !sort.IntsAreSorted(occupied) {
		t.Fatalf("OccupiedCPUs = %v, not sorted", occupied)
	}
	for i := 1; i < len(occupied); i++ {
		if occupied[i] == occupied[i-1] {
			t.Fatalf("OccupiedCPUs = %v, duplicate cpu %d", occupied, occupied[i])
		}
	}

	pinned, _ := PinnedMemoryContainers(nrt)
	for _, c := range pinned {
		if len(c.NUMAs) == 0 {
			t.Fatalf("PinnedMemoryContainers returned zone %q with no NUMAs", c.Zone)
		}
		if !sort.IntsAreSorted(c.NUMAs) {
			t.Fatalf("PinnedMemoryContainers zone %q NUMAs = %v, not sorted", c.Zone, c.NUMAs)
		}
		for i := 1; i < len(c.NUMAs); i++ {
			if c.NUMAs[i] == c.NUMAs[i-1] {
				// A duplicated NUMA id would double-count memory.
				t.Fatalf("PinnedMemoryContainers zone %q NUMAs = %v, duplicate id", c.Zone, c.NUMAs)
			}
		}
	}

	names, _ := ContainerZoneNames(nrt)

	// HasZones is the callers' precondition, so it must agree with the readers above.
	if !HasZones(nrt) && (len(occupied) > 0 || len(pinned) > 0 || len(names) > 0) {
		t.Fatalf("HasZones = false, but the readers returned %d cpus, %d pinned containers, %d container zones",
			len(occupied), len(pinned), len(names))
	}

	for _, name := range names {
		if name == "" {
			t.Fatal("ContainerZoneNames returned an empty zone name")
		}
		balloon, found := ContainerBalloon(nrt, name)
		if found && (balloon == "" || balloon == reservedBalloonName) {
			t.Fatalf("ContainerBalloon(%q) = (%q, true), want a workload balloon", name, balloon)
		}
		ns, pod, container := SplitContainerZoneName(name)
		if ns != "" && pod != "" && container != "" && ns+"/"+pod+"/"+container != name {
			t.Fatalf("SplitContainerZoneName(%q) = (%q, %q, %q), does not rejoin", name, ns, pod, container)
		}
	}
}
