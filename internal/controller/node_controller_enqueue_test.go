// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
)

// recordingQueue records every Add / AddAfter the enqueue handler performs; it
// embeds the interface only to satisfy the type.
type recordingQueue struct {
	workqueue.TypedRateLimitingInterface[reconcile.Request]
	immediate []reconcile.Request
	delayed   []delayedAdd
}

type delayedAdd struct {
	req   reconcile.Request
	delay time.Duration
}

func (q *recordingQueue) Add(item reconcile.Request) {
	q.immediate = append(q.immediate, item)
}

func (q *recordingQueue) AddAfter(item reconcile.Request, d time.Duration) {
	q.delayed = append(q.delayed, delayedAdd{req: item, delay: d})
}

// enqueueTestReconciler builds a NodeReconciler over a fake client holding nodes.
func enqueueTestReconciler(t *testing.T, nodeCount int) *NodeReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	objs := make([]runtime.Object, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		objs = append(objs, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("n%d", i)}})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &NodeReconciler{
		Client:      c,
		Scheme:      scheme,
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: match.NewIndex(),
	}
}

// TestReconcileConcurrency_DefaultAndOverride: zero or negative falls back to the
// default; a positive value is used verbatim.
func TestReconcileConcurrency_DefaultAndOverride(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		want       int
	}{
		{"unset falls back to default", 0, defaultNodeReconcileConcurrency},
		{"negative falls back to default", -3, defaultNodeReconcileConcurrency},
		{"positive override honored", 12, 12},
		{"one honored", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &NodeReconciler{MaxConcurrentReconciles: tc.configured}
			if got := r.reconcileConcurrency(); got != tc.want {
				t.Fatalf("reconcileConcurrency() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestReconcileConcurrency_DefaultIsRaised guards against regressing to the serial default of 1.
func TestReconcileConcurrency_DefaultIsRaised(t *testing.T) {
	if defaultNodeReconcileConcurrency <= 1 {
		t.Fatalf("defaultNodeReconcileConcurrency = %d, want > 1 (a serial default reconciles a large fleet one node at a time)", defaultNodeReconcileConcurrency)
	}
}

// TestEnqueueAllNodes_SmallFleetImmediate: a fleet within one chunk is enqueued
// immediately, each node once.
func TestEnqueueAllNodes_SmallFleetImmediate(t *testing.T) {
	r := enqueueTestReconciler(t, enqueueBurstChunkSize) // exactly one chunk
	q := &recordingQueue{}

	r.enqueueAllNodes(context.Background(), q)

	if len(q.delayed) != 0 {
		t.Fatalf("expected no delayed adds for a single-chunk fleet, got %d", len(q.delayed))
	}
	if len(q.immediate) != enqueueBurstChunkSize {
		t.Fatalf("immediate adds = %d, want %d", len(q.immediate), enqueueBurstChunkSize)
	}
}

// TestEnqueueAllNodes_LargeFleetChunked: the first chunk is immediate and each
// later one is delayed one more enqueueBurstChunkDelay; every node is enqueued once.
func TestEnqueueAllNodes_LargeFleetChunked(t *testing.T) {
	const nodes = 45 // 5 chunks: 10,10,10,10,5
	r := enqueueTestReconciler(t, nodes)
	q := &recordingQueue{}

	r.enqueueAllNodes(context.Background(), q)

	if len(q.immediate) != enqueueBurstChunkSize {
		t.Fatalf("immediate adds = %d, want %d (first chunk)", len(q.immediate), enqueueBurstChunkSize)
	}
	if want := nodes - enqueueBurstChunkSize; len(q.delayed) != want {
		t.Fatalf("delayed adds = %d, want %d", len(q.delayed), want)
	}

	// Every request across immediate + delayed must be unique and cover all nodes.
	seen := map[string]bool{}
	for _, req := range q.immediate {
		seen[req.Name] = true
	}
	// Delayed chunks must carry strictly increasing per-chunk delays.
	for i, d := range q.delayed {
		wantChunk := (enqueueBurstChunkSize + i) / enqueueBurstChunkSize
		wantDelay := time.Duration(wantChunk) * enqueueBurstChunkDelay
		if d.delay != wantDelay {
			t.Fatalf("delayed[%d] delay = %s, want %s", i, d.delay, wantDelay)
		}
		if d.delay <= 0 {
			t.Fatalf("delayed[%d] must have a positive delay, got %s", i, d.delay)
		}
		if seen[d.req.Name] {
			t.Fatalf("node %q enqueued more than once", d.req.Name)
		}
		seen[d.req.Name] = true
	}
	if len(seen) != nodes {
		t.Fatalf("covered %d unique nodes, want %d", len(seen), nodes)
	}
}

// TestEnqueueAllNodes_ListErrorEnqueuesNothing: a failed List enqueues nothing
// rather than applying a config change partially.
func TestEnqueueAllNodes_ListErrorEnqueuesNothing(t *testing.T) {
	// A reconciler with no scheme registered for Node makes List fail.
	r := &NodeReconciler{
		Client:      fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build(),
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: match.NewIndex(),
	}
	q := &recordingQueue{}

	r.enqueueAllNodes(context.Background(), q)

	if len(q.immediate)+len(q.delayed) != 0 {
		t.Fatalf("expected no enqueues on List error, got immediate=%d delayed=%d", len(q.immediate), len(q.delayed))
	}
}

// TestEnqueueAllNodes_AdoptsUnprovisionedNodes: a config change also re-enqueues
// nodes not yet provisioned.
func TestEnqueueAllNodes_AdoptsUnprovisionedNodes(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	provisioned := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "provisioned",
		Labels: map[string]string{LabelBalloonsEnabled: "true"},
	}}
	fresh := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fresh"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(provisioned, fresh).Build()
	r := &NodeReconciler{
		Client:      c,
		Scheme:      scheme,
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: match.NewIndex(),
	}
	q := &recordingQueue{}

	r.enqueueAllNodes(context.Background(), q)

	enqueued := map[string]bool{}
	for _, req := range q.immediate {
		enqueued[req.Name] = true
	}
	for _, d := range q.delayed {
		enqueued[d.req.Name] = true
	}
	for _, want := range []string{"provisioned", "fresh"} {
		if !enqueued[want] {
			t.Errorf("node %q was not enqueued; enqueued = %v", want, enqueued)
		}
	}
}
