// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// fastProbeTiming shrinks the lifecycle timers for the test and restores them on cleanup.
func fastProbeTiming(t *testing.T, poll, deleteTimeout time.Duration) {
	t.Helper()
	origPoll, origDel := probePollInterval, probeDeleteTimeout
	probePollInterval, probeDeleteTimeout = poll, deleteTimeout
	t.Cleanup(func() { probePollInterval, probeDeleteTimeout = origPoll, origDel })
}

func lifecycleScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add clientgo scheme: %v", err)
	}
	return s
}

// terminatingProbePod builds a probe pod stuck terminating (deletionTimestamp plus a finalizer).
func terminatingProbePod(ns, podName string) *corev1.Pod {
	now := metav1.Now()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              podName,
			Namespace:         ns,
			DeletionTimestamp: &now,
			Finalizers:        []string{"cpu-pinning-operator.intel.com/test-hold"},
		},
	}
}

// TestDeleteProbePodAndWait_TimesOutWhenTerminating proves a stuck predecessor yields ErrProbePending.
func TestDeleteProbePodAndWait_TimesOutWhenTerminating(t *testing.T) {
	fastProbeTiming(t, 2*time.Millisecond, 20*time.Millisecond)
	const ns, podName = "kube-system", "cpu-topo-probe-node-a"

	c := fake.NewClientBuilder().
		WithScheme(lifecycleScheme(t)).
		WithObjects(terminatingProbePod(ns, podName)).
		Build()

	err := deleteProbePodAndWait(context.Background(), c, ns, podName)
	if err == nil {
		t.Fatal("deleteProbePodAndWait returned nil, want ErrProbePending for a stuck-terminating pod")
	}
	if !IsProbePending(err) {
		t.Fatalf("deleteProbePodAndWait err = %v, want IsProbePending", err)
	}
}

// TestDeleteProbePodAndWait_ReturnsWhenAbsent proves the wait returns nil once the pod is gone.
func TestDeleteProbePodAndWait_ReturnsWhenAbsent(t *testing.T) {
	fastProbeTiming(t, 2*time.Millisecond, 20*time.Millisecond)
	const ns, podName = "kube-system", "cpu-topo-probe-node-a"

	c := fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).Build()

	if err := deleteProbePodAndWait(context.Background(), c, ns, podName); err != nil {
		t.Fatalf("deleteProbePodAndWait on absent pod = %v, want nil", err)
	}
}

// TestDiscoverNode_CreateRacingTerminatingPod_ReturnsPending proves an AlreadyExists
// on create is the transient ErrProbePending.
func TestDiscoverNode_CreateRacingTerminatingPod_ReturnsPending(t *testing.T) {
	fastProbeTiming(t, 2*time.Millisecond, 20*time.Millisecond)
	const ns, podName, nodeName = "kube-system", "cpu-topo-probe-node-a", "node-a"
	t.Setenv("POD_NAMESPACE", ns)
	// Image set, so the racing create is the only reason discovery can fail here.
	t.Setenv("PROBE_IMAGE", testProbeImage)

	gr := schema.GroupResource{Resource: "pods"}
	c := fake.NewClientBuilder().
		WithScheme(lifecycleScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if pod, ok := obj.(*corev1.Pod); ok && pod.Name == podName {
					return apierrors.NewAlreadyExists(gr, podName)
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	_, err := DiscoverNode(context.Background(), c, fakeLogs{}, nodeName)
	if err == nil {
		t.Fatal("DiscoverNode returned nil, want ErrProbePending on a racing create")
	}
	if !IsProbePending(err) {
		t.Fatalf("DiscoverNode err = %v, want IsProbePending", err)
	}
}

// TestDiscoverNode_RefusesWhenProbeImageEmpty proves an empty PROBE_IMAGE is a hard
// failure before any pod is created.
func TestDiscoverNode_RefusesWhenProbeImageEmpty(t *testing.T) {
	const ns, nodeName = "kube-system", "node-a"
	t.Setenv("POD_NAMESPACE", ns)
	t.Setenv("PROBE_IMAGE", "")

	var creates int32
	c := fake.NewClientBuilder().
		WithScheme(lifecycleScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					atomic.AddInt32(&creates, 1)
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	topo, err := DiscoverNode(context.Background(), c, fakeLogs{}, nodeName)
	if err == nil {
		t.Fatal("DiscoverNode returned nil error for an empty PROBE_IMAGE, want a refusal")
	}
	if topo != nil {
		t.Errorf("topology = %+v, want nil: an unprobed node must read as unknown, not as an empty machine", topo)
	}
	if IsProbePending(err) {
		t.Error("the refusal must not report as transient: an unset image does not resolve itself on a retry")
	}
	// The message names the variable and the node.
	for _, want := range []string{"PROBE_IMAGE", nodeName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}
	if got := atomic.LoadInt32(&creates); got != 0 {
		t.Errorf("probe pod creates = %d, want 0: nothing may be scheduled without a configured image", got)
	}
}

// fakeLogs is a no-op LogsGetter.
type fakeLogs struct{}

func (fakeLogs) GetLogs(_ context.Context, _, _ string) (string, error) { return "", nil }
