// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"reflect"
	"testing"
)

// TestNodeCacheRoundTrip proves CPU placement and per-NUMA memory survive encode/decode exactly.
func TestNodeCacheRoundTrip(t *testing.T) {
	cpus := map[int]CPUInfo{
		0:  {CPU: 0, NUMA: 0, Core: 0},
		1:  {CPU: 1, NUMA: 0, Core: 0}, // HT sibling of 0
		2:  {CPU: 2, NUMA: 0, Core: 2},
		10: {CPU: 10, NUMA: 1, Core: 10},
	}
	mem := map[int]int64{0: 64 * 1024 * 1024 * 1024, 1: 32 * 1024 * 1024 * 1024}

	s, err := EncodeNodeCache(cpus, mem)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	gotCPUs, gotMem, err := DecodeNodeCache(s)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(gotCPUs, cpus) {
		t.Errorf("decoded CPUs = %v, want %v", gotCPUs, cpus)
	}
	if !reflect.DeepEqual(gotMem, mem) {
		t.Errorf("decoded mem = %v, want %v", gotMem, mem)
	}
}

// TestNodeCacheDeterministic proves the encoding does not depend on map iteration order.
func TestNodeCacheDeterministic(t *testing.T) {
	cpus := map[int]CPUInfo{
		3: {CPU: 3, NUMA: 1, Core: 3},
		0: {CPU: 0, NUMA: 0, Core: 0},
		2: {CPU: 2, NUMA: 0, Core: 2},
		1: {CPU: 1, NUMA: 0, Core: 0},
	}
	mem := map[int]int64{1: 2, 0: 1}
	first, err := EncodeNodeCache(cpus, mem)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := 0; i < 20; i++ {
		s, err := EncodeNodeCache(cpus, mem)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if s != first {
			t.Fatalf("encoding not deterministic: %q != %q", s, first)
		}
	}
}

// TestNodeCacheNoMemory proves a payload without memory round-trips to a nil memory map.
func TestNodeCacheNoMemory(t *testing.T) {
	cpus := map[int]CPUInfo{0: {CPU: 0, NUMA: 0, Core: 0}}
	s, err := EncodeNodeCache(cpus, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	gotCPUs, gotMem, err := DecodeNodeCache(s)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(gotCPUs, cpus) {
		t.Errorf("decoded CPUs = %v, want %v", gotCPUs, cpus)
	}
	if gotMem != nil {
		t.Errorf("decoded mem = %v, want nil (no memory reading)", gotMem)
	}
}

// TestDecodeNodeCacheMalformed proves a garbage annotation is an error, not a panic or empty topology.
func TestDecodeNodeCacheMalformed(t *testing.T) {
	if _, _, err := DecodeNodeCache("not json"); err == nil {
		t.Fatal("decode of malformed payload must return an error")
	}
}
