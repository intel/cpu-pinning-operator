// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
)

// pinningPolicyReconciler builds a BalloonPinningPolicyReconciler over a fake client and its Index.
func pinningPolicyReconciler(objs ...runtime.Object) (*BalloonPinningPolicyReconciler, *match.Index) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	_ = admissionregistrationv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&cpopv1alpha1.BalloonPinningPolicy{}).
		// Same pinned-by field index SetupWithManager registers.
		WithIndex(&corev1.Pod{}, PodPinnedByField, indexPodByPinnedBy).
		WithRuntimeObjects(objs...).
		Build()
	idx := match.NewIndex()
	return &BalloonPinningPolicyReconciler{
		Client:      c,
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: idx,
	}, idx
}

// webhookConfig returns the MutatingWebhookConfiguration the reconciler patches.
func webhookConfig() *admissionregistrationv1.MutatingWebhookConfiguration {
	return &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: webhookConfigName()},
		Webhooks: []admissionregistrationv1.MutatingWebhook{
			{Name: webhookName},
		},
	}
}

// gateNamespaces returns the sorted metadata.name In[...] values of the webhook's namespaceSelector.
func gateNamespaces(t *testing.T, r *BalloonPinningPolicyReconciler) []string {
	t.Helper()
	cfg := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: webhookConfigName()}, cfg); err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	for i := range cfg.Webhooks {
		if cfg.Webhooks[i].Name != webhookName {
			continue
		}
		sel := cfg.Webhooks[i].NamespaceSelector
		if sel == nil {
			return nil
		}
		for _, req := range sel.MatchExpressions {
			if req.Key == "kubernetes.io/metadata.name" {
				out := append([]string(nil), req.Values...)
				sort.Strings(out)
				return out
			}
		}
	}
	t.Fatalf("webhook %q not found in config", webhookName)
	return nil
}

// gateSelector returns the webhook's raw namespaceSelector, to tell a nil selector
// (matches all) from the DoesNotExist closed gate (matches none).
func gateSelector(t *testing.T, r *BalloonPinningPolicyReconciler) *metav1.LabelSelector {
	t.Helper()
	cfg := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: webhookConfigName()}, cfg); err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	for i := range cfg.Webhooks {
		if cfg.Webhooks[i].Name == webhookName {
			return cfg.Webhooks[i].NamespaceSelector
		}
	}
	t.Fatalf("webhook %q not found in config", webhookName)
	return nil
}

func nsWithLabels(name string, lbls map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbls}}
}

// matchTargetContainer is the container name the test policies match on.
const matchTargetContainer = "vllm"

// namespaceMatches reports whether idx.Match admits a pod in ns.
func namespaceMatches(idx *match.Index, ns string) bool {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: matchTargetContainer}}},
	}
	return idx.Match(pod).Rule != nil
}

func strSlice(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestNamespaceSelector_Default_ExactNameMode: a nil selector uses only exact names and globs.
func TestNamespaceSelector_Default_ExactNameMode(t *testing.T) {
	policy := &cpopv1alpha1.BalloonPinningPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: cpopv1alpha1.BalloonPinningPolicySpec{
			Match:      cpopv1alpha1.MatchSpec{ByContainerName: []string{matchTargetContainer}},
			Namespaces: []string{"inference"},
			// NamespaceSelector nil: exact-name mode.
		},
	}
	// A label a selector would match; a nil selector must not pull it in.
	labeled := nsWithLabels("team-a", map[string]string{"pinning": "enabled"})
	r, idx := pinningPolicyReconciler(policy, webhookConfig(), labeled)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := gateNamespaces(t, r); !slices.Equal(got, []string{"inference"}) {
		t.Errorf("gate namespaces = %v, want [inference]", got)
	}

	if namespaceMatches(idx, "team-a") {
		t.Error("team-a should not match in exact-name mode (nil selector)")
	}
	if !namespaceMatches(idx, "inference") {
		t.Error("inference should match its exact name")
	}
}

