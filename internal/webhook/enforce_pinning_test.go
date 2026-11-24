// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	admissionv1 "k8s.io/api/admission/v1"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
)

// rawErrText is the interceptors' error text; it must never reach the client or an Event.
const rawErrText = "simulated apiserver read failure"

func skipCount(reason string) float64 {
	return testutil.ToFloat64(metrics.PodsSkipped.WithLabelValues("inference", reason))
}

// matchedPod builds a pod the fixture rule matches, one container per name requesting cpu;
// node, when set, pre-binds it.
func matchedPod(name, node, cpu string, containers ...string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "inference",
			Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": "1"},
		},
		Spec: corev1.PodSpec{NodeName: node},
	}
	for _, c := range containers {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
			Name: c,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
			},
		})
	}
	return pod
}

// admitPodInRequestNamespace admits a matched pod whose request namespace differs, to tell
// which one a metric is labelled from.
func admitPodInRequestNamespace(t *testing.T, m *PodMutator, name, reqNamespace string) admission.Response {
	t.Helper()
	req := podRequest(t, matchedPod(name, "", "2", "vllm"))
	req.Namespace = reqNamespace
	return m.Handle(context.Background(), req)
}

// assertAdmittedUnpinned requires allowed with no patch.
func assertAdmittedUnpinned(t *testing.T, resp admission.Response) {
	t.Helper()
	if !resp.Allowed {
		t.Fatalf("expected the pod to be ADMITTED unpinned, got denied: %q", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patch on an admitted-unpinned pod, got %v", resp.Patches)
	}
}

// podScopedIndex matches by pod label, so the whole-pod fractional-CPU gate runs.
func podScopedIndex(t *testing.T) *match.Index {
	t.Helper()
	idx := match.NewIndex()
	idx.Update([]match.PolicyRule{{
		Namespaces:        []string{"inference"},
		ByPodLabels:       map[string]string{"app": "llm"},
		EnforcePinning:    true,
		Intent:            fixtureIntent(t),
		CapacityCheckMode: "warn",
	}})
	return idx
}

// globContainerIndex matches containers by glob, so several matches are ambiguous under wholePod.
func globContainerIndex(t *testing.T) *match.Index {
	t.Helper()
	idx := match.NewIndex()
	idx.Update([]match.PolicyRule{{
		Namespaces:        []string{"inference"},
		ByContainerName:   []string{"vllm*"},
		EnforcePinning:    true,
		Intent:            fixtureIntent(t),
		CapacityCheckMode: "warn",
	}})
	return idx
}

func fixtureIntent(t *testing.T) match.IntentRules {
	t.Helper()
	rules, err := match.CompileIntent(cpopv1alpha1.IntentSpec{
		Span: cpopv1alpha1.IntentSourceSpec{
			Annotation: "cpu-pinning-operator.intel.com/tp",
			Default:    1,
		},
	})
	if err != nil {
		t.Fatalf("compile intent: %v", err)
	}
	return rules
}

// assertSanitizedDenial checks an enforcePinning denial: Allowed:false with Forbidden (not
// 500), no patch, no internal error text, and a message naming cause.
func assertSanitizedDenial(t *testing.T, resp admission.Response, cause string) {
	t.Helper()
	if resp.Allowed {
		t.Fatalf("expected the pod to be DENIED under enforcePinning, got Allowed=true")
	}
	if resp.Result == nil {
		t.Fatal("denial carries no Result")
	}
	if resp.Result.Code != http.StatusForbidden {
		t.Errorf("denial Result.Code = %d, want %d (admission.Denied, never admission.Errored)",
			resp.Result.Code, http.StatusForbidden)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("denial returned %d patches, want 0", len(resp.Patches))
	}
	if strings.Contains(resp.Result.Message, rawErrText) {
		t.Errorf("denial message leaked internal error text: %q", resp.Result.Message)
	}
	if !strings.Contains(resp.Result.Message, "enforcePinning") {
		t.Errorf("denial message should name enforcePinning so the operator can act on it, got %q", resp.Result.Message)
	}
	if !strings.Contains(resp.Result.Message, cause) {
		t.Errorf("denial message should name %q, got %q", cause, resp.Result.Message)
	}
}

// assertWarningEvent requires an event with this type and reason whose message contains msg.
func assertWarningEvent(t *testing.T, events []string, reason, msg string) {
	t.Helper()
	want := "Warning " + reason + " "
	for _, e := range events {
		if strings.HasPrefix(e, want) && strings.Contains(e, msg) {
			return
		}
	}
	t.Errorf("expected a %q event containing %q; events=%v", want, msg, events)
}

// TestEnforcePinningDeniesOnNodeLookupError: a failed Node read under enforcePinning denies,
// sanitized, and is not counted as a bypass.
func TestEnforcePinningDeniesOnNodeLookupError(t *testing.T) {
	before := bypassCount("inference", metrics.BypassNodeLookup)

	m, rec := newHandleMutatorFailingList(t, true,
		labeledNode("n1", true, "2"),
	)
	resp := admitPod(t, m, "vllm-x", "2", "1")

	assertSanitizedDenial(t, resp, metrics.BypassNodeLookup)
	if got := bypassCount("inference", metrics.BypassNodeLookup) - before; got != 0 {
		t.Errorf("enforce denial incremented webhook_bypasses_total by %v, want 0", got)
	}
	events := drainEvents(rec)
	assertWarningEvent(t, events, "PinningDenied", metrics.BypassNodeLookup)
	for _, e := range events {
		if strings.Contains(e, rawErrText) {
			t.Errorf("PinningDenied event leaked internal error text: %q", e)
		}
	}
}

// TestEnforcePinningDeniesPreScheduledOnNodeReadError covers the bound-pod Get call site.
func TestEnforcePinningDeniesPreScheduledOnNodeReadError(t *testing.T) {
	before := bypassCount("inference", metrics.BypassNodeLookup)

	enforcing, rec := newHandleMutatorFailingGet(t, true, labeledNode("n1", true, "2"))
	resp := admitCustomPod(t, enforcing, matchedPod("vllm-bound-err", "n1", "2", "vllm"))
	assertSanitizedDenial(t, resp, metrics.BypassNodeLookup)
	assertWarningEvent(t, drainEvents(rec), "PinningDenied", metrics.BypassNodeLookup)
	if got := bypassCount("inference", metrics.BypassNodeLookup) - before; got != 0 {
		t.Errorf("enforce denial incremented webhook_bypasses_total by %v, want 0", got)
	}

	bestEffort, _ := newHandleMutatorFailingGet(t, false, labeledNode("n1", true, "2"))
	admitted := admitCustomPod(t, bestEffort, matchedPod("vllm-bound-err", "n1", "2", "vllm"))
	if !admitted.Allowed || len(admitted.Patches) != 0 {
		t.Errorf("best-effort policy must admit unmutated, got allowed=%v patches=%d", admitted.Allowed, len(admitted.Patches))
	}
	if got := bypassCount("inference", metrics.BypassNodeLookup) - before; got != 1 {
		t.Errorf("best-effort path incremented webhook_bypasses_total by %v, want 1", got)
	}
}

// TestEnforceDeniesWhereBestEffortAdmits: same pod and failure, differing only in enforcePinning.
func TestEnforceDeniesWhereBestEffortAdmits(t *testing.T) {
	// One baseline for both halves.
	before := bypassCount("inference", metrics.BypassNodeLookup)

	enforcing, _ := newHandleMutatorFailingList(t, true, labeledNode("n1", true, "2"))
	enforced := admitPod(t, enforcing, "vllm-split", "2", "1")
	if got := bypassCount("inference", metrics.BypassNodeLookup) - before; got != 0 {
		t.Errorf("enforce half incremented webhook_bypasses_total by %v, want 0", got)
	}

	bestEffort, _ := newHandleMutatorFailingList(t, false, labeledNode("n1", true, "2"))
	admitted := admitPod(t, bestEffort, "vllm-split", "2", "1")

	assertSanitizedDenial(t, enforced, metrics.BypassNodeLookup)
	if !admitted.Allowed {
		t.Errorf("best-effort policy denied a pod: %q — best-effort must keep failing open", admitted.Result.Message)
	}
	if len(admitted.Patches) != 0 {
		t.Errorf("best-effort fail-open returned %d patches, want 0 (admitted unmutated)", len(admitted.Patches))
	}
	if got := bypassCount("inference", metrics.BypassNodeLookup) - before; got != 1 {
		t.Errorf("best-effort path incremented webhook_bypasses_total by %v, want 1", got)
	}
}

// TestEnforcePinningDeniesWithNoProvisionedNode: no balloons-enabled node denies under
// enforcePinning; best-effort skips with reason no-balloon.
func TestEnforcePinningDeniesWithNoProvisionedNode(t *testing.T) {
	unprovisioned := labeledNode("n1", false, "8")
	skipped := skipCount(metrics.ReasonNoBalloon)

	enforcing, rec := newHandleMutatorEnforce(t, true, unprovisioned)
	resp := admitPod(t, enforcing, "vllm-nonode", "2", "1")
	assertSanitizedDenial(t, resp, "no balloons-enabled node")
	assertWarningEvent(t, drainEvents(rec), "PinningDenied", "no balloons-enabled node")
	if got := skipCount(metrics.ReasonNoBalloon) - skipped; got != 0 {
		t.Errorf("enforce denial incremented pods_skipped_total{no-balloon} by %v, want 0", got)
	}

	// A refusal, not an operator failure.
	if _, result := enforcing.handle(context.Background(), podRequest(t, matchedPod("vllm-nonode2", "", "2", "vllm"))); result != resultDenied {
		t.Errorf("no-provisioned-node denial classified as %q, want %q", result, resultDenied)
	}

	bestEffort, _ := newHandleMutatorEnforce(t, false, unprovisioned)
	admitted := admitPod(t, bestEffort, "vllm-nonode", "2", "1")
	if !admitted.Allowed || len(admitted.Patches) != 0 {
		t.Errorf("best-effort policy must admit unpinned, got allowed=%v patches=%d", admitted.Allowed, len(admitted.Patches))
	}
	if got := skipCount(metrics.ReasonNoBalloon) - skipped; got != 1 {
		t.Errorf("best-effort skip incremented pods_skipped_total{no-balloon} by %v, want 1", got)
	}
}

// TestEnforcePinningKeepsReadinessGateWhenPluginNotReady: warn mode drops the init gate,
// but enforcePinning keeps it (pinned and waiting, not denied).
func TestEnforcePinningKeepsReadinessGateWhenPluginNotReady(t *testing.T) {
	srv := notReadyServer(t)
	t.Setenv("BALLOONS_READY_ENDPOINT", srv.URL)

	t.Setenv("INIT_CONTAINER_IMAGE", testInitImage)

	enforcing, rec := newHandleMutatorEnforce(t, true, labeledNode("n1", true, "2"))
	resp := admitScheduledPod(t, enforcing)
	if !resp.Allowed {
		t.Fatalf("a not-ready plugin must not deny under enforcePinning + warn (the gate covers it), got %q", resp.Result.Message)
	}
	if !patchesTouchInitContainers(resp) {
		t.Error("enforcePinning must keep the wait-for-balloons gate when the plugin is not ready")
	}
	if stampedBalloon(resp) == "" {
		t.Error("the pod must still be stamped with its balloon annotation")
	}
	assertWarningEvent(t, drainEvents(rec), "PluginNotReady", "held in Init")

	bestEffort, _ := newHandleMutatorEnforce(t, false, labeledNode("n1", true, "2"))
	admitted := admitScheduledPod(t, bestEffort)
	if !admitted.Allowed {
		t.Errorf("best-effort policy must keep the warn behaviour, got denied: %q", admitted.Result.Message)
	}
	if patchesTouchInitContainers(admitted) {
		t.Error("best-effort warn mode must not inject the init container when the plugin is not ready")
	}
	if stampedBalloon(admitted) == "" {
		t.Error("best-effort warn mode must still pin the pod, it only drops the gate")
	}
}

// TestEnforcePinningAdmitsNotPinnableMatches: matches that are unpinnable by design
// (burstable, ambiguous glob, excluded node) stay admitted unpinned under enforcePinning.
func TestEnforcePinningAdmitsNotPinnableMatches(t *testing.T) {
	t.Run("fractional CPU request", func(t *testing.T) {
		m, rec := newHandleMutatorEnforce(t, true, labeledNode("n1", true, "2"))
		before := skipCount(metrics.ReasonFractional)
		resp := admitCustomPod(t, m, matchedPod("vllm-frac", "", "500m", "vllm"))
		assertAdmittedUnpinned(t, resp)
		assertWarningEvent(t, drainEvents(rec), "NotPinned", "unpinned")
		if got := skipCount(metrics.ReasonFractional) - before; got != 1 {
			t.Errorf("pods_skipped_total{fractional} increment = %v, want 1", got)
		}
	})

	t.Run("fractional CPU request, pod-scoped match", func(t *testing.T) {

		m, rec := newHandleMutatorEnforce(t, true, labeledNode("n1", true, "2"))
		m.PolicyIndex = podScopedIndex(t)
		before := skipCount(metrics.ReasonFractional)
		pod := matchedPod("vllm-frac-pod", "", "500m", "vllm")
		pod.Labels = map[string]string{"app": "llm"}
		assertAdmittedUnpinned(t, admitCustomPod(t, m, pod))
		assertWarningEvent(t, drainEvents(rec), "NotPinned", "unpinned")
		if got := skipCount(metrics.ReasonFractional) - before; got != 1 {
			t.Errorf("pods_skipped_total{fractional} increment = %v, want 1", got)
		}
	})

	t.Run("ambiguous wholePod match", func(t *testing.T) {
		m, rec := newHandleMutatorEnforce(t, true, labeledNode("n1", true, "2"))
		before := skipCount(metrics.ReasonAmbiguous)

		m.PolicyIndex = globContainerIndex(t)
		resp := admitCustomPod(t, m, matchedPod("vllm-ambig", "", "2", "vllm", "vllm-2"))
		assertAdmittedUnpinned(t, resp)
		assertWarningEvent(t, drainEvents(rec), "NotPinned", "unpinned")
		if got := skipCount(metrics.ReasonAmbiguous) - before; got != 1 {
			t.Errorf("pods_skipped_total{ambiguous} increment = %v, want 1", got)
		}
	})

	t.Run("pre-bound to a node that is not balloons-enabled", func(t *testing.T) {
		m, rec := newHandleMutatorEnforce(t, true, labeledNode("n1", false, "2"))
		before := skipCount(metrics.ReasonExcluded)
		resp := admitCustomPod(t, m, matchedPod("vllm-bound", "n1", "2", "vllm"))
		assertAdmittedUnpinned(t, resp)
		assertWarningEvent(t, drainEvents(rec), "NotPinned", "unpinned")
		if got := skipCount(metrics.ReasonExcluded) - before; got != 1 {
			t.Errorf("pods_skipped_total{excluded} increment = %v, want 1", got)
		}
	})
}

// TestNilDecoderAdmitsUnmutated: with no decoder, admit unmutated and count it.
func TestNilDecoderAdmitsUnmutated(t *testing.T) {
	m, _ := newHandleMutatorEnforce(t, true, labeledNode("n1", true, "2"))
	m.decoder = nil
	before := bypassCount("inference", metrics.BypassDecode)

	resp := admitPod(t, m, "vllm-nodecoder", "2", "1")
	if !resp.Allowed {
		t.Fatalf("an uninjected decoder must fail open, got denied: %q", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("got %d patches with no decoder, want 0", len(resp.Patches))
	}
	if got := bypassCount("inference", metrics.BypassDecode) - before; got != 1 {
		t.Errorf("webhook_bypasses_total{reason=decode} increment = %v, want 1", got)
	}
}

// TestUnmatchedPodNeverDeniedUnderEnforce: an unselected pod passes even when Node reads fail.
func TestUnmatchedPodNeverDeniedUnderEnforce(t *testing.T) {
	m, _ := newHandleMutatorFailingList(t, true, labeledNode("n1", true, "2"))
	nonTarget := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sidecar-only", Namespace: "inference"},
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
		t.Fatalf("a pod no policy selects was DENIED: %q", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("unmatched pod got %d patches, want 0", len(resp.Patches))
	}
}

// TestUndecodableRequestAdmitsUnmutated: with no pod, enforcePinning is unknown, so fail
// open (counted) rather than deny every pod in selected namespaces.
func TestUndecodableRequestAdmitsUnmutated(t *testing.T) {
	m, _ := newHandleMutatorEnforce(t, true, labeledNode("n1", true, "2"))
	before := bypassCount("inference", metrics.BypassDecode)

	resp := m.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		Operation: admissionv1.Create,
		Namespace: "inference",
		Object:    runtime.RawExtension{Raw: []byte(`{`)},
	}})

	if !resp.Allowed {
		t.Fatalf("an undecodable request must fail open, got denied: %q", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("undecodable request got %d patches, want 0", len(resp.Patches))
	}
	if got := bypassCount("inference", metrics.BypassDecode) - before; got != 1 {
		t.Errorf("webhook_bypasses_total{reason=decode} increment = %v, want 1", got)
	}
}

