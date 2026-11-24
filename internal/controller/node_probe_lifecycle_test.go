// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nriv1alpha1 "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// bootedNode returns a pool node with a stable boot id for the skip-probe guard to key on.
func bootedNode(name, bootID string) *corev1.Node {
	n := poolNode(name)
	n.Status.NodeInfo.BootID = bootID
	return n
}

// countingProbeReconciler builds a NodeReconciler with a real TopoCache that counts
// probe-pod creates; probe pods succeed immediately.
func countingProbeReconciler(t *testing.T, output string, objs ...runtime.Object) (*NodeReconciler, *int32) {
	t.Helper()
	// Discovery refuses to probe without an image.
	t.Setenv("PROBE_IMAGE", testProbeImage)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	_ = nriv1alpha1.AddToScheme(scheme)

	var probeCreates int32
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithIndex(&corev1.Pod{}, podNodeNameField, func(obj client.Object) []string {
			pod, ok := obj.(*corev1.Pod)
			if !ok || pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if pod, ok := obj.(*corev1.Pod); ok && strings.HasPrefix(pod.Name, "cpu-topo-probe-") {
					atomic.AddInt32(&probeCreates, 1)
					pod.Status.Phase = corev1.PodSucceeded
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	r := &NodeReconciler{
		Client:      c,
		Scheme:      scheme,
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: match.NewIndex(),
		LogsGetter:  fakeLogsGetter{output: output},
		Recorder:    record.NewFakeRecorder(16),
		TopoCache:   topology.NewCache(),
	}
	return r, &probeCreates
}

// TestNode_SkipProbe_WhenTopologyCachedForBootID: reconciles under the same boot id
// reuse the cached topology instead of re-creating the probe pod, which leaks sandboxes.
func TestNode_SkipProbe_WhenTopologyCachedForBootID(t *testing.T) {
	r, probeCreates := countingProbeReconciler(t, twoNumaProbe, bootedNode("n1", "boot-1"))

	reconcileNode(t, r, "n1")
	if got := atomic.LoadInt32(probeCreates); got != 1 {
		t.Fatalf("probe creates after first reconcile = %d, want 1", got)
	}

	for i := 0; i < 5; i++ {
		reconcileNode(t, r, "n1")
	}
	if got := atomic.LoadInt32(probeCreates); got != 1 {
		t.Errorf("probe creates after config-only reconcile burst = %d, want 1 (probe skipped while boot id unchanged)", got)
	}
}

// TestNode_ReProbe_WhenBootIDChanges: a new boot id (reboot) forces a re-probe.
func TestNode_ReProbe_WhenBootIDChanges(t *testing.T) {
	r, probeCreates := countingProbeReconciler(t, twoNumaProbe, bootedNode("n1", "boot-1"))

	reconcileNode(t, r, "n1")
	if got := atomic.LoadInt32(probeCreates); got != 1 {
		t.Fatalf("probe creates after first reconcile = %d, want 1", got)
	}

	// Simulate a reboot.
	node := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, node); err != nil {
		t.Fatalf("get n1: %v", err)
	}
	node.Status.NodeInfo.BootID = "boot-2"
	if err := r.Status().Update(context.Background(), node); err != nil {
		t.Fatalf("update n1 status: %v", err)
	}

	reconcileNode(t, r, "n1")
	if got := atomic.LoadInt32(probeCreates); got != 2 {
		t.Errorf("probe creates after reboot (new boot id) = %d, want 2 (re-probe on topology-changing event)", got)
	}
}

// TestNode_NoBootID_AlwaysReProbes: a node with no boot id never serves a cached reading.
func TestNode_NoBootID_AlwaysReProbes(t *testing.T) {
	r, probeCreates := countingProbeReconciler(t, twoNumaProbe, poolNode("n1"))

	reconcileNode(t, r, "n1")
	reconcileNode(t, r, "n1")
	if got := atomic.LoadInt32(probeCreates); got != 2 {
		t.Errorf("probe creates with no boot id = %d, want 2 (unknown boot id → always re-probe)", got)
	}
}

// TestNode_ProbePending_RequeuesWithoutFailureEvent: while a same-named probe pod is
// still terminating, Reconcile requeues with backoff and emits no ProvisioningFailed warning.
func TestNode_ProbePending_RequeuesWithoutFailureEvent(t *testing.T) {
	// Image set, so only the racing create can fail discovery.
	t.Setenv("PROBE_IMAGE", testProbeImage)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	_ = nriv1alpha1.AddToScheme(scheme)

	gr := schema.GroupResource{Resource: "pods"}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(bootedNode("n1", "boot-1")).
		WithIndex(&corev1.Pod{}, podNodeNameField, func(obj client.Object) []string {
			pod, ok := obj.(*corev1.Pod)
			if !ok || pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}).
		WithInterceptorFuncs(interceptor.Funcs{
			// The API server rejects a create racing a terminating same-named pod.
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if pod, ok := obj.(*corev1.Pod); ok && strings.HasPrefix(pod.Name, "cpu-topo-probe-") {
					return apierrors.NewAlreadyExists(gr, pod.Name)
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	rec := record.NewFakeRecorder(16)
	r := &NodeReconciler{
		Client:      c,
		Scheme:      scheme,
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: match.NewIndex(),
		LogsGetter:  fakeLogsGetter{output: twoNumaProbe},
		Recorder:    rec,
		TopoCache:   topology.NewCache(),
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "n1"}})
	if err != nil {
		t.Fatalf("reconcile returned error %v, want nil (transient pending is not an error)", err)
	}
	if res.RequeueAfter != probePendingRequeue {
		t.Errorf("RequeueAfter = %v, want %v (backoff on transient probe-pending)", res.RequeueAfter, probePendingRequeue)
	}
	for _, ev := range drainNodeEvents(rec) {
		if strings.Contains(ev, reasonProvisioningFailed) {
			t.Errorf("emitted a ProvisioningFailed event on transient probe-pending: %q", ev)
		}
	}
}