// TestNamespaceSelector_OptIn_MatchesByLabel: the selector resolves namespaces into
// both the webhook gate and the request-time filter.
func TestNamespaceSelector_OptIn_MatchesByLabel(t *testing.T) {
	policy := &cpopv1alpha1.BalloonPinningPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: cpopv1alpha1.BalloonPinningPolicySpec{
			Match: cpopv1alpha1.MatchSpec{ByContainerName: []string{matchTargetContainer}},
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"pinning": "enabled"},
			},
		},
	}
	yes1 := nsWithLabels("team-a", map[string]string{"pinning": "enabled"})
	yes2 := nsWithLabels("team-b", map[string]string{"pinning": "enabled", "extra": "1"})
	no := nsWithLabels("team-c", map[string]string{"pinning": "disabled"})
	r, idx := pinningPolicyReconciler(policy, webhookConfig(), yes1, yes2, no)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := gateNamespaces(t, r); !slices.Equal(got, []string{"team-a", "team-b"}) {
		t.Errorf("gate namespaces = %v, want [team-a team-b]", got)
	}

	for _, ns := range []string{"team-a", "team-b"} {
		if !namespaceMatches(idx, ns) {
			t.Errorf("%s should match by label selector", ns)
		}
	}
	if namespaceMatches(idx, "team-c") {
		t.Error("team-c (label mismatch) must not match")
	}
}

// TestNamespaceSelector_UnionWithExactNames: selector matches are added to exact names, deduped.
func TestNamespaceSelector_UnionWithExactNames(t *testing.T) {
	policy := &cpopv1alpha1.BalloonPinningPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: cpopv1alpha1.BalloonPinningPolicySpec{
			Match:      cpopv1alpha1.MatchSpec{ByContainerName: []string{matchTargetContainer}},
			Namespaces: []string{"inference", "team-a"}, // team-a also matches the label
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"pinning": "enabled"},
			},
		},
	}
	yesA := nsWithLabels("team-a", map[string]string{"pinning": "enabled"})
	yesB := nsWithLabels("team-b", map[string]string{"pinning": "enabled"})
	r, idx := pinningPolicyReconciler(policy, webhookConfig(), yesA, yesB)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	want := strSlice([]string{"inference", "team-a", "team-b"})
	if got := gateNamespaces(t, r); !slices.Equal(got, want) {
		t.Errorf("gate namespaces = %v, want %v", got, want)
	}
	for _, ns := range []string{"inference", "team-a", "team-b"} {
		if !namespaceMatches(idx, ns) {
			t.Errorf("%s should match (union of exact names + selector)", ns)
		}
	}
}

// TestNamespaceSelector_Empty_MatchesAll: an empty selector ({}) matches every namespace.
func TestNamespaceSelector_Empty_MatchesAll(t *testing.T) {
	policy := &cpopv1alpha1.BalloonPinningPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: cpopv1alpha1.BalloonPinningPolicySpec{
			Match:             cpopv1alpha1.MatchSpec{ByContainerName: []string{matchTargetContainer}},
			NamespaceSelector: &metav1.LabelSelector{}, // empty → all
		},
	}
	r, _ := pinningPolicyReconciler(policy, webhookConfig(),
		nsWithLabels("a", nil), nsWithLabels("b", map[string]string{"x": "y"}))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := gateNamespaces(t, r); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("gate namespaces = %v, want [a b] (empty selector matches all)", got)
	}
}

// TestNamespaceSelector_Invalid_MatchesNone: a malformed selector is logged, matches
// nothing, and does not fail the reconcile.
func TestNamespaceSelector_Invalid_MatchesNone(t *testing.T) {
	policy := &cpopv1alpha1.BalloonPinningPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: cpopv1alpha1.BalloonPinningPolicySpec{
			Match: cpopv1alpha1.MatchSpec{ByContainerName: []string{matchTargetContainer}},
			// In with no values is invalid.
			NamespaceSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "pinning", Operator: metav1.LabelSelectorOpIn},
				},
			},
		},
	}
	labeled := nsWithLabels("team-a", map[string]string{"pinning": "enabled"})
	r, idx := pinningPolicyReconciler(policy, webhookConfig(), labeled)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile must not fail on invalid selector: %v", err)
	}
	// A nil or empty selector would also show zero names but match everything; the
	// closed gate must be DoesNotExist on kubernetes.io/metadata.name.
	sel := gateSelector(t, r)
	if sel == nil {
		t.Fatal("namespaceSelector is nil — that matches ALL namespaces, not none")
	}
	if len(sel.MatchLabels) != 0 {
		t.Errorf("namespaceSelector has matchLabels %v — want none", sel.MatchLabels)
	}
	if len(sel.MatchExpressions) != 1 {
		t.Fatalf("namespaceSelector matchExpressions = %v, want exactly one (closed gate)", sel.MatchExpressions)
	}
	req := sel.MatchExpressions[0]
	if req.Key != "kubernetes.io/metadata.name" ||
		req.Operator != metav1.LabelSelectorOpDoesNotExist ||
		len(req.Values) != 0 {
		t.Errorf("closed-gate requirement = %+v, want {kubernetes.io/metadata.name DoesNotExist []}", req)
	}

	if got := gateNamespaces(t, r); len(got) != 0 {
		t.Errorf("gate namespaces = %v, want empty (invalid selector matches none)", got)
	}

	if namespaceMatches(idx, "team-a") {
		t.Error("team-a must not match: invalid selector matches no namespaces")
	}
}

