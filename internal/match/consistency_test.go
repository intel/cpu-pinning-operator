// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package match_test

import (
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/balloons"
	"github.com/intel/cpu-pinning-operator/internal/match"
)

// TestWebhookAndBalloonGenAgreeOnNames proves every span name the webhook can
// stamp exists in the generated BalloonsPolicy.
func TestWebhookAndBalloonGenAgreeOnNames(t *testing.T) {
	// 8 NUMA nodes on 2 packages: every span 1/2/4/8 is generated.
	pkgNode := map[int][]int{0: {0, 1, 2, 3}, 1: {4, 5, 6, 7}}
	defs := balloons.GenerateBalloonDefs(pkgNode, 8, "physical-cores")

	defsByName := map[string]bool{}
	for _, d := range defs {
		defsByName[d.Name] = true
	}

	for _, span := range []int{1, 2, 4, 8} {
		// A pod whose annotation intent resolves to this span.
		spec := cpopv1alpha1.IntentSpec{
			Span: cpopv1alpha1.IntentSourceSpec{
				Annotation: "test.example.com/span",
				Default:    1,
			},
		}
		rules, err := match.CompileIntent(spec)
		if err != nil {
			t.Fatalf("compile intent: %v", err)
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{"test.example.com/span": strconv.Itoa(span)},
			},
		}
		name := match.BalloonNameForPod(pod, nil, rules, 8)
		if !defsByName[name] {
			t.Errorf("BalloonNameForPod for span %d = %q — MISSING from GenerateBalloonDefs (have: %v)",
				span, name, defsByName)
		}
	}
}

// TestWebhookAndBalloonGenAgreeOnColocateSuffix proves every (span, coLocate)
// name the webhook can stamp on an 8-NUMA/2-package node is generated.
func TestWebhookAndBalloonGenAgreeOnColocateSuffix(t *testing.T) {
	// Same 8-NUMA/2-pkg topology as the base test.
	pkgNode := map[int][]int{0: {0, 1, 2, 3}, 1: {4, 5, 6, 7}}
	defs := balloons.GenerateBalloonDefs(pkgNode, 8, "physical-cores")

	defsByName := map[string]bool{}
	for _, d := range defs {
		defsByName[d.Name] = true
	}

	// Exceptions: span=1 has no variants, and span=8 colocate needs an 8-NUMA package.
	spans := []int{1, 2, 4, 8}
	coLocates := []string{"", "package", "numa"}
	for _, span := range spans {
		for _, coLocate := range coLocates {
			spec := cpopv1alpha1.IntentSpec{
				Span: cpopv1alpha1.IntentSourceSpec{
					Annotation: "test.example.com/span",
					Default:    1,
				},
				CoLocate: coLocate,
			}
			rules, err := match.CompileIntent(spec)
			if err != nil {
				t.Fatalf("compile intent(span=%d,coLocate=%q): %v", span, coLocate, err)
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{"test.example.com/span": strconv.Itoa(span)},
			}}
			name := match.BalloonNameForPod(pod, nil, rules, 8)
			// span=1 has no colocate/spread variants.
			if span == 1 && coLocate != "" {
				continue
			}
			// span=8 colocate needs an 8-NUMA package, which this topology lacks.
			if span == 8 && coLocate == "package" {
				if defsByName[name] {
					t.Errorf("BalloonNameForPod(span=8, coLocate=package) = %q — unexpectedly EXISTS on a topology with only 4 NUMA per package", name)
				}
				continue
			}
			if !defsByName[name] {
				t.Errorf("BalloonNameForPod(span=%d, coLocate=%q) = %q — MISSING from generator (have: %v)",
					span, coLocate, name, keys(defsByName))
			}
		}
	}
}

