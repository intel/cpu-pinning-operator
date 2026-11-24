// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Chaos e2e: with the webhook down under failurePolicy=Fail, pod creation is blocked
// only in namespaces the live webhook namespaceSelector matches. That bound is the
// safety argument for ever recommending Fail. The selector is reconciled at runtime,
// so it is read live. Gated behind E2E_CHAOS_FAILURE_POLICY: it scales the operator
// to 0 and flips the webhook, both restored on cleanup.
package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// e2eChaosFailurePolicyGate opts the blast-radius chaos test in.
const e2eChaosFailurePolicyGate = "E2E_CHAOS_FAILURE_POLICY"

// unmatchedChaosNamespace sits outside the webhook selector: its only label is its
// own metadata.name, which the exact-name selector never lists.
const unmatchedChaosNamespace = "e2e-chaos-fp-unmatched"

// TestChaosWebhookFailurePolicyBlastRadius proves that with the webhook down under
// Fail, matched-namespace creates are blocked, unmatched ones succeed, and admission
// resumes after recovery.
func TestChaosWebhookFailurePolicyBlastRadius(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(e2eChaosFailurePolicyGate) == "" {
		skipNotSelected(at, "set %s=1 to run the webhook failurePolicy=Fail blast-radius chaos test "+
			"(scales the operator to 0 and flips the live webhook to fail-closed; self-restoring)", e2eChaosFailurePolicyGate)
	}
	c := newClient(t) // skips cleanly when no kubeconfig
	ctx := context.Background()
	opNS := env("E2E_OPERATOR_NAMESPACE", "cpu-pinning-operator-system")

	var dep appsv1.Deployment
	depKey := client.ObjectKey{Name: env("E2E_OPERATOR_DEPLOYMENT", operatorDeployment), Namespace: opNS}
	if err := c.Get(ctx, depKey, &dep); err != nil {
		skipNoConfig(at, "operator Deployment %s/%s not found (set E2E_OPERATOR_NAMESPACE/E2E_OPERATOR_DEPLOYMENT): %v",
			depKey.Namespace, depKey.Name, err)
	}
	whName := webhookConfigName()
	selector := liveWebhookSelector(t, c, whName)

	matchedNS := useTargetNamespace(t, c)
	if !namespaceMatchesSelector(t, c, matchedNS, selector) {
		// An E2E_NAMESPACE choice may be out of scope; a namespace from a policy's
		// spec.namespaces cannot be, so a miss there is a defect.
		msg := fmt.Sprintf("target namespace %q is not selected by the live webhook %q (selector %s)",
			matchedNS, whName, selector)
		if os.Getenv("E2E_NAMESPACE") != "" {
			skipNoConfig(at, "%s; nothing to block there — point E2E_NAMESPACE at a namespace the webhook intercepts", msg)
		}
		at.Fatalf("%s, yet it came from an installed policy's spec.namespaces — the operator has not reconciled "+
			"the webhook namespaceSelector", msg)
	}
	// An empty selector matches everything, leaving no bound to demonstrate.
	if selector.Empty() {
		skipNoConfig(at, "live webhook %q selects EVERY namespace (empty selector); the blast radius is cluster-wide and "+
			"cannot be bounded — this is the broad-selector risk the failurePolicy docs describe", whName)
	}

	requireUnmatchedNamespace(t, c, unmatchedChaosNamespace, selector)

	origReplicas := int32(1)
	if dep.Spec.Replicas != nil {
		origReplicas = *dep.Spec.Replicas
	}
	origPolicy := webhookFailurePolicy(t, c, whName)

	// Reset failurePolicy before scaling up so the operator's reconcile does not fight
	// a stale value.
	t.Cleanup(func() {
		bg := context.Background()
		_ = setWebhookFailurePolicy(bg, c, whName, origPolicy)
		_ = scaleOperator(bg, c, depKey, origReplicas)
		waitOperatorReplicas(bg, c, depKey, origReplicas, 4*time.Minute)
	})

	if err := scaleOperator(ctx, c, depKey, 0); err != nil {
		at.Fatalf("scaling operator %s to 0 failed: %v", depKey, err)
	}
	if !waitOperatorPodsGone(t, c, opNS, dep.Spec.Selector, 3*time.Minute) {
		at.Fatal("operator pods did not terminate after scaling to 0; cannot simulate a webhook outage")
	}

	// The operator is down, so nothing reverts this.
	if err := setWebhookFailurePolicy(ctx, c, whName, admissionregistrationv1.Fail); err != nil {
		at.Fatalf("setting webhook %q failurePolicy=Fail failed: %v", whName, err)
	}

	// Endpoints drain asynchronously, so poll until a create is actually rejected.
	if !waitMatchedCreateBlocked(t, c, matchedNS, 3*time.Minute) {
		at.Fatalf("with the operator down and failurePolicy=Fail, a pod create in the MATCHED namespace %q was never "+
			"blocked within the deadline — the webhook is still reachable or the selector does not cover it", matchedNS)
	}
	at.Logf("blast radius confirmed INSIDE the selector: pod creation in %q is blocked while the webhook is down under Fail", matchedNS)

	unmatchedPod := chaosPod("e2e-chaos-unmatched", unmatchedChaosNamespace)
	_ = c.Delete(ctx, unmatchedPod, client.GracePeriodSeconds(0))
	if err := e2eCreate(ctx, c, unmatchedPod); err != nil {
		at.Fatalf("pod creation in the UNMATCHED namespace %q was blocked while the webhook was down (%v); "+
			"the blast radius is NOT bounded to the selector — the selector must be matching more than intended",
			unmatchedChaosNamespace, err)
	}
	t.Cleanup(func() {
		e2eDelete(t, c, unmatchedPod, client.GracePeriodSeconds(0))
	})
	at.Logf("blast radius bounded OUTSIDE the selector: pod creation in %q succeeds even while the webhook is down under Fail", unmatchedChaosNamespace)

	// Cleanup also restores these; doing it here lets the recovery assertion run.
	if err := setWebhookFailurePolicy(ctx, c, whName, origPolicy); err != nil {
		at.Fatalf("restoring webhook %q failurePolicy=%s failed: %v", whName, origPolicy, err)
	}
	if err := scaleOperator(ctx, c, depKey, origReplicas); err != nil {
		at.Fatalf("scaling operator %s back to %d failed: %v", depKey, origReplicas, err)
	}
	if !waitOperatorReplicas(ctx, c, depKey, origReplicas, 4*time.Minute) {
		at.Fatal("operator did not return to Available after scaling back up; recovery not confirmed")
	}
	if !waitMatchedCreateAdmitted(t, c, matchedNS, 3*time.Minute) {
		at.Errorf("matched-namespace admission did not resume in %q after the operator recovered", matchedNS)
	}
	at.Logf("recovery confirmed: matched-namespace admission resumed in %q after the operator scaled back up", matchedNS)
}

