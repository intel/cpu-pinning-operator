// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
)

// newPolicyGuard builds a PolicyGuard with a decoder and a fake client seeded with objs.
func newPolicyGuard(t *testing.T, objs ...runtime.Object) *PolicyGuard {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	g := &PolicyGuard{Client: c}
	if err := g.InjectDecoder(admission.NewDecoder(scheme)); err != nil {
		t.Fatalf("inject decoder: %v", err)
	}
	return g
}

// guardedPolicy builds a BalloonPinningPolicy matching container "vllm" in the
// given namespaces, with the given guard and bound pods.
func guardedPolicy(guard bool, namespaces []string, containers []string, bound ...cpopv1alpha1.BoundPod) *cpopv1alpha1.BalloonPinningPolicy {
	p := &cpopv1alpha1.BalloonPinningPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: cpopv1alpha1.BalloonPinningPolicySpec{
			Match:      cpopv1alpha1.MatchSpec{ByContainerName: containers},
			Namespaces: namespaces,
		},
		Status: cpopv1alpha1.BalloonPinningPolicyStatus{BoundPods: bound, BoundPodsTotal: len(bound)},
	}
	if guard {
		g := true
		p.Spec.GuardActivePods = &g
	}
	return p
}

// vllmPod builds an unlabeled pod with a "vllm" container in the given namespace.
func vllmPod(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm"}}},
	}
}

// deleteReq builds a DELETE admission request carrying `old` in OldObject.
func deleteReq(t *testing.T, old *cpopv1alpha1.BalloonPinningPolicy) admission.Request {
	t.Helper()
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("marshal old: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
		OldObject: runtime.RawExtension{Raw: raw},
	}}
}

// updateReq builds an UPDATE admission request with old + new objects.
func updateReq(t *testing.T, old, updated *cpopv1alpha1.BalloonPinningPolicy) admission.Request {
	t.Helper()
	oldRaw, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("marshal old: %v", err)
	}
	newRaw, err := json.Marshal(updated)
	if err != nil {
		t.Fatalf("marshal new: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		OldObject: runtime.RawExtension{Raw: oldRaw},
		Object:    runtime.RawExtension{Raw: newRaw},
	}}
}

// (a) DELETE rejected when guarded + dependents exist.
func TestGuardDeleteRejectedWithDependents(t *testing.T) {
	old := guardedPolicy(true, []string{"inference"}, []string{"vllm"},
		cpopv1alpha1.BoundPod{Namespace: "inference", Name: "vllm-0", Balloon: "balloon-span1"})
	g := newPolicyGuard(t)
	resp := g.Handle(context.Background(), deleteReq(t, old))
	if resp.Allowed {
		t.Fatalf("delete of a guarded policy with dependent pods must be denied, got Allowed")
	}
	if !strings.Contains(resp.Result.Message, "inference/vllm-0") {
		t.Errorf("denial should name the blocking pod, got %q", resp.Result.Message)
	}
}

// (b) DELETE allowed when guard off.
func TestGuardDeleteAllowedWhenGuardOff(t *testing.T) {
	old := guardedPolicy(false, []string{"inference"}, []string{"vllm"},
		cpopv1alpha1.BoundPod{Namespace: "inference", Name: "vllm-0", Balloon: "balloon-span1"})
	g := newPolicyGuard(t)
	if resp := g.Handle(context.Background(), deleteReq(t, old)); !resp.Allowed {
		t.Fatalf("delete must be allowed when guardActivePods is unset, got denied: %q", resp.Result.Message)
	}
}

// (c) DELETE allowed when guarded but no dependents.
func TestGuardDeleteAllowedWhenNoDependents(t *testing.T) {
	old := guardedPolicy(true, []string{"inference"}, []string{"vllm"})
	g := newPolicyGuard(t)
	if resp := g.Handle(context.Background(), deleteReq(t, old)); !resp.Allowed {
		t.Fatalf("delete must be allowed when the guarded policy holds no pods, got denied: %q", resp.Result.Message)
	}
}

