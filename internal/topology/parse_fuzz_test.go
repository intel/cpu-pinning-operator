// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// FuzzParseProbeOutput proves every parsed field is consistent for any probe output.
func FuzzParseProbeOutput(f *testing.F) {
	f.Add(`NUMA node(s): 2
CPU(s): 128
Thread(s) per core: 2
AMX: true
PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=1
MEMNODE node=0 kb=131072000
MEMNODE node=1 kb=131072000
CPUINFO cpu=0 numa=0 core=0
CPUINFO cpu=64 numa=0 core=0
CPUINFO cpu=1 numa=1 core=1`)
	f.Add("NUMA node(s): 1\nCPU(s): 8\n")
	f.Add("NUMA node(s): 8\nCPU(s): 1\nThread(s) per core: 4\n")
	f.Add("numa node(s):\t3\ncpu(s):  0009\nMEMNODE node=0 kb=9223372036854775807\n")
	f.Add("NUMA node(s): 1\nCPU(s): 9223372036854775808\nCPUINFO cpu=9223372036854775808 numa=0 core=0\n")
	f.Add("NUMA node(s): 1\nCPU(s): 8\nCPUINFO cpu=9223372036854775807 numa=0 core=0\nCPUINFO cpu=1 numa=0 core=1\n")
	f.Add("NUMA node(s): 2\nCPU(s): 4\nMEMNODE node=0 kb=1\nMEMNODE node=5 kb=1\n")
	f.Add("NUMA node(s): 2\nCPU(s): 4\nPKGNODE node=0 pkg=0\nPKGNODE node=0 pkg=1\nPKGNODE node=1 pkg=1\n")
	f.Add("NUMA node(s): 2\nCPU(s): 8\nCPUINFO cpu=0 numa=0 core=0\nCPUINFO cpu=0 numa=1 core=1\n")
	f.Add("NUMA node(s): 2\nCPU(s): 8\nCPUINFO cpu=0 numa=0 core=0\nCPUINFO cpu=1 numa=1 core=0\n")
	f.Add("NUMA node(s): 2\nCPU(s): 8\nThread(s) per core: 2\nCPU(s): 4096\nNUMA node(s): 8\nThread(s) per core: 4\n")
	f.Add("NUMA node(s): 1\nCPU(s): 8\nPKGNODE node=0 pkg=0\nMEMNODE node=0 kb=1024\nMEMNODE node=0 kb=2048\n")
	f.Add("NUMA node(s): 1\nCPU(s): 8\nPKGNODE node=0 pkg=0\nMEMNODE node=0 kb=0\nMEMNODE node=0 kb=2048\n")
	// A summary count that outruns the detailed lines: 8 NUMA nodes claimed, one named.
	f.Add("NUMA node(s): 8\nCPU(s): 64\nPKGNODE node=0 pkg=0\n")
	// CPUINFO names NUMAs that PKGNODE does not: they never reach the generator, so
	// they must not raise the advertised span.
	f.Add("NUMA node(s): 2\nCPU(s): 4\nCPUINFO cpu=0 numa=0 core=0\nCPUINFO cpu=1 numa=1 core=1\n")
	// A NUMA id past its bound but within int64: only the explicit bound rejects it.
	f.Add("NUMA node(s): 2\nCPU(s): 4\nPKGNODE node=100000 pkg=0\nPKGNODE node=1 pkg=1\n")
	f.Add("NUMA node(s): 2\nCPU(s): 4\nPKGNODE node=99999999999999999999 pkg=0\n")
	// A package id has no bound, so a large one is accepted.
	f.Add("NUMA node(s): 2\nCPU(s): 4\nPKGNODE node=0 pkg=70000\nPKGNODE node=1 pkg=1\n")
	f.Add("")

	f.Fuzz(func(t *testing.T, out string) {
		topo, err := ParseProbeOutput(out)
		if err != nil {
			if topo != nil {
				t.Fatalf("ParseProbeOutput returned a topology alongside error %v", err)
			}
			return
		}

		if topo.NumaNodes < 1 || topo.LogicalCPUs < 1 || topo.ThreadsPerCore < 1 {
			t.Fatalf("counts must be positive: numa=%d cpus=%d threads=%d",
				topo.NumaNodes, topo.LogicalCPUs, topo.ThreadsPerCore)
		}
		// Impossible counts must not survive parsing.
		if topo.NumaNodes > MaxNUMAID+1 || topo.LogicalCPUs > maxCPUID+1 || topo.ThreadsPerCore > maxThreadsPerCore {
			t.Fatalf("counts past their bounds: numa=%d cpus=%d threads=%d",
				topo.NumaNodes, topo.LogicalCPUs, topo.ThreadsPerCore)
		}
		if want := topo.LogicalCPUs / topo.ThreadsPerCore; topo.PhysicalCPUs != want {
			t.Fatalf("PhysicalCPUs = %d, want LogicalCPUs/ThreadsPerCore = %d", topo.PhysicalCPUs, want)
		}
		switch topo.MaxSpan {
		case 1, 2, 4, 8:
		default:
			t.Fatalf("MaxSpan = %d, not a supported span", topo.MaxSpan)
		}

		// The first well-formed line naming a CPU wins.
		firstClaims := firstCPUClaims(out)
		for cpu, info := range topo.CPUs {
			if want, ok := firstClaims[cpu]; !ok || want != info {
				t.Fatalf("CPUs[%d] = %+v, want the first claim %+v (present %v)", cpu, info, want, ok)
			}
			if info.CPU != cpu {
				t.Fatalf("CPUs[%d] carries CPU %d", cpu, info.CPU)
			}
			// Ids are bounded, not just non-negative.
			if info.CPU < 0 || info.NUMA < 0 || info.Core < 0 ||
				info.CPU > maxCPUID || info.NUMA > MaxNUMAID || info.Core > maxCPUID {
				t.Fatalf("CPUs[%d] = %+v carries an id past its bound (cpu/core %d, numa %d)",
					cpu, info, maxCPUID, MaxNUMAID)
			}
		}
		// A core belongs to one NUMA node.
		coreNUMA := map[int]int{}
		for _, info := range topo.CPUs {
			if numa, claimed := coreNUMA[info.Core]; claimed && numa != info.NUMA {
				t.Fatalf("core %d placed on both NUMA %d and %d (CPUs %v)", info.Core, numa, info.NUMA, topo.CPUs)
			}
			coreNUMA[info.Core] = info.NUMA
		}
		// A NUMA id appears once, on one package, and is bounded; package ids are not.
		onPkg := map[int]int{}
		for pkg, nodes := range topo.PkgNode {
			if !sort.IntsAreSorted(nodes) {
				t.Fatalf("PkgNode[%d] = %v, not sorted", pkg, nodes)
			}
			for _, n := range nodes {
				if n < 0 || n > MaxNUMAID {
					t.Fatalf("PkgNode places NUMA %d on package %d, outside [0,%d]", n, pkg, MaxNUMAID)
				}
				if other, dup := onPkg[n]; dup {
					t.Fatalf("NUMA %d listed on both package %d and %d (PkgNode %v)", n, other, pkg, topo.PkgNode)
				}
				onPkg[n] = pkg
			}
		}
		// MaxSpan never exceeds what the PKGNODE lines support.
		if want := MaxSpanForNUMA(min(topo.NumaNodes, len(onPkg))); topo.MaxSpan != want {
			t.Fatalf("MaxSpan = %d, want %d: summary claims %d NUMA nodes, PKGNODE names %d",
				topo.MaxSpan, want, topo.NumaNodes, len(onPkg))
		}

		// Map and floor are published together, the floor is the minimum, entries are positive.
		if (topo.MemoryPerNUMABytes == 0) != (len(topo.MemoryPerNUMABytesMap) == 0) {
			t.Fatalf("memory floor %d and map %v disagree on whether memory is known",
				topo.MemoryPerNUMABytes, topo.MemoryPerNUMABytesMap)
		}
		if topo.MemoryPerNUMABytes < 0 {
			t.Fatalf("MemoryPerNUMABytes = %d, negative", topo.MemoryPerNUMABytes)
		}
		if len(topo.MemoryPerNUMABytesMap) > 0 {
			ids := numaIDsOf(topo)
			floor := int64(math.MaxInt64)
			for numa, bytes := range topo.MemoryPerNUMABytesMap {
				if bytes <= 0 {
					t.Fatalf("MemoryPerNUMABytesMap[%d] = %d, not positive", numa, bytes)
				}
				if !ids[numa] {
					t.Fatalf("MemoryPerNUMABytesMap carries NUMA %d, absent from the topology ids %v", numa, ids)
				}
				if bytes < floor {
					floor = bytes
				}
			}
			if len(topo.MemoryPerNUMABytesMap) != len(ids) {
				t.Fatalf("MemoryPerNUMABytesMap %v does not cover every topology NUMA id %v",
					topo.MemoryPerNUMABytesMap, ids)
			}
			if topo.MemoryPerNUMABytes != floor {
				t.Fatalf("MemoryPerNUMABytes = %d, want the map floor %d", topo.MemoryPerNUMABytes, floor)
			}
		}
	})
}

