// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const guardWebhookPath = "/validate-v1alpha1-balloonpinningpolicy"

// TestGuardActivePodsBlocksDeleteAndNarrowing proves the live guard webhook rejects a
// Delete and a narrowing Update of a policy holding bound pods, and allows turning
// the guard off. The pod uses a container name the default policy does not match, so
// boundPods is attributed to this policy; NRI need not pin it.
func TestGuardActivePodsBlocksDeleteAndNarrowing(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_GUARD_ACTIVE_PODS") == "" {
		skipNotSelected(at, "set E2E_GUARD_ACTIVE_PODS=1 to run the guardActivePods validating-webhook test (creates a BalloonPinningPolicy + pod, self-cleaning)")
	}
	c := newClient(at)
	ns := useTargetNamespace(at, c)
	requireGuardWebhook(t, c)

	ctx := context.Background()
	const (
		policyName  = "e2e-guard-active-pods"
		podName     = "e2e-guard-pod"
		podLabelKey = "app"
		podLabelVal = "guard-active-pods-e2e"
		otherNS     = "e2e-guard-nonexistent" // a namespace the pod is NOT in
	)

	// Cleanup order matters: release the guard, drain the pod, then delete the policy
	// with retries, or the policy leaks into the next run.
	t.Cleanup(func() {
		bg := context.Background()
		releaseGuard(bg, c, policyName, ns)
		e2eDelete(t, c, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns}},
			client.GracePeriodSeconds(0))
		waitForPodGone(bg, c, ns, podName, 2*time.Minute)
		if err := deletePolicyWithRetry(bg, c, policyName, ns, 2*time.Minute); err != nil {
			t.Errorf("cleanup: delete BalloonPinningPolicy %s: %v (left behind; `make test-e2e-sweep` removes it)", policyName, err)
		}
	})

	// A crashed run can leave a guarded policy behind; release and delete it first.
	releaseGuard(ctx, c, policyName, ns)
	_ = c.Delete(ctx, guardPolicy(policyName, ns, false))
	if err := e2eCreate(ctx, c, guardPolicy(policyName, ns, true)); err != nil {
		at.Fatalf("create guarded BalloonPinningPolicy %q: %v", policyName, err)
	}

	// No pinned-by stamp means the webhook does not intercept here: nothing to guard.
	pod := guardPod(podName, ns, podLabelKey, podLabelVal)
	_ = c.Delete(ctx, pod)
	if err := e2eCreate(ctx, c, pod); err != nil {
		at.Fatalf("create matching pod %s/%s: %v", ns, podName, err)
	}
	created := &corev1.Pod{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: podName}, created); err != nil {
		at.Fatalf("get created pod: %v", err)
	}
	if created.Annotations[pinnedByAnnotation] != "cpu-pinning-operator" {
		skipNoConfig(at, "pod %s/%s was not stamped pinned-by (mutating webhook not intercepting %q?); cannot exercise the guard", ns, podName, ns)
	}

	// The guard fires only while boundPods is non-empty, which needs the pod scheduled
	// and a reconcile; hence the multi-minute deadline.
	if !waitForBoundPod(t, c, policyName, ns, podName, 5*time.Minute) {
		at.Fatalf("policy %q status.boundPods never listed pod %s/%s within the deadline; "+
			"the guard has nothing to protect and cannot be exercised", policyName, ns, podName)
	}

	// (a) DELETE of the guarded policy holding a pod must be rejected.
	delErr := c.Delete(ctx, guardPolicy(policyName, ns, false))
	if delErr == nil {
		at.Fatal("Delete of a guarded policy holding a bound pod was ALLOWED; the guard did not fire")
	}
	if !apierrors.IsForbidden(delErr) {
		at.Errorf("Delete rejection is not Forbidden: %v", delErr)
	}
	if !strings.Contains(delErr.Error(), "guardActivePods enabled and still holds") {
		at.Errorf("Delete rejection missing the guard's delete message, got: %v", delErr)
	}

	// (b) A narrowing update (namespace dropped, guard still on) must be rejected.
	narrowPatch := fmt.Sprintf(`{"spec":{"namespaces":[%q]}}`, otherNS)
	updErr := c.Patch(ctx, guardPolicy(policyName, ns, false),
		client.RawPatch(types.MergePatchType, []byte(narrowPatch)))
	if updErr == nil {
		at.Fatal("narrowing Update of a guarded policy holding a bound pod was ALLOWED; the guard did not fire")
	}
	if !apierrors.IsForbidden(updErr) {
		at.Errorf("Update rejection is not Forbidden: %v", updErr)
	}
	if !strings.Contains(updErr.Error(), "this update would stop matching") {
		at.Errorf("Update rejection missing the guard's narrowing message, got: %v", updErr)
	}

	// Escape hatch: turning the guard off is always allowed, even with pods bound.
	if err := c.Patch(ctx, guardPolicy(policyName, ns, false),
		client.RawPatch(types.MergePatchType, []byte(`{"spec":{"guardActivePods":false}}`))); err != nil {
		at.Fatalf("escape hatch: setting guardActivePods=false was rejected: %v", err)
	}

	if err := c.Delete(ctx, guardPolicy(policyName, ns, false)); err != nil {
		at.Fatalf("Delete of an unguarded policy was rejected: %v", err)
	}
	at.Logf("guard confirmed: delete + narrowing rejected while %s/%s was bound; delete succeeded once guardActivePods was unset", ns, podName)
}

