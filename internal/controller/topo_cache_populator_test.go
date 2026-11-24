// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// topoCachePopulator builds a TopoCachePopulator over a fake client and its Cache.
func topoCachePopulator(objs ...runtime.Object) (*TopoCachePopulator, *topology.Cache) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	cache := topology.NewCache()
	return &TopoCachePopulator{
		Client:    c,
		Log:       ctrl.Log.WithName("test"),
		TopoCache: cache,
	}, cache
}

// nodeWithTopoAnnotation builds a Node carrying the topology-cache annotation.
func nodeWithTopoAnnotation(t *testing.T, name string, cpus map[int]topology.CPUInfo, mem map[int]int64) *corev1.Node {
	t.Helper()
	payload, err := topology.EncodeNodeCache(cpus, mem)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Annotations: map[string]string{AnnotationTopologyCache: payload},
	}}
}

func reconcilePopulator(t *testing.T, p *TopoCachePopulator, name string) {
	t.Helper()
	if _, err := p.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

// TestTopoCachePopulator_NotLeaderGated: the populator must run on every replica,
// or a follower's webhook falls back to the static capacity formula.
func TestTopoCachePopulator_NotLeaderGated(t *testing.T) {
	p, _ := topoCachePopulator()
	var r manager.LeaderElectionRunnable = p
	if r.NeedLeaderElection() {
		t.Fatal("populator must NOT be leader-gated: it has to warm the topology cache on every replica")
	}
}

// TestTopoCachePopulator_PopulatesCache proves the annotation reaches the cache
// in the shape the webhook reads (Cache.Get / GetMemory), without leader election.
func TestTopoCachePopulator_PopulatesCache(t *testing.T) {
	cpus := map[int]topology.CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0},
		1: {CPU: 1, NUMA: 1, Core: 1},
	}
	mem := map[int]int64{0: 8, 1: 8}
	node := nodeWithTopoAnnotation(t, "n1", cpus, mem)
	p, cache := topoCachePopulator(node)

	if _, ok := cache.Get("n1"); ok {
		t.Fatal("cache should be empty before reconcile")
	}

	reconcilePopulator(t, p, "n1")

	got, ok := cache.Get("n1")
	if !ok {
		t.Fatal("after reconcile the follower cache must hold the node's per-CPU topology")
	}
	if len(got) != 2 || got[0].NUMA != 0 || got[1].NUMA != 1 {
		t.Errorf("cached CPUs = %v, want the encoded placement", got)
	}
	gotMem, ok := cache.GetMemory("n1")
	if !ok || gotMem[0] != 8 || gotMem[1] != 8 {
		t.Errorf("cached memory = %v (ok=%v), want the encoded per-NUMA map", gotMem, ok)
	}
}

// TestTopoCachePopulator_NodeDeletedDropsEntry proves a deleted Node's entry is dropped.
func TestTopoCachePopulator_NodeDeletedDropsEntry(t *testing.T) {
	p, cache := topoCachePopulator() // no Node in the client → Get is NotFound
	cache.Set("gone", map[int]topology.CPUInfo{0: {CPU: 0, NUMA: 0, Core: 0}})

	reconcilePopulator(t, p, "gone")

	if _, ok := cache.Get("gone"); ok {
		t.Error("a deleted node's cache entry must be dropped, got a stale reading")
	}
}

// TestTopoCachePopulator_MissingAnnotationDropsEntry proves an unannotated Node's entry is dropped.
func TestTopoCachePopulator_MissingAnnotationDropsEntry(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "bare"}}
	p, cache := topoCachePopulator(node)
	cache.Set("bare", map[int]topology.CPUInfo{0: {CPU: 0, NUMA: 0, Core: 0}})

	reconcilePopulator(t, p, "bare")

	if _, ok := cache.Get("bare"); ok {
		t.Error("a node without the topology annotation must not retain a cached reading")
	}
}

// TestTopoCachePopulator_MalformedAnnotationDropsEntry proves a garbage annotation
// drops the entry without failing the reconcile.
func TestTopoCachePopulator_MalformedAnnotationDropsEntry(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "bad",
		Annotations: map[string]string{AnnotationTopologyCache: "not json"},
	}}
	p, cache := topoCachePopulator(node)
	cache.Set("bad", map[int]topology.CPUInfo{0: {CPU: 0, NUMA: 0, Core: 0}})

	reconcilePopulator(t, p, "bad") // must not error

	if _, ok := cache.Get("bad"); ok {
		t.Error("a malformed annotation must drop the entry (treated as unknown), not retain stale topology")
	}
}