// TestNilPolicyIndexAdmitsUnmutated: no compiled policies means nothing to deny.
func TestNilPolicyIndexAdmitsUnmutated(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	m := &PodMutator{Recorder: record.NewFakeRecorder(16)}
	if err := m.InjectDecoder(admission.NewDecoder(scheme)); err != nil {
		t.Fatalf("inject decoder: %v", err)
	}
	before := bypassCount("inference", metrics.BypassPolicyIndex)

	// The pod decoded, so the counter uses its namespace, not the request's.
	resp := admitPodInRequestNamespace(t, m, "vllm-noindex", "other-ns")
	if !resp.Allowed {
		t.Fatalf("expected fail-open admit with no policy index, got denied: %q", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("got %d patches with no policy index, want 0", len(resp.Patches))
	}
	if got := bypassCount("inference", metrics.BypassPolicyIndex) - before; got != 1 {
		t.Errorf("webhook_bypasses_total{reason=policy-index} increment = %v, want 1", got)
	}
}

// TestAbortMatchedVerdictIndependentOfStage: the verdict depends on the policy, not the stage.
func TestAbortMatchedVerdictIndependentOfStage(t *testing.T) {
	stages := []string{
		metrics.BypassNodeLookup,
		metrics.BypassCapacityCheck,
		metrics.BypassMarshal,
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vllm-stage", Namespace: "inference"}}
	err := fmt.Errorf("%s", rawErrText)

	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			m := &PodMutator{Recorder: record.NewFakeRecorder(16)}
			before := bypassCount("inference", stage)

			resp, result := m.abortMatched(pod, true, stage, err)
			assertSanitizedDenial(t, resp, stage)
			if result != resultError {
				t.Errorf("enforce abort classified as %q, want %q", result, resultError)
			}
			if got := bypassCount("inference", stage) - before; got != 0 {
				t.Errorf("enforce abort incremented webhook_bypasses_total{%s} by %v, want 0", stage, got)
			}

			resp, _ = m.abortMatched(pod, false, stage, err)
			if !resp.Allowed {
				t.Errorf("best-effort abort denied the pod: %q", resp.Result.Message)
			}
			if got := bypassCount("inference", stage) - before; got != 1 {
				t.Errorf("webhook_bypasses_total{reason=%s} increment = %v, want 1", stage, got)
			}
		})
	}
}
