// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseProbeOutput_TwoNuma(t *testing.T) {
	output := `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=1
Architecture:            x86_64
CPU(s):                  64
On-line CPU(s) list:     0-63
Thread(s) per core:      2
Core(s) per socket:      16
Socket(s):               2
NUMA node(s):            2
NUMA node0 CPU(s):       0-15,32-47
NUMA node1 CPU(s):       16-31,48-63
AMX: true
`
	topo, err := ParseProbeOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.NumaNodes != 2 {
		t.Errorf("NumaNodes = %d, want 2", topo.NumaNodes)
	}
	if !topo.AMX {
		t.Errorf("AMX = false, want true")
	}
	if topo.LogicalCPUs != 64 {
		t.Errorf("LogicalCPUs = %d, want 64", topo.LogicalCPUs)
	}
	if topo.ThreadsPerCore != 2 {
		t.Errorf("ThreadsPerCore = %d, want 2", topo.ThreadsPerCore)
	}
	if topo.PhysicalCPUs != 32 {
		t.Errorf("PhysicalCPUs = %d, want 32", topo.PhysicalCPUs)
	}
	if topo.MaxSpan != 2 {
		t.Errorf("MaxSpan = %d, want 2", topo.MaxSpan)
	}
	want := map[int][]int{0: {0}, 1: {1}}
	if !reflect.DeepEqual(topo.PkgNode, want) {
		t.Errorf("PkgNode = %v, want %v", topo.PkgNode, want)
	}

	// CPUINFO lines populate the per-CPU numa/core map (siblings share a core).
	cpuOut := `PKGNODE node=0 pkg=0
CPUINFO cpu=0 numa=0 core=0
CPUINFO cpu=32 numa=0 core=0
CPUINFO cpu=1 numa=0 core=1
NUMA node(s):            1
CPU(s):                  3
Thread(s) per core:      2
AMX: false
`
	ct, err := ParseProbeOutput(cpuOut)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ct.CPUs) != 3 {
		t.Fatalf("CPUs len = %d, want 3", len(ct.CPUs))
	}
	if ct.CPUs[0] != (CPUInfo{CPU: 0, NUMA: 0, Core: 0}) {
		t.Errorf("CPUs[0] = %+v, want {0,0,0}", ct.CPUs[0])
	}
	// cpu 32 is the hyperthread sibling of cpu 0 -> same core id.
	if ct.CPUs[32].Core != 0 {
		t.Errorf("CPUs[32].Core = %d, want 0 (sibling of cpu0)", ct.CPUs[32].Core)
	}
	if ct.CPUs[1].Core != 1 {
		t.Errorf("CPUs[1].Core = %d, want 1", ct.CPUs[1].Core)
	}
}

func TestParseProbeOutput_FourNuma(t *testing.T) {
	output := `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=0
PKGNODE node=2 pkg=1
PKGNODE node=3 pkg=1
CPU(s):                  128
Thread(s) per core:      1
NUMA node(s):            4
`
	topo, err := ParseProbeOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.NumaNodes != 4 {
		t.Errorf("NumaNodes = %d, want 4", topo.NumaNodes)
	}
	if topo.AMX {
		t.Errorf("AMX = true, want false (no AMX line in output)")
	}
	if topo.MaxSpan != 4 {
		t.Errorf("MaxSpan = %d, want 4", topo.MaxSpan)
	}
	if topo.PhysicalCPUs != 128 {
		t.Errorf("PhysicalCPUs = %d, want 128 (threads_per_core=1)", topo.PhysicalCPUs)
	}
	want := map[int][]int{0: {0, 1}, 1: {2, 3}}
	if !reflect.DeepEqual(topo.PkgNode, want) {
		t.Errorf("PkgNode = %v, want %v", topo.PkgNode, want)
	}
}

