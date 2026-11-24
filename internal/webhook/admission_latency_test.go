// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	admissionv1 "k8s.io/api/admission/v1"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// latencyBudget sits well under the webhook's 10s timeout; past that, failurePolicy:
// Ignore admits pods unmutated.
const latencyBudget = 5 * time.Second

// latencyFleetNodes is the synthetic fleet size for the latency test and benchmark.
const latencyFleetNodes = 500

// buildLatencyFleet wires a PodMutator over n balloons-enabled nodes with labels, NRT CRs
// and topology cache: the heaviest unscheduled-pod admission path.
func buildLatencyFleet(t testing.TB, n int) *PodMutator {
	t.Helper()
	const floor = int64(256) << 30 // 256 GiB per NUMA — roomy enough that the pod always fits
	topo := topology.NewCache()
	objs := make([]runtime.Object, 0, 2*n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("node-%03d", i)

		objs = append(objs, withMemoryFloor(topoNode(name, 4, 2), floor))
		objs = append(objs, nrtObj(name)) // empty zones → all CPUs free
		topo.Set(name, simpleTopo())
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	nrtGVK := nrtObj("x").GroupVersionKind()
	scheme.AddKnownTypeWithName(nrtGVK, &unstructured.Unstructured{})
	listGVK := nrtGVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

	// Inlined from newHandleMutatorFromClient, which takes *testing.T, not testing.TB.
	idx := match.NewIndex()
	rules, err := match.CompileIntent(cpopv1alpha1.IntentSpec{
		Span: cpopv1alpha1.IntentSourceSpec{Annotation: "cpu-pinning-operator.intel.com/tp", Default: 1},
	})
	if err != nil {
		t.Fatalf("compile intent: %v", err)
	}
	idx.Update([]match.PolicyRule{{
		Namespaces:        []string{"inference"},
		ByContainerName:   []string{"vllm"},
		EnforcePinning:    true,
		Intent:            rules,
		CapacityCheckMode: "enforce",
	}})

	// Recorder nil: a FakeRecorder blocks once its buffer fills.
	m := &PodMutator{
		Client:       c,
		PolicyIndex:  idx,
		TopoCache:    topo,
		Reservations: NewReservationStore(0),
	}
	if err := m.InjectDecoder(admission.NewDecoder(scheme)); err != nil {
		t.Fatalf("inject decoder: %v", err)
	}
	return m
}

// buildLatencyRequest builds an unscheduled CREATE for a whole-core "vllm" pod with a
// memory request, uniquely named so reservations accumulate.
func buildLatencyRequest(tb testing.TB, name string) admission.Request {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "inference",
			Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": "2"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "vllm",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
				},
			},
		}}},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		tb.Fatalf("marshal pod: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}),
		Operation: admissionv1.Create,
		Namespace: pod.Namespace,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

// admitLatencyPod runs one admission and returns the response and Handle's wall time.
func admitLatencyPod(tb testing.TB, m *PodMutator, name string) (admission.Response, time.Duration) {
	req := buildLatencyRequest(tb, name)
	start := time.Now()
	resp := m.Handle(context.Background(), req)
	return resp, time.Since(start)
}

// TestAdmissionLatencyP99: p99 of unscheduled admissions on a 500-node fleet stays under latencyBudget.
func TestAdmissionLatencyP99(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 500-node admission latency test in -short mode")
	}
	m := buildLatencyFleet(t, latencyFleetNodes)

	const iterations = 100
	durations := make([]time.Duration, 0, iterations)
	pinned := 0
	for i := 0; i < iterations; i++ {
		resp, d := admitLatencyPod(t, m, fmt.Sprintf("vllm-%04d", i))
		if !resp.Allowed {
			t.Fatalf("admission %d denied on a fleet with ample headroom: %q", i, resp.Result.Message)
		}
		if stampedBalloon(resp) != "" {
			pinned++
		}
		durations = append(durations, d)
	}

	// Every sample must pin, or cheap early returns dilute the percentile.
	if pinned != iterations {
		t.Fatalf("only %d/%d admissions stamped a balloon — some took a cheap early-return, latency is not representative", pinned, iterations)
	}

	p99 := percentile(durations, 0.99)
	t.Logf("admission latency over %d admissions on a %d-node fleet: p50=%s p99=%s max=%s (pinned=%d)",
		iterations, latencyFleetNodes, percentile(durations, 0.50), p99, percentile(durations, 1.0), pinned)
	if p99 > latencyBudget {
		t.Errorf("admission p99 %s exceeds the %s budget (10s webhook timeout under failurePolicy: Ignore)", p99, latencyBudget)
	}
}

// percentile returns the nearest-rank q-quantile (rank ceil(q*N)); empty input yields 0.
func percentile(durations []time.Duration, q float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(math.Ceil(q * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// BenchmarkAdmission profiles the unscheduled admission path on the 500-node fleet.
func BenchmarkAdmission(b *testing.B) {
	m := buildLatencyFleet(b, latencyFleetNodes)
	// Advance past the TTL each iteration so reservations expire instead of exhausting capacity.
	clk := time.Now()
	m.Reservations.now = func() time.Time { return clk }
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Keep request building out of the timed section.
		b.StopTimer()
		clk = clk.Add(DefaultReservationTTL + time.Second)
		req := buildLatencyRequest(b, fmt.Sprintf("vllm-%08d", i))
		b.StartTimer()

		resp := m.Handle(ctx, req)

		b.StopTimer()
		if !resp.Allowed || stampedBalloon(resp) == "" {
			b.Fatalf("admission %d not pinned (allowed=%v): %q", i, resp.Allowed, resp.Result.Message)
		}
	}
}