// liveWebhookSelector returns the live mpod.kb.io namespaceSelector, skipping when
// the webhook is absent.
func liveWebhookSelector(t *testing.T, c client.Client, name string) labels.Selector {
	t.Helper()
	cfg := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, cfg); err != nil {
		skipNoConfig(t, "MutatingWebhookConfiguration %q not found (set E2E_WEBHOOK_CONFIG_NAME): %v", name, err)
	}
	for i := range cfg.Webhooks {
		wh := &cfg.Webhooks[i]
		if wh.Name != "mpod.kb.io" {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(wh.NamespaceSelector)
		if err != nil {
			t.Fatalf("webhook %q has an invalid namespaceSelector: %v", name, err)
		}
		return sel
	}
	skipNoConfig(t, "MutatingWebhookConfiguration %q has no mpod.kb.io webhook; the mutating webhook is not installed", name)
	return nil
}

// webhookFailurePolicy returns the mpod.kb.io failurePolicy, Ignore when unset.
func webhookFailurePolicy(t *testing.T, c client.Client, name string) admissionregistrationv1.FailurePolicyType {
	t.Helper()
	cfg := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, cfg); err != nil {
		t.Fatalf("get webhook %q for failurePolicy: %v", name, err)
	}
	for i := range cfg.Webhooks {
		if cfg.Webhooks[i].Name == "mpod.kb.io" && cfg.Webhooks[i].FailurePolicy != nil {
			return *cfg.Webhooks[i].FailurePolicy
		}
	}
	return admissionregistrationv1.Ignore
}

// setWebhookFailurePolicy sets the mpod.kb.io failurePolicy with Get-then-Update.
func setWebhookFailurePolicy(ctx context.Context, c client.Client, name string, policy admissionregistrationv1.FailurePolicyType) error {
	cfg := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, cfg); err != nil {
		return err
	}
	changed := false
	for i := range cfg.Webhooks {
		if cfg.Webhooks[i].Name != "mpod.kb.io" {
			continue
		}
		p := policy
		cfg.Webhooks[i].FailurePolicy = &p
		changed = true
	}
	if !changed {
		return fmt.Errorf("webhook %q has no mpod.kb.io webhook to patch", name)
	}
	return c.Update(ctx, cfg)
}

// scaleOperator sets the operator Deployment's replica count.
func scaleOperator(ctx context.Context, c client.Client, key client.ObjectKey, replicas int32) error {
	var dep appsv1.Deployment
	if err := c.Get(ctx, key, &dep); err != nil {
		return err
	}
	r := replicas
	dep.Spec.Replicas = &r
	return c.Update(ctx, &dep)
}

