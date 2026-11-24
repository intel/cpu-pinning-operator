// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
)

// twoPolicyMutator wires two rules matching different container names in "inference".
func twoPolicyMutator(t *testing.T) *PodMutator {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)

	intent, err := match.CompileIntent(cpopv1alpha1.IntentSpec{
		Span: cpopv1alpha1.IntentSourceSpec{Annotation: "cpu-pinning-operator.intel.com/tp", Default: 1},
	})
	if err != nil {
		t.Fatalf("compile intent: %v", err)
	}
	idx := match.NewIndex()
	idx.Update([]match.PolicyRule{
		{
			Name: "vllm-serving", Namespaces: []string{"inference"},
			ByContainerName: []string{"vllm"}, Intent: intent, CapacityCheckMode: "off",
		},
		{
			Name: "sglang-serving", Namespaces: []string{"inference"},
			ByContainerName: []string{"sglang"}, Intent: intent, CapacityCheckMode: "off",
		},
	})

	m := &PodMutator{
		Client:      fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(labeledNode("node-a", true, "1")).Build(),
		PolicyIndex: idx,
		Recorder:    record.NewFakeRecorder(16),
	}
	if err := m.InjectDecoder(admission.NewDecoder(scheme)); err != nil {
		t.Fatalf("inject decoder: %v", err)
	}
	return m
}

func pinnedCount(policy string) float64 {
	return testutil.ToFloat64(metrics.PodsPinned.WithLabelValues("inference", "balloon-span1", policy))
}

// TestPodsPinnedPolicyLabelFollowsMatchingPolicy: the policy label names the policy
// that actually matched the pod.
func TestPodsPinnedPolicyLabelFollowsMatchingPolicy(t *testing.T) {
	m := twoPolicyMutator(t)

	beforeVLLM, beforeSGLang := pinnedCount("vllm-serving"), pinnedCount("sglang-serving")

	for _, container := range []string{"vllm", "sglang"} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        container + "-pod",
				Namespace:   "inference",
				Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": "1"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: container,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				},
			}}},
		}
		if resp := admitCustomPod(t, m, pod); len(resp.Patches) == 0 {
			t.Fatalf("%s pod was not pinned: %+v", container, resp.Result)
		}
	}

	if got := pinnedCount("vllm-serving") - beforeVLLM; got != 1 {
		t.Errorf("pods_pinned_total{policy=\"vllm-serving\"} rose by %v, want 1", got)
	}

	if got := pinnedCount("sglang-serving") - beforeSGLang; got != 1 {
		t.Errorf("pods_pinned_total{policy=\"sglang-serving\"} rose by %v, want 1 — "+
			"the sglang pod was counted under another policy, so the label is not read from the match", got)
	}
}
