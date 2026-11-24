// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// E2e for the driftPolicy: restart path of the PodDriftReconciler and its metrics. A
// pod whose desired balloon no longer matches its stamped hash gets a Drifted event
// and is evicted, PDB-aware. restart evicts every drifted pod a policy holds, so each
// case uses its own namespace and cluster-scoped policy.
package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestDriftRestartEvictsAndMeters proves a drifted pod is evicted under driftPolicy:
// restart, or held by a PDB, and that both outcomes are metered.
func TestDriftRestartEvictsAndMeters(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_DRIFT_RESTART") == "" {
		skipNotSelected(at, "set E2E_DRIFT_RESTART=1 to run the driftPolicy: restart eviction test "+
			"(creates its own namespace + BalloonPinningPolicy, then deletes them)")
	}
	c := newClient(at)

	_ = balloonsEnabledNode(at, c)

	// One Allure case per subtest.
	t.Run("evicted", func(t *testing.T) { runDriftRestartCase(allureCase(t), c, false) })
	t.Run("pdb-blocked", func(t *testing.T) { runDriftRestartCase(allureCase(t), c, true) })
}

// runDriftRestartCase runs one sub-case; withPDB adds a PDB that blocks the eviction.
func runDriftRestartCase(at *allureT, c client.Client, withPDB bool) {
	ctx := context.Background()
	metricsOn := os.Getenv("E2E_METRICS") != ""

	suffix := "evicted"
	if withPDB {
		suffix = "pdb"
	}
	ns := "cpu-pinning-e2e-drift-" + suffix
	appLabel := "drift-" + suffix
	policyName := "e2e-drift-" + suffix
	const containerName = "drift"

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	_ = c.Delete(ctx, nsObj) // best-effort clean of a leftover run
	if err := createEventuallyGone(c, nsObj); err != nil {
		at.Fatalf("create namespace %s: %v", ns, err)
	}
	at.Cleanup(func() {
		e2eDelete(at, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	// enforcePinning keeps the pod on the balloons node so its init container clears.
	policy := newUnstructured(bppGVK)
	policy.SetName(policyName)
	policy.Object["spec"] = map[string]interface{}{
		"match":       map[string]interface{}{"byContainerName": []interface{}{containerName}},
		"namespaces":  []interface{}{ns},
		"driftPolicy": "restart",
		"intent":      map[string]interface{}{"span": map[string]interface{}{"default": int64(1)}},
		"scheduling":  map[string]interface{}{"enforcePinning": true},
	}

	_ = c.Delete(ctx, policy)
	if err := createEventuallyGone(c, policy); err != nil {
		at.Fatalf("create BalloonPinningPolicy %s: %v", policyName, err)
	}
	at.Cleanup(func() {
		p := newUnstructured(bppGVK)
		p.SetName(policyName)
		e2eDelete(at, c, p)
	})

	// The webhook is CREATE-only and ns joins its selector asynchronously, so wait for a
	// pinned canary first.
	if !waitForWebhookNamespace(at.T, c, ns, containerName) {
		skipNoConfig(at, "webhook never began pinning pods in %q (policy reconcile / selector patch did not take effect)", ns)
	}

	dep := driftDeployment(ns, appLabel, containerName)
	if err := e2eCreate(ctx, c, dep); err != nil {
		at.Fatalf("create deployment: %v", err)
	}
	at.Cleanup(func() {
		e2eDelete(at, c, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: dep.Name, Namespace: ns}})
	})

	if withPDB {
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: appLabel, Namespace: ns},
			Spec: policyv1.PodDisruptionBudgetSpec{
				// minAvailable equal to the replica count leaves disruptionsAllowed=0.
				MinAvailable: ptrIOS(intstr.FromInt(1)),
				Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
			},
		}
		if err := e2eCreate(ctx, c, pdb); err != nil {
			at.Fatalf("create PDB: %v", err)
		}
		at.Cleanup(func() {
			e2eDelete(at, c, &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: appLabel, Namespace: ns}})
		})
	}

	pod := waitForRunningPinnedPod(at.T, c, ns, appLabel)
	if pod == nil {
		skipNoConfig(at, "no pinned pod reached Running in %q within the deadline "+
			"(needs the node's balloons plugin so the wait-for-balloons init container clears)", ns)
	}
	origName := pod.Name
	origHash := pod.Annotations[pinnedHashAnnotation]
	at.Logf("case %s: pinned pod %s running (hash=%s)", suffix, origName, origHash)

	var baseDrifted, baseEvict float64
	if metricsOn {
		body := scrapeOperatorMetrics(at.T)
		baseDrifted = counterSum(body, "cpu_pinning_pods_drifted_total", `workload_namespace="`+ns+`"`)
		baseEvict = counterSum(body, "cpu_pinning_drift_evictions_total",
			`workload_namespace="`+ns+`"`, `outcome="`+driftOutcome(withPDB)+`"`)
	}

	// coLocate=package changes the desired balloon name, so this drifts on any topology.
	driftPatch := []byte(`{"spec":{"intent":{"coLocate":"package"}}}`)
	cur := newUnstructured(bppGVK)
	cur.SetName(policyName)
	if err := c.Patch(ctx, cur, client.RawPatch(types.MergePatchType, driftPatch)); err != nil {
		at.Fatalf("patch policy intent.coLocate to trigger drift: %v", err)
	}

	// The match index refreshes asynchronously: wait, then nudge the pod to re-evaluate.
	time.Sleep(3 * time.Second)
	nudge := []byte(`{"metadata":{"annotations":{"cpu-pinning-operator.intel.com/e2e-drift-nudge":"1"}}}`)
	_ = c.Patch(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: origName, Namespace: ns}},
		client.RawPatch(types.MergePatchType, nudge))

	if !waitForDriftedEvent(at.T, c, ns, origName) {
		at.Fatalf("no Warning \"Drifted\" event on pod %s/%s after the config change", ns, origName)
	}
	at.Logf("case %s: observed Drifted event on %s", suffix, origName)

	if withPDB {

		if evicted := waitForPodEvicted(at.T, c, ns, origName, 45*time.Second); evicted {
			at.Errorf("pod %s/%s was evicted despite a PodDisruptionBudget forbidding it", ns, origName)
		} else {
			at.Logf("case %s: pod %s correctly NOT evicted (PDB blocked)", suffix, origName)
		}
	} else {

		if evicted := waitForPodEvicted(at.T, c, ns, origName, 90*time.Second); !evicted {
			at.Errorf("pod %s/%s drifted under driftPolicy: restart but was never evicted", ns, origName)
		} else {
			at.Logf("case %s: pod %s evicted as expected", suffix, origName)
		}
	}

	if !metricsOn {
		at.Logf("case %s: E2E_METRICS unset — skipping the /metrics delta assertions", suffix)
		return
	}

	// The reconciler requeues on rate limit or PDB backoff, so poll.
	deadline := time.Now().Add(2 * time.Minute)
	var drifted, evict float64
	for time.Now().Before(deadline) {
		after := scrapeOperatorMetrics(at.T)
		drifted = counterSum(after, "cpu_pinning_pods_drifted_total", `workload_namespace="`+ns+`"`)
		evict = counterSum(after, "cpu_pinning_drift_evictions_total",
			`workload_namespace="`+ns+`"`, `outcome="`+driftOutcome(withPDB)+`"`)
		if drifted > baseDrifted && evict > baseEvict {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if drifted <= baseDrifted {
		at.Errorf("cpu_pinning_pods_drifted_total{workload_namespace=%q} did not increment (was %v, now %v)", ns, baseDrifted, drifted)
	}
	if evict <= baseEvict {
		at.Errorf("cpu_pinning_drift_evictions_total{workload_namespace=%q,outcome=%q} did not increment (was %v, now %v)",
			ns, driftOutcome(withPDB), baseEvict, evict)
	} else {
		at.Logf("case %s: drift_evictions_total{outcome=%q} %v→%v, pods_drifted_total %v→%v",
			suffix, driftOutcome(withPDB), baseEvict, evict, baseDrifted, drifted)
	}
}

// driftOutcome is the drift_evictions_total outcome label a case expects.
func driftOutcome(withPDB bool) string {
	if withPDB {
		return "pdb-blocked"
	}
	return "evicted"
}

// driftDeployment builds a single-replica pause Deployment whose pod is a whole-core
// pinning target.
func driftDeployment(ns, appLabel, containerName string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: appLabel, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": appLabel}},
				Spec: corev1.PodSpec{
					Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:    containerName,
						Image:   "registry.k8s.io/pause:3.9",
						Command: []string{"/pause"},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
						},
					}},
				},
			},
		},
	}
}

