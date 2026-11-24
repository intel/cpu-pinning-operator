// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/intel/cpu-pinning-operator/internal/metrics"
	"github.com/intel/cpu-pinning-operator/internal/readiness"
	"github.com/prometheus/client_golang/prometheus/testutil"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// admitScheduledPod runs a CREATE admission for a whole-core vllm pod on node n1.
func admitScheduledPod(t *testing.T, m *PodMutator) admission.Response {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "vllm-ok",
			Namespace:   "inference",
			Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": "1"},
		},
		Spec: corev1.PodSpec{
			NodeName: "n1",
			Containers: []corev1.Container{{
				Name: "vllm",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				},
			}},
		},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	return m.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}),
		Operation: admissionv1.Create,
		Namespace: pod.Namespace, // as the apiserver sets it
		Object:    runtime.RawExtension{Raw: raw},
	}})
}

func notReadyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != readiness.Path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"ready":false}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func readyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != readiness.Path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ready":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPreflightDeniesEnforceWhenNotReady(t *testing.T) {
	srv := notReadyServer(t)
	t.Setenv("BALLOONS_READY_ENDPOINT", srv.URL)

	m, _ := newHandleMutator(t, "enforce", labeledNode("n1", true, "2"))
	resp := admitScheduledPod(t, m)
	if resp.Allowed {
		t.Fatalf("expected deny (enforce + plugin not ready), got allowed")
	}
	if !strings.Contains(resp.Result.Message, "not ready") {
		t.Errorf("deny message should mention not ready, got %q", resp.Result.Message)
	}
}

// TestPreflightDenialDisclosesNoNodeName: the denial withholds the node name, which stays
// in the PluginNotReady Event. Both are asserted so it cannot pass by vanishing from both.
func TestPreflightDenialDisclosesNoNodeName(t *testing.T) {
	const nodeName = "shared-worker-11"
	srv := notReadyServer(t)
	t.Setenv("BALLOONS_READY_ENDPOINT", srv.URL)

	m, rec := newHandleMutator(t, "enforce", labeledNode(nodeName, true, "2"))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "vllm-bound",
			Namespace:   "inference",
			Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": "1"},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name: "vllm",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				},
			}},
		},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshalling the pod: %v", err)
	}
	resp := m.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Namespace: pod.Namespace,
			Object:    runtime.RawExtension{Raw: raw},
		},
	})
	if resp.Allowed {
		t.Fatalf("expected deny (enforce + plugin not ready), got allowed")
	}
	if strings.Contains(resp.Result.Message, nodeName) {
		t.Errorf("deny message discloses the node name: %q", resp.Result.Message)
	}
	if !hasEventContaining(drainEvents(rec), nodeName) {
		t.Error("the PluginNotReady event should name the node for the operator")
	}
}

// TestPreflightWarnsAndSkipsInit: warn mode on a best-effort policy drops the gate.
func TestPreflightWarnsAndSkipsInit(t *testing.T) {
	srv := notReadyServer(t)
	t.Setenv("BALLOONS_READY_ENDPOINT", srv.URL)

	t.Setenv("INIT_CONTAINER_IMAGE", testInitImage)

	m, rec := newHandleMutatorEnforce(t, false, labeledNode("n1", true, "2"))
	resp := admitScheduledPod(t, m)
	if !resp.Allowed {
		t.Fatalf("expected allowed (warn), got denied: %q", resp.Result.Message)
	}
	if patchesTouchInitContainers(resp) {
		t.Errorf("warn mode must NOT inject init container when plugin not ready")
	}
	var sawEvent bool
	for len(rec.Events) > 0 {
		if strings.Contains(<-rec.Events, "PluginNotReady") {
			sawEvent = true
		}
	}
	if !sawEvent {
		t.Errorf("expected PluginNotReady warning event")
	}
}

func TestPreflightInjectsInitWhenReady(t *testing.T) {
	srv := readyServer(t)
	t.Setenv("BALLOONS_READY_ENDPOINT", srv.URL)
	t.Setenv("INIT_CONTAINER_IMAGE", testInitImage)

	m, _ := newHandleMutator(t, "warn", labeledNode("n1", true, "2"))
	resp := admitScheduledPod(t, m)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied")
	}
	if !patchesTouchInitContainers(resp) {
		t.Errorf("plugin ready — init container should be injected")
	}
}

// liveReservations counts a node's outstanding reservations under the store's lock.
func liveReservations(s *ReservationStore, node string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pruneLocked(node))
}

