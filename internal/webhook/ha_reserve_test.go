// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// stubReserver is a reserveClient double: it records the ranked list and returns either
// an err (unreachable leader) or a definite (node, accepted) verdict.
type stubReserver struct {
	node     string
	accepted bool
	err      error
	calls    int32
	lastReqs []ReserveRequest
	keys     []string // every idempotency key handed to the seam, in order
}

func (s *stubReserver) reserveBest(_ context.Context, key string, reqs []ReserveRequest) (string, []int, bool, error) {
	atomic.AddInt32(&s.calls, 1)
	s.lastReqs = reqs
	s.keys = append(s.keys, key)
	return s.node, nil, s.accepted, s.err
}

// TestReserveAuthorityNeedsLeaderElection: the authoritative store must run only on the
// lease holder, or every replica serves its own store and over-admits.
func TestReserveAuthorityNeedsLeaderElection(t *testing.T) {

	var _ manager.LeaderElectionRunnable = &ReserveAuthority{}

	if !(&ReserveAuthority{}).NeedLeaderElection() {
		t.Fatal("ReserveAuthority.NeedLeaderElection() = false; the authoritative store would start on every replica and stores would diverge")
	}
}

// TestAdmitWithHeadroomFallsBackToLocalOnReserverError: under warn/off a transport error
// does not deny and reserves locally. TestCommitUnaccountableSplitsByMode covers enforce.
func TestAdmitWithHeadroomFallsBackToLocalOnReserverError(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("empty", simpleTopo()) // 4 CPUs, 2 NUMA, 2 cores each, no occupancy
	m := newMutatorWithTopo(topo, topoNode("empty", 4, 2), nrtObj("empty"))
	m.Reservations = NewReservationStore(0)

	stub := &stubReserver{err: errors.New("leader unreachable: connection refused")}
	m.SetReserver(stub)

	pod := unscheduledPod("p", "1")
	verdict := m.admitWithHeadroom(ctx, pod, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawLive || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=true (a reserver error must fall back to local, not deny)", verdict)
	}
	if stub.calls != 2 {
		t.Fatalf("reserver called %d times, want 2 (one retry on the same key before the local fallback)", stub.calls)
	}

	if r := reservedCores(m.Reservations, "empty"); len(r) == 0 {
		t.Fatal("expected the local-fallback reservation on 'empty', got none — the failover path must still reserve")
	}
	if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "empty" {
		t.Errorf("REQUIRED hostnames = %v, want [empty] (the pod must still be steered to the fitting node)", req)
	}
	if got := affinityPreferredHostname(pod); got != "" {
		t.Errorf("PREFERRED hostname = %q, want none (a local-fallback commit pins the node too)", got)
	}
}