// webhookFieldOf reads the webhook's namespaceSelector and failurePolicy.
func webhookFieldOf(t *testing.T, c client.Client) (*metav1.LabelSelector, *admissionregistrationv1.FailurePolicyType) {
	t.Helper()
	cfg := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: webhookConfigName()}, cfg); err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	for i := range cfg.Webhooks {
		if cfg.Webhooks[i].Name == webhookName {
			return cfg.Webhooks[i].NamespaceSelector, cfg.Webhooks[i].FailurePolicy
		}
	}
	t.Fatalf("webhook %q not found", webhookName)
	return nil, nil
}

// TestWebhookReconcilers_DisjointFieldsCoexist: the two reconcilers write disjoint
// fields of one webhook config, in either order, without dropping each other's.
// The fake client cannot model the atomic-list rejection; see webhook_reconcile_envtest_test.go.
func TestWebhookReconcilers_DisjointFieldsCoexist(t *testing.T) {
	run := func(t *testing.T, nsFirst bool) {
		scheme := runtime.NewScheme()
		_ = clientgoscheme.AddToScheme(scheme)
		_ = cpopv1alpha1.AddToScheme(scheme)
		_ = admissionregistrationv1.AddToScheme(scheme)

		policy := &cpopv1alpha1.BalloonPinningPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: cpopv1alpha1.BalloonPinningPolicySpec{
				Match:      cpopv1alpha1.MatchSpec{ByContainerName: []string{matchTargetContainer}},
				Namespaces: []string{"inference"},
			},
		}
		bcc := &cpopv1alpha1.BalloonsClusterConfig{
			ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
			Spec: cpopv1alpha1.BalloonsClusterConfigSpec{
				Webhook: cpopv1alpha1.WebhookSpec{FailurePolicy: string(admissionregistrationv1.Fail)},
			},
		}
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&cpopv1alpha1.BalloonPinningPolicy{}, &cpopv1alpha1.BalloonsClusterConfig{}).
			WithIndex(&corev1.Pod{}, PodPinnedByField, indexPodByPinnedBy).
			WithRuntimeObjects(policy, bcc, webhookConfig()).
			Build()

		nsRec := &BalloonPinningPolicyReconciler{Client: c, Log: ctrl.Log.WithName("test"), PolicyIndex: match.NewIndex()}
		fpRec := &BalloonsClusterConfigReconciler{Client: c, Log: ctrl.Log.WithName("test"), PolicyIndex: match.NewIndex()}

		reconcile := func() {
			if nsFirst {
				if _, err := nsRec.Reconcile(context.Background(), ctrl.Request{}); err != nil {
					t.Fatalf("ns reconcile: %v", err)
				}
				if _, err := fpRec.Reconcile(context.Background(), ctrl.Request{}); err != nil {
					t.Fatalf("fp reconcile: %v", err)
				}
			} else {
				if _, err := fpRec.Reconcile(context.Background(), ctrl.Request{}); err != nil {
					t.Fatalf("fp reconcile: %v", err)
				}
				if _, err := nsRec.Reconcile(context.Background(), ctrl.Request{}); err != nil {
					t.Fatalf("ns reconcile: %v", err)
				}
			}
		}
		reconcile()

		sel, fp := webhookFieldOf(t, c)
		if sel == nil {
			t.Errorf("namespaceSelector is nil after both reconcilers ran — the failurePolicy apply clobbered it")
		}
		if fp == nil || *fp != admissionregistrationv1.Fail {
			t.Errorf("failurePolicy = %v, want Fail — the namespaceSelector apply clobbered it", fp)
		}

		// A steady-state pass must keep both fields.
		reconcile()
		sel, fp = webhookFieldOf(t, c)
		if sel == nil || fp == nil || *fp != admissionregistrationv1.Fail {
			t.Errorf("after re-reconcile: namespaceSelector=%v failurePolicy=%v, want both set", sel, fp)
		}
	}

	t.Run("namespaceSelector applied first", func(t *testing.T) { run(t, true) })
	t.Run("failurePolicy applied first", func(t *testing.T) { run(t, false) })
}