// (d) narrowing UPDATE rejected: the new spec drops the bound pod's namespace.
func TestGuardUpdateNarrowingRejected(t *testing.T) {
	bound := cpopv1alpha1.BoundPod{Namespace: "inference", Name: "vllm-0", Balloon: "balloon-span1"}
	old := guardedPolicy(true, []string{"inference"}, []string{"vllm"}, bound)
	// Narrow to "other", orphaning the pod in "inference".
	updated := guardedPolicy(true, []string{"other"}, []string{"vllm"}, bound)
	g := newPolicyGuard(t, vllmPod("inference", "vllm-0"))
	resp := g.Handle(context.Background(), updateReq(t, old, updated))
	if resp.Allowed {
		t.Fatalf("a narrowing update must be denied, got Allowed")
	}
	if !strings.Contains(resp.Result.Message, "inference/vllm-0") {
		t.Errorf("denial should name the orphaned pod, got %q", resp.Result.Message)
	}
}

// (e) widening UPDATE allowed: the new spec still covers the bound pod.
func TestGuardUpdateWideningAllowed(t *testing.T) {
	bound := cpopv1alpha1.BoundPod{Namespace: "inference", Name: "vllm-0", Balloon: "balloon-span1"}
	old := guardedPolicy(true, []string{"inference"}, []string{"vllm"}, bound)
	updated := guardedPolicy(true, []string{"inference", "other"}, []string{"vllm"}, bound)
	g := newPolicyGuard(t, vllmPod("inference", "vllm-0"))
	if resp := g.Handle(context.Background(), updateReq(t, old, updated)); !resp.Allowed {
		t.Fatalf("a widening update that still covers the bound pod must be allowed, got denied: %q", resp.Result.Message)
	}
}

// A guarded policy with a capped boundPods list fails closed on any update, even a
// widening: the elided pods cannot be re-matched.
func TestGuardUpdateCappedFailsClosed(t *testing.T) {
	bound := make([]cpopv1alpha1.BoundPod, 100)
	for i := range bound {
		bound[i] = cpopv1alpha1.BoundPod{Namespace: "inference", Name: fmt.Sprintf("vllm-%03d", i), Balloon: "balloon-span1"}
	}
	old := guardedPolicy(true, []string{"inference"}, []string{"vllm"}, bound...)
	old.Status.BoundPodsTotal = 250 // capped: 150 elided pods have no identity here
	// A pure widening, still denied while capped.
	updated := guardedPolicy(true, []string{"inference", "other"}, []string{"vllm"}, bound...)
	updated.Status.BoundPodsTotal = 250
	// Seed every retained pod so the denial can only come from the cap.
	objs := make([]runtime.Object, len(bound))
	for i := range bound {
		objs[i] = vllmPod(bound[i].Namespace, bound[i].Name)
	}
	g := newPolicyGuard(t, objs...)
	resp := g.Handle(context.Background(), updateReq(t, old, updated))
	if resp.Allowed {
		t.Fatalf("an update on a capped guarded policy must fail closed, got Allowed")
	}
	if !strings.Contains(resp.Result.Message, "250 pinned pod") {
		t.Errorf("denial should report the true held count 250, got %q", resp.Result.Message)
	}
	if !strings.Contains(resp.Result.Message, "guardActivePods=false") {
		t.Errorf("denial should point to the escape hatch, got %q", resp.Result.Message)
	}
}

// Unsetting guardActivePods releases even a capped policy.
func TestGuardUpdateCappedUnsettingGuardAllowed(t *testing.T) {
	bound := make([]cpopv1alpha1.BoundPod, 100)
	for i := range bound {
		bound[i] = cpopv1alpha1.BoundPod{Namespace: "inference", Name: fmt.Sprintf("vllm-%03d", i), Balloon: "balloon-span1"}
	}
	old := guardedPolicy(true, []string{"inference"}, []string{"vllm"}, bound...)
	old.Status.BoundPodsTotal = 250
	updated := guardedPolicy(false, []string{"other"}, []string{"vllm"}, bound...)
	off := false
	updated.Spec.GuardActivePods = &off
	updated.Status.BoundPodsTotal = 250
	g := newPolicyGuard(t)
	if resp := g.Handle(context.Background(), updateReq(t, old, updated)); !resp.Allowed {
		t.Fatalf("unsetting the guard must be allowed even when capped, got denied: %q", resp.Result.Message)
	}
}

// (f) unsetting guardActivePods is allowed even while narrowing in the same edit.
func TestGuardUpdateUnsettingGuardAllowed(t *testing.T) {
	bound := cpopv1alpha1.BoundPod{Namespace: "inference", Name: "vllm-0", Balloon: "balloon-span1"}
	old := guardedPolicy(true, []string{"inference"}, []string{"vllm"}, bound)
	// Explicit false, as the docs and chart examples use, plus a narrowing.
	updated := guardedPolicy(false, []string{"other"}, []string{"vllm"}, bound)
	off := false
	updated.Spec.GuardActivePods = &off
	g := newPolicyGuard(t, vllmPod("inference", "vllm-0"))
	if resp := g.Handle(context.Background(), updateReq(t, old, updated)); !resp.Allowed {
		t.Fatalf("unsetting guardActivePods must be allowed as the escape hatch, got denied: %q", resp.Result.Message)
	}
}

