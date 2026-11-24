// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Real-apiserver tests for the two reconcilers sharing the MutatingWebhookConfiguration.
// They use read-modify-write because the Webhooks list is atomic under SSA, which
// the fake client does not model. `make test-crd` provisions the binaries.
package controller

import (
	"context"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/intel/cpu-pinning-operator/internal/match"
	"github.com/intel/cpu-pinning-operator/test/ctrlplane"
)

// startWebhookEnv boots an envtest apiserver and returns a client plus a stop func.
func startWebhookEnv(t *testing.T) (client.Client, func()) {
	t.Helper()
	sch := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	return ctrlplane.Start(t, &envtest.Environment{Scheme: sch})
}

// fullWebhookConfig builds an apiserver-valid MutatingWebhookConfiguration like the
// install manifest's; all but namespaceSelector and failurePolicy must survive reconciles.
func fullWebhookConfig() *admissionregistrationv1.MutatingWebhookConfiguration {
	sideEffects := admissionregistrationv1.SideEffectClassNone
	ignore := admissionregistrationv1.Ignore
	scope := admissionregistrationv1.AllScopes
	url := "https://cpu-pinning-operator-webhook.example.svc/mutate"
	return &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: webhookConfigName()},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{
			Name:         webhookName,
			ClientConfig: admissionregistrationv1.WebhookClientConfig{URL: &url},
			Rules: []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"pods"},
					Scope:       &scope,
				},
			}},
			FailurePolicy:           &ignore,
			SideEffects:             &sideEffects,
			AdmissionReviewVersions: []string{"v1"},
		}},
	}
}

// TestWebhookReconcilers_Envtest_RMWPreservesRequiredFields runs both reconcilers in
// both orders against a real apiserver: each owned field lands and the installer
// fields (sideEffects, admissionReviewVersions, rules, clientConfig) survive.
func TestWebhookReconcilers_Envtest_RMWPreservesRequiredFields(t *testing.T) {
	run := func(t *testing.T, nsFirst bool) {
		c, stop := startWebhookEnv(t)
		defer stop()
		ctx := context.Background()

		if err := c.Create(ctx, fullWebhookConfig()); err != nil {
			t.Fatalf("seed webhook config: %v", err)
		}

		nsRec := &BalloonPinningPolicyReconciler{Client: c, Log: ctrl.Log.WithName("test"), PolicyIndex: match.NewIndex()}
		fpRec := &BalloonsClusterConfigReconciler{Client: c, Log: ctrl.Log.WithName("test"), PolicyIndex: match.NewIndex()}

		nsWrite := func() {
			if err := nsRec.reconcileWebhookNamespaceSelector(ctx, []string{"inference", "default"}); err != nil {
				t.Fatalf("reconcile namespaceSelector: %v", err)
			}
		}
		fpWrite := func() {
			if err := fpRec.reconcileFailurePolicy(ctx, admissionregistrationv1.Fail); err != nil {
				t.Fatalf("reconcile failurePolicy: %v", err)
			}
		}
		if nsFirst {
			nsWrite()
			fpWrite()
		} else {
			fpWrite()
			nsWrite()
		}

		got := &admissionregistrationv1.MutatingWebhookConfiguration{}
		if err := c.Get(ctx, client.ObjectKey{Name: webhookConfigName()}, got); err != nil {
			t.Fatalf("get reconciled config: %v", err)
		}
		wh, ok := webhookEntry(got, webhookName)
		if !ok {
			t.Fatal("webhook entry missing after reconcile")
		}

		// Both owned fields landed.
		if wh.NamespaceSelector == nil {
			t.Error("namespaceSelector nil — the namespaceSelector write was lost")
		}
		if wh.FailurePolicy == nil || *wh.FailurePolicy != admissionregistrationv1.Fail {
			t.Errorf("failurePolicy = %v, want Fail", wh.FailurePolicy)
		}
		// Installer fields a minimal SSA apply of the atomic list would drop.
		if wh.SideEffects == nil || *wh.SideEffects != admissionregistrationv1.SideEffectClassNone {
			t.Errorf("sideEffects = %v, want None (preserved)", wh.SideEffects)
		}
		if len(wh.AdmissionReviewVersions) == 0 {
			t.Error("admissionReviewVersions empty — required installer field dropped")
		}
		if len(wh.Rules) == 0 {
			t.Error("rules empty — installer field dropped")
		}
		if wh.ClientConfig.URL == nil && wh.ClientConfig.Service == nil {
			t.Error("clientConfig empty — installer field dropped")
		}
	}
	t.Run("namespaceSelector first", func(t *testing.T) { run(t, true) })
	t.Run("failurePolicy first", func(t *testing.T) { run(t, false) })
}

// TestWebhook_MinimalSSAApply_RejectedByApiserver is the negative control: a minimal
// SSA apply of one field of the atomic Webhooks list is rejected by the apiserver.
func TestWebhook_MinimalSSAApply_RejectedByApiserver(t *testing.T) {
	c, stop := startWebhookEnv(t)
	defer stop()
	ctx := context.Background()

	if err := c.Create(ctx, fullWebhookConfig()); err != nil {
		t.Fatalf("seed webhook config: %v", err)
	}

	// ForceOwnership replaces the atomic list with this partial entry, which lacks
	// sideEffects and admissionReviewVersions.
	apply := &admissionregistrationv1.MutatingWebhookConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: admissionregistrationv1.SchemeGroupVersion.String(),
			Kind:       "MutatingWebhookConfiguration",
		},
		ObjectMeta: metav1.ObjectMeta{Name: webhookConfigName()},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{
			Name:              webhookName,
			NamespaceSelector: namespaceNameSelector([]string{"inference"}),
		}},
	}
	err := c.Patch(ctx, apply, client.Apply,
		client.FieldOwner("cpu-pinning-operator/webhook-namespace-selector"),
		client.ForceOwnership)
	if err == nil {
		t.Fatal("minimal SSA apply of one webhook field was accepted — expected the apiserver to reject the atomic-list partial for missing sideEffects/admissionReviewVersions; if this ever passes, the atomic-list hazard is gone and read-modify-write may no longer be required")
	}
}
