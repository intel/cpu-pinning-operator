// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/intel/cpu-pinning-operator/internal/metrics"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// admitColdStartDryRun runs a dry-run CREATE admission for a whole-core "vllm" pod.
func admitColdStartDryRun(t *testing.T, m *PodMutator, name, cores, tp string) admission.Response {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "inference",
			Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": tp},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "vllm",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cores)},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cores)},
			},
		}}},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	dry := true
	return m.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}),
		Operation: admissionv1.Create,
		Namespace: pod.Namespace,
		DryRun:    &dry,
		Object:    runtime.RawExtension{Raw: raw},
	}})
}

// coldStartMutator builds a Handle-wired mutator with an empty topology cache and no
// NRT CRs, so admission always takes the static fallback; the non-nil TopoCache
// also wires a ReservationStore.
func coldStartMutator(t *testing.T, mode string, nodes ...runtime.Object) (*PodMutator, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	return newHandleMutatorFromClient(t, mode, c, topology.NewCache())
}

// TestColdStartEnforceSequentialReserves proves the static fallback reserves: two
// span-2 6-core pods each fit 4 cores/NUMA alone, so the second must be denied.
func TestColdStartEnforceSequentialReserves(t *testing.T) {
	m, rec := coldStartMutator(t, "enforce", topoNode("n1", 8, 2))

	first := admitPod(t, m, "vllm-a", "6", "2")
	if !first.Allowed {
		t.Fatalf("first cold-start pod must be admitted (fits statically), got denied: %q", first.Result.Message)
	}
	second := admitPod(t, m, "vllm-b", "6", "2")
	if second.Allowed {
		t.Fatalf("second cold-start pod must be DENIED — the first's reservation leaves only 1 core/NUMA, request needs 3; got Allowed (over-admission race)")
	}
	events := drainEvents(rec)
	if !hasEventContaining(events, "CapacityCheckFailed") {
		t.Errorf("expected a CapacityCheckFailed event on the denied second pod, got %v", events)
	}
}