// An update on a never-guarded policy is unaffected regardless of narrowing.
func TestGuardUpdateUnguardedAllowed(t *testing.T) {
	bound := cpopv1alpha1.BoundPod{Namespace: "inference", Name: "vllm-0", Balloon: "balloon-span1"}
	old := guardedPolicy(false, []string{"inference"}, []string{"vllm"}, bound)
	updated := guardedPolicy(false, []string{"other"}, []string{"vllm"}, bound)
	g := newPolicyGuard(t, vllmPod("inference", "vllm-0"))
	if resp := g.Handle(context.Background(), updateReq(t, old, updated)); !resp.Allowed {
		t.Fatalf("an update on an unguarded policy must always be allowed, got denied: %q", resp.Result.Message)
	}
}

// A bound pod that no longer exists cannot make an update a narrowing.
func TestGuardUpdateBoundPodGoneAllowed(t *testing.T) {
	bound := cpopv1alpha1.BoundPod{Namespace: "inference", Name: "vllm-gone", Balloon: "balloon-span1"}
	old := guardedPolicy(true, []string{"inference"}, []string{"vllm"}, bound)
	updated := guardedPolicy(true, []string{"other"}, []string{"vllm"}, bound)
	// No pod seeded: the bound pod is gone.
	g := newPolicyGuard(t)
	if resp := g.Handle(context.Background(), updateReq(t, old, updated)); !resp.Allowed {
		t.Fatalf("an already-gone bound pod must not block an update, got denied: %q", resp.Result.Message)
	}
}

// An update matching a bound pod only ambiguously (byContainerName hitting several
// containers under wholePod) is a narrowing: an ambiguous match is a de-match.
func TestGuardUpdateAmbiguousMatchRejected(t *testing.T) {
	bound := cpopv1alpha1.BoundPod{Namespace: "inference", Name: "vllm-0", Balloon: "balloon-span1"}
	old := guardedPolicy(true, []string{"inference"}, []string{"vllm"}, bound)
	// The glob matches both containers: ambiguous under wholePod.
	updated := guardedPolicy(true, []string{"inference"}, []string{"vllm*"}, bound)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "inference", Name: "vllm-0"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm"}, {Name: "vllm-sidecar"}}},
	}
	g := newPolicyGuard(t, pod)
	if resp := g.Handle(context.Background(), updateReq(t, old, updated)); resp.Allowed {
		t.Fatalf("an update that makes a bound pod match only ambiguously must be denied")
	}
}

// A terminating or finished bound pod holds no cpuset, so it never blocks an update.
func TestGuardUpdateTerminatingBoundPodAllowed(t *testing.T) {
	bound := cpopv1alpha1.BoundPod{Namespace: "inference", Name: "vllm-0", Balloon: "balloon-span1"}
	old := guardedPolicy(true, []string{"inference"}, []string{"vllm"}, bound)
	updated := guardedPolicy(true, []string{"other"}, []string{"vllm"}, bound)
	pod := vllmPod("inference", "vllm-0")
	pod.Status.Phase = corev1.PodSucceeded
	g := newPolicyGuard(t, pod)
	if resp := g.Handle(context.Background(), updateReq(t, old, updated)); !resp.Allowed {
		t.Fatalf("a finished bound pod must not block a narrowing update, got denied: %q", resp.Result.Message)
	}
}

// A large bound-pod list is truncated in the denial message with a "+N more".
func TestJoinBoundPodsTruncates(t *testing.T) {
	var pods []cpopv1alpha1.BoundPod
	for i := 0; i < maxListedBoundPods+5; i++ {
		pods = append(pods, cpopv1alpha1.BoundPod{Namespace: "inference", Name: fmt.Sprintf("vllm-%d", i)})
	}
	got := joinBoundPods(pods, len(pods))
	if !strings.HasSuffix(got, ", +5 more") {
		t.Fatalf("expected a %q suffix, got %q", ", +5 more", got)
	}
	if n := strings.Count(got, "inference/"); n != maxListedBoundPods {
		t.Fatalf("expected %d listed pods, got %d in %q", maxListedBoundPods, n, got)
	}

	small := pods[:3]
	if got := joinBoundPods(small, len(small)); strings.Contains(got, "more") || strings.Count(got, "inference/") != 3 {
		t.Fatalf("a short list must render in full without a suffix, got %q", got)
	}
}