// waitForWebhookNamespace reports whether a canary pod in ns gets pinned by the webhook
// before the deadline. Each canary is deleted immediately.
func waitForWebhookNamespace(t testing.TB, c client.Client, ns, containerName string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		canary := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "drift-canary", Namespace: ns},
			Spec: corev1.PodSpec{
				Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
				Containers: []corev1.Container{{
					Name:    containerName,
					Image:   "registry.k8s.io/pause:3.9",
					Command: []string{"/pause"},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
					},
				}},
			},
		}
		_ = c.Delete(context.Background(), canary, client.GracePeriodSeconds(0))
		if err := e2eCreate(context.Background(), c, canary); err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		got := &corev1.Pod{}
		pinned := false
		if err := c.Get(context.Background(), types.NamespacedName{Name: "drift-canary", Namespace: ns}, got); err == nil {
			pinned = got.Annotations[pinnedByAnnotation] == "cpu-pinning-operator"
		}
		_ = c.Delete(context.Background(), canary, client.GracePeriodSeconds(0))
		if pinned {
			return true
		}
		time.Sleep(5 * time.Second)
	}
	return false
}

// waitForDriftedEvent waits 90s for a Warning Drifted event on the pod. Matching the
// type keeps it from passing if Drifted were downgraded to Normal.
func waitForDriftedEvent(t *testing.T, c client.Client, ns, pod string) bool {
	t.Helper()
	// nil reportErr: list errors are retried until the deadline.
	return waitForWarningEvent(t, c, ns, pod, "Drifted", "", 90*time.Second, nil)
}

// createEventuallyGone creates obj, retrying while a previous instance is still
// terminating. It reports only through its return value, so callers without a T can use it.
func createEventuallyGone(c client.Client, obj client.Object) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := e2eCreate(context.Background(), c, obj)
		if err == nil || !apierrors.IsAlreadyExists(err) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(3 * time.Second)
	}
}

func ptrIOS(v intstr.IntOrString) *intstr.IntOrString { return &v }
