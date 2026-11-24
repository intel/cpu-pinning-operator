// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// CPUInfo is one logical CPU's placement.
type CPUInfo struct {
	CPU int

	NUMA int
	// Core is the lowest logical CPU on the physical core, the representative thread.
	Core int
}

// NodeTopology holds a node's discovered CPU, NUMA, memory and feature information.
type NodeTopology struct {
	NumaNodes      int
	LogicalCPUs    int
	ThreadsPerCore int
	PhysicalCPUs   int
	MaxSpan        int
	// MemoryPerNUMABytes is the smallest per-NUMA MemTotal. Zero means the
	// probe reported none and the memory capacity check is skipped.
	MemoryPerNUMABytes int64
	// MemoryPerNUMABytesMap is the full per-NUMA map so live headroom can
	// subtract from the exact NUMA, not the floor. Nil when unknown.
	MemoryPerNUMABytesMap map[int]int64

	AMX bool

	PkgNode map[int][]int

	CPUs map[int]CPUInfo
}

const bytesPerKB = 1024

// Probe id/count ceilings. Shared with the webhook so both reject the same
// inputs; raise only against a fresh measurement.
const (
	maxCPUID = 8191

	// MaxNUMAID and MaxNUMAsPerPackage also bound the package-topology label.
	MaxNUMAID          = 15
	MaxNUMAsPerPackage = 8

	maxThreadsPerCore = 8
)

var (
	pkgNodeLineRe  = regexp.MustCompile(`^PKGNODE\s+node=(\d+)\s+pkg=(\d+)`)
	memNodeLineRe  = regexp.MustCompile(`^MEMNODE\s+node=(\d+)\s+kb=(\d+)`)
	cpuInfoLineRe  = regexp.MustCompile(`^CPUINFO\s+cpu=(\d+)\s+numa=(\d+)\s+core=(\d+)`)
	numaNodesRe    = regexp.MustCompile(`(?i)^NUMA node\(s\):\s*(\d+)`)
	logicalCPUsRe  = regexp.MustCompile(`(?i)^CPU\(s\):\s*(\d+)`)
	threadsPerCPre = regexp.MustCompile(`(?i)^Thread\(s\) per core:\s*(\d+)`)
	amxRe          = regexp.MustCompile(`(?i)^AMX:\s*(true|false)`)
)

// probeID parses an id from a probe line, rejecting values above max.
func probeID(field, raw string, max int) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("failed to parse %s: %w", field, err)
	}
	if v < 0 || v > max {
		return 0, fmt.Errorf("%s %d is outside [0,%d]", field, v, max)
	}
	return v, nil
}

// probeCount rejects a repeat that disagrees: otherwise line order would decide
// NUMA count, physical-core count, reserved-pool size and max span.
func probeCount(field, raw string, current, max int) (int, error) {
	v, err := probeID(field, raw, max)
	if err != nil {
		return 0, err
	}
	if v == 0 {
		return 0, fmt.Errorf("%s reported as 0, which is not a topology", field)
	}
	if current != 0 && current != v {
		return 0, fmt.Errorf("%s reported as both %d and %d", field, current, v)
	}
	return v, nil
}