// The "+N more" suffix counts the true held total, not the capped slice length.
func TestJoinBoundPodsSuffixUsesTrueTotal(t *testing.T) {
	// 100 retained, 250 held: 10 named, "+240 more".
	retained := make([]cpopv1alpha1.BoundPod, 100)
	for i := range retained {
		retained[i] = cpopv1alpha1.BoundPod{Namespace: "inference", Name: fmt.Sprintf("vllm-%03d", i)}
	}
	got := joinBoundPods(retained, 250)
	wantSuffix := fmt.Sprintf(", +%d more", 250-maxListedBoundPods)
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("expected suffix %q reflecting the true total, got %q", wantSuffix, got)
	}
	if n := strings.Count(got, "inference/"); n != maxListedBoundPods {
		t.Fatalf("expected %d listed pods, got %d in %q", maxListedBoundPods, n, got)
	}
}

// A capped boundPods list reports the true held count in the denial.
func TestGuardDeleteReportsTrueCappedTotal(t *testing.T) {
	bound := make([]cpopv1alpha1.BoundPod, 100)
	for i := range bound {
		bound[i] = cpopv1alpha1.BoundPod{Namespace: "inference", Name: fmt.Sprintf("vllm-%03d", i), Balloon: "balloon-span1"}
	}
	old := guardedPolicy(true, []string{"inference"}, []string{"vllm"}, bound...)
	old.Status.BoundPodsTotal = 250
	g := newPolicyGuard(t)
	resp := g.Handle(context.Background(), deleteReq(t, old))
	if resp.Allowed {
		t.Fatalf("a capped guarded policy holding pods must be denied delete")
	}
	if !strings.Contains(resp.Result.Message, "250 pinned pod") {
		t.Errorf("denial should report the true uncapped count 250, got %q", resp.Result.Message)
	}
	// The suffix must reflect 250, not 100.
	wantSuffix := fmt.Sprintf(", +%d more", 250-maxListedBoundPods)
	if !strings.Contains(resp.Result.Message, wantSuffix) {
		t.Errorf("denial's pod-list suffix should reflect the true total (%q), got %q", wantSuffix, resp.Result.Message)
	}
}

// TestGuardDecodeErrorSanitized proves a decode error is not echoed to the requester.
func TestGuardDecodeErrorSanitized(t *testing.T) {
	g := newPolicyGuard(t)
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
		OldObject: runtime.RawExtension{Raw: []byte(`{"spec":`)}, // truncated JSON
	}}
	resp := g.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("a decode failure must not be allowed")
	}
	if resp.Result.Message != errCouldNotDecodePolicy.Error() {
		t.Errorf("decode error must be sanitized to the generic message, got %q", resp.Result.Message)
	}
}

// TestGuardAPIReadErrorSanitized proves an API read error, which can carry
// RBAC/apiserver detail, is logged and never echoed.
func TestGuardAPIReadErrorSanitized(t *testing.T) {
	const rawErr = "simulated apiserver read failure with secret detail"
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				return fmt.Errorf(rawErr)
			},
		}).Build()
	g := &PolicyGuard{Client: c}
	if err := g.InjectDecoder(admission.NewDecoder(scheme)); err != nil {
		t.Fatalf("inject decoder: %v", err)
	}

	// Guarded, non-narrowing update so the handler reaches the bound-pod Get loop.
	bound := cpopv1alpha1.BoundPod{Namespace: "inference", Name: "vllm-0"}
	old := guardedPolicy(true, []string{"inference"}, []string{"vllm"}, bound)
	updated := guardedPolicy(true, []string{"inference"}, []string{"vllm"}, bound)
	resp := g.Handle(context.Background(), updateReq(t, old, updated))

	if resp.Allowed {
		t.Fatal("an API read error must fail closed (not allowed)")
	}
	if strings.Contains(resp.Result.Message, rawErr) {
		t.Errorf("raw API error text leaked into the admission response: %q", resp.Result.Message)
	}
	if resp.Result.Message != errPolicyGuardInternal.Error() {
		t.Errorf("API read error must be sanitized to the generic message, got %q", resp.Result.Message)
	}
}
