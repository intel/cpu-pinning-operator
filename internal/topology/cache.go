// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import "sync"

// Cache is a thread-safe, in-memory store of per-node topology published by the
// Node reconciler: per-CPU placement, per-NUMA memory and the pinnable pool. Read
// by the NRT metrics reconciler and the webhook. Until the next reconcile repopulates
// it, a miss leaves metric NUMA/core labels empty and makes the webhook's capacity
// checks fall back to static estimates.
type Cache struct {
	mu   sync.RWMutex
	cpus map[string]map[int]CPUInfo
	mem  map[string]map[int]int64
	// pinnable is balloons.PinnableCPUs per node; absent until first provisioning.
	pinnable map[string]map[int]bool
	// topo is the full topology per node, keyed by boot id so config-only
	// reconciles skip the probe pod (probe churn leaks NotReady CRI sandboxes).
	topo map[string]cachedTopology
}

// cachedTopology is a topology tagged with its boot id; only a reboot can change CPU topology.
type cachedTopology struct {
	bootID string
	topo   *NodeTopology
}

// NewCache returns an empty topology Cache.
func NewCache() *Cache {
	return &Cache{
		cpus:     map[string]map[int]CPUInfo{},
		mem:      map[string]map[int]int64{},
		pinnable: map[string]map[int]bool{},
		topo:     map[string]cachedTopology{},
	}
}

// SetPinnable stores the node's pinnable CPUs; an empty set reads back as unknown.
func (c *Cache) SetPinnable(node string, pinnable map[int]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pinnable[node] = pinnable
}

// GetPinnable returns the node's pinnable CPUs, or (nil, false) for unknown;
// callers then fall back to the hardware total, not an empty pool.
func (c *Cache) GetPinnable(node string) (map[int]bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.pinnable[node]
	if !ok || len(m) == 0 {
		return nil, false
	}
	return m, true
}

// Set stores a node's per-CPU topology map (replacing any previous entry).
func (c *Cache) Set(node string, cpus map[int]CPUInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cpus[node] = cpus
}

// Get returns a node's per-CPU topology map, or (nil, false) if not yet probed.
func (c *Cache) Get(node string) (map[int]CPUInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.cpus[node]
	return m, ok
}

// SetMemory stores a node's per-NUMA memory map (numa id -> bytes); an empty map is not stored.
func (c *Cache) SetMemory(node string, mem map[int]int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(mem) == 0 {
		delete(c.mem, node)
		return
	}
	c.mem[node] = mem
}

// GetMemory returns a node's per-NUMA memory map, or (nil, false) when there is none.
func (c *Cache) GetMemory(node string) (map[int]int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.mem[node]
	if !ok || len(m) == 0 {
		return nil, false
	}
	return m, true
}

// SetTopology stores a node's full topology tagged with its boot id.
func (c *Cache) SetTopology(node, bootID string, topo *NodeTopology) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.topo[node] = cachedTopology{bootID: bootID, topo: topo}
}

// GetTopology returns the cached topology only if it was discovered under bootID;
// an empty bootID never matches, forcing a re-probe.
func (c *Cache) GetTopology(node, bootID string) (*NodeTopology, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if bootID == "" {
		return nil, false
	}
	ct, ok := c.topo[node]
	if !ok || ct.bootID != bootID || ct.topo == nil {
		return nil, false
	}
	return ct.topo, true
}

// Delete drops all of a node's entries.
func (c *Cache) Delete(node string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cpus, node)
	delete(c.mem, node)
	delete(c.pinnable, node)
	delete(c.topo, node)
}