// numaIDsOf returns the NUMA ids the parsed topology lines name, the set the
// memory map must cover exactly.
func numaIDsOf(topo *NodeTopology) map[int]bool {
	ids := map[int]bool{}
	for _, nodes := range topo.PkgNode {
		for _, n := range nodes {
			ids[n] = true
		}
	}
	for _, info := range topo.CPUs {
		ids[info.NUMA] = true
	}
	return ids
}

// firstCPUClaims returns each CPU's first valid CPUINFO placement, the one the parser must keep.
func firstCPUClaims(out string) map[int]CPUInfo {
	claims := map[int]CPUInfo{}
	coreNUMA := map[int]int{}
	for _, line := range strings.Split(out, "\n") {
		m := cpuInfoLineRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		cpu, err1 := strconv.Atoi(m[1])
		numa, err2 := strconv.Atoi(m[2])
		core, err3 := strconv.Atoi(m[3])
		if err1 != nil || err2 != nil || err3 != nil ||
			cpu > maxCPUID || numa > MaxNUMAID || core > maxCPUID {
			continue
		}
		if _, claimed := claims[cpu]; claimed {
			continue
		}
		if owner, claimed := coreNUMA[core]; claimed && owner != numa {
			continue
		}
		coreNUMA[core] = numa
		claims[cpu] = CPUInfo{CPU: cpu, NUMA: numa, Core: core}
	}
	return claims
}
