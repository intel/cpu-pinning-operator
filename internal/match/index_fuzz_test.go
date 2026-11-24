// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package match

import (
	"regexp"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
)

// Both the policy spec and the admitted pod are untrusted; these targets fuzz
// the balloon derivation that combines them.

// balloonNameRe is every balloon name the generator emits; BalloonNameForPod must stay inside it.
var balloonNameRe = regexp.MustCompile(`^balloon-(device|span[1248](-colocate|-spread)?)$`)

// hugeExponentRe captures a quantity's significant exponent digits.
// resource.ParseQuantity hangs on an exponent past int32 range. The apiserver uses the
// same parser, so such input never reaches the webhook as a parsed Quantity; the
// harness skips it to avoid hanging.
var hugeExponentRe = regexp.MustCompile(`[eE][-+]?0*([0-9]+)`)

// skipUnparseableQuantity reports whether qty's exponent would hang ParseQuantity; leading zeros are ignored.
func skipUnparseableQuantity(qty string) bool {
	m := hugeExponentRe.FindStringSubmatch(qty)
	return m != nil && len(m[1]) > 6
}

// FuzzIntentDerivation compiles an arbitrary intent spec and derives a pod's
// span and balloon name from arbitrary annotation, arg and env values.
func FuzzIntentDerivation(f *testing.F) {
	f.Add(`--tensor-parallel-size[= ](\d+)`, "--tensor-parallel-size 4", "2", "8", 1, "package", 8, true)
	f.Add(`-tp[= ]?(\d+)`, "-tp=2", "", "", 1, "", 4, false)
	f.Add(`(\d+)`, "9999999999999999999", "9999999999999999999", "9999999999999999999", 1, "numa", 8, true)
	f.Add(`(\d+)`, "3037000500", "3037000500", "3037000500", 1, "", 8, true)
	f.Add("", "", "", "", 0, "", 1, false)
	f.Add(`(`, "x", "x", "x", -1, "bogus", 0, true)

	f.Fuzz(func(t *testing.T, argRegex, args, annotation, env string, def int, coLocate string, nodeMaxSpan int, withMultiplier bool) {
		spec := cpopv1alpha1.IntentSpec{
			Span: cpopv1alpha1.IntentSourceSpec{
				Annotation: "span-annotation",
				ArgRegex:   argRegex,
				EnvVar:     "SPAN_ENV",
				Default:    def,
			},
			CoLocate: coLocate,
		}
		if withMultiplier {
			spec.Multiplier = &cpopv1alpha1.IntentSourceSpec{
				Annotation: "mult-annotation",
				ArgRegex:   argRegex,
				EnvVar:     "MULT_ENV",
				Default:    def,
			}
		}
		rules, err := CompileIntent(spec)
		if err != nil {
			// An invalid argRegex must be reported, not dropped.
			if _, cerr := regexp.Compile(argRegex); cerr == nil {
				t.Fatalf("CompileIntent rejected a valid argRegex %q: %v", argRegex, err)
			}
			return
		}
		if rules.Span.Default < 1 {
			t.Fatalf("CompileIntent left span default %d, below 1", rules.Span.Default)
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod",
				Namespace: "ns",
				Annotations: map[string]string{
					"span-annotation": annotation,
					"mult-annotation": annotation,
				},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "c",
				Args: []string{args},
				Env: []corev1.EnvVar{
					{Name: "SPAN_ENV", Value: env},
					{Name: "MULT_ENV", Value: env},
				},
			}}},
		}
		container := &pod.Spec.Containers[0]

		for _, c := range []*corev1.Container{container, nil} {
			desired := DesiredSpan(pod, c, rules)
			assertSupportedSpan(t, "DesiredSpan", desired)

			span := SpanForPod(pod, c, rules, nodeMaxSpan)
			assertSupportedSpan(t, "SpanForPod", span)
			if span > desired {
				t.Fatalf("SpanForPod = %d exceeds the uncapped DesiredSpan %d", span, desired)
			}
			if nodeMaxSpan >= 1 && span > nodeMaxSpan {
				t.Fatalf("SpanForPod = %d exceeds nodeMaxSpan %d", span, nodeMaxSpan)
			}

			if name := BalloonNameForPod(pod, c, rules, nodeMaxSpan); !balloonNameRe.MatchString(name) {
				t.Fatalf("BalloonNameForPod = %q, not a generated balloon name", name)
			}

			// A multiplier (>= 1) must never shrink the span.
			if rules.Multiplier != nil {
				spanOnly := IntentRules{Span: rules.Span, CoLocate: rules.CoLocate}
				if base := DesiredSpan(pod, c, spanOnly); desired < base {
					t.Fatalf("DesiredSpan with a multiplier = %d, below the span-only %d", desired, base)
				}
			}
		}
	})
}

func assertSupportedSpan(t *testing.T, what string, span int) {
	t.Helper()
	switch span {
	case 1, 2, 4, 8:
	default:
		t.Fatalf("%s = %d, not a supported span", what, span)
	}
}

