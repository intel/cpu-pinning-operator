// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
)

// clusterConfigSingleton returns the BalloonsClusterConfig singleton with a fixed UID.
func clusterConfigSingleton(uid string) *cpopv1alpha1.BalloonsClusterConfig {
	return &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: cpopv1alpha1.ClusterConfigName,
			UID:  types.UID(uid),
		},
	}
}

// ownsBalloonsClusterConfig reports whether refs has a plain (no Controller, no
// BlockOwnerDeletion) ownerReference to the singleton with uid.
func ownsBalloonsClusterConfig(t *testing.T, refs []metav1.OwnerReference, uid string) bool {
	t.Helper()
	for i := range refs {
		ref := &refs[i]
		if ref.Kind != "BalloonsClusterConfig" || string(ref.UID) != uid {
			continue
		}
		if ref.APIVersion != cpopv1alpha1.GroupVersion.String() {
			t.Errorf("ownerRef APIVersion = %q, want %q", ref.APIVersion, cpopv1alpha1.GroupVersion.String())
		}
		if ref.Name != cpopv1alpha1.ClusterConfigName {
			t.Errorf("ownerRef Name = %q, want %q", ref.Name, cpopv1alpha1.ClusterConfigName)
		}
		if ref.Controller != nil && *ref.Controller {
			t.Errorf("ownerRef Controller = true, want unset (plain ownerRef)")
		}
		if ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion {
			t.Errorf("ownerRef BlockOwnerDeletion = true, want unset (plain ownerRef)")
		}
		return true
	}
	return false
}

// TestNode_OwnerRefStampedOnCreate: a new BalloonsPolicy CR is owned by the singleton,
// so GC reaps it on uninstall.
func TestNode_OwnerRefStampedOnCreate(t *testing.T) {
	const uid = "bcc-uid-create"
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"), clusterConfigSingleton(uid))
	reconcileNode(t, r, "n1")

	policy := getBalloonsPolicy(t, r, "n1")
	if !ownsBalloonsClusterConfig(t, policy.OwnerReferences, uid) {
		t.Errorf("BalloonsPolicy ownerReferences = %+v, want an ownerRef to the BCC singleton (uid %s)",
			policy.OwnerReferences, uid)
	}
}

// TestNode_OwnerRefBackfilledOnUpdate: a CR created without the ownerRef gets it on
// resync even with no other change, so uninstall after upgrade does not orphan it.
func TestNode_OwnerRefBackfilledOnUpdate(t *testing.T) {
	const uid = "bcc-uid-backfill"
	// No singleton yet: the CR is created without an ownerRef.
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	reconcileNode(t, r, "n1")

	pre := getBalloonsPolicy(t, r, "n1")
	if len(pre.OwnerReferences) != 0 {
		t.Fatalf("precondition: BalloonsPolicy created with no singleton must have no ownerRef, got %+v", pre.OwnerReferences)
	}

	if err := r.Create(context.Background(), clusterConfigSingleton(uid)); err != nil {
		t.Fatalf("create singleton: %v", err)
	}
	reconcileNode(t, r, "n1")

	post := getBalloonsPolicy(t, r, "n1")
	if !ownsBalloonsClusterConfig(t, post.OwnerReferences, uid) {
		t.Errorf("BalloonsPolicy ownerReferences = %+v, want the backfilled ownerRef to the BCC singleton (uid %s)",
			post.OwnerReferences, uid)
	}
}

// TestNode_OwnerRefSurvivesSpecUpdate: the ownerRef survives a spec Update and is not duplicated.
func TestNode_OwnerRefSurvivesSpecUpdate(t *testing.T) {
	const uid = "bcc-uid-survive"
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"), clusterConfigSingleton(uid))
	reconcileNode(t, r, "n1")

	// Force a spec change: 4 NUMA re-renders the policy.
	r.LogsGetter = fakeLogsGetter{output: fourNumaProbe}
	// poolNode has no boot id, so nothing is cached and the new probe is used.
	reconcileNode(t, r, "n1")

	policy := getBalloonsPolicy(t, r, "n1")
	if !ownsBalloonsClusterConfig(t, policy.OwnerReferences, uid) {
		t.Errorf("BalloonsPolicy ownerReferences = %+v, want the ownerRef preserved across the spec update", policy.OwnerReferences)
	}

	// A steady-state resync must not add a second copy of the same ownerRef.
	reconcileNode(t, r, "n1")
	policy = getBalloonsPolicy(t, r, "n1")
	owners := 0
	for i := range policy.OwnerReferences {
		if string(policy.OwnerReferences[i].UID) == uid {
			owners++
		}
	}
	if owners != 1 {
		t.Errorf("ownerRef count for uid %s = %d, want exactly 1 (idempotent stamping)", uid, owners)
	}
}

// TestNode_NoSingletonNoOwnerRef: without the singleton the CR is created without an
// ownerRef instead of failing.
func TestNode_NoSingletonNoOwnerRef(t *testing.T) {
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	reconcileNode(t, r, "n1")

	policy := getBalloonsPolicy(t, r, "n1")
	for i := range policy.OwnerReferences {
		if policy.OwnerReferences[i].Kind == "BalloonsClusterConfig" {
			t.Errorf("BalloonsPolicy has a BCC ownerRef %+v, want none (singleton absent)", policy.OwnerReferences[i])
		}
	}
}

// TestNode_OwnerRefPreservesForeignOwnerRefs: backfilling appends and keeps foreign ownerRefs.
func TestNode_OwnerRefPreservesForeignOwnerRefs(t *testing.T) {
	const uid = "bcc-uid-foreign"
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	reconcileNode(t, r, "n1")

	// Stamp a foreign ownerRef on the created CR, then make the singleton exist.
	policy := getBalloonsPolicy(t, r, "n1")
	foreign := metav1.OwnerReference{
		APIVersion: "example.com/v1",
		Kind:       "Foo",
		Name:       "foo",
		UID:        types.UID("foreign-uid"),
	}
	policy.OwnerReferences = append(policy.OwnerReferences, foreign)
	if err := r.Update(context.Background(), policy); err != nil {
		t.Fatalf("seed foreign ownerRef: %v", err)
	}
	if err := r.Create(context.Background(), clusterConfigSingleton(uid)); err != nil {
		t.Fatalf("create singleton: %v", err)
	}

	reconcileNode(t, r, "n1")

	post := getBalloonsPolicy(t, r, "n1")
	if !ownsBalloonsClusterConfig(t, post.OwnerReferences, uid) {
		t.Errorf("BalloonsPolicy ownerReferences = %+v, want the backfilled BCC ownerRef", post.OwnerReferences)
	}
	foundForeign := false
	for i := range post.OwnerReferences {
		if string(post.OwnerReferences[i].UID) == "foreign-uid" {
			foundForeign = true
		}
	}
	if !foundForeign {
		t.Errorf("foreign ownerRef was clobbered; ownerReferences = %+v", post.OwnerReferences)
	}
}