func TestParseProbeOutput_GNR3Tile(t *testing.T) {
	// One socket with three NUMA nodes.
	output := `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=0
PKGNODE node=2 pkg=0
CPU(s):                  96
Thread(s) per core:      2
NUMA node(s):            3
`
	topo, err := ParseProbeOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.NumaNodes != 3 {
		t.Errorf("NumaNodes = %d, want 3", topo.NumaNodes)
	}
	// numa=3 falls in the "≥2" bucket → MaxSpan=2
	if topo.MaxSpan != 2 {
		t.Errorf("MaxSpan = %d, want 2", topo.MaxSpan)
	}
	want := map[int][]int{0: {0, 1, 2}}
	if !reflect.DeepEqual(topo.PkgNode, want) {
		t.Errorf("PkgNode = %v, want %v", topo.PkgNode, want)
	}
}

func TestParseProbeOutput_EightNuma(t *testing.T) {
	output := `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=0
PKGNODE node=2 pkg=1
PKGNODE node=3 pkg=1
PKGNODE node=4 pkg=2
PKGNODE node=5 pkg=2
PKGNODE node=6 pkg=3
PKGNODE node=7 pkg=3
CPU(s):                  256
Thread(s) per core:      2
NUMA node(s):            8
`
	topo, err := ParseProbeOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.MaxSpan != 8 {
		t.Errorf("MaxSpan = %d, want 8", topo.MaxSpan)
	}
	if topo.PhysicalCPUs != 128 {
		t.Errorf("PhysicalCPUs = %d, want 128", topo.PhysicalCPUs)
	}
}

func TestParseProbeOutput_MemoryPerNUMAFloor(t *testing.T) {
	// Slightly different MemTotal per NUMA: the floor is the smallest, in bytes.
	output := `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=1
MEMNODE node=0 kb=263758548
MEMNODE node=1 kb=264145172
CPU(s): 64
Thread(s) per core: 2
NUMA node(s): 2
`
	topo, err := ParseProbeOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := int64(263758548) * 1024 // smaller of the two, in bytes
	if topo.MemoryPerNUMABytes != want {
		t.Errorf("MemoryPerNUMABytes = %d, want %d (min per-NUMA kB × 1024)", topo.MemoryPerNUMABytes, want)
	}

	// The full map carries both nodes in bytes.
	wantMap := map[int]int64{
		0: int64(263758548) * 1024,
		1: int64(264145172) * 1024,
	}
	if !reflect.DeepEqual(topo.MemoryPerNUMABytesMap, wantMap) {
		t.Errorf("MemoryPerNUMABytesMap = %v, want %v", topo.MemoryPerNUMABytesMap, wantMap)
	}
	// The floor stays consistent with the full map: it is exactly the minimum.
	var min int64
	for _, b := range topo.MemoryPerNUMABytesMap {
		if min == 0 || b < min {
			min = b
		}
	}
	if topo.MemoryPerNUMABytes != min {
		t.Errorf("MemoryPerNUMABytes = %d, want %d (min of the full map)", topo.MemoryPerNUMABytes, min)
	}
}

func TestParseProbeOutput_MemoryAbsentIsUnknown(t *testing.T) {
	// No MEMNODE lines: MemoryPerNUMABytes is 0 (unknown).
	output := `PKGNODE node=0 pkg=0
CPU(s): 32
NUMA node(s): 1
`
	topo, err := ParseProbeOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.MemoryPerNUMABytes != 0 {
		t.Errorf("MemoryPerNUMABytes = %d, want 0 (no MEMNODE lines → unknown)", topo.MemoryPerNUMABytes)
	}
	if topo.MemoryPerNUMABytesMap != nil {
		t.Errorf("MemoryPerNUMABytesMap = %v, want nil (no MEMNODE lines → unknown)", topo.MemoryPerNUMABytesMap)
	}
}

func TestParseProbeOutput_MemoryPartialIsUnknown(t *testing.T) {
	// MemTotal for only one of two NUMA nodes: unknown.
	output := `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=1
MEMNODE node=0 kb=263758548
CPU(s): 64
Thread(s) per core: 2
NUMA node(s): 2
`
	topo, err := ParseProbeOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.MemoryPerNUMABytes != 0 {
		t.Errorf("MemoryPerNUMABytes = %d, want 0 (partial per-NUMA memory → unknown)", topo.MemoryPerNUMABytes)
	}
	if topo.MemoryPerNUMABytesMap != nil {
		t.Errorf("MemoryPerNUMABytesMap = %v, want nil (partial per-NUMA memory → unknown)", topo.MemoryPerNUMABytesMap)
	}
}