// requireGuardWebhook skips unless a ValidatingWebhookConfiguration for the guard is
// installed; without it a Delete just succeeds.
func requireGuardWebhook(t *testing.T, c client.Client) {
	t.Helper()
	var vwcs admissionregistrationv1.ValidatingWebhookConfigurationList
	if err := c.List(context.Background(), &vwcs); err != nil {
		skipNoConfig(t, "cannot list ValidatingWebhookConfigurations: %v", err)
	}
	for i := range vwcs.Items {
		for _, wh := range vwcs.Items[i].Webhooks {
			if wh.ClientConfig.Service != nil && wh.ClientConfig.Service.Path != nil &&
				*wh.ClientConfig.Service.Path == guardWebhookPath {
				return
			}
		}
	}
	skipNoConfig(t, "no ValidatingWebhookConfiguration targets %q; the guardActivePods webhook is not installed", guardWebhookPath)
}

// releaseGuard best-effort patches guardActivePods off so the policy is deletable.
func releaseGuard(ctx context.Context, c client.Client, name, ns string) {
	_ = c.Patch(ctx, guardPolicy(name, ns, false),
		client.RawPatch(types.MergePatchType, []byte(`{"spec":{"guardActivePods":false}}`)))
}

// deletePolicyWithRetry deletes the policy until gone or the deadline, absorbing a
// transient guard Forbidden, and returns the last error once the deadline passes.
func deletePolicyWithRetry(ctx context.Context, c client.Client, name, ns string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := c.Delete(ctx, guardPolicy(name, ns, false))
		if err == nil || apierrors.IsNotFound(err) {
			return nil
		}
		// A still-armed guard can Forbid the delete; re-release and retry.
		releaseGuard(ctx, c, name, ns)
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(5 * time.Second)
	}
}

// waitForBoundPod polls status.boundPods until it lists ns/name or the deadline passes.
func waitForBoundPod(t *testing.T, c client.Client, policyName, podNS, podName string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		pol := newUnstructured(bppGVK)
		if err := c.Get(context.Background(), client.ObjectKey{Name: policyName}, pol); err != nil {
			t.Fatalf("get policy %q for boundPods: %v", policyName, err)
		}
		bound, _, _ := unstructured.NestedSlice(pol.Object, "status", "boundPods")
		for _, b := range bound {
			bp, ok := b.(map[string]interface{})
			if !ok {
				continue
			}
			n, _, _ := unstructured.NestedString(bp, "namespace")
			nm, _, _ := unstructured.NestedString(bp, "name")
			if n == podNS && nm == podName {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Second)
	}
}

// guardPolicy builds a minimal BalloonPinningPolicy matching a unique pod label in one
// namespace; guard sets spec.guardActivePods.
func guardPolicy(name, ns string, guard bool) *unstructured.Unstructured {
	pol := newUnstructured(bppGVK)
	pol.SetName(name)
	spec := map[string]interface{}{
		"match": map[string]interface{}{
			"byPodLabels": map[string]interface{}{"app": "guard-active-pods-e2e"},
		},
		"namespaces":      []interface{}{ns},
		"guardActivePods": guard,
	}
	_ = unstructured.SetNestedMap(pol.Object, spec, "spec")
	return pol
}

// guardPod builds a whole-core pod matched only by the guard policy (container
// "workload" is not in the default policy).
func guardPod(name, ns, labelKey, labelVal string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{labelKey: labelVal},
		},
		Spec: corev1.PodSpec{
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:    "workload",
				Image:   "registry.k8s.io/pause:3.9",
				Command: []string{"/pause"},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				},
			}},
		},
	}
}