// TestAdmitWithHeadroomHonorsReserverReject: an authority reject denies with a capacity
// verdict, never admits on the follower's stale local ranking.
func TestAdmitWithHeadroomHonorsReserverReject(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("empty", simpleTopo())
	m := newMutatorWithTopo(topo, topoNode("empty", 4, 2), nrtObj("empty"))
	m.Reservations = NewReservationStore(0)

	// accepted=false, err=nil: a definite reject, not a transport failure.
	m.SetReserver(&stubReserver{accepted: false})

	pod := unscheduledPod("p", "1")
	verdict := m.admitWithHeadroom(ctx, pod, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawLive {
		t.Fatalf("verdict = %+v, want sawLive=true", verdict)
	}
	if verdict.fits {
		t.Fatal("verdict.fits = true; a definite authority reject must deny, not admit on the local ranking")
	}
	if verdict.reason != "capacity" {
		t.Errorf("verdict.reason = %q, want capacity", verdict.reason)
	}

	if r := reservedCores(m.Reservations, "empty"); len(r) != 0 {
		t.Errorf("local store = %v, want empty — an authority reject must not reserve locally", r)
	}
}

// TestAdmitWithHeadroomSteersToAuthorityChosenNode: the required hostname is the node the
// authority committed, not the follower's top choice.
func TestAdmitWithHeadroomSteersToAuthorityChosenNode(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	topo.Set("b", simpleTopo())
	m := newMutatorWithTopo(topo,
		topoNode("a", 4, 2), topoNode("b", 4, 2), nrtObj("a"), nrtObj("b"))
	m.Reservations = NewReservationStore(0)

	m.SetReserver(&stubReserver{node: "b", accepted: true})

	pod := unscheduledPod("p", "1")
	verdict := m.admitWithHeadroom(ctx, pod, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.fits {
		t.Fatalf("verdict = %+v, want fits=true", verdict)
	}
	if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "b" {
		t.Errorf("REQUIRED hostnames = %v, want [b] (the node the authority committed to, not the local top choice)", req)
	}
	if got := affinityPreferredHostname(pod); got != "" {
		t.Errorf("PREFERRED hostname = %q, want none (the committed node is REQUIRED)", got)
	}
}

// TestAdmitWithHeadroomDryRunDoesNotConsultReserver: dry-run never forwards or reserves.
func TestAdmitWithHeadroomDryRunDoesNotConsultReserver(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("empty", simpleTopo())
	m := newMutatorWithTopo(topo, topoNode("empty", 4, 2), nrtObj("empty"))
	m.Reservations = NewReservationStore(0)

	stub := &stubReserver{node: "empty", accepted: true}
	m.SetReserver(stub)

	pod := unscheduledPod("p", "1")
	verdict := m.admitWithHeadroom(ctx, pod, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{dryRun: true}, logr.Discard())
	if !verdict.fits {
		t.Fatalf("verdict = %+v, want fits=true", verdict)
	}
	if stub.calls != 0 {
		t.Fatalf("reserver called %d times on a dry-run, want 0 (a preview must not forward or reserve)", stub.calls)
	}
	if r := reservedCores(m.Reservations, "empty"); len(r) != 0 {
		t.Errorf("local store = %v, want empty on a dry-run", r)
	}
	if got := affinityPreferredHostname(pod); got != "empty" {
		t.Errorf("PREFERRED hostname = %q, want empty (affinity still injected on a dry-run)", got)
	}
}

// TestAdmitStaticHeadroomFallsBackToLocalOnReserverError: the cold-start (static) path also
// falls back to the local store on a reserve error.
func TestAdmitStaticHeadroomFallsBackToLocalOnReserverError(t *testing.T) {
	m, _ := coldStartMutator(t, "enforce", topoNode("n1", 8, 2))

	stub := &stubReserver{err: errors.New("leader unreachable: i/o timeout")}
	m.SetReserver(stub)

	pod := unscheduledPod("p", "2")
	verdict := m.admitStaticHeadroom(context.Background(), pod, 2, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawStatic || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawStatic=true fits=true (a reserver error must fall back to local, not deny)", verdict)
	}
	if stub.calls != 2 {
		t.Fatalf("reserver called %d times, want 2 (one retry on the same key before the local fallback)", stub.calls)
	}
	if r := reservedCores(m.Reservations, "n1"); len(r) == 0 {
		t.Fatal("expected the local-fallback reservation on 'n1', got none — the cold-start failover path must still reserve")
	}
}

// TestAdmitStaticHeadroomHonorsReserverReject: the cold-start path denies on an authority reject.
func TestAdmitStaticHeadroomHonorsReserverReject(t *testing.T) {
	m, _ := coldStartMutator(t, "enforce", topoNode("n1", 8, 2))
	m.SetReserver(&stubReserver{accepted: false})

	pod := unscheduledPod("p", "2")
	verdict := m.admitStaticHeadroom(context.Background(), pod, 2, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawStatic {
		t.Fatalf("verdict = %+v, want sawStatic=true", verdict)
	}
	if verdict.fits {
		t.Fatal("verdict.fits = true; a definite authority reject must deny at cold start too")
	}
	if verdict.reason != "capacity" {
		t.Errorf("verdict.reason = %q, want capacity", verdict.reason)
	}
	if r := reservedCores(m.Reservations, "n1"); len(r) != 0 {
		t.Errorf("local store = %v, want empty — an authority reject must not reserve locally", r)
	}
}