// waitOperatorPodsGone polls until no pods match the Deployment's selector.
func waitOperatorPodsGone(t *testing.T, c client.Client, ns string, sel *metav1.LabelSelector, timeout time.Duration) bool {
	t.Helper()
	m, err := metav1.LabelSelectorAsMap(sel)
	if err != nil {
		t.Fatalf("bad deployment selector: %v", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		if len(operatorPodNames(t, c, ns, m)) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Second)
	}
}

// waitOperatorReplicas polls until the Deployment controller has observed the current
// spec and reports at least want Available replicas, so a status left over from an
// earlier scale (with old pods still terminating) does not count.
func waitOperatorReplicas(ctx context.Context, c client.Client, key client.ObjectKey, want int32, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		var dep appsv1.Deployment
		if err := c.Get(ctx, key, &dep); err == nil && dep.Status.ObservedGeneration >= dep.Generation && dep.Status.AvailableReplicas >= want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Second)
	}
}

// waitMatchedCreateBlocked polls until a matched-namespace create is rejected by the
// webhook, deleting any pod that slips through while endpoints drain.
func waitMatchedCreateBlocked(t *testing.T, c client.Client, ns string, timeout time.Duration) bool {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for i := 0; ; i++ {
		pod := chaosPod(fmt.Sprintf("e2e-chaos-matched-%d", i), ns)
		_ = c.Delete(ctx, pod, client.GracePeriodSeconds(0))
		err := e2eCreate(ctx, c, pod)
		if err != nil {
			// Require a webhook-attributable error so quota or validation failures do not count.
			if isWebhookCallFailure(err) {
				return true
			}
			t.Logf("matched create returned a non-webhook error (retrying): %v", err)
		} else {

			_ = c.Delete(ctx, pod, client.GracePeriodSeconds(0))
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Second)
	}
}

// waitMatchedCreateAdmitted polls until a matched-namespace create succeeds again.
func waitMatchedCreateAdmitted(t *testing.T, c client.Client, ns string, timeout time.Duration) bool {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for i := 0; ; i++ {
		pod := chaosPod(fmt.Sprintf("e2e-chaos-recover-%d", i), ns)
		_ = c.Delete(ctx, pod, client.GracePeriodSeconds(0))
		if err := e2eCreate(ctx, c, pod); err == nil {
			_ = c.Delete(ctx, pod, client.GracePeriodSeconds(0))
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Second)
	}
}

// isWebhookCallFailure reports whether err comes from the apiserver failing to call
// the mutating webhook. It matches on the message because the error type varies.
func isWebhookCallFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "failed calling webhook") ||
		strings.Contains(msg, "mpod.kb.io") ||
		(strings.Contains(msg, "webhook") && strings.Contains(msg, "context deadline")) ||
		(strings.Contains(msg, "webhook") && strings.Contains(msg, "connection refused"))
}

// requireUnmatchedNamespace ensures the namespace exists and does not match selector.
// It only deletes a namespace it created, and skips if the namespace matches.
func requireUnmatchedNamespace(t *testing.T, c client.Client, name string, selector labels.Selector) {
	t.Helper()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := e2eCreate(ctx, c, ns)
	switch {
	case err == nil:

		t.Cleanup(func() {
			e2eDelete(t, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
		})
	case apierrors.IsAlreadyExists(err):
		// Pre-existing: leave it on cleanup.
	default:
		skipNoConfig(t, "cannot create the unmatched test namespace %q (RBAC?): %v", name, err)
	}
	// Re-Get so the automatic metadata.name label is populated.
	got := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, got); err != nil {
		t.Fatalf("get unmatched namespace %q: %v", name, err)
	}
	if selector.Matches(labels.Set(got.Labels)) {
		skipNoConfig(t, "test namespace %q MATCHES the live webhook selector, so it cannot serve as the unmatched control "+
			"(the selector is broader than expected — an environment precondition, not a test failure)", name)
	}
}

// namespaceMatchesSelector reports whether the namespace's labels match selector.
func namespaceMatchesSelector(t *testing.T, c client.Client, name string, selector labels.Selector) bool {
	t.Helper()
	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, ns); err != nil {
		t.Fatalf("get namespace %q: %v", name, err)
	}
	return selector.Matches(labels.Set(ns.Labels))
}

// chaosPod builds a pause pod that tolerates everything, so only admission decides
// whether the create succeeds.
func chaosPod(name, ns string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"app": "chaos-failure-policy-e2e"},
		},
		Spec: corev1.PodSpec{
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:    "pause",
				Image:   "registry.k8s.io/pause:3.9",
				Command: []string{"/pause"},
			}},
		},
	}
}
