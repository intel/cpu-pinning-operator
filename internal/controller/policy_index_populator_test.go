// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
)

// indexPopulator builds a PolicyIndexPopulator over a fake client and its match Index.
func indexPopulator(objs ...runtime.Object) (*PolicyIndexPopulator, *match.Index) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()
	idx := match.NewIndex()
	return &PolicyIndexPopulator{
		Client:      c,
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: idx,
	}, idx
}

// TestPolicyIndexPopulator_NotLeaderGated: the populator runs on every replica, so a
// follower's webhook sees a populated index.
func TestPolicyIndexPopulator_NotLeaderGated(t *testing.T) {
	p, _ := indexPopulator()
	var r manager.LeaderElectionRunnable = p
	if r.NeedLeaderElection() {
		t.Fatal("populator must NOT be leader-gated: it has to populate the index on every replica")
	}
}

// TestPolicyIndexPopulator_PopulatesIndex proves populate fills the Index from
// policies and the cluster config without leader election.
func TestPolicyIndexPopulator_PopulatesIndex(t *testing.T) {
	policy := &cpopv1alpha1.BalloonPinningPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "infer"},
		Spec: cpopv1alpha1.BalloonPinningPolicySpec{
			Match:      cpopv1alpha1.MatchSpec{ByContainerName: []string{matchTargetContainer}},
			Namespaces: []string{"inference"},
		},
	}
	cfg := &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
		Spec: cpopv1alpha1.BalloonsClusterConfigSpec{
			Balloon: cpopv1alpha1.BalloonSpec{CPUAllocation: "hyperthreads"},
		},
	}
	p, idx := indexPopulator(policy, cfg)

	// An empty index admits matched pods unmutated.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "inference"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: matchTargetContainer}}},
	}
	if idx.Match(pod).Rule != nil {
		t.Fatal("index should be empty before populate")
	}

	if err := p.populate(context.Background()); err != nil {
		t.Fatalf("populate: %v", err)
	}

	if idx.Match(pod).Rule == nil {
		t.Error("after populate a matching pod must resolve to a rule (follower webhook would otherwise admit it unmutated)")
	}
	if got := idx.CPUAllocation(); got != "hyperthreads" {
		t.Errorf("CPUAllocation = %q, want hyperthreads (cluster config not published)", got)
	}
}

// TestPolicyIndexPopulator_NoClusterConfig: without the singleton, rules still populate
// and the cluster config resets to defaults.
func TestPolicyIndexPopulator_NoClusterConfig(t *testing.T) {
	policy := &cpopv1alpha1.BalloonPinningPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "infer"},
		Spec: cpopv1alpha1.BalloonPinningPolicySpec{
			Match:      cpopv1alpha1.MatchSpec{ByContainerName: []string{matchTargetContainer}},
			Namespaces: []string{"inference"},
		},
	}
	p, idx := indexPopulator(policy)

	if err := p.populate(context.Background()); err != nil {
		t.Fatalf("populate: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "inference"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: matchTargetContainer}}},
	}
	if idx.Match(pod).Rule == nil {
		t.Error("rules must still be populated when no cluster config singleton exists")
	}
	if got := idx.CPUAllocation(); got != "" {
		t.Errorf("CPUAllocation = %q, want empty when no singleton present", got)
	}
}

// TestPolicyIndexPopulator_NamespaceLabelChange: namespaceSelector policies follow
// Namespace label additions and removals on the next populate.
func TestPolicyIndexPopulator_NamespaceLabelChange(t *testing.T) {
	policy := &cpopv1alpha1.BalloonPinningPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "infer"},
		Spec: cpopv1alpha1.BalloonPinningPolicySpec{
			Match: cpopv1alpha1.MatchSpec{ByContainerName: []string{matchTargetContainer}},
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"pinning": "enabled"},
			},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "inference"}}
	p, idx := indexPopulator(policy, ns)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "inference"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: matchTargetContainer}}},
	}

	if err := p.populate(context.Background()); err != nil {
		t.Fatalf("populate: %v", err)
	}
	if idx.Match(pod).Rule != nil {
		t.Error("pod must not match before its namespace carries the selector label")
	}

	ns.Labels = map[string]string{"pinning": "enabled"}
	if err := p.Update(context.Background(), ns); err != nil {
		t.Fatalf("label namespace: %v", err)
	}
	if err := p.populate(context.Background()); err != nil {
		t.Fatalf("populate: %v", err)
	}
	if idx.Match(pod).Rule == nil {
		t.Error("pod must match after its namespace gains the selector label (follower index went stale otherwise)")
	}

	ns.Labels = nil
	if err := p.Update(context.Background(), ns); err != nil {
		t.Fatalf("unlabel namespace: %v", err)
	}
	if err := p.populate(context.Background()); err != nil {
		t.Fatalf("populate: %v", err)
	}
	if idx.Match(pod).Rule != nil {
		t.Error("pod must stop matching after its namespace loses the selector label")
	}
}

// TestPolicyIndexPopulator_EnqueueSingleKey proves every watched kind funnels
// through enqueue to the one fixed "populate" request key — including a
// Namespace object, the kind the watch was added for — so any event triggers
// exactly one populate reconcile.
func TestPolicyIndexPopulator_EnqueueSingleKey(t *testing.T) {
	p, _ := indexPopulator()
	for _, obj := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "inference"}},
		&cpopv1alpha1.BalloonPinningPolicy{ObjectMeta: metav1.ObjectMeta{Name: "infer"}},
		&cpopv1alpha1.BalloonsClusterConfig{ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName}},
	} {
		reqs := p.enqueue(context.Background(), obj)
		if len(reqs) != 1 || reqs[0].Name != "populate" {
			t.Errorf("enqueue(%T) = %v, want single request key %q", obj, reqs, "populate")
		}
	}
}
