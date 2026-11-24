// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Renaming a test here means updating the -run filter in the pre-commit hook and in
// the Makefile's test-e2e-guards.

package e2e

import (
	"context"
	"io"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestSweepRestoresAndDeletesOnlyLabelled proves the sweep puts the snapshotted
// shared objects back, through a JSON round trip, deletes every labelled object and
// nothing else, and leaves an already-restored object alone.
func TestSweepRestoresAndDeletesOnlyLabelled(t *testing.T) {
	defer func(w io.Writer) { sweepOut = w }(sweepOut)
	sweepOut = io.Discard
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	s.AddKnownTypeWithName(bppGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(bppGVK.GroupVersion().WithKind(bppGVK.Kind+"List"), &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(bccGVK, &unstructured.Unstructured{})

	cr := func(gvk schema.GroupVersionKind, name string, spec map[string]interface{}, labels map[string]string) *unstructured.Unstructured {
		u := newUnstructured(gvk)
		u.SetName(name)
		u.SetLabels(labels)
		u.Object["spec"] = spec
		return u
	}
	ours := map[string]string{runLabel: "earlier"}
	pod := func(name string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "work", Labels: labels}}
	}
	dsKey := client.ObjectKey{Name: "balloons", Namespace: "kube-system"}
	depKey := client.ObjectKey{Name: env("E2E_OPERATOR_DEPLOYMENT", operatorDeployment), Namespace: operatorNamespace()}
	npKey := client.ObjectKey{Name: env("E2E_NETWORK_POLICY", operatorDeployment), Namespace: operatorNamespace()}
	two, ignore := int32(2), admissionregistrationv1.Ignore
	rule := networkingv1.NetworkPolicyIngressRule{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		cr(bppGVK, "default", map[string]interface{}{
			"match":    map[string]interface{}{"byContainerName": []interface{}{"vllm"}},
			"priority": int64(5), // int64 live, float64 after the snapshot's JSON round trip
		}, nil),
		cr(bppGVK, "e2e-made", map[string]interface{}{}, ours),
		cr(bppGVK, "e2e-guarded", map[string]interface{}{"guardActivePods": true}, ours),
		cr(bccGVK, bccName, map[string]interface{}{"balloon": map[string]interface{}{"htSiblingPolicy": "exclude"}}, nil),
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: dsKey.Name, Namespace: dsKey.Namespace}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"keep": "1", "blank": "", operatorLabelPrefix + "x": "a"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "e2e-ns", Labels: ours}},
		pod("theirs", nil), pod("e2e-pod", ours),
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: depKey.Name, Namespace: depKey.Namespace},
			Spec: appsv1.DeploymentSpec{Replicas: &two}, Status: appsv1.DeploymentStatus{AvailableReplicas: 2}},
		&admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookConfigName()},
			Webhooks: []admissionregistrationv1.MutatingWebhook{{Name: "mpod.kb.io", FailurePolicy: &ignore}}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: npKey.Name, Namespace: npKey.Namespace},
			Spec: networkingv1.NetworkPolicySpec{Ingress: []networkingv1.NetworkPolicyIngressRule{rule}}},
	).Build()
	ctx := context.Background()

	snap, err := takeSnapshot(ctx, c, "host uid", dsKey)
	if err != nil {
		t.Fatalf("takeSnapshot: %v", err)
	}
	t.Setenv("E2E_SWEEP_SNAPSHOT", t.TempDir()+"/snap.json")
	if err := saveSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	if snap, err = loadSnapshot(); err != nil {
		t.Fatal(err)
	}

	// What the tests leave changed.
	bpp := newUnstructured(bppGVK)
	mustGet(t, c, client.ObjectKey{Name: "default"}, bpp)
	_ = unstructured.SetNestedStringSlice(bpp.Object, []string{"vllm-*"}, "spec", "match", "byContainerName")
	mustUpdate(t, c, bpp)
	bcc := newUnstructured(bccGVK)
	mustGet(t, c, client.ObjectKey{Name: bccName}, bcc)
	_ = unstructured.SetNestedField(bcc.Object, "reserved", "spec", "balloon", "htSiblingPolicy")
	mustUpdate(t, c, bcc)
	var ds appsv1.DaemonSet
	mustGet(t, c, dsKey, &ds)
	ds.Spec.Template.Spec.NodeSelector = map[string]string{"suspend": "true"}
	ds.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}}
	mustUpdate(t, c, &ds)
	var n corev1.Node
	mustGet(t, c, client.ObjectKey{Name: "n1"}, &n)
	n.Spec.Unschedulable = true
	n.Labels = map[string]string{"added": "1", operatorLabelPrefix + "x": "b"}
	mustUpdate(t, c, &n)
	if err := scaleOperator(ctx, c, depKey, 0); err != nil {
		t.Fatal(err)
	}
	if err := setWebhookFailurePolicy(ctx, c, webhookConfigName(), admissionregistrationv1.Fail); err != nil {
		t.Fatal(err)
	}
	var np networkingv1.NetworkPolicy
	mustGet(t, c, npKey, &np)
	np.Spec.Ingress = nil
	mustUpdate(t, c, &np)

	if restoreErr, deleteErr := sweepCluster(ctx, c, snap); restoreErr != nil || deleteErr != nil {
		t.Fatalf("sweepCluster: restore %v, delete %v", restoreErr, deleteErr)
	}

	mustGet(t, c, client.ObjectKey{Name: "default"}, bpp)
	if got, _, _ := unstructured.NestedStringSlice(bpp.Object, "spec", "match", "byContainerName"); len(got) != 1 || got[0] != "vllm" {
		t.Errorf("default policy match = %v, want [vllm]", got)
	}
	mustGet(t, c, client.ObjectKey{Name: bccName}, bcc)
	if got, _, _ := unstructured.NestedString(bcc.Object, "spec", "balloon", "htSiblingPolicy"); got != "exclude" {
		t.Errorf("htSiblingPolicy = %q, want exclude", got)
	}
	mustGet(t, c, dsKey, &ds)
	if ds.Spec.Template.Spec.NodeSelector != nil || ds.Spec.Template.Spec.Affinity != nil {
		t.Errorf("DaemonSet placement not restored: nodeSelector=%v affinity=%v", ds.Spec.Template.Spec.NodeSelector, ds.Spec.Template.Spec.Affinity)
	}
	mustGet(t, c, client.ObjectKey{Name: "n1"}, &n)
	_, blank := n.Labels["blank"]
	if n.Spec.Unschedulable || n.Labels["keep"] != "1" || !blank || n.Labels["added"] != "" || n.Labels[operatorLabelPrefix+"x"] != "b" {
		t.Errorf("node n1 = unschedulable %v labels %v; want schedulable, keep=1, blank present, no added, the operator label left as the operator set it",
			n.Spec.Unschedulable, n.Labels)
	}
	var dep appsv1.Deployment
	mustGet(t, c, depKey, &dep)
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Errorf("operator replicas = %v, want 2", dep.Spec.Replicas)
	}
	var wh admissionregistrationv1.MutatingWebhookConfiguration
	mustGet(t, c, client.ObjectKey{Name: webhookConfigName()}, &wh)
	if fp := wh.Webhooks[0].FailurePolicy; fp == nil || *fp != ignore {
		t.Errorf("webhook failurePolicy = %v, want Ignore", fp)
	}
	mustGet(t, c, npKey, &np)
	if len(np.Spec.Ingress) != 1 {
		t.Errorf("NetworkPolicy ingress = %v, want the one snapshotted rule", np.Spec.Ingress)
	}
	for _, gone := range []client.Object{pod("e2e-pod", nil), cr(bppGVK, "e2e-made", nil, nil), cr(bppGVK, "e2e-guarded", nil, nil),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "e2e-ns"}}} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(gone), gone); !apierrors.IsNotFound(err) {
			t.Errorf("labelled %T %s survived the sweep (err %v)", gone, gone.GetName(), err)
		}
	}
	for _, kept := range []client.Object{pod("theirs", nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}}} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(kept), kept); err != nil {
			t.Errorf("unlabelled %T %s was touched by the sweep: %v", kept, kept.GetName(), err)
		}
	}

	// Idempotent: a second sweep writes nothing.
	rv := bpp.GetResourceVersion()
	if restoreErr, deleteErr := sweepCluster(ctx, c, snap); restoreErr != nil || deleteErr != nil {
		t.Fatalf("second sweepCluster: restore %v, delete %v", restoreErr, deleteErr)
	}
	mustGet(t, c, client.ObjectKey{Name: "default"}, bpp)
	if bpp.GetResourceVersion() != rv {
		t.Errorf("a second restore rewrote the already-restored policy (resourceVersion %s -> %s)", rv, bpp.GetResourceVersion())
	}
}

func mustGet(t *testing.T, c client.Client, key client.ObjectKey, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), key, obj); err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
}

func mustUpdate(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Update(context.Background(), obj); err != nil {
		t.Fatalf("update %s: %v", obj.GetName(), err)
	}
}
