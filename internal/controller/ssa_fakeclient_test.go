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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestFakeClientSupportsServerSideApply proves the fake client merges SSA applies
// from two field managers, so SSA reconcile paths can be unit-tested.
func TestFakeClientSupportsServerSideApply(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	// TypeMeta is required: the fake client keys SSA on the object's GVK.
	first := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ssa-probe",
			Namespace: "default",
		},
		Data: map[string]string{"owned-by-first": "1"},
	}
	if err := c.Patch(ctx, first, client.Apply, client.FieldOwner("first-manager")); err != nil {
		t.Fatalf("server-side apply (create) failed — fake client lacks SSA support: %v", err)
	}

	// SSA merges by field manager, so both keys must survive.
	second := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ssa-probe",
			Namespace: "default",
		},
		Data: map[string]string{"owned-by-second": "1"},
	}
	if err := c.Patch(ctx, second, client.Apply, client.FieldOwner("second-manager"), client.ForceOwnership); err != nil {
		t.Fatalf("server-side apply (merge) failed: %v", err)
	}

	got := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Name: "ssa-probe", Namespace: "default"}, got); err != nil {
		t.Fatalf("get after apply: %v", err)
	}
	if got.Data["owned-by-first"] != "1" {
		t.Errorf("first manager's field was lost after the second apply: data=%v", got.Data)
	}
	if got.Data["owned-by-second"] != "1" {
		t.Errorf("second manager's field missing: data=%v", got.Data)
	}
}
