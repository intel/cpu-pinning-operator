// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/yaml"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
)

// shippedDefaultMutator compiles the chart's values.yaml balloonPinningPolicy, with the
// docs/examples/values/kserve.yaml target layered on as helm would, into a PodMutator.
func shippedDefaultMutator(t *testing.T, nodes ...runtime.Object) *PodMutator {
	t.Helper()
	var values struct {
		Policy cpopv1alpha1.BalloonPinningPolicySpec `json:"balloonPinningPolicy"`
	}
	for _, path := range []string{
		filepath.Join("..", "..", "deploy", "cpu-pinning-operator", "values.yaml"),
		filepath.Join("..", "..", "docs", "examples", "values", "kserve.yaml"),
	} {
		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			t.Fatalf("read chart values: %v", err)
		}
		if err := yaml.Unmarshal(data, &values); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
	}
	spec := values.Policy
	intent, err := match.CompileIntent(spec.Intent)
	if err != nil {
		t.Fatalf("compile shipped intent: %v", err)
	}
	idx := match.NewIndex()
	idx.Update([]match.PolicyRule{{
		Name:              "default",
		Namespaces:        spec.Namespaces,
		ByContainerName:   spec.Match.ByContainerName,
		ByPodLabels:       spec.Match.ByPodLabels,
		EnforcePinning:    spec.Scheduling.EnforcePinning == nil || *spec.Scheduling.EnforcePinning,
		Intent:            intent,
		CapacityCheckMode: spec.CapacityCheck.Mode,
	}})

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	m := &PodMutator{
		Client:      fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build(),
		PolicyIndex: idx,
		Recorder:    record.NewFakeRecorder(16),
	}
	if err := m.InjectDecoder(admission.NewDecoder(scheme)); err != nil {
		t.Fatalf("inject decoder: %v", err)
	}
	return m
}

// admitServingPod admits a whole-core Guaranteed pod in "inference" with one named container.
func admitServingPod(t *testing.T, m *PodMutator, container string, args ...string) admission.Response {
	t.Helper()
	cpu := resource.MustParse("4")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "predictor",
			Namespace: "inference",

			Annotations: map[string]string{"prometheus.kserve.io/port": "8080"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: container,
			Args: args,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: cpu},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: cpu},
			},
		}}},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	return m.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}),
		Operation: admissionv1.Create,
		Namespace: pod.Namespace,
		Object:    runtime.RawExtension{Raw: raw},
	}})
}

// TestShippedDefaultPinsKServePredictor: the chart defaults plus the KServe example pin a vLLM predictor to
// balloon-span2 and no longer matches a container named vllm.
func TestShippedDefaultPinsKServePredictor(t *testing.T) {
	m := shippedDefaultMutator(t, labeledNode("wide", true, "8"))

	resp := admitServingPod(t, m, "kserve-container", "--model_name=llama", "--tensor-parallel-size=2")
	if !resp.Allowed {
		t.Fatalf("KServe predictor denied: %q", resp.Result.Message)
	}
	if got := stampedBalloon(resp); got != "balloon-span2" {
		t.Errorf("KServe predictor stamped %q, want balloon-span2 (TP=2 read from kserve-container args)", got)
	}

	resp = admitServingPod(t, m, "vllm", "--tensor-parallel-size=2")
	if !resp.Allowed {
		t.Fatalf("unmatched vllm pod denied: %q", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("shipped default mutated a vllm container (%+v), want no match: kserve-container replaced vllm", resp.Patches)
	}
}