// boundPinnedPod builds an operator-pinned pod in ns with the given TP span, bound to node.
func boundPinnedPod(name, ns, node string, tp int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Annotations: map[string]string{
				pinnedByAnnotation: pinnedByValue,
			},
		},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name: matchTargetContainer,
				Args: []string{"--tensor-parallel-size", strconv.Itoa(tp)},
			}},
		},
	}
}

// bppWithTPIntent is a policy on ns with the vLLM TP intent source.
func bppWithTPIntent(name, ns string) *cpopv1alpha1.BalloonPinningPolicy {
	return &cpopv1alpha1.BalloonPinningPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cpopv1alpha1.BalloonPinningPolicySpec{
			Match:      cpopv1alpha1.MatchSpec{ByContainerName: []string{matchTargetContainer}},
			Namespaces: []string{ns},
			Intent: cpopv1alpha1.IntentSpec{
				Span: cpopv1alpha1.IntentSourceSpec{
					ArgRegex: `(?:^|\s)(?:--tensor-parallel-size|-tp)(?:=|\s+)(\d+)`,
					Default:  1,
				},
			},
		},
	}
}

func policyStatus(t *testing.T, r *BalloonPinningPolicyReconciler, name string) cpopv1alpha1.BalloonPinningPolicyStatus {
	t.Helper()
	fresh := &cpopv1alpha1.BalloonPinningPolicy{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: name}, fresh); err != nil {
		t.Fatalf("get policy %q: %v", name, err)
	}
	return fresh.Status
}

// enabledSpanNode is a balloons-enabled node with a max-span label.
func enabledSpanNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "n1",
		Labels: map[string]string{
			LabelBalloonsEnabled: "true",
			LabelMaxSpan:         "8",
		},
	}}
}

