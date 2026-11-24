// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package match

import (
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
)

// TestKServeExampleReadsVLLMAndSGLangFlags proves docs/examples/policy-kserve.yaml
// derives the same span from either runtime's flags in kserve-container.
func TestKServeExampleReadsVLLMAndSGLangFlags(t *testing.T) {
	data, err := os.ReadFile("../../docs/examples/policy-kserve.yaml")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	var policy cpopv1alpha1.BalloonPinningPolicy
	if err := yaml.UnmarshalStrict(data, &policy); err != nil {
		t.Fatalf("parse example: %v", err)
	}
	rules, err := CompileIntent(policy.Spec.Intent)
	if err != nil {
		t.Fatalf("compile intent: %v", err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"vLLM --tensor-parallel-size=", []string{"--model", "m", "--tensor-parallel-size=2"}, 2},
		{"vLLM -tp", []string{"-tp", "4"}, 4},
		{"vLLM tp x pp", []string{"--tensor-parallel-size", "2", "--pipeline-parallel-size", "2"}, 4},
		{"SGLang --tp-size", []string{"--model-path", "m", "--tp-size", "2"}, 2},
		{"SGLang --tp", []string{"--tp", "4"}, 4},
		{"SGLang tp x pp", []string{"--tp-size=2", "--pp-size=2"}, 4},
		{"no parallel flag", []string{"--model", "m"}, 1},
		{"flag-like model path", []string{"--model", "/models/llama-tp=8", "--tensor-parallel-size=2"}, 2},
		{"flag-like path, pp", []string{"--model-path", "/m/x-pp=8", "--tp-size", "2", "--pp-size", "2"}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := corev1.Container{Name: "kserve-container", Args: tc.args}
			pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{c}}}
			if got := DesiredSpan(pod, &pod.Spec.Containers[0], rules); got != tc.want {
				t.Errorf("span = %d, want %d for args %q", got, tc.want, tc.args)
			}
		})
	}
}
