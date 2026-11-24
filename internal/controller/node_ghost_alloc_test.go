// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/intel/cpu-pinning-operator/internal/metrics"
)

// ghostNode is the Node the detection pass runs against; its name is the NRT CR name.
func ghostNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
}

// livePod builds a pod, so a zone referencing it is not a ghost.
func livePod(namespace, name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
}

// containerZone builds an NRT container zone bound to a workload balloon.
func containerZone(zoneName, balloon, cpuset string) map[string]interface{} {
	return nrtZone(zoneName, balloon, cpuset)
}

// ghostReconciler wires a NodeReconciler serving the unstructured NRT CR, with a
// buffered recorder for event assertions.
func ghostReconciler(t *testing.T, objs ...runtime.Object) (*NodeReconciler, *record.FakeRecorder) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(driftScheme()).WithRuntimeObjects(objs...).Build()
	rec := record.NewFakeRecorder(8)
	return &NodeReconciler{
		Client:   c,
		Scheme:   driftScheme(),
		Log:      ctrl.Log.WithName("test"),
		Recorder: rec,
	}, rec
}

func drainEvents(rec *record.FakeRecorder) []string {
	var got []string
	for {
		select {
		case e := <-rec.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

// TestDetectGhostAllocations_GhostAndLive: only the deleted pod's zone is a ghost,
// and one Warning names it.
func TestDetectGhostAllocations_GhostAndLive(t *testing.T) {
	metrics.GhostAllocations.Reset()
	const node = "n1"
	live := livePod("inference", "live-pod")
	nrt := nrtObj(
		containerZone("inference/live-pod/vllm", "balloon-span1[0]", "3-18"),
		containerZone("inference/ghost-pod/vllm", "balloon-span1[0]", "19-34"),
	)
	r, rec := ghostReconciler(t, live, nrt)
	r.detectGhostAllocations(context.Background(), ghostNode())

	if got := testutil.ToFloat64(metrics.GhostAllocations.WithLabelValues(node)); got != 1 {
		t.Fatalf("ghost_allocations = %v, want 1 (one ghost zone, one live)", got)
	}
	events := drainEvents(rec)
	if len(events) != 1 {
		t.Fatalf("emitted %d events, want exactly 1 aggregated Warning: %v", len(events), events)
	}
	e := events[0]
	if !strings.Contains(e, reasonGhostBalloonAllocation) || !strings.Contains(e, "Warning") {
		t.Errorf("event = %q, want a Warning with reason %q", e, reasonGhostBalloonAllocation)
	}
	if !strings.Contains(e, "inference/ghost-pod/vllm") {
		t.Errorf("event = %q, want it to name the ghost zone", e)
	}
	if strings.Contains(e, "live-pod") {
		t.Errorf("event = %q, must NOT name the live pod's zone", e)
	}
}

// TestDetectGhostAllocations_Clean: all zones live reports 0 and no event.
func TestDetectGhostAllocations_Clean(t *testing.T) {
	metrics.GhostAllocations.Reset()
	const node = "n1"
	nrt := nrtObj(
		containerZone("inference/a/vllm", "balloon-span1[0]", "3-18"),
		containerZone("inference/b/vllm", "balloon-span2[0]", "19-34"),
	)
	r, rec := ghostReconciler(t, livePod("inference", "a"), livePod("inference", "b"), nrt)
	r.detectGhostAllocations(context.Background(), ghostNode())

	if got := testutil.ToFloat64(metrics.GhostAllocations.WithLabelValues(node)); got != 0 {
		t.Errorf("ghost_allocations = %v, want 0 (all zones live)", got)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("emitted %d events, want 0 on a clean node: %v", len(events), events)
	}
}

// TestDetectGhostAllocations_DuplicateZonesCounted: three leaked zones for one deleted
// pod all count, with one aggregated Warning.
func TestDetectGhostAllocations_DuplicateZonesCounted(t *testing.T) {
	metrics.GhostAllocations.Reset()
	const node = "n1"
	nrt := nrtObj(
		containerZone("inference/llama/kserve", "balloon-span1[0]", "3-18"),
		containerZone("inference/llama/kserve", "balloon-span1[0]", "19-34"),
		containerZone("inference/llama/kserve", "balloon-span1[0]", "35-50"),
	)
	// No pod seeded → llama is deleted → all three zones are ghosts.
	r, rec := ghostReconciler(t, nrt)
	r.detectGhostAllocations(context.Background(), ghostNode())

	if got := testutil.ToFloat64(metrics.GhostAllocations.WithLabelValues(node)); got != 3 {
		t.Errorf("ghost_allocations = %v, want 3 (three leaked zones for one deleted pod)", got)
	}
	if events := drainEvents(rec); len(events) != 1 {
		t.Errorf("emitted %d events, want exactly 1 aggregated Warning: %v", len(events), events)
	}
}

// TestDetectGhostAllocations_ReservedZonesIgnored: reserved-pool subzones are never ghosts.
func TestDetectGhostAllocations_ReservedZonesIgnored(t *testing.T) {
	metrics.GhostAllocations.Reset()
	const node = "n1"
	nrt := nrtObj(
		containerZone("kube-system/some-daemon/c", "reserved[0]", "0-2"),
	)
	r, rec := ghostReconciler(t, nrt)
	r.detectGhostAllocations(context.Background(), ghostNode())

	if got := testutil.ToFloat64(metrics.GhostAllocations.WithLabelValues(node)); got != 0 {
		t.Errorf("ghost_allocations = %v, want 0 (reserved subzones are not workload allocations)", got)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("emitted %d events, want 0: %v", len(events), events)
	}
}

// TestDetectGhostAllocations_NRTNotFound: a missing NRT is unknown, so no series and no event.
func TestDetectGhostAllocations_NRTNotFound(t *testing.T) {
	metrics.GhostAllocations.Reset()
	r, rec := ghostReconciler(t) // no NRT seeded
	r.detectGhostAllocations(context.Background(), ghostNode())

	if n := testutil.CollectAndCount(metrics.GhostAllocations); n != 0 {
		t.Errorf("ghost_allocations series count = %d, want 0 (absent NRT → unknown, gauge untouched)", n)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("emitted %d events, want 0 when NRT is absent: %v", len(events), events)
	}
}

// TestDetectGhostAllocations_UnknownPassClearsStaleSeries: an unknown pass deletes
// the series an earlier pass set, so a stale count cannot drive alerts.
func TestDetectGhostAllocations_UnknownPassClearsStaleSeries(t *testing.T) {
	metrics.GhostAllocations.Reset()
	const node = "n1"

	nrt := nrtObj(containerZone("inference/ghost/vllm", "balloon-span1[0]", "3-18"))
	r, _ := ghostReconciler(t, nrt)
	r.detectGhostAllocations(context.Background(), ghostNode())
	if got := testutil.ToFloat64(metrics.GhostAllocations.WithLabelValues(node)); got != 1 {
		t.Fatalf("setup: ghost_allocations = %v, want 1", got)
	}

	// Pass 2: NRT absent, so the series must be dropped.
	rEmpty, _ := ghostReconciler(t) // no NRT seeded
	rEmpty.detectGhostAllocations(context.Background(), ghostNode())
	if n := testutil.CollectAndCount(metrics.GhostAllocations); n != 0 {
		t.Errorf("ghost_allocations series count = %d, want 0 (unknown pass must delete the stale series)", n)
	}
}

// TestDetectGhostAllocations_NilRecorderNoPanic: the gauge is still set with no Recorder.
func TestDetectGhostAllocations_NilRecorderNoPanic(t *testing.T) {
	metrics.GhostAllocations.Reset()
	const node = "n1"
	nrt := nrtObj(containerZone("inference/ghost/vllm", "balloon-span1[0]", "3-18"))
	c := fake.NewClientBuilder().WithScheme(driftScheme()).WithRuntimeObjects(nrt).Build()
	r := &NodeReconciler{Client: c, Scheme: driftScheme(), Log: ctrl.Log.WithName("test")} // Recorder nil

	r.detectGhostAllocations(context.Background(), ghostNode())

	if got := testutil.ToFloat64(metrics.GhostAllocations.WithLabelValues(node)); got != 1 {
		t.Errorf("ghost_allocations = %v, want 1 even without a Recorder", got)
	}
}