// TestBoundPods_PopulatedFromPinnedPods: status.boundPods lists the policy's pinned
// pods sorted, with balloons snapped to the node's max-span.
func TestBoundPods_PopulatedFromPinnedPods(t *testing.T) {
	policy := bppWithTPIntent("infer", "inference")
	node := enabledSpanNode()
	// Out of order to prove sorting; the unpinned and untargeted pods must not appear.
	podB := boundPinnedPod("vllm-b", "inference", "n1", 2)
	podA := boundPinnedPod("vllm-a", "inference", "n1", 4)
	unpinned := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "inference"},
		Spec:       corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: matchTargetContainer}}},
	}
	offTarget := boundPinnedPod("vllm-x", "other", "n1", 2)

	r, _ := pinningPolicyReconciler(policy, node, podB, podA, unpinned, offTarget)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := policyStatus(t, r, "infer").BoundPods
	want := []cpopv1alpha1.BoundPod{
		{Namespace: "inference", Name: "vllm-a", Balloon: "balloon-span4"},
		{Namespace: "inference", Name: "vllm-b", Balloon: "balloon-span2"},
	}
	if len(got) != len(want) {
		t.Fatalf("boundPods = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("boundPods[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestBoundPods_ExcludesTerminatingAndCompleted: terminating and finished pods are
// excluded; Running and Pending pinned pods count.
func TestBoundPods_ExcludesTerminatingAndCompleted(t *testing.T) {
	policy := bppWithTPIntent("infer", "inference")
	node := enabledSpanNode()

	running := boundPinnedPod("vllm-run", "inference", "n1", 2)
	running.Status.Phase = corev1.PodRunning
	pending := boundPinnedPod("vllm-pend", "inference", "n1", 4)
	pending.Status.Phase = corev1.PodPending

	terminating := boundPinnedPod("vllm-term", "inference", "n1", 2)
	terminating.Status.Phase = corev1.PodRunning
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"cpu-pinning-operator.intel.com/test-hold"} // fake client requires a finalizer to accept a DeletionTimestamp

	succeeded := boundPinnedPod("vllm-done", "inference", "n1", 2)
	succeeded.Status.Phase = corev1.PodSucceeded
	failed := boundPinnedPod("vllm-fail", "inference", "n1", 2)
	failed.Status.Phase = corev1.PodFailed

	r, _ := pinningPolicyReconciler(policy, node, running, pending, terminating, succeeded, failed)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := policyStatus(t, r, "infer").BoundPods
	want := []cpopv1alpha1.BoundPod{
		{Namespace: "inference", Name: "vllm-pend", Balloon: "balloon-span4"},
		{Namespace: "inference", Name: "vllm-run", Balloon: "balloon-span2"},
	}
	if len(got) != len(want) {
		t.Fatalf("boundPods = %+v, want %+v (only running + pending)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("boundPods[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestBoundPods_ExcludesUnscheduledPending: a Pending pod without spec.nodeName holds
// no balloon and is excluded; a scheduled Pending pod counts.
func TestBoundPods_ExcludesUnscheduledPending(t *testing.T) {
	policy := bppWithTPIntent("infer", "inference")
	node := enabledSpanNode()

	scheduled := boundPinnedPod("vllm-sched", "inference", "n1", 4)
	scheduled.Status.Phase = corev1.PodPending

	unscheduled := boundPinnedPod("vllm-unsched", "inference", "", 2)
	unscheduled.Status.Phase = corev1.PodPending

	r, _ := pinningPolicyReconciler(policy, node, scheduled, unscheduled)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := policyStatus(t, r, "infer").BoundPods
	want := []cpopv1alpha1.BoundPod{
		{Namespace: "inference", Name: "vllm-sched", Balloon: "balloon-span4"},
	}
	if len(got) != len(want) {
		t.Fatalf("boundPods = %+v, want %+v (scheduled pending only)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("boundPods[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestBoundPods_Empty: a policy holding no pinned pods reports empty boundPods.
func TestBoundPods_Empty(t *testing.T) {
	policy := bppWithTPIntent("infer", "inference")
	node := enabledSpanNode()
	r, _ := pinningPolicyReconciler(policy, node)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := policyStatus(t, r, "infer").BoundPods; len(got) != 0 {
		t.Errorf("boundPods = %+v, want empty", got)
	}
}

// TestBoundPods_AttributedPerPolicy: each pod lands on the first matching policy only.
func TestBoundPods_AttributedPerPolicy(t *testing.T) {
	p1 := bppWithTPIntent("infer-a", "team-a")
	p2 := bppWithTPIntent("infer-b", "team-b")
	node := enabledSpanNode()
	podA := boundPinnedPod("vllm-a", "team-a", "n1", 2)
	podB := boundPinnedPod("vllm-b", "team-b", "n1", 4)

	r, _ := pinningPolicyReconciler(p1, p2, node, podA, podB)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	a := policyStatus(t, r, "infer-a").BoundPods
	if len(a) != 1 || a[0].Name != "vllm-a" || a[0].Balloon != "balloon-span2" {
		t.Errorf("infer-a boundPods = %+v, want [team-a/vllm-a balloon-span2]", a)
	}
	b := policyStatus(t, r, "infer-b").BoundPods
	if len(b) != 1 || b[0].Name != "vllm-b" || b[0].Balloon != "balloon-span4" {
		t.Errorf("infer-b boundPods = %+v, want [team-b/vllm-b balloon-span4]", b)
	}
}

// TestBoundPods_OverlapAttributedToNameFirst: a pod two policies match is
// attributed to the policy whose name sorts first, as admission stamps it.
func TestBoundPods_OverlapAttributedToNameFirst(t *testing.T) {
	first := bppWithTPIntent("00-first", "inference")
	second := bppWithTPIntent("01-second", "inference")
	pod := boundPinnedPod("vllm-a", "inference", "n1", 2)

	r, _ := pinningPolicyReconciler(first, second, enabledSpanNode(), pod)
	// The fake client lists by name; reverse it so the name-last policy comes first,
	// as a real cache map walk can.
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if l, ok := list.(*cpopv1alpha1.BalloonPinningPolicyList); ok {
				slices.Reverse(l.Items)
			}
			return nil
		},
	})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := policyStatus(t, r, "00-first").BoundPods; len(got) != 1 || got[0].Name != "vllm-a" {
		t.Errorf("00-first boundPods = %+v, want [inference/vllm-a]", got)
	}
	if got := policyStatus(t, r, "01-second").BoundPods; len(got) != 0 {
		t.Errorf("01-second boundPods = %+v, want none", got)
	}
}

// TestBoundPods_CappedWithTrueTotal: boundPods truncates to the first maxBoundPods by
// namespace then name while boundPodsTotal keeps the full count.
func TestBoundPods_CappedWithTrueTotal(t *testing.T) {
	policy := bppWithTPIntent("infer", "inference")
	node := enabledSpanNode()

	const total = maxBoundPods + 25
	objs := []runtime.Object{policy, node}
	for i := 0; i < total; i++ {
		// Zero-pad so lexical order matches numeric order.
		objs = append(objs, boundPinnedPod(fmt.Sprintf("vllm-%04d", i), "inference", "n1", 2))
	}

	r, _ := pinningPolicyReconciler(objs...)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	status := policyStatus(t, r, "infer")
	if len(status.BoundPods) != maxBoundPods {
		t.Fatalf("boundPods length = %d, want capped at %d", len(status.BoundPods), maxBoundPods)
	}
	if status.BoundPodsTotal != total {
		t.Errorf("boundPodsTotal = %d, want the true uncapped count %d", status.BoundPodsTotal, total)
	}

	for i := range status.BoundPods {
		want := fmt.Sprintf("vllm-%04d", i)
		if status.BoundPods[i].Name != want {
			t.Fatalf("boundPods[%d].Name = %q, want %q (deterministic ns→name prefix)", i, status.BoundPods[i].Name, want)
		}
	}
}

// TestBoundPods_TotalMatchesLenBelowCap: boundPodsTotal equals the list length below the cap.
func TestBoundPods_TotalMatchesLenBelowCap(t *testing.T) {
	policy := bppWithTPIntent("infer", "inference")
	node := enabledSpanNode()
	podA := boundPinnedPod("vllm-a", "inference", "n1", 2)
	podB := boundPinnedPod("vllm-b", "inference", "n1", 4)

	r, _ := pinningPolicyReconciler(policy, node, podA, podB)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	status := policyStatus(t, r, "infer")
	if status.BoundPodsTotal != 2 || len(status.BoundPods) != 2 {
		t.Errorf("boundPods len=%d total=%d, want both 2", len(status.BoundPods), status.BoundPodsTotal)
	}
}

// TestPoolReady_CountsOnlyProvisionedNodes: Ready counts the pinning pool, not the fleet.
func TestPoolReady_CountsOnlyProvisionedNodes(t *testing.T) {
	policy := bppWithTPIntent("infer", "inference")
	provisioned := enabledSpanNode()
	fresh := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}}
	alsoFresh := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n3"}}

	r, _ := pinningPolicyReconciler(policy, provisioned, fresh, alsoFresh)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ready := meta.FindStatusCondition(policyStatus(t, r, "infer").Conditions, "Ready")
	if ready == nil {
		t.Fatal("no Ready condition on the policy status")
	}
	if ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %s, want True with one provisioned node", ready.Status)
	}
	if want := "1 node(s) provisioned for CPU pinning"; ready.Message != want {
		t.Errorf("Ready message = %q, want %q", ready.Message, want)
	}
}

// TestPoolReady_NoProvisionedNodesInAFleetThatHasNodes: with no labelled nodes the pool is not ready.
func TestPoolReady_NoProvisionedNodesInAFleetThatHasNodes(t *testing.T) {
	policy := bppWithTPIntent("infer", "inference")
	fresh := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}}

	r, _ := pinningPolicyReconciler(policy, fresh)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ready := meta.FindStatusCondition(policyStatus(t, r, "infer").Conditions, "Ready")
	if ready == nil {
		t.Fatal("no Ready condition on the policy status")
	}
	if ready.Status != metav1.ConditionFalse || ready.Reason != "NoNodesProvisioned" {
		t.Errorf("Ready = %s/%s, want False/NoNodesProvisioned", ready.Status, ready.Reason)
	}
}
