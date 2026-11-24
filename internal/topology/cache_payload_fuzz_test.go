// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"encoding/json"
	"fmt"
	"testing"
)

// FuzzDecodeNodeCache proves an arbitrary annotation is rejected or decodes to a
// payload that re-encodes unchanged.
func FuzzDecodeNodeCache(f *testing.F) {
	f.Add(`{"cpus":[[0,0,0],[1,1,1]],"mem":[[0,1024],[1,2048]]}`)
	f.Add(`{"cpus":[[0,0,0]]}`)
	f.Add(`{"cpus":[],"mem":[]}`)
	f.Add(`{"cpus":[[-1,-2,-3]],"mem":[[-1,-1]]}`)
	f.Add(`{"cpus":[[0,0,0],[0,1,1]]}`)
	f.Add(`{"cpus":[[]]}`)
	f.Add(`{"cpus":[[0,0,0],[1,1,1]],"mem":[[0,1024]]}`)
	f.Add(`{"cpus":[[0,0,0]],"mem":[[0,1024],[0,2048]]}`)
	f.Add(`{"cpus":[[0,0,0,0]],"mem":[[0]]}`)
	f.Add(`{"cpus":[[0,0,0],[1,1,0]]}`)
	f.Add(`{"mem":[[0,9223372036854775807]]}`)
	f.Add(`{}`)
	f.Add(`null`)
	f.Add("")

	f.Fuzz(func(t *testing.T, payload string) {
		cpus, mem, err := DecodeNodeCache(payload)
		// A payload the producer could not have written must be rejected, not reduced.
		if reason := malformedPayload(payload); reason != "" && err == nil {
			t.Fatalf("DecodeNodeCache(%q) accepted a payload that %s", payload, reason)
		}
		if err != nil {
			return
		}
		for id, info := range cpus {
			if info.CPU != id {
				t.Fatalf("DecodeNodeCache(%q) keyed CPU %d under %d", payload, info.CPU, id)
			}
			// An impossible id must be rejected.
			if info.CPU < 0 || info.NUMA < 0 || info.Core < 0 ||
				info.CPU > maxCPUID || info.NUMA > MaxNUMAID || info.Core > maxCPUID {
				t.Fatalf("DecodeNodeCache(%q) accepted cpu entry %+v", payload, info)
			}
		}
		for id, bytes := range mem {
			// A non-positive capacity must be rejected.
			if id < 0 || id > MaxNUMAID || bytes <= 0 {
				t.Fatalf("DecodeNodeCache(%q) accepted memory entry %d=%d", payload, id, bytes)
			}
		}
		// A present memory map must cover every NUMA the CPUs use.
		if len(mem) > 0 {
			for _, info := range cpus {
				if _, ok := mem[info.NUMA]; !ok {
					t.Fatalf("DecodeNodeCache(%q) accepted a memory map with no entry for NUMA %d", payload, info.NUMA)
				}
			}
		}

		encoded, err := EncodeNodeCache(cpus, mem)
		if err != nil {
			t.Fatalf("EncodeNodeCache after decoding %q: %v", payload, err)
		}
		cpus2, mem2, err := DecodeNodeCache(encoded)
		if err != nil {
			t.Fatalf("DecodeNodeCache(%q) re-encoded to %q, which does not decode: %v", payload, encoded, err)
		}
		if len(cpus2) != len(cpus) || len(mem2) != len(mem) {
			t.Fatalf("round trip of %q changed sizes: cpus %d->%d, mem %d->%d",
				payload, len(cpus), len(cpus2), len(mem), len(mem2))
		}
		for id, info := range cpus {
			if cpus2[id] != info {
				t.Fatalf("round trip of %q changed CPU %d: %+v -> %+v", payload, id, info, cpus2[id])
			}
		}
		for id, bytes := range mem {
			if mem2[id] != bytes {
				t.Fatalf("round trip of %q changed NUMA %d memory: %d -> %d", payload, id, bytes, mem2[id])
			}
		}

		// Re-encoding must be byte-stable, or the leader would patch the Node every reconcile.
		if again, err := EncodeNodeCache(cpus2, mem2); err != nil || again != encoded {
			t.Fatalf("EncodeNodeCache not stable for %q: %q vs %q (err %v)", payload, encoded, again, err)
		}
	})
}

// malformedPayload independently reports why a well-shaped JSON payload must be rejected, or "".
func malformedPayload(payload string) string {
	var raw struct {
		CPUs [][]int   `json:"cpus"`
		Mem  [][]int64 `json:"mem"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return ""
	}
	cpuSeen := map[int]bool{}
	coreNUMA := map[int]int{}
	numasWithCPU := map[int]bool{}
	for _, t := range raw.CPUs {
		if len(t) != 3 {
			return fmt.Sprintf("carries a cpu tuple of %d fields", len(t))
		}
		if t[0] < 0 || t[1] < 0 || t[2] < 0 || t[0] > maxCPUID || t[1] > MaxNUMAID || t[2] > maxCPUID {
			return fmt.Sprintf("carries an out-of-range cpu tuple %v", t)
		}
		if cpuSeen[t[0]] {
			return fmt.Sprintf("lists cpu %d twice", t[0])
		}
		if numa, claimed := coreNUMA[t[2]]; claimed && numa != t[1] {
			return fmt.Sprintf("places core %d on NUMA %d and %d", t[2], numa, t[1])
		}
		cpuSeen[t[0]] = true
		coreNUMA[t[2]] = t[1]
		numasWithCPU[t[1]] = true
	}
	if len(raw.Mem) == 0 {
		return ""
	}
	memSeen := map[int]bool{}
	for _, t := range raw.Mem {
		if len(t) != 2 {
			return fmt.Sprintf("carries a memory tuple of %d fields", len(t))
		}
		if t[0] < 0 || t[0] > MaxNUMAID || t[1] <= 0 {
			return fmt.Sprintf("carries an out-of-range memory tuple %v", t)
		}
		if memSeen[int(t[0])] {
			return fmt.Sprintf("lists memory for NUMA %d twice", t[0])
		}
		memSeen[int(t[0])] = true
	}
	for numa := range numasWithCPU {
		if !memSeen[numa] {
			return fmt.Sprintf("carries no memory for NUMA %d, which its cpus reference", numa)
		}
	}
	return ""
}