func TestParseProbeOutput_MemoryIDMismatchIsUnknown(t *testing.T) {
	// NUMA ids {0,1} but MemTotal for {1,2}: right count, wrong ids → unknown.
	output := `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=1
MEMNODE node=1 kb=263758548
MEMNODE node=2 kb=264145172
CPU(s): 64
Thread(s) per core: 2
NUMA node(s): 2
`
	topo, err := ParseProbeOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.MemoryPerNUMABytes != 0 {
		t.Errorf("MemoryPerNUMABytes = %d, want 0 (mem ids {1,2} don't cover node ids {0,1} → unknown)", topo.MemoryPerNUMABytes)
	}
	if topo.MemoryPerNUMABytesMap != nil {
		t.Errorf("MemoryPerNUMABytesMap = %v, want nil (id mismatch → unknown)", topo.MemoryPerNUMABytesMap)
	}
}

func TestParseProbeOutput_MissingNuma(t *testing.T) {
	output := `CPU(s): 64
Thread(s) per core: 2
`
	if _, err := ParseProbeOutput(output); err == nil {
		t.Error("expected error for missing NUMA node(s) line, got nil")
	}
}

func TestParseProbeOutput_DefaultThreadsPerCore(t *testing.T) {
	output := `PKGNODE node=0 pkg=0
CPU(s): 32
NUMA node(s): 1
`
	topo, err := ParseProbeOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.ThreadsPerCore != 1 {
		t.Errorf("ThreadsPerCore = %d, want default 1", topo.ThreadsPerCore)
	}
	if topo.PhysicalCPUs != 32 {
		t.Errorf("PhysicalCPUs = %d, want 32", topo.PhysicalCPUs)
	}
	if topo.MaxSpan != 1 {
		t.Errorf("MaxSpan = %d, want 1", topo.MaxSpan)
	}
}

// An overflowing all-digit field is an error, never coerced to zero.
func TestParseProbeOutput_MalformedValueRejected(t *testing.T) {
	const overflow = "99999999999999999999999999999"
	// A minimal valid body; each case malforms one field.
	base := "PKGNODE node=0 pkg=0\nNUMA node(s): 1\nCPU(s): 32\nThread(s) per core: 2\n"
	cases := map[string]string{
		"NUMA node(s)":       base + "NUMA node(s): " + overflow + "\n",
		"CPU(s)":             base + "CPU(s): " + overflow + "\n",
		"Thread(s) per core": base + "Thread(s) per core: " + overflow + "\n",
		"MEMNODE node":       base + "MEMNODE node=" + overflow + " kb=1024\n",
		"MEMNODE kb":         base + "MEMNODE node=0 kb=" + overflow + "\n",
		"CPUINFO cpu":        base + "CPUINFO cpu=" + overflow + " numa=0 core=0\n",
		"CPUINFO numa":       base + "CPUINFO cpu=0 numa=" + overflow + " core=0\n",
		"CPUINFO core":       base + "CPUINFO cpu=0 numa=0 core=" + overflow + "\n",
		"PKGNODE node":       base + "PKGNODE node=" + overflow + " pkg=0\n",
		"PKGNODE pkg":        base + "PKGNODE node=0 pkg=" + overflow + "\n",
	}
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseProbeOutput(output); err == nil {
				t.Errorf("expected error for malformed %s value, got nil", name)
			}
		})
	}
}