// TestWebhookAndBalloonGenAgreeOnDeviceName proves the device balloon name, duplicated
// across match and balloons to avoid an import cycle, is generated.
func TestWebhookAndBalloonGenAgreeOnDeviceName(t *testing.T) {
	pkgNode := map[int][]int{0: {0, 1, 2, 3}, 1: {4, 5, 6, 7}}
	defs := balloons.GenerateBalloonDefs(pkgNode, 8, "physical-cores")

	defsByName := map[string]bool{}
	for _, d := range defs {
		defsByName[d.Name] = true
	}

	// A device pod bypasses intent derivation, so any IntentRules will do.
	spec := cpopv1alpha1.IntentSpec{
		Span: cpopv1alpha1.IntentSourceSpec{
			Annotation: "test.example.com/span",
			Default:    1,
		},
		CoLocate: "package",
	}
	rules, err := match.CompileIntent(spec)
	if err != nil {
		t.Fatalf("compile intent: %v", err)
	}

	// Device resources go on both requests and limits.
	gpu := resource.MustParse("1")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			// Would resolve to a span balloon without the device route.
			Annotations: map[string]string{"test.example.com/span": "4"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "worker",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{"nvidia.com/gpu": gpu},
					Limits:   corev1.ResourceList{"nvidia.com/gpu": gpu},
				},
			}},
		},
	}

	name := match.BalloonNameForPod(pod, nil, rules, 8)
	if !defsByName[name] {
		t.Errorf("BalloonNameForPod for a device pod = %q — MISSING from GenerateBalloonDefs (have: %v)",
			name, keys(defsByName))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestVariantEmissionMatrix states, per topology and (span, variant), which
// balloon names must and must not be generated; "" is the no-suffix balloon.
func TestVariantEmissionMatrix(t *testing.T) {
	topos := []struct {
		label   string
		pkgNode map[int][]int
		maxSpan int
		expect  map[string]bool // balloon name → must-be-present (true) or must-be-absent (false)
	}{
		{
			label:   "1-NUMA-per-pkg × 2 pkg",
			pkgNode: map[int][]int{0: {0}, 1: {1}},
			maxSpan: 8,
			expect: map[string]bool{
				"balloon-span1":          true,
				"balloon-span2":          true,
				"balloon-span2-colocate": false, // no pkg has 2 NUMAs → can't co-locate
				"balloon-span2-spread":   true,  // 2 pkgs available for spread
				"balloon-span4":          false, // only 2 NUMA total, can't build span=4
				"balloon-span8":          false,
			},
		},
		{
			label:   "single-pkg × 4 NUMA (SNC / single-socket)",
			pkgNode: map[int][]int{0: {0, 1, 2, 3}},
			maxSpan: 8,
			expect: map[string]bool{
				"balloon-span1":          true,
				"balloon-span2":          true,
				"balloon-span2-colocate": true,  // pkg has 4 NUMAs, colocate span=2 fits
				"balloon-span2-spread":   false, // only 1 pkg, nothing to spread across
				"balloon-span4":          true,
				"balloon-span4-colocate": true,
				"balloon-span4-spread":   false,
				"balloon-span8":          false, // only 4 NUMAs, span=8 needs 8
			},
		},
		{
			label:   "single-pkg × 8 NUMA (large SNC)",
			pkgNode: map[int][]int{0: {0, 1, 2, 3, 4, 5, 6, 7}},
			maxSpan: 8,
			expect: map[string]bool{
				"balloon-span8":          true,
				"balloon-span8-colocate": true,  // pkg has 8 NUMAs, colocate span=8 fits exactly
				"balloon-span8-spread":   false, // only 1 pkg, no spread — THIS ROW is the bug guard
			},
		},
		{
			label:   "dual-pkg × 2 NUMA each",
			pkgNode: map[int][]int{0: {0, 1}, 1: {2, 3}},
			maxSpan: 8,
			expect: map[string]bool{
				"balloon-span2":          true,
				"balloon-span2-colocate": true,  // each pkg has 2 NUMAs
				"balloon-span2-spread":   true,  // 2 pkgs available
				"balloon-span4":          true,  // 4 NUMAs total
				"balloon-span4-colocate": false, // no pkg has 4 NUMAs
				"balloon-span4-spread":   true,  // cross-package span=4 (2+2)
				"balloon-span8":          false, // only 4 NUMAs
			},
		},
		{
			label:   "dual-pkg × 4 NUMA each (rich topology)",
			pkgNode: map[int][]int{0: {0, 1, 2, 3}, 1: {4, 5, 6, 7}},
			maxSpan: 8,
			expect: map[string]bool{
				"balloon-span4-colocate": true, // each pkg has 4 NUMAs
				"balloon-span4-spread":   true, // 2 pkgs, cross-package span=4
				"balloon-span8":          true,
				"balloon-span8-colocate": false, // no pkg has 8 NUMAs
				"balloon-span8-spread":   true,  // 2 pkgs, cross-package span=8
			},
		},
		{
			label:   "quad-pkg × 2 NUMA each",
			pkgNode: map[int][]int{0: {0, 1}, 1: {2, 3}, 2: {4, 5}, 3: {6, 7}},
			maxSpan: 8,
			expect: map[string]bool{
				"balloon-span8-colocate": false, // no pkg has 8 NUMAs
				"balloon-span8-spread":   true,  // 4 pkgs, cross-package span=8
			},
		},
	}

	for _, topo := range topos {
		t.Run(topo.label, func(t *testing.T) {
			defs := balloons.GenerateBalloonDefs(topo.pkgNode, topo.maxSpan, "physical-cores")
			defsByName := map[string]bool{}
			for _, d := range defs {
				defsByName[d.Name] = true
			}
			for name, wantPresent := range topo.expect {
				got := defsByName[name]
				if got != wantPresent {
					if wantPresent {
						t.Errorf("%s: %s MUST be present, but is missing (have: %v)", topo.label, name, keys(defsByName))
					} else {
						t.Errorf("%s: %s MUST NOT be present, but is emitted (topology cannot back this variant)", topo.label, name)
					}
				}
			}
		})
	}
}