// TestPreflightDenialBooksNoReservation: an enforce-mode pre-flight denial leaves no
// reservation; controllers recreate refused pods at once, so leaks would accumulate.
// CPU-only and memory/span>1 pods both run since the reserve is sized per dimension.
func TestPreflightDenialBooksNoReservation(t *testing.T) {
	srv := notReadyServer(t)
	t.Setenv("BALLOONS_READY_ENDPOINT", srv.URL)

	m, _ := coldStartMutator(t, "enforce", withMemoryFloor(topoNode("n1", 64, 2), 100*gib))

	const attempts = 3
	shapes := []struct{ name, cores, tp, mem string }{
		{"daemon-cpu", "2", "1", ""},
		{"daemon-mem", "8", "2", "8Gi"},
	}
	for _, s := range shapes {
		for i := 1; i <= attempts; i++ {
			resp := admitPrescheduled(t, m, s.name, s.cores, s.tp, s.mem, "n1", false)
			if resp.Allowed {
				t.Fatalf("%s attempt %d: expected deny (enforce + plugin not ready), got allowed", s.name, i)
			}
			if !strings.Contains(resp.Result.Message, "pre-flight") {
				t.Fatalf("%s attempt %d: expected the pre-flight denial, got %q", s.name, i, resp.Result.Message)
			}
		}

		if got := liveReservations(m.Reservations, "n1"); got != 0 {
			t.Fatalf("%s: %d denied admissions left %d reservations on n1, want 0 — the pod was refused but its headroom stays booked for the TTL", s.name, attempts, got)
		}
	}
}

// TestPreflightPrecedesTheCPUCapacityGate: a pod both over capacity and on a not-ready
// node is denied with the pre-flight reason, the more actionable one.
func TestPreflightPrecedesTheCPUCapacityGate(t *testing.T) {
	// PluginNotReady is process-wide: use a node label no other test uses.
	const node = "preflight-precedence-node"
	metrics.PluginNotReady.DeleteLabelValues(node)
	t.Cleanup(func() { metrics.PluginNotReady.DeleteLabelValues(node) })
	capacityFailures := func() float64 {
		return testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues(node, "inference", "capacity", "enforce"))
	}
	before := capacityFailures()

	srv := notReadyServer(t)
	t.Setenv("BALLOONS_READY_ENDPOINT", srv.URL)

	m, rec := coldStartMutator(t, "enforce", topoNode(node, 2, 2))
	resp := admitPrescheduled(t, m, "daemon-a", "6", "2", "", node, false)

	if resp.Allowed {
		t.Fatal("expected deny: enforce mode, plugin not ready, and the request does not fit")
	}
	if !strings.Contains(resp.Result.Message, "pre-flight") {
		t.Errorf("the pre-flight must own the denial, got %q", resp.Result.Message)
	}
	if got := testutil.ToFloat64(metrics.PluginNotReady.WithLabelValues(node)); got != 1 {
		t.Errorf("plugin_not_ready = %v, want 1", got)
	}
	if got := capacityFailures() - before; got != 0 {
		t.Errorf("capacity_check_failures{reason=capacity,result=enforce} rose by %v, want 0 — one denial is one attribution", got)
	}
	if !hasEventContaining(drainEvents(rec), "PluginNotReady") {
		t.Error("expected a PluginNotReady event on the denied pod")
	}
}

func apiErrorServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != readiness.Path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"ready":false,"error":"error reading balloons DaemonSet"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPreflightInconclusiveOnAPIErrorInjectsInit(t *testing.T) {
	// A 503 with a non-empty "error" field is a handler failure, not a not-ready verdict,
	// so even enforce mode falls through to the init container.
	srv := apiErrorServer(t)
	t.Setenv("BALLOONS_READY_ENDPOINT", srv.URL)
	t.Setenv("INIT_CONTAINER_IMAGE", testInitImage)

	m, _ := newHandleMutator(t, "enforce", labeledNode("n1", true, "2"))
	resp := admitScheduledPod(t, m)
	if !resp.Allowed {
		t.Fatalf("API-error 503 should be inconclusive, not denied: %q", resp.Result.Message)
	}
	if !patchesTouchInitContainers(resp) {
		t.Errorf("API-error 503 should inject init container as fallback")
	}
}

func TestPreflightInconclusiveInjectsInit(t *testing.T) {
	// Unreachable endpoint: inconclusive, so the init container is injected.
	t.Setenv("BALLOONS_READY_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("INIT_CONTAINER_IMAGE", testInitImage)

	m, _ := newHandleMutator(t, "enforce", labeledNode("n1", true, "2"))
	resp := admitScheduledPod(t, m)
	if !resp.Allowed {
		t.Fatalf("inconclusive should not deny, got denied: %q", resp.Result.Message)
	}
	if !patchesTouchInitContainers(resp) {
		t.Errorf("inconclusive should inject init container as fallback")
	}
}