// Each bound is pinned at its edge with literal numbers (maxCPUID 8191, MaxNUMAID 15,
// maxThreadsPerCore 8), so raising one is a visible decision.
func TestParseProbeOutput_BoundEdges(t *testing.T) {
	const base = "NUMA node(s): 2\nCPU(s): 64\n"
	for name, tc := range map[string]struct {
		out     string
		wantErr bool
	}{
		"cpu id at the ceiling":       {base + "CPUINFO cpu=8191 numa=0 core=0\n", false},
		"cpu id past it":              {base + "CPUINFO cpu=8192 numa=0 core=0\n", true},
		"core id at the ceiling":      {base + "CPUINFO cpu=0 numa=0 core=8191\n", false},
		"core id past it":             {base + "CPUINFO cpu=0 numa=0 core=8192\n", true},
		"cpuinfo numa at the ceiling": {base + "CPUINFO cpu=0 numa=15 core=0\n", false},
		"cpuinfo numa past it":        {base + "CPUINFO cpu=0 numa=16 core=0\n", true},
		"pkgnode numa at the ceiling": {base + "PKGNODE node=15 pkg=0\n", false},
		"pkgnode numa past it":        {base + "PKGNODE node=16 pkg=0\n", true},
		"memnode numa at the ceiling": {base + "MEMNODE node=15 kb=1024\n", false},
		"memnode numa past it":        {base + "MEMNODE node=16 kb=1024\n", true},
		"numa count at the ceiling":   {"NUMA node(s): 16\nCPU(s): 64\n", false},
		"numa count past it":          {"NUMA node(s): 17\nCPU(s): 64\n", true},
		"cpu count at the ceiling":    {"NUMA node(s): 2\nCPU(s): 8192\n", false},
		"cpu count past it":           {"NUMA node(s): 2\nCPU(s): 8193\n", true},
		"threads at the ceiling":      {base + "Thread(s) per core: 8\n", false},
		"threads past it":             {base + "Thread(s) per core: 9\n", true},
		// The per-package count bounds the span-8 search.
		"eight NUMAs on one package": {"NUMA node(s): 8\nCPU(s): 64\n" +
			pkgLines(0, 0, 1, 2, 3, 4, 5, 6, 7), false},
		"nine NUMAs on one package": {"NUMA node(s): 9\nCPU(s): 64\n" +
			pkgLines(0, 0, 1, 2, 3, 4, 5, 6, 7, 8), true},
		"sixteen NUMAs split over two packages": {"NUMA node(s): 16\nCPU(s): 64\n" +
			pkgLines(0, 0, 1, 2, 3, 4, 5, 6, 7) + pkgLines(1, 8, 9, 10, 11, 12, 13, 14, 15), false},
		"sixteen NUMAs on one package": {"NUMA node(s): 16\nCPU(s): 64\n" +
			pkgLines(0, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15), true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseProbeOutput(tc.out)
			if tc.wantErr && err == nil {
				t.Error("expected the probe to be rejected, got nil error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected the probe to be accepted, got %v", err)
			}
		})
	}
}

// A package id has no ceiling: it is platform-dependent and only a grouping key.
func TestParseProbeOutput_PackageIDNeedsNoCeiling(t *testing.T) {
	out := "NUMA node(s): 2\nCPU(s): 64\nPKGNODE node=0 pkg=0\nPKGNODE node=1 pkg=70000\n"
	topo, err := ParseProbeOutput(out)
	if err != nil {
		t.Fatalf("ParseProbeOutput: %v", err)
	}
	if got := topo.PkgNode[70000]; len(got) != 1 || got[0] != 1 {
		t.Errorf("PkgNode = %v, want NUMA 1 on package 70000", topo.PkgNode)
	}
}

// TestMaxSpanNeverOutrunsDetailedTopology proves MaxSpan follows the PKGNODE lines, not the summary count.
func TestMaxSpanNeverOutrunsDetailedTopology(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     string
		wantMax int
	}{
		{
			name:    "summary claims 8, one PKGNODE named",
			out:     "NUMA node(s): 8\nCPU(s): 64\nPKGNODE node=0 pkg=0\n",
			wantMax: 1,
		},
		{
			name:    "summary claims 8, two PKGNODEs named",
			out:     "NUMA node(s): 8\nCPU(s): 64\nPKGNODE node=0 pkg=0\nPKGNODE node=1 pkg=1\n",
			wantMax: 2,
		},
		{
			name: "summary and detailed lines agree on 4",
			out: "NUMA node(s): 4\nCPU(s): 64\n" +
				"PKGNODE node=0 pkg=0\nPKGNODE node=1 pkg=0\nPKGNODE node=2 pkg=1\nPKGNODE node=3 pkg=1\n",
			wantMax: 4,
		},
		{
			// CPUINFO NUMAs do not raise the span.
			name:    "CPUINFO alone, no PKGNODE",
			out:     "NUMA node(s): 2\nCPU(s): 4\nCPUINFO cpu=0 numa=0 core=0\nCPUINFO cpu=1 numa=1 core=1\n",
			wantMax: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			topo, err := ParseProbeOutput(tc.out)
			if err != nil {
				t.Fatalf("ParseProbeOutput: %v", err)
			}
			if topo.MaxSpan != tc.wantMax {
				t.Errorf("MaxSpan = %d, want %d", topo.MaxSpan, tc.wantMax)
			}
		})
	}
}

