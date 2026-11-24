// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestLeaderPodOnceRejectsStaleLease is cluster-free: a Lease naming a gone pod, as
// right after a rollout, is not a leader until the holder's pod exists.
func TestLeaderPodOnceRejectsStaleLease(t *testing.T) {
	holder := "mgr-new_1a2b"
	cs := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	})
	if _, why := leaderPodOnce(context.Background(), cs, "ns", "l"); !strings.Contains(why, "may be stale") {
		t.Errorf("gone holder pod: reason %q, want a stale-Lease one", why)
	}
	// A cancelled ctx ends the retry at once, as a strict config gap, not after leaderLeaseWait.
	t.Setenv(e2eStrictEnv, "1")
	t.Setenv("E2E_LEADER_LEASE", "l")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := &routingTB{TB: t}
	start := time.Now()
	func() {
		defer rec.recoverStop()
		leaderPodName(ctx, rec, cs, "ns")
	}()
	if took := time.Since(start); rec.fatals != 1 || took > time.Second {
		t.Errorf("cancelled ctx: %d strict failure(s) after %s, want 1 at once", rec.fatals, took)
	}
	if err := cs.Tracker().Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "mgr-new", Namespace: "ns"}}); err != nil {
		t.Fatal(err)
	}
	if pod, why := leaderPodOnce(context.Background(), cs, "ns", "l"); pod != "mgr-new" || why != "" {
		t.Errorf("live holder: got %q, %q; want mgr-new", pod, why)
	}
}

// TestRaxLedgerHeldNeedsTheAuthority is cluster-free: the drain skips its TTL wait only
// on a scraped, empty lease-holder ledger, never on followers' zeros alone.
func TestRaxLedgerHeldNeedsTheAuthority(t *testing.T) {
	zero := raLiveMetric + `{node="n1"} 0` + "\n"
	held := raLiveMetric + `{node="n1"} 2` + "\n"
	for _, tc := range []struct {
		name      string
		byReplica map[string]string
		leader    string
		noLeader  string
		wantEmpty bool
	}{
		{"leader scraped and empty", map[string]string{"lead": zero, "f": zero}, "lead", "", true},
		{"leader scraped, node never reserved", map[string]string{"lead": "", "f": zero}, "lead", "", true},
		{"leader scrape failed, followers zero", map[string]string{"f": zero}, "lead", "", false},
		{"no leader resolvable", map[string]string{"f": zero}, "", "no Lease", false},
		{"leader holds reservations", map[string]string{"lead": held}, "lead", "", false},
		{"nothing scraped", nil, "lead", "", false},
	} {
		if why := raxLedgerHeld(tc.byReplica, tc.leader, "n1", tc.noLeader); (why == "") != tc.wantEmpty {
			t.Errorf("%s: reason %q, want empty=%v", tc.name, why, tc.wantEmpty)
		}
	}
}