// TestColdStartEnforceConcurrentNotBothAdmitted proves exactly one of two concurrent
// cold-start admissions passes.
func TestColdStartEnforceConcurrentNotBothAdmitted(t *testing.T) {
	m, _ := coldStartMutator(t, "enforce", topoNode("n1", 8, 2))

	var wg sync.WaitGroup
	resps := make([]admission.Response, 2)
	names := []string{"vllm-a", "vllm-b"}
	wg.Add(2)
	for i := range names {
		go func(i int) {
			defer wg.Done()
			resps[i] = admitPod(t, m, names[i], "6", "2")
		}(i)
	}
	wg.Wait()

	admitted := 0
	for _, r := range resps {
		if r.Allowed {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("exactly one concurrent cold-start admission must pass (the other sees the reservation); got %d admitted", admitted)
	}
}

// TestColdStartWarnAdmitsBoth proves warn mode records no reservation and admits both.
func TestColdStartWarnAdmitsBoth(t *testing.T) {
	m, _ := coldStartMutator(t, "warn", topoNode("n1", 8, 2))

	first := admitPod(t, m, "vllm-a", "6", "2")
	second := admitPod(t, m, "vllm-b", "6", "2")
	if !first.Allowed || !second.Allowed {
		t.Fatalf("warn mode must admit both cold-start pods (static formula fits, no reservation tracking): a=%v b=%v", first.Allowed, second.Allowed)
	}
}

// TestColdStartOffAdmitsBoth proves off mode admits both with no reservation or event.
func TestColdStartOffAdmitsBoth(t *testing.T) {
	m, rec := coldStartMutator(t, "off", topoNode("n1", 8, 2))

	first := admitPod(t, m, "vllm-a", "6", "2")
	second := admitPod(t, m, "vllm-b", "6", "2")
	if !first.Allowed || !second.Allowed {
		t.Fatalf("off mode must admit both cold-start pods: a=%v b=%v", first.Allowed, second.Allowed)
	}
	events := drainEvents(rec)
	if hasEventContaining(events, "CapacityCheckFailed") || hasEventContaining(events, "CapacityCheckWarning") {
		t.Errorf("off mode must emit no capacity-check event, got %v", events)
	}
}

// TestColdStartEnforceDryRunNoReservation proves cold-start dry-runs record no
// reservation.
func TestColdStartEnforceDryRunNoReservation(t *testing.T) {
	m, _ := coldStartMutator(t, "enforce", topoNode("n1", 8, 2))

	first := admitColdStartDryRun(t, m, "vllm-a", "6", "2")
	second := admitColdStartDryRun(t, m, "vllm-b", "6", "2")
	if !first.Allowed || !second.Allowed {
		t.Fatalf("dry-run cold-start admissions must not reserve, so both stay admitted: a=%v b=%v", first.Allowed, second.Allowed)
	}
	// A real admission still fits, so the dry-runs reserved nothing.
	real := admitPod(t, m, "vllm-c", "6", "2")
	if !real.Allowed {
		t.Fatalf("a real admission after two dry-runs must fit (dry-runs left no reservation), got denied: %q", real.Result.Message)
	}
}

// TestColdStartEnforceMemoryReserves proves the static fallback reserves memory
// too: the floor fits one span-2 pod's share, not two.
func TestColdStartEnforceMemoryReserves(t *testing.T) {
	// 40Gi/NUMA floor; a 60Gi span-2 pod needs 30Gi/NUMA, so only one fits.
	node := withMemoryFloor(topoNode("n1", 16, 2), 40*gib)
	m, rec := coldStartMutator(t, "enforce", node)

	first := admitPodMem(t, m, "vllm-a", "4", "2", "60Gi")
	if !first.Allowed {
		t.Fatalf("first cold-start pod must fit memory (30Gi/NUMA <= 40Gi floor), got denied: %q", first.Result.Message)
	}
	second := admitPodMem(t, m, "vllm-b", "4", "2", "60Gi")
	if second.Allowed {
		t.Fatalf("second cold-start pod must be DENIED on memory — the first reserved 30Gi/NUMA leaving 10Gi, request needs 30Gi; got Allowed")
	}
	events := drainEvents(rec)
	if !hasEventContaining(events, "CapacityCheckFailed") {
		t.Errorf("expected a CapacityCheckFailed event on the memory-denied second pod, got %v", events)
	}
}

// TestColdStartEnforceUnknownCandidateFallsBackNotDeny proves the fallback does not
// deny when an unlabeled candidate of unknown capacity exists: it defers to
// capacityFits, which treats unknown topology as "don't block".
func TestColdStartEnforceUnknownCandidateFallsBackNotDeny(t *testing.T) {
	// n1: 4 cores/NUMA; n2: no topology labels, so unknown capacity.
	n1 := topoNode("n1", 8, 2)
	n2 := withoutTopologyLabels(topoNode("n2", 8, 2))
	m, _ := coldStartMutator(t, "enforce", n1, n2)

	first := admitPod(t, m, "vllm-a", "6", "2")
	if !first.Allowed {
		t.Fatalf("first cold-start pod must be admitted (fits n1 statically), got denied: %q", first.Result.Message)
	}
	// n1 no longer fits; n2's unknown capacity must admit.
	second := admitPod(t, m, "vllm-b", "6", "2")
	if !second.Allowed {
		t.Fatalf("second cold-start pod must be ADMITTED via read-only fallback (an unlabeled, span-eligible candidate exists — never deny on partial information), got denied: %q", second.Result.Message)
	}
}

// TestColdStartReservationsRecordedMetricIncremented proves the static path
// increments reservations_recorded_total. Asserted directly: the admit/deny outcome
// is decided before the Inc().
func TestColdStartReservationsRecordedMetricIncremented(t *testing.T) {
	// Process-wide CounterVec: isolate on a node label no other test uses.
	const node = "cold-start-metric-node"
	metrics.ReservationsRecorded.DeleteLabelValues(node)
	t.Cleanup(func() { metrics.ReservationsRecorded.DeleteLabelValues(node) })

	m, _ := coldStartMutator(t, "enforce", topoNode(node, 8, 2))
	before := testutil.ToFloat64(metrics.ReservationsRecorded.WithLabelValues(node))

	// Dry run must not move the counter; run it while the node has room so a denial
	// cannot mask that.
	if dry := admitColdStartDryRun(t, m, "vllm-dry", "6", "2"); !dry.Allowed {
		t.Fatalf("dry-run cold-start pod must be admitted, got denied: %q", dry.Result.Message)
	}
	if got := testutil.ToFloat64(metrics.ReservationsRecorded.WithLabelValues(node)); got != before {
		t.Fatalf("reservations_recorded_total{node=%s} = %v after dry-run, want %v unchanged (sideEffects: None)", node, got, before)
	}

	if first := admitPod(t, m, "vllm-a", "6", "2"); !first.Allowed {
		t.Fatalf("first cold-start pod must be admitted, got denied: %q", first.Result.Message)
	}
	if got := testutil.ToFloat64(metrics.ReservationsRecorded.WithLabelValues(node)); got != before+1 {
		t.Fatalf("reservations_recorded_total{node=%s} = %v, want %v — admitStaticHeadroom must count the reservation it commits", node, got, before+1)
	}
}

// TestAdmitWithHeadroomStaticMemFallbackReservation proves the live path's static
// memory fallback (live CPU, no live memory) subtracts outstanding reservations
// from the floor.
func TestAdmitWithHeadroomStaticMemFallbackReservation(t *testing.T) {
	topo := topology.NewCache()
	// Live CPU only, so memory falls back to the floor. 4 cores/NUMA (from the topology
	// cache) so the second pod's CPU share still fits and only memory can deny it.
	topo.Set("n1", fourCorePerNUMATopo()) // 8 CPUs, 2 NUMA, 4 cores each
	// 40Gi/NUMA floor; a 60Gi span-2 pod needs 30Gi/NUMA, so only one fits.
	node := withMemoryFloor(topoNode("n1", 16, 2), 40*gib)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	// Empty zones: no live memory data.
	nrt := nrtObj("n1") // empty, so no occupancy data
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(node, nrt).Build()
	m, rec := newHandleMutatorFromClient(t, "enforce", c, topo)

	first := admitPodMem(t, m, "vllm-a", "4", "2", "60Gi")
	if !first.Allowed {
		t.Fatalf("first pod must fit static memory floor (30Gi/NUMA <= 40Gi/NUMA floor), got denied: %q", first.Result.Message)
	}
	// The first reservation leaves 10Gi/NUMA, so this must be denied.
	second := admitPodMem(t, m, "vllm-b", "4", "2", "60Gi")
	if second.Allowed {
		t.Fatalf("second pod must be DENIED — static fallback must account for first's 30Gi/NUMA reservation, leaving only 10Gi/NUMA free for the 30Gi/NUMA request")
	}
	// Must be denied on memory; a CPU denial would prove nothing here.
	if !strings.Contains(second.Result.Message, "memory request") {
		t.Fatalf("denial must come from the static memory fallback, got %q — a non-memory denial means the test is no longer exercising the static memory fallback", second.Result.Message)
	}
	events := drainEvents(rec)
	if !hasEventContaining(events, "CapacityCheckFailed") {
		t.Errorf("expected a CapacityCheckFailed event on the denied second pod, got %v", events)
	}
}
