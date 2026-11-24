// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
)

// cpuQ builds a CPU Quantity from milliCPU.
func cpuQ(milli int64) resource.Quantity {
	return *resource.NewMilliQuantity(milli, resource.DecimalSI)
}

// reservedPod builds a Running, non-pinned pod on nodeName; reqMilli 0 means no request.
func reservedPod(name, nodeName string, reqMilli ...int64) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: nodeName},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	for i, m := range reqMilli {
		c := corev1.Container{Name: name}
		if m > 0 {
			c.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: cpuQ(m)}
		}
		c.Name = name + "-c" + string(rune('0'+i))
		pod.Spec.Containers = append(pod.Spec.Containers, c)
	}
	return pod
}

// reservedUsageReconciler builds a NodeReconciler over a fake client with the
// pod-by-nodeName index the reconciler lists with.
func reservedUsageReconciler(t *testing.T, objs ...client.Object) *NodeReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&corev1.Pod{}, podNodeNameField, func(obj client.Object) []string {
			pod, ok := obj.(*corev1.Pod)
			if !ok || pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}).
		Build()
	return &NodeReconciler{
		Client:      c,
		Scheme:      scheme,
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: match.NewIndex(),
	}
}

// TestReservedUsageCPUs_ExcludesPinnedAndTerminal: sums non-pinned, non-terminal pod
// requests on the node, rounded up to whole CPUs.
func TestReservedUsageCPUs_ExcludesPinnedAndTerminal(t *testing.T) {
	// Counted: 1500m + 500m = 2000m → 2 CPUs.
	p1 := reservedPod("plain-a", "n1", 1500)
	p2 := reservedPod("plain-b", "n1", 500)

	// Pinned model pod: excluded even though it requests CPU.
	pinned := reservedPod("pinned", "n1", 4000)
	pinned.Annotations = map[string]string{pinnedByAnnotation: pinnedByValue}

	// Terminal pods: excluded regardless of request.
	succeeded := reservedPod("done", "n1", 8000)
	succeeded.Status.Phase = corev1.PodSucceeded
	failed := reservedPod("failed", "n1", 8000)
	failed.Status.Phase = corev1.PodFailed

	// A pod on another node: not counted for n1.
	other := reservedPod("elsewhere", "n2", 16000)

	r := reservedUsageReconciler(t, p1, p2, pinned, succeeded, failed, other)

	got, err := r.reservedUsageCPUs(context.Background(), "n1")
	if err != nil {
		t.Fatalf("reservedUsageCPUs: %v", err)
	}
	if got != 2 {
		t.Errorf("reservedUsageCPUs(n1) = %d, want 2 (1500m+500m non-pinned, pinned/terminal/other excluded)", got)
	}
}

// TestReservedUsageCPUs_RoundsUp: fractional demand rounds up to whole CPUs.
func TestReservedUsageCPUs_RoundsUp(t *testing.T) {
	// 250m + 300m = 550m → ceil = 1 CPU.
	p := reservedPod("frac", "n1", 250, 300)
	r := reservedUsageReconciler(t, p)

	got, err := r.reservedUsageCPUs(context.Background(), "n1")
	if err != nil {
		t.Fatalf("reservedUsageCPUs: %v", err)
	}
	if got != 1 {
		t.Errorf("reservedUsageCPUs(n1) = %d, want 1 (550m rounds up)", got)
	}
}

// TestReservedUsageCPUs_ColdStart: no non-pinned pods yields 0.
func TestReservedUsageCPUs_ColdStart(t *testing.T) {
	r := reservedUsageReconciler(t)
	got, err := r.reservedUsageCPUs(context.Background(), "n1")
	if err != nil {
		t.Fatalf("reservedUsageCPUs: %v", err)
	}
	if got != 0 {
		t.Errorf("reservedUsageCPUs(n1) = %d, want 0 (no non-pinned pods)", got)
	}
}

// TestPodCPURequestMilli_LimitFallback: a container with no request falls back to its limit, as the scheduler does.
func TestPodCPURequestMilli_LimitFallback(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "req",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: cpuQ(500)},
					},
				},
				{
					// No request → fall back to limit (750m).
					Name: "limit-only",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceCPU: cpuQ(750)},
					},
				},
			},
		},
	}
	if got := podCPURequestMilli(pod); got != 1250 {
		t.Errorf("podCPURequestMilli = %d, want 1250 (500m request + 750m limit fallback)", got)
	}
}

// TestPodCPURequestMilli_InitContainerMax: effective request is max(sum of regular
// requests, largest init request).
func TestPodCPURequestMilli_InitContainerMax(t *testing.T) {
	// Regular sum = 1000m; init 3000m dominates.
	dominantInit := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "init-a", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: cpuQ(3000)}}},
				{Name: "init-b", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: cpuQ(1000)}}},
			},
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: cpuQ(1000)}}},
			},
		},
	}
	if got := podCPURequestMilli(dominantInit); got != 3000 {
		t.Errorf("podCPURequestMilli = %d, want 3000 (largest init dominates regular sum)", got)
	}

	// Regular sum = 4000m > any init (2000m) → regular sum wins.
	dominantRegular := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "init", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: cpuQ(2000)}}},
			},
			Containers: []corev1.Container{
				{Name: "a", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: cpuQ(2500)}}},
				{Name: "b", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: cpuQ(1500)}}},
			},
		},
	}
	if got := podCPURequestMilli(dominantRegular); got != 4000 {
		t.Errorf("podCPURequestMilli = %d, want 4000 (regular sum dominates init)", got)
	}
}
