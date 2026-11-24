// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"testing"

	"github.com/go-logr/logr"

	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// TestOffModeStillAccountsMemory: with the capacity check off, the reservation still
// records the pod's memory, so the next pod is not sized against committed memory.
func TestOffModeStillAccountsMemory(t *testing.T) {
	ctx := context.Background()
	const node = "off-mode-node"
	const gib = int64(1) << 30

	topo := topology.NewCache()
	topo.Set(node, fourCorePerNUMATopo()) // 8 CPUs, 2 NUMA
	m := newMutatorWithTopo(topo, withMemoryFloor(topoNode(node, 8, 2), 100*gib), nrtObj(node))
	m.Reservations = NewReservationStore(0)

	// filterMem=false is what handle() passes for capacityCheckMode: off.
	pod := unscheduledPod("batch", "1")
	v := m.admitWithHeadroom(ctx, pod, 1, 1, 100*gib, false, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !v.fits {
		t.Fatalf("off mode must not deny: verdict = %+v", v)
	}

	// ceil(100GiB / span 1) = 100GiB on the one chosen NUMA.
	mem := reservedMem(m.Reservations, node)
	if len(mem) == 0 {
		t.Fatalf("no memory reserved on %q — the pod's 100GiB demand was dropped, so the next admission is sized against memory already committed", node)
	}
	var total int64
	for _, b := range mem {
		total += b
	}
	if total != 100*gib {
		t.Errorf("reserved memory = %d bytes across %v, want %d (ceil(request/span))", total, mem, 100*gib)
	}

	if cpu := reservedCores(m.Reservations, node); len(cpu) == 0 {
		t.Error("expected a CPU reservation as well")
	}
}

// TestOffModeDoesNotFilterOnMemory: with filtering off, memory does not narrow the
// candidates, so a request beyond every node's floor is still admitted.
func TestOffModeDoesNotFilterOnMemory(t *testing.T) {
	ctx := context.Background()
	const node = "off-mode-tiny"
	const gib = int64(1) << 30

	topo := topology.NewCache()
	topo.Set(node, fourCorePerNUMATopo())

	m := newMutatorWithTopo(topo, withMemoryFloor(topoNode(node, 8, 2), 1*gib), nrtObj(node))
	m.Reservations = NewReservationStore(0)

	pod := unscheduledPod("batch", "1")
	v := m.admitWithHeadroom(ctx, pod, 1, 1, 500*gib, false, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !v.fits {
		t.Fatalf("with filtering off, memory must not exclude the node: verdict = %+v", v)
	}
	if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != node {
		t.Errorf("REQUIRED hostnames = %v, want [%s] — the pod must still be steered", req, node)
	}
	if got := affinityPreferredHostname(pod); got != "" {
		t.Errorf("PREFERRED hostname = %q, want none (the reserved node is REQUIRED)", got)
	}

	// With filtering on the same request must be rejected.
	pod2 := unscheduledPod("batch2", "1")
	v2 := m.admitWithHeadroom(ctx, pod2, 1, 1, 500*gib, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if v2.fits {
		t.Fatalf("with filtering on, 500GiB against a 1GiB/NUMA floor must not fit: verdict = %+v", v2)
	}
	if v2.reason != "memory" {
		t.Errorf("verdict reason = %q, want \"memory\"", v2.reason)
	}
}