// A MEMNODE kb value that overflows when scaled to bytes is rejected.
func TestParseProbeOutput_MemoryOverflowRejected(t *testing.T) {
	output := "PKGNODE node=0 pkg=0\nNUMA node(s): 1\nCPU(s): 32\nThread(s) per core: 2\n" +
		"MEMNODE node=0 kb=9223372036854775807\n"
	if _, err := ParseProbeOutput(output); err == nil {
		t.Error("expected error for MEMNODE kb that overflows on the kB→byte scale, got nil")
	}
}

// TestParseProbeOutput_ConflictingRepeatRejected proves a self-contradicting repeat
// is rejected while a same-value repeat is accepted.
func TestParseProbeOutput_ConflictingRepeatRejected(t *testing.T) {
	base := "PKGNODE node=0 pkg=0\nPKGNODE node=1 pkg=1\nNUMA node(s): 2\nCPU(s): 32\nThread(s) per core: 2\n"
	for name, tc := range map[string]struct {
		out       string
		wantError bool
	}{
		"NUMA node(s) contradicts itself": {base + "NUMA node(s): 8\n", true},
		"CPU(s) contradicts itself":       {base + "CPU(s): 64\n", true},
		"Thread(s) per core contradicts":  {base + "Thread(s) per core: 1\n", true},
		"MEMNODE contradicts itself":      {base + "MEMNODE node=0 kb=1024\nMEMNODE node=0 kb=2048\n", true},
		"a count reported as zero":        {base + "CPU(s): 0\n", true},
		"an id past the probe bound":      {base + "CPUINFO cpu=100000 numa=0 core=0\n", true},
		"repeat carrying the same value":  {base + "NUMA node(s): 2\nCPU(s): 32\n", false},
		"MEMNODE repeat, same capacity":   {base + "MEMNODE node=0 kb=1024\nMEMNODE node=0 kb=1024\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseProbeOutput(tc.out)
			if tc.wantError && err == nil {
				t.Error("expected the probe to be rejected, got nil error")
			}
			if !tc.wantError && err != nil {
				t.Errorf("expected the probe to be accepted, got %v", err)
			}
		})
	}
}

func TestParseProbeOutput_ThreadsPerCoreRangeForm(t *testing.T) {
	// PhysicalCPUs = LogicalCPUs / ThreadsPerCore, downstream of the probe fix.
	output := `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=1
CPU(s):                  64
Thread(s) per core:      2
NUMA node(s):            2
`
	topo, err := ParseProbeOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.ThreadsPerCore != 2 {
		t.Errorf("ThreadsPerCore = %d, want 2", topo.ThreadsPerCore)
	}
	if topo.PhysicalCPUs != 32 {
		t.Errorf("PhysicalCPUs = %d, want 32 (64 logical / 2 threads per core)", topo.PhysicalCPUs)
	}
	if topo.LogicalCPUs != 64 {
		t.Errorf("LogicalCPUs = %d, want 64", topo.LogicalCPUs)
	}
}

// pkgLines renders PKGNODE lines putting the given NUMA ids on one package.
func pkgLines(pkg int, numas ...int) string {
	var b strings.Builder
	for _, n := range numas {
		fmt.Fprintf(&b, "PKGNODE node=%d pkg=%d\n", n, pkg)
	}
	return b.String()
}
