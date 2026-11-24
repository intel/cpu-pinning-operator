// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"reflect"
	"testing"
)

func TestCacheSetGet(t *testing.T) {
	c := NewCache()

	if _, ok := c.Get("node-a"); ok {
		t.Fatal("Get on empty cache returned ok=true")
	}

	cpus := map[int]CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0},
		1: {CPU: 1, NUMA: 1, Core: 1},
	}
	c.Set("node-a", cpus)

	got, ok := c.Get("node-a")
	if !ok {
		t.Fatal("Get after Set returned ok=false")
	}
	if !reflect.DeepEqual(got, cpus) {
		t.Errorf("Get = %v, want %v", got, cpus)
	}
}

func TestCacheSetGetMemory(t *testing.T) {
	c := NewCache()

	// Unprobed node: no memory entry.
	if m, ok := c.GetMemory("node-a"); ok || m != nil {
		t.Fatalf("GetMemory on empty cache = (%v, %v), want (nil, false)", m, ok)
	}

	mem := map[int]int64{
		0: 263758548 * 1024,
		1: 264145172 * 1024,
	}
	c.SetMemory("node-a", mem)

	got, ok := c.GetMemory("node-a")
	if !ok {
		t.Fatal("GetMemory after SetMemory returned ok=false")
	}
	if !reflect.DeepEqual(got, mem) {
		t.Errorf("GetMemory = %v, want %v", got, mem)
	}

	// SetMemory replaces the previous entry rather than merging.
	newMem := map[int]int64{0: 42 * 1024}
	c.SetMemory("node-a", newMem)
	got, _ = c.GetMemory("node-a")
	if !reflect.DeepEqual(got, newMem) {
		t.Errorf("GetMemory after replace = %v, want %v", got, newMem)
	}

	// An empty or nil map is not stored and drops an existing entry.
	c.SetMemory("node-a", map[int]int64{})
	if m, ok := c.GetMemory("node-a"); ok || m != nil {
		t.Errorf("GetMemory after SetMemory(empty) = (%v, %v), want (nil, false)", m, ok)
	}
	c.SetMemory("node-b", nil)
	if m, ok := c.GetMemory("node-b"); ok || m != nil {
		t.Errorf("GetMemory after SetMemory(nil) = (%v, %v), want (nil, false)", m, ok)
	}
}

// TestCacheMemoryIndependentOfCPUs proves the memory and per-CPU maps do not disturb each other.
func TestCacheMemoryIndependentOfCPUs(t *testing.T) {
	c := NewCache()

	cpus := map[int]CPUInfo{0: {CPU: 0, NUMA: 0, Core: 0}}
	c.Set("node-a", cpus)

	// A node with only memory published has no per-CPU entry, and vice versa.
	c.SetMemory("node-b", map[int]int64{0: 1024})

	if _, ok := c.Get("node-b"); ok {
		t.Error("Get(node-b) ok=true, but only memory was set")
	}
	if _, ok := c.GetMemory("node-a"); ok {
		t.Error("GetMemory(node-a) ok=true, but only cpus were set")
	}

	// Setting memory on node-a must leave its per-CPU map intact.
	c.SetMemory("node-a", map[int]int64{0: 2048})
	got, ok := c.Get("node-a")
	if !ok || !reflect.DeepEqual(got, cpus) {
		t.Errorf("Get(node-a) after SetMemory = (%v, %v), want the original cpus", got, ok)
	}
}

func TestCacheDeleteDropsCPUMemoryAndTopology(t *testing.T) {
	c := NewCache()
	c.Set("node-a", map[int]CPUInfo{0: {CPU: 0, NUMA: 0, Core: 0}})
	c.SetMemory("node-a", map[int]int64{0: 1024})
	c.SetTopology("node-a", "boot-1", &NodeTopology{NumaNodes: 2})

	c.SetPinnable("node-a", map[int]bool{0: true})

	c.Delete("node-a")

	if _, ok := c.Get("node-a"); ok {
		t.Error("Get after Delete returned ok=true (per-CPU entry not dropped)")
	}
	if _, ok := c.GetPinnable("node-a"); ok {
		t.Error("GetPinnable after Delete returned ok=true (pinnable pool not dropped)")
	}
	if _, ok := c.GetMemory("node-a"); ok {
		t.Error("GetMemory after Delete returned ok=true (memory entry not dropped)")
	}
	if _, ok := c.GetTopology("node-a", "boot-1"); ok {
		t.Error("GetTopology after Delete returned ok=true (topology entry not dropped)")
	}
}

// TestCacheTopologyBootIDGuard proves a cached topology is reused only under the same, non-empty boot id.
func TestCacheTopologyBootIDGuard(t *testing.T) {
	c := NewCache()

	// Unprobed node: nothing cached.
	if _, ok := c.GetTopology("node-a", "boot-1"); ok {
		t.Fatal("GetTopology on empty cache returned ok=true")
	}

	topo := &NodeTopology{NumaNodes: 2, PhysicalCPUs: 32, MaxSpan: 2}
	c.SetTopology("node-a", "boot-1", topo)

	// Same boot id → cached reading is current, probe is skipped.
	got, ok := c.GetTopology("node-a", "boot-1")
	if !ok {
		t.Fatal("GetTopology with matching boot id returned ok=false")
	}
	if got != topo {
		t.Errorf("GetTopology returned a different topology than was stored")
	}

	// Different boot id (node rebooted) → stale, must re-probe.
	if _, ok := c.GetTopology("node-a", "boot-2"); ok {
		t.Error("GetTopology with a changed boot id returned ok=true, want stale (false)")
	}

	// Empty boot id (unknown) → never current, always re-probe.
	if _, ok := c.GetTopology("node-a", ""); ok {
		t.Error("GetTopology with empty boot id returned ok=true, want false")
	}

	// Re-probing under the new boot id refreshes the entry.
	topo2 := &NodeTopology{NumaNodes: 4, MaxSpan: 4}
	c.SetTopology("node-a", "boot-2", topo2)
	got, ok = c.GetTopology("node-a", "boot-2")
	if !ok || got != topo2 {
		t.Errorf("GetTopology after re-probe = (%v, %v), want the refreshed topology", got, ok)
	}
	// The old boot id no longer matches.
	if _, ok := c.GetTopology("node-a", "boot-1"); ok {
		t.Error("GetTopology with the pre-reboot boot id still ok=true after refresh")
	}
}

// TestCachePinnablePool proves a stored pool reads back as stored, and an unset or empty one as unknown.
func TestCachePinnablePool(t *testing.T) {
	c := NewCache()

	if _, ok := c.GetPinnable("node-a"); ok {
		t.Error("GetPinnable on an unprovisioned node returned ok=true")
	}

	c.SetPinnable("node-a", map[int]bool{0: true, 2: true})
	got, ok := c.GetPinnable("node-a")
	if !ok {
		t.Fatal("GetPinnable after SetPinnable returned ok=false")
	}
	if len(got) != 2 || !got[0] || !got[2] {
		t.Errorf("pinnable pool = %v, want CPUs 0 and 2", got)
	}

	// An empty pool is not a claim about the node, it is the absence of one.
	c.SetPinnable("node-a", map[int]bool{})
	if _, ok := c.GetPinnable("node-a"); ok {
		t.Error("an empty pinnable pool must read as unknown, not as zero capacity")
	}
}
