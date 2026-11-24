// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package unlabel

import (
	"context"
	"io"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/intel/cpu-pinning-operator/internal/controller"
)

func newClient(objs ...runtime.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
}

func node(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// TestStripsManagedLabelsHeterogeneousFleet proves only managed labels are stripped,
// foreign labels survive, and a node with none is not patched.
func TestStripsManagedLabelsHeterogeneousFleet(t *testing.T) {
	n1 := node("n1", map[string]string{
		controller.LabelBalloonsEnabled: "true",
		controller.LabelMaxSpan:         "4",
		controller.LabelNumaNodes:       "2",
		controller.LabelPhysicalCPUs:    "64",
		controller.LabelThreadsPerCore:  "2",
		controller.LabelReservedCPUs:    "12",
		controller.LabelCPUAllocation:   "physical-cores",
	})
	n2 := node("n2", map[string]string{
		controller.LabelBalloonsEnabled: "true",
		controller.LabelMaxSpan:         "2",
		"kubernetes.io/arch":            "amd64", // foreign: must survive
	})
	n3 := node("n3", map[string]string{
		"node-role.kubernetes.io/control-plane": "", // never provisioned
	})

	c := newClient(n1, n2, n3)
	patched, err := StripManagedLabels(context.Background(), c, io.Discard)
	if err != nil {
		t.Fatalf("StripManagedLabels: %v", err)
	}

	if patched != 2 {
		t.Errorf("patched %d nodes, want 2 (n1, n2)", patched)
	}

	ctx := context.Background()

	for _, name := range []string{"n1", "n2"} {
		got := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, got); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		for _, l := range controller.ManagedNodeLabels {
			if _, ok := got.Labels[l]; ok {
				t.Errorf("node %s still carries managed label %q after sweep", name, l)
			}
		}
	}

	got := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: "n2"}, got); err != nil {
		t.Fatalf("get n2: %v", err)
	}
	if got.Labels["kubernetes.io/arch"] != "amd64" {
		t.Errorf("n2 lost its foreign label; labels = %v", got.Labels)
	}

	if err := c.Get(ctx, client.ObjectKey{Name: "n3"}, got); err != nil {
		t.Fatalf("get n3: %v", err)
	}
	if _, ok := got.Labels["node-role.kubernetes.io/control-plane"]; !ok {
		t.Errorf("n3 lost its unrelated label; labels = %v", got.Labels)
	}
}

// TestIdempotentOnUnlabelledFleet proves a sweep with nothing to strip is a clean no-op.
func TestIdempotentOnUnlabelledFleet(t *testing.T) {
	c := newClient(
		node("n1", map[string]string{"kubernetes.io/os": "linux"}),
		node("n2", nil),
	)
	patched, err := StripManagedLabels(context.Background(), c, io.Discard)
	if err != nil {
		t.Fatalf("StripManagedLabels: %v", err)
	}
	if patched != 0 {
		t.Errorf("patched %d nodes with no managed labels, want 0", patched)
	}
}
