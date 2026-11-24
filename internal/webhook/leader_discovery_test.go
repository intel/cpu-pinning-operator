// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func discoveryScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func lease(holder string) *coordinationv1.Lease {
	l := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "cpu-op", Namespace: "opns"}}
	if holder != "" {
		l.Spec.HolderIdentity = &holder
	}
	return l
}

func leaderPod(name, ns, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     corev1.PodStatus{PodIP: ip},
	}
}

// TestLeaderDiscoveryResolvesPodIP: a follower resolves the holder to the leader pod IP.
func TestLeaderDiscoveryResolvesPodIP(t *testing.T) {
	s := discoveryScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		lease("cpu-op-leader_uuid-123"),
		leaderPod("cpu-op-leader", "opns", "10.1.2.3"),
	).Build()
	d := &LeaderDiscovery{Reader: c, Namespace: "opns", LeaseName: "cpu-op", SelfPodName: "cpu-op-follower"}

	ip, isSelf, err := d.Leader(context.Background())
	if err != nil {
		t.Fatalf("Leader() error: %v", err)
	}
	if isSelf {
		t.Fatal("follower must not report isSelf")
	}
	if ip != "10.1.2.3" {
		t.Fatalf("ip = %q, want 10.1.2.3", ip)
	}
}

// TestLeaderDiscoverySelf: the lease holder recognizes itself without a pod lookup.
func TestLeaderDiscoverySelf(t *testing.T) {
	s := discoveryScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		lease("cpu-op-leader_uuid-123"),
	).Build()
	d := &LeaderDiscovery{Reader: c, Namespace: "opns", LeaseName: "cpu-op", SelfPodName: "cpu-op-leader"}

	_, isSelf, err := d.Leader(context.Background())
	if err != nil {
		t.Fatalf("Leader() error: %v", err)
	}
	if !isSelf {
		t.Fatal("the lease holder must report isSelf")
	}
}

// TestLeaderDiscoveryNoHolder: an unheld lease is an error, never a wrong reservation.
func TestLeaderDiscoveryNoHolder(t *testing.T) {
	s := discoveryScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		lease(""),
	).Build()
	d := &LeaderDiscovery{Reader: c, Namespace: "opns", LeaseName: "cpu-op", SelfPodName: "cpu-op-follower"}

	if _, _, err := d.Leader(context.Background()); err == nil {
		t.Fatal("an unheld lease must yield an error so the caller falls back")
	}
}

// TestLeaderDiscoveryLeaderPodNoIP: a leader pod without an IP is an error, not a dial to "".
func TestLeaderDiscoveryLeaderPodNoIP(t *testing.T) {
	s := discoveryScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		lease("cpu-op-leader_uuid"),
		leaderPod("cpu-op-leader", "opns", ""),
	).Build()
	d := &LeaderDiscovery{Reader: c, Namespace: "opns", LeaseName: "cpu-op", SelfPodName: "cpu-op-follower"}

	if _, _, err := d.Leader(context.Background()); err == nil {
		t.Fatal("a leader pod without an IP must yield an error so the caller falls back")
	}
}
