// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/intel/cpu-pinning-operator/internal/metrics"
)

// denialHelpers maps each denial helper to the result it must report. Operator decisions
// (capacity, preflight) are resultDenied; an unreachable reservation authority is
// resultError. denyEnforced serves both, so its call sites are checked separately.
var denialHelpers = map[string]string{
	"deniedCapacity":    "resultDenied",
	"deniedPreflight":   "resultDenied",
	"denyUnaccountable": "resultError",
}

// TestEveryDenialReturnSiteCarriesItsResult checks each denial return site's result label:
// resultSkipped would hide refusals in the skip count, resultError would page for a
// correct refusal.
func TestEveryDenialReturnSiteCarriesItsResult(t *testing.T) {
	const file = "pod_webhook.go"
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	perHelper := map[string]int{}
	ast.Inspect(parsed, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 2 {
			return true
		}
		call, ok := ret.Results[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		name := calleeName(call.Fun)
		want, tracked := denialHelpers[name]
		if !tracked {
			return true
		}
		perHelper[name]++
		label, ok := ret.Results[1].(*ast.Ident)
		if !ok || label.Name != want {
			got := "an expression that is not a result constant"
			if ok {
				got = label.Name
			}
			t.Errorf("%s: %s denies the pod and reports %s, want %s",
				fset.Position(ret.Pos()), name, got, want)
		}
		return true
	})

	// Guard against a walk that matches nothing: the text count must agree.
	source, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	for helper := range denialHelpers {
		inText := len(regexp.MustCompile(`return (m\.)?`+helper+`\(`).FindAllString(string(source), -1))
		if inText == 0 {
			t.Fatalf("%s returns %s nowhere; this test is scanning the wrong thing", file, helper)
		}
		if perHelper[helper] != inText {
			t.Fatalf("the walk found %d %s return sites and the text holds %d; the walk is missing sites",
				perHelper[helper], helper, inText)
		}
	}
}

// calleeName returns the function name of a plain or method call.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// TestResultLabelValues pins the label literals that the dashboard, chart alerts and
// docs/observability.md match on.
func TestResultLabelValues(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{resultPinned, "pinned"},
		{resultSkipped, "skipped"},
		{resultDenied, "denied"},
		{resultError, "error"},
	} {
		if c.got != c.want {
			t.Errorf("result label = %q, want %q", c.got, c.want)
		}
	}
}

// TestRefusalAndSkipAreSeparateResults: refusals and skips report distinct result labels.
func TestRefusalAndSkipAreSeparateResults(t *testing.T) {
	cases := []struct {
		name string

		setup func(t *testing.T) (*PodMutator, *corev1.Pod)
		want  string
		// reason is the pods_skipped_total reason a skip must record, so a fixture that stops
		// reaching its gate fails.
		reason string
	}{
		{
			name: "span wider than any node supports is refused",
			want: resultDenied,
			setup: func(t *testing.T) (*PodMutator, *corev1.Pod) {
				m, _ := newHandleMutator(t, "enforce", labeledNode("small", true, "2"))
				pod := matchedPod("vllm-span", "", "2", "vllm")
				pod.Annotations["cpu-pinning-operator.intel.com/tp"] = "4"
				return m, pod
			},
		},
		{
			name: "no room for the memory request is refused",
			want: resultDenied,
			setup: func(t *testing.T) (*PodMutator, *corev1.Pod) {
				m, _ := newHandleMutator(t, "enforce", withMemoryFloor(topoNode("n1", 64, 2), 1<<30))
				pod := matchedPod("vllm-mem", "n1", "2", "vllm")
				res := &pod.Spec.Containers[0].Resources
				res.Requests[corev1.ResourceMemory] = resource.MustParse("100Gi")
				res.Limits[corev1.ResourceMemory] = resource.MustParse("100Gi")
				return m, pod
			},
		},
		{
			name: "no room for the CPU request is refused",
			want: resultDenied,
			setup: func(t *testing.T) (*PodMutator, *corev1.Pod) {
				m, _ := newHandleMutator(t, "enforce", topoNode("n1", 4, 2))
				return m, matchedPod("vllm-cpu", "n1", "8", "vllm")
			},
		},
		{
			name: "a plugin that is not ready refuses the pod",
			want: resultDenied,
			setup: func(t *testing.T) (*PodMutator, *corev1.Pod) {
				t.Setenv("BALLOONS_READY_ENDPOINT", notReadyServer(t).URL)
				m, _ := newHandleMutator(t, "enforce", labeledNode("n1", true, "2"))
				return m, matchedPod("vllm-preflight", "n1", "1", "vllm")
			},
		},
		{
			name: "no balloons-enabled node in the cluster refuses the pod",
			want: resultDenied,
			setup: func(t *testing.T) (*PodMutator, *corev1.Pod) {
				m, _ := newHandleMutatorEnforce(t, true, labeledNode("n1", false, "8"))
				return m, matchedPod("vllm-nofleet", "", "2", "vllm")
			},
		},
		{
			name: "a pod no policy selects is skipped",
			want: resultSkipped,
			setup: func(t *testing.T) (*PodMutator, *corev1.Pod) {
				m, _ := newHandleMutator(t, "enforce", labeledNode("n1", true, "2"))
				pod := matchedPod("nginx", "", "2", "nginx")
				pod.Namespace = "default"
				return m, pod
			},
		},
		{
			name:   "a fractional CPU request is skipped",
			want:   resultSkipped,
			reason: metrics.ReasonFractional,
			setup: func(t *testing.T) (*PodMutator, *corev1.Pod) {
				m, _ := newHandleMutator(t, "enforce", labeledNode("n1", true, "2"))
				return m, matchedPod("vllm-frac", "", "500m", "vllm")
			},
		},
		{
			name:   "a target node outside the pinning pool is skipped",
			want:   resultSkipped,
			reason: metrics.ReasonExcluded,
			setup: func(t *testing.T) (*PodMutator, *corev1.Pod) {
				m, _ := newHandleMutator(t, "enforce", labeledNode("n1", false, "8"))
				return m, matchedPod("vllm-excluded", "n1", "2", "vllm")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, pod := tc.setup(t)
			before, beforeReason := skippedTotal(), 0.0
			if tc.reason != "" {
				beforeReason = skipCount(tc.reason)
			}

			_, result := m.handle(context.Background(), podRequest(t, pod))

			if result != tc.want {
				t.Errorf("admission result = %q, want %q", result, tc.want)
			}
			// A refusal must not appear in pods_skipped_total either.
			if tc.want == resultDenied && skippedTotal() != before {
				t.Errorf("a refusal incremented pods_skipped_total by %v, want 0", skippedTotal()-before)
			}
			if tc.reason != "" {
				if got := skipCount(tc.reason) - beforeReason; got != 1 {
					t.Errorf("the skip incremented pods_skipped_total{reason=%q} by %v, want 1 — "+
						"this admission took a different path than the one under test", tc.reason, got)
				}
			} else if tc.want == resultSkipped && skippedTotal() != before {
				t.Errorf("a pod no policy selects incremented pods_skipped_total by %v, want 0",
					skippedTotal()-before)
			}
		})
	}
}

// skippedTotal sums pods_skipped_total over every reason in the fixture's namespace.
func skippedTotal() float64 {
	var total float64
	for _, reason := range []string{
		metrics.ReasonFractional,
		metrics.ReasonAmbiguous,
		metrics.ReasonExcluded,
		metrics.ReasonNoBalloon,
	} {
		total += skipCount(reason)
	}
	return total
}
