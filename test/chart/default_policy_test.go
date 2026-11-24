// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
)

// renderedDefaultPolicy returns the BalloonPinningPolicy the chart renders.
func renderedDefaultPolicy(t *testing.T, extra ...string) cpopv1alpha1.BalloonPinningPolicy {
	t.Helper()
	for _, doc := range strings.Split(renderChart(t, extra...), "\n---") {
		if !hasKind(doc, "BalloonPinningPolicy") {
			continue
		}
		var p cpopv1alpha1.BalloonPinningPolicy
		if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
			t.Fatalf("parse rendered BalloonPinningPolicy: %v", err)
		}
		return p
	}
	t.Fatal("chart rendered no BalloonPinningPolicy")
	return cpopv1alpha1.BalloonPinningPolicy{}
}

// renderWithoutTarget runs `helm template` WITHOUT policyTargetValues.
func renderWithoutTarget(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")
	out, err := exec.Command(helmBin(t), append([]string{"template", "t", chartDir}, extra...)...).CombinedOutput()
	requireSubcharts(t, string(out))
	return string(out), err
}

// TestPolicyTargetIsRequired proves the chart ships no default target: a policy without
// a match or without a namespace target fails the render with a message naming the fix.
func TestPolicyTargetIsRequired(t *testing.T) {
	for name, args := range map[string][]string{
		"chart defaults": nil,
		"match only":     {"--set", "balloonPinningPolicy.match.byContainerName={vllm}"},
		"namespace only": {"--set", "balloonPinningPolicy.namespaces={inference}"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := renderWithoutTarget(t, args...)
			if err == nil {
				t.Fatalf("render succeeded without a full policy target:\n%s", out)
			}
			for _, want := range []string{
				"balloonPinningPolicy.match.byContainerName={vllm}",
				"balloonPinningPolicy.namespaces={inference}",
				"balloonPinningPolicy.create=false",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("failure message does not mention %q:\n%s", want, out)
				}
			}
		})
	}

	for name, args := range map[string][]string{
		"byPodLabels and namespaceSelector": {"--set", "balloonPinningPolicy.match.byPodLabels.app=vllm",
			"--set", "balloonPinningPolicy.namespaceSelector.matchLabels.pin=on"},
		"create=false needs no target": {"--set", "balloonPinningPolicy.create=false"},
	} {
		t.Run(name, func(t *testing.T) {
			if out, err := renderWithoutTarget(t, args...); err != nil {
				t.Fatalf("render failed: %v\n%s", err, out)
			}
		})
	}
	if out, _ := renderWithoutTarget(t, "--set", "balloonPinningPolicy.create=false"); hasKind(out, "BalloonPinningPolicy") {
		t.Error("create=false rendered a BalloonPinningPolicy")
	}
}

// TestPolicyRendersTheGivenTarget proves the worked example renders the KServe policy,
// and switching to bare vLLM changes nothing but the container name.
func TestPolicyRendersTheGivenTarget(t *testing.T) {
	def := renderedDefaultPolicy(t)
	if def.Name != "default" {
		t.Errorf("policy name = %q, want default", def.Name)
	}
	if got, want := def.Spec.Match.ByContainerName, []string{"kserve-container"}; !reflect.DeepEqual(got, want) {
		t.Errorf("match.byContainerName = %v, want %v", got, want)
	}
	if got, want := def.Spec.Namespaces, []string{"inference"}; !reflect.DeepEqual(got, want) {
		t.Errorf("namespaces = %v, want %v", got, want)
	}

	vllm := renderedDefaultPolicy(t, "--set", "balloonPinningPolicy.match.byContainerName={vllm}")
	if got, want := vllm.Spec.Match.ByContainerName, []string{"vllm"}; !reflect.DeepEqual(got, want) {
		t.Errorf("with byContainerName={vllm}: match.byContainerName = %v, want %v", got, want)
	}
	vllm.Spec.Match.ByContainerName = def.Spec.Match.ByContainerName
	if !reflect.DeepEqual(vllm.Spec, def.Spec) {
		t.Errorf("byContainerName={vllm} changed more than the container name:\n kserve: %+v\n vllm:   %+v", def.Spec, vllm.Spec)
	}
}