// FuzzIndexMatch matches an arbitrary pod against an arbitrary rule.
func FuzzIndexMatch(f *testing.F) {
	f.Add("inference", "inference", "vllm", "vllm", "app", "llm", "app", "llm", "wholePod", "2", "1")
	f.Add("inference-*", "inference-a", "vllm*", "vllm-worker", "app", "llm", "app", "other", "largestContainer", "2", "1")
	f.Add("[", "[", "[", "[", "", "", "", "", "", "1", "1")
	f.Add("*", "any", "*", "c0", "k", "v", "k", "v", "wholePod", "1", "2")
	f.Add("inference", "inference", "*", "vllm", "app", "llm", "app", "llm", "largestContainer", "9223372036854775807", "1")
	f.Add("inference", "inference", "*", "vllm", "app", "llm", "app", "llm", "largestContainer", "1", "9223372036854775807")

	f.Fuzz(func(t *testing.T, nsPattern, podNS, namePattern, containerName, ruleLabelKey, ruleLabelValue, podLabelKey, podLabelValue, selection, firstCPU, secondCPU string) {
		// largestContainer ranks by these; an absurd request must still rank highest.
		if skipUnparseableQuantity(firstCPU) || skipUnparseableQuantity(secondCPU) {
			return
		}
		firstQ, err := resource.ParseQuantity(firstCPU)
		if err != nil {
			return
		}
		secondQ, err := resource.ParseQuantity(secondCPU)
		if err != nil {
			return
		}
		rule := PolicyRule{
			Name:               "policy",
			Namespaces:         []string{nsPattern},
			ByContainerName:    []string{namePattern, "sidecar-*"},
			ByPodLabels:        map[string]string{ruleLabelKey: ruleLabelValue},
			ContainerSelection: selection,
		}
		idx := NewIndex()
		idx.Update([]PolicyRule{rule})

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod",
				Namespace: podNS,
				Labels:    map[string]string{podLabelKey: podLabelValue},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: containerName, Resources: cpuRequirements(firstQ)},
				{Name: "sidecar-proxy", Resources: cpuRequirements(secondQ)},
			}},
		}

		res := idx.Match(pod)
		if res.Rule == nil {
			if res.Container != nil || res.PodScoped || res.Ambiguous {
				t.Fatalf("no rule matched but the result carries a decision: %+v", res)
			}
			return
		}
		// A matched pod must be in a namespace the rule is scoped to.
		if !NamespaceMatches(nsPattern, podNS) {
			t.Fatalf("rule scoped to %q matched a pod in namespace %q", nsPattern, podNS)
		}
		if res.Container != nil {
			// No other candidate may outrank the one largestContainer returns.
			if rule.ContainerSelection == "largestContainer" {
				candidates := matchByContainerName(pod, rule.ByContainerName)
				if len(candidates) == 0 {
					candidates = containerPtrs(pod)
				}
				chosen := res.Container.Resources.Requests[corev1.ResourceCPU]
				for _, other := range candidates {
					q := other.Resources.Requests[corev1.ResourceCPU]
					if chosen.Cmp(q) < 0 {
						t.Fatalf("selected candidate %q (%s) over %q (%s)",
							res.Container.Name, chosen.String(), other.Name, q.String())
					}
				}
			}
			if res.PodScoped || res.Ambiguous {
				t.Fatalf("container-scoped match also flagged pod-scoped/ambiguous: %+v", res)
			}
			found := false
			for i := range pod.Spec.Containers {
				if res.Container == &pod.Spec.Containers[i] {
					found = true
				}
			}
			if !found {
				t.Fatalf("match returned a container that is not the pod's: %q", res.Container.Name)
			}
		}
		if res.Ambiguous {
			if res.Container != nil || res.PodScoped {
				t.Fatalf("ambiguous match also carries a target: %+v", res)
			}
			if res.Reason == "" {
				t.Fatal("ambiguous match carries no reason for the warning event")
			}
		}
	})
}

func cpuRequirements(q resource.Quantity) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: q},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: q},
	}
}

// FuzzNamespaceMatches fuzzes the shared namespace glob matcher.
func FuzzNamespaceMatches(f *testing.F) {
	f.Add("inference-*", "inference-a")
	f.Add("*", "")
	f.Add("[a-", "a")
	f.Add("a/b", "a/b")
	f.Add("", "")

	f.Fuzz(func(t *testing.T, pattern, ns string) {
		got := NamespaceMatches(pattern, ns)
		if !IsGlob(pattern) && got != (pattern == ns) {
			t.Fatalf("NamespaceMatches(%q, %q) = %v for a non-glob pattern", pattern, ns, got)
		}
		// Namespace and container-name globbing must agree.
		if other := containerNameMatches(pattern, ns); other != got {
			t.Fatalf("NamespaceMatches(%q, %q) = %v but containerNameMatches = %v", pattern, ns, got, other)
		}
	})
}

// FuzzSpanFromBalloonName fuzzes the inverse mapping over a pod-patchable annotation;
// a span outside the generated widths would misaccount reservations.
func FuzzSpanFromBalloonName(f *testing.F) {
	f.Add("balloon-span2")
	f.Add("balloon-span8-colocate")
	f.Add("balloon-device")
	f.Add("balloon-span+2")
	f.Add("balloon-span02")
	f.Add("balloon-span3")
	f.Add("balloon-span2-elsewhere")
	f.Add("balloon-span9999999999999999999999")
	f.Add("balloon-span-2")
	f.Add("balloon-span2-")
	f.Add("")

	f.Fuzz(func(t *testing.T, name string) {
		span, ok := SpanFromBalloonName(name)
		if !ok {
			if span != 0 {
				t.Fatalf("SpanFromBalloonName(%q) rejected the name but returned span %d", name, span)
			}
			// A generated name must never be rejected.
			if balloonNameRe.MatchString(name) {
				t.Fatalf("SpanFromBalloonName(%q) rejected a generated balloon name", name)
			}
			return
		}
		assertSupportedSpan(t, "SpanFromBalloonName", span)
		// Nothing outside the generated set may be accepted.
		if !balloonNameRe.MatchString(name) {
			t.Fatalf("SpanFromBalloonName(%q) = %d, but the name is not one the generator emits", name, span)
		}
	})
}
