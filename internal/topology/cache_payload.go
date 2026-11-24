// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"encoding/json"
	"fmt"
	"sort"
)

// nodeCacheJSON is the wire form of the per-CPU and per-NUMA-memory maps the
// webhook reads, shared with follower replicas. Sorted so an unchanged topology
// re-encodes identically. Slices, not arrays: json zero-fills short arrays.
type nodeCacheJSON struct {
	CPUs [][]int   `json:"cpus"`
	Mem  [][]int64 `json:"mem,omitempty"`
}

// cpuTupleLen and memTupleLen are the exact field counts of the wire tuples:
// [cpu, numa, core] and [numa, bytes].
const (
	cpuTupleLen = 3
	memTupleLen = 2
)

// EncodeNodeCache serializes the per-CPU and per-NUMA memory maps deterministically
// for the Node annotation; mem may be empty.
func EncodeNodeCache(cpus map[int]CPUInfo, mem map[int]int64) (string, error) {
	payload := nodeCacheJSON{}

	cpuIDs := make([]int, 0, len(cpus))
	for id := range cpus {
		cpuIDs = append(cpuIDs, id)
	}
	sort.Ints(cpuIDs)
	for _, id := range cpuIDs {
		info := cpus[id]
		payload.CPUs = append(payload.CPUs, []int{info.CPU, info.NUMA, info.Core})
	}

	numaIDs := make([]int, 0, len(mem))
	for id := range mem {
		numaIDs = append(numaIDs, id)
	}
	sort.Ints(numaIDs)
	for _, id := range numaIDs {
		payload.Mem = append(payload.Mem, []int64{int64(id), mem[id]})
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodeNodeCache is the inverse of EncodeNodeCache; missing memory yields an empty map, not an error.
func DecodeNodeCache(s string) (cpus map[int]CPUInfo, mem map[int]int64, err error) {
	var payload nodeCacheJSON
	if err := json.Unmarshal([]byte(s), &payload); err != nil {
		return nil, nil, err
	}
	// All or nothing: reject anything the producer could not have written.
	cpus = make(map[int]CPUInfo, len(payload.CPUs))
	numasWithCPU := map[int]bool{}
	coreNUMA := map[int]int{}
	for _, t := range payload.CPUs {
		if len(t) != cpuTupleLen {
			return nil, nil, fmt.Errorf("node cache payload carries a cpu entry with %d fields, want %d", len(t), cpuTupleLen)
		}
		if t[0] < 0 || t[1] < 0 || t[2] < 0 || t[0] > maxCPUID || t[1] > MaxNUMAID || t[2] > maxCPUID {
			return nil, nil, fmt.Errorf("node cache payload carries an out-of-range cpu entry %v", t)
		}
		// A repeated CPU id means conflicting readings.
		if _, dup := cpus[t[0]]; dup {
			return nil, nil, fmt.Errorf("node cache payload lists cpu %d twice", t[0])
		}
		// A core belongs to one NUMA; disagreement makes FreePerNUMA nondeterministic.
		if numa, claimed := coreNUMA[t[2]]; claimed && numa != t[1] {
			return nil, nil, fmt.Errorf("node cache payload places core %d on NUMA %d and %d", t[2], numa, t[1])
		}
		coreNUMA[t[2]] = t[1]
		cpus[t[0]] = CPUInfo{CPU: t[0], NUMA: t[1], Core: t[2]}
		numasWithCPU[t[1]] = true
	}
	if len(payload.Mem) > 0 {
		mem = make(map[int]int64, len(payload.Mem))
		for _, t := range payload.Mem {
			if len(t) != memTupleLen {
				return nil, nil, fmt.Errorf("node cache payload carries a memory entry with %d fields, want %d", len(t), memTupleLen)
			}
			// A non-positive capacity would reject every memory request.
			if t[0] < 0 || t[0] > MaxNUMAID || t[1] <= 0 {
				return nil, nil, fmt.Errorf("node cache payload carries an out-of-range memory entry %v", t)
			}
			numa := int(t[0])
			// A repeated NUMA id could overstate free memory.
			if _, dup := mem[numa]; dup {
				return nil, nil, fmt.Errorf("node cache payload lists memory for NUMA %d twice", numa)
			}
			mem[numa] = t[1]
		}
		// A NUMA used by CPUs but missing from memory means a partial payload.
		for numa := range numasWithCPU {
			if _, ok := mem[numa]; !ok {
				return nil, nil, fmt.Errorf("node cache payload carries no memory for NUMA %d, which its cpus reference", numa)
			}
		}
	}
	return cpus, mem, nil
}
