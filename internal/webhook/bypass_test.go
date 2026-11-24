// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	admissionv1 "k8s.io/api/admission/v1"

	"github.com/intel/cpu-pinning-operator/internal/metrics"
)

// newHandleMutatorFailingList builds a Handle mutator whose client fails every List, so an
// unscheduled matched pod fails at clusterMaxSpan (the node-lookup bypass). enforce picks
// deny vs fail-open.
func newHandleMutatorFailingList(t *testing.T, enforce bool, nodes ...runtime.Object) (*PodMutator, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return fmt.Errorf("simulated apiserver read failure")
			},
		}).Build()
	return newHandleMutatorFull(t, "warn", enforce, nil, c, nil)
}

// newHandleMutatorFailingGet is the Get twin, for the bound-pod node lookup.
func newHandleMutatorFailingGet(t *testing.T, enforce bool, nodes ...runtime.Object) (*PodMutator, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				return fmt.Errorf("simulated apiserver read failure")
			},
		}).Build()
	return newHandleMutatorFull(t, "warn", enforce, nil, c, nil)
}

func bypassCount(ns, reason string) float64 {
	return testutil.ToFloat64(metrics.WebhookBypasses.WithLabelValues(ns, reason))
}

// podRequest builds a CREATE AdmissionRequest with Namespace set as the apiserver sets it.
func podRequest(t *testing.T, pod *corev1.Pod) admission.Request {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}),
		Operation: admissionv1.Create,
		Namespace: pod.Namespace,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func admitCustomPod(t *testing.T, m *PodMutator, pod *corev1.Pod) admission.Response {
	t.Helper()
	return m.Handle(context.Background(), podRequest(t, pod))
}

// TestHandleMatchedUnmutatedEmitsBypass: under a best-effort policy a matched pod the
// webhook cannot mutate is admitted unmutated and counted as a bypass.
func TestHandleMatchedUnmutatedEmitsBypass(t *testing.T) {
	before := bypassCount("inference", metrics.BypassNodeLookup)

	m, rec := newHandleMutatorFailingList(t, false,
		labeledNode("n1", true, "2"),
	)
	resp := admitPod(t, m, "vllm-x", "2", "1")

	if !resp.Allowed {
		t.Fatalf("expected the internal read failure to fail OPEN (Allowed:true, admit unmutated), got Allowed=false: %q", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patch on the fail-open bypass path, got %d patches", len(resp.Patches))
	}
	if got := bypassCount("inference", metrics.BypassNodeLookup) - before; got != 1 {
		t.Errorf("webhook_bypasses_total{reason=node-lookup} increment = %v, want 1", got)
	}
	if !hasEventContaining(drainEvents(rec), "WebhookBypass") {
		t.Error("expected a WebhookBypass event on the matched-but-unmutated pod")
	}
}

// TestBypassEventOmitsRawError: the WebhookBypass Event names the reason stage but never
// the raw internal error, which would leak to the namespace.
func TestBypassEventOmitsRawError(t *testing.T) {
	m, rec := newHandleMutatorFailingList(t, false,
		labeledNode("n1", true, "2"),
	)
	resp := admitPod(t, m, "vllm-leak", "2", "1")
	if !resp.Allowed {
		t.Fatalf("expected fail-open admit, got Allowed=false: %q", resp.Result.Message)
	}

	events := drainEvents(rec)
	if !hasEventContaining(events, "WebhookBypass") {
		t.Fatal("expected a WebhookBypass event")
	}

	for _, e := range events {
		if strings.Contains(e, "simulated apiserver read failure") {
			t.Errorf("WebhookBypass event leaked raw error text: %q", e)
		}
	}

	if !hasEventContaining(events, metrics.BypassNodeLookup) {
		t.Errorf("WebhookBypass event should name the reason stage %q; events=%v", metrics.BypassNodeLookup, events)
	}
}

// TestHandleNormalPinNoBypass: a normal pin leaves the bypass counter alone.
func TestHandleNormalPinNoBypass(t *testing.T) {
	before := bypassCount("inference", metrics.BypassNodeLookup)

	m, _ := newHandleMutator(t, "warn",
		labeledNode("n1", true, "2"),
	)
	resp := admitPod(t, m, "vllm-ok", "2", "1")
	if !resp.Allowed || len(resp.Patches) == 0 {
		t.Fatalf("expected a pinned (patched, allowed) response, got allowed=%v patches=%d", resp.Allowed, len(resp.Patches))
	}
	if got := bypassCount("inference", metrics.BypassNodeLookup) - before; got != 0 {
		t.Errorf("normal pin path incremented the bypass counter by %v, want 0", got)
	}
}

// TestHandleSkipNoBypass: an intentional skip counts in pods_skipped_total, not as a bypass.
func TestHandleSkipNoBypass(t *testing.T) {
	before := bypassCount("inference", metrics.BypassNodeLookup)

	m, _ := newHandleMutator(t, "warn",
		labeledNode("n1", true, "2"),
	)
	frac := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "vllm-frac",
			Namespace:   "inference",
			Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": "1"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "vllm",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			},
		}}},
	}
	resp := admitCustomPod(t, m, frac)
	if !resp.Allowed {
		t.Fatalf("a fractional matched pod must be admitted (skipped), got denied: %q", resp.Result.Message)
	}
	if got := bypassCount("inference", metrics.BypassNodeLookup) - before; got != 0 {
		t.Errorf("skip path incremented the bypass counter by %v, want 0", got)
	}
}

// TestHandleNonMatchedNoBypass: a non-matching pod is never a bypass.
func TestHandleNonMatchedNoBypass(t *testing.T) {
	before := bypassCount("other-ns", metrics.BypassNodeLookup)

	m, _ := newHandleMutator(t, "warn",
		labeledNode("n1", true, "2"),
	)
	nonTarget := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "not-a-target", Namespace: "other-ns"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			},
		}}},
	}
	resp := admitCustomPod(t, m, nonTarget)
	if !resp.Allowed {
		t.Fatalf("a non-target pod must be admitted, got %q", resp.Result.Message)
	}
	if got := bypassCount("other-ns", metrics.BypassNodeLookup) - before; got != 0 {
		t.Errorf("non-matched path incremented the bypass counter by %v, want 0", got)
	}
}