// ParseProbeOutput parses PKGNODE + lscpu probe output. Out-of-range ids fail the
// probe, and so does a missing NUMA node(s) or CPU(s) summary; a missing Thread(s)
// per core defaults to 1.
func ParseProbeOutput(output string) (*NodeTopology, error) {
	pkgNode := map[int][]int{}
	// Repeated PKGNODE for the same NUMA is dropped: keeping it emits the leaf
	// balloon twice and the NRI plugin cannot load the policy.
	seenNUMA := map[int]bool{}
	coreNUMA := map[int]int{}
	cpus := map[int]CPUInfo{}
	memNodeKB := map[int]int64{}
	var numaNodes, logicalCPUs, threadsPerCore int
	var amx bool

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := pkgNodeLineRe.FindStringSubmatch(line); m != nil {
			node, err := probeID("PKGNODE node id", m[1], MaxNUMAID)
			if err != nil {
				return nil, err
			}
			// Package id is unbounded: it is only a grouping key over bounded NUMA ids.
			pkg, err := strconv.Atoi(m[2])
			if err != nil {
				return nil, fmt.Errorf("failed to parse PKGNODE pkg id: %w", err)
			}
			if seenNUMA[node] {
				continue
			}
			seenNUMA[node] = true
			pkgNode[pkg] = append(pkgNode[pkg], node)
			continue
		}
		if m := memNodeLineRe.FindStringSubmatch(line); m != nil {
			node, err := probeID("MEMNODE node id", m[1], MaxNUMAID)
			if err != nil {
				return nil, err
			}
			kb, err := strconv.ParseInt(m[2], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("failed to parse MEMNODE kb: %w", err)
			}
			// kb is scaled later; reject overflow rather than a wrapped negative capacity.
			if kb > math.MaxInt64/bytesPerKB {
				return nil, fmt.Errorf("MEMNODE kb %d exceeds the representable byte range", kb)
			}
			// Contradictory repeat: otherwise line order picks the per-NUMA capacity.
			if prev, seen := memNodeKB[node]; seen && prev != kb {
				return nil, fmt.Errorf("MEMNODE node %d reported as both %d and %d kb", node, prev, kb)
			}
			if kb > 0 {
				memNodeKB[node] = kb
			}
			continue
		}
		if m := cpuInfoLineRe.FindStringSubmatch(line); m != nil {
			cpu, err := probeID("CPUINFO cpu id", m[1], maxCPUID)
			if err != nil {
				return nil, err
			}
			numa, err := probeID("CPUINFO numa id", m[2], MaxNUMAID)
			if err != nil {
				return nil, err
			}
			core, err := probeID("CPUINFO core id", m[3], maxCPUID)
			if err != nil {
				return nil, err
			}
			// First claim wins. Overwriting would let line order decide sibling
			// exclusion and per-NUMA free-core counts.
			if _, seen := cpus[cpu]; seen {
				continue
			}
			if claimed, ok := coreNUMA[core]; ok && claimed != numa {
				continue
			}
			coreNUMA[core] = numa
			cpus[cpu] = CPUInfo{CPU: cpu, NUMA: numa, Core: core}
			continue
		}
		if m := numaNodesRe.FindStringSubmatch(line); m != nil {
			n, err := probeCount("NUMA node(s)", m[1], numaNodes, MaxNUMAID+1)
			if err != nil {
				return nil, err
			}
			numaNodes = n
			continue
		}
		if m := logicalCPUsRe.FindStringSubmatch(line); m != nil {
			n, err := probeCount("CPU(s)", m[1], logicalCPUs, maxCPUID+1)
			if err != nil {
				return nil, err
			}
			logicalCPUs = n
			continue
		}
		if m := threadsPerCPre.FindStringSubmatch(line); m != nil {
			n, err := probeCount("Thread(s) per core", m[1], threadsPerCore, maxThreadsPerCore)
			if err != nil {
				return nil, err
			}
			threadsPerCore = n
			continue
		}
		if m := amxRe.FindStringSubmatch(line); m != nil {
			amx = strings.EqualFold(m[1], "true")
			continue
		}
	}

	if numaNodes == 0 {
		return nil, fmt.Errorf("failed to parse NUMA node(s) from probe output")
	}
	if logicalCPUs == 0 {
		return nil, fmt.Errorf("failed to parse CPU(s) from probe output")
	}
	if threadsPerCore == 0 {
		threadsPerCore = 1
	}

	// 16 NUMA on one package emit ~13k balloon defs vs ~150 split across two.
	// Reject the shape rather than hand the generator a policy NRI cannot load.
	for pkg := range pkgNode {
		if len(pkgNode[pkg]) > MaxNUMAsPerPackage {
			return nil, fmt.Errorf("package %d carries %d NUMA nodes, more than %d",
				pkg, len(pkgNode[pkg]), MaxNUMAsPerPackage)
		}
		sort.Ints(pkgNode[pkg])
	}

	numaIDs := map[int]bool{}
	for _, nodes := range pkgNode {
		for _, n := range nodes {
			numaIDs[n] = true
		}
	}
	for _, ci := range cpus {
		numaIDs[ci.NUMA] = true
	}

	// Cap span to PKGNODE NUMA ids, so it never exceeds what the generator emits.
	spanNUMA := min(numaNodes, len(seenNUMA))

	return &NodeTopology{
		NumaNodes:             numaNodes,
		LogicalCPUs:           logicalCPUs,
		ThreadsPerCore:        threadsPerCore,
		PhysicalCPUs:          logicalCPUs / threadsPerCore,
		MaxSpan:               MaxSpanForNUMA(spanNUMA),
		MemoryPerNUMABytes:    minPerNUMAMemBytes(memNodeKB, numaIDs),
		MemoryPerNUMABytesMap: perNUMAMemBytes(memNodeKB, numaIDs),
		AMX:                   amx,
		PkgNode:               pkgNode,
		CPUs:                  cpus,
	}, nil
}

// memMapComplete requires a positive value for every NUMA id, not just the right count.
func memMapComplete(memNodeKB map[int]int64, numaIDs map[int]bool) bool {
	if len(numaIDs) == 0 {
		return false
	}
	for id := range numaIDs {
		if memNodeKB[id] <= 0 {
			return false
		}
	}
	return true
}

// minPerNUMAMemBytes returns 0 (skip the memory check) on a partial map: a
// partial floor can be too low and drive false rejections.
func minPerNUMAMemBytes(memNodeKB map[int]int64, numaIDs map[int]bool) int64 {
	if !memMapComplete(memNodeKB, numaIDs) {
		return 0
	}
	minKB := int64(0)
	for id := range numaIDs {
		kb := memNodeKB[id]
		if minKB == 0 || kb < minKB {
			minKB = kb
		}
	}
	return minKB * bytesPerKB
}

// perNUMAMemBytes returns nil on a partial map: a missing NUMA would read as 0 free.
func perNUMAMemBytes(memNodeKB map[int]int64, numaIDs map[int]bool) map[int]int64 {
	if !memMapComplete(memNodeKB, numaIDs) {
		return nil
	}
	out := make(map[int]int64, len(numaIDs))
	for id := range numaIDs {
		out[id] = memNodeKB[id] * bytesPerKB
	}
	return out
}

// MaxSpanForNUMA is the generator's cap ({1,2,4,8}), not the max-span node
// label — that comes from the defs actually emitted (balloons.MaxAdvertisableSpan)
// and is smaller on shapes the span-4 composition misses.
func MaxSpanForNUMA(numa int) int {
	switch {
	case numa >= 8:
		return 8
	case numa >= 4:
		return 4
	case numa >= 2:
		return 2
	default:
		return 1
	}
}
