// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Chaos e2e for the fleet-wide stuck-drift case CPUPinningDriftEvictionsStuck
// catches: flipping the BalloonsClusterConfig cpuAllocation (part of every pinned
// pod's config hash) drifts every pinned pod at once, and a PDB blocks the eviction.
// It asserts the pdb-blocked series the alert fires on increments; it does not wait
// out the alert's 30m `for`. Gated behind E2E_CHAOS_PDB_STUCK because the flip
// drifts every pinned pod on the cluster; cleanup restores the original value.
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestChaosFleetFlipPDBStuckAlerts proves a cluster-wide cpuAllocation flip drifts a
// PDB-protected pod without evicting it and raises the stuck-drift alert's input series.
func TestChaosFleetFlipPDBStuckAlerts(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_CHAOS_PDB_STUCK") == "" {
		skipNotSelected(at, "set E2E_CHAOS_PDB_STUCK=1 to run the fleet-wide config-flip + PDB-stuck drift chaos test "+
			"(flips the singleton BalloonsClusterConfig cpuAllocation, which drifts EVERY pinned pod on the cluster; self-restored)")
	}
	c := newClient(at)

	_ = balloonsEnabledNode(at, c)

	ctx := context.Background()
	metricsOn := os.Getenv("E2E_METRICS") != ""

	const (
		ns            = "cpu-pinning-e2e-chaos-pdb"
		appLabel      = "chaos-pdb"
		policyName    = "e2e-chaos-pdb"
		containerName = "drift"
	)

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	_ = c.Delete(ctx, nsObj) // best-effort clean of a leftover run
	if err := createEventuallyGone(c, nsObj); err != nil {
		at.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Cleanup(func() {
		e2eDelete(t, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
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
	t.Cleanup(func() {
		p := newUnstructured(bppGVK)
		p.SetName(policyName)
		e2eDelete(t, c, p)
	})

	// The policy reconciler adds ns to the webhook selector asynchronously.
	if !waitForWebhookNamespace(at.T, c, ns, containerName) {
		skipNoConfig(at, "webhook never began pinning pods in %q (policy reconcile / selector patch did not take effect)", ns)
	}

	dep := driftDeployment(ns, appLabel, containerName)
	if err := e2eCreate(ctx, c, dep); err != nil {
		at.Fatalf("create deployment: %v", err)
	}
	t.Cleanup(func() {
		e2eDelete(t, c, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: dep.Name, Namespace: ns}})
	})

	// maxUnavailable=0 makes the eviction of the one healthy replica return TooManyRequests.
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: appLabel, Namespace: ns},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: ptrIOS(intstr.FromInt(0)),
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
		},
	}
	if err := e2eCreate(ctx, c, pdb); err != nil {
		at.Fatalf("create PDB: %v", err)
	}
	t.Cleanup(func() {
		e2eDelete(t, c, &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: appLabel, Namespace: ns}})
	})

	pod := waitForRunningPinnedPod(at.T, c, ns, appLabel)
	if pod == nil {
		skipNoConfig(at, "no pinned pod reached Running in %q within the deadline "+
			"(needs the node's balloons plugin so the wait-for-balloons init container clears)", ns)
	}
	origName := pod.Name
	at.Logf("pinned pod %s running (hash=%s)", origName, pod.Annotations[pinnedHashAnnotation])

	var baseDrifted, basePDB, baseAlertInput float64
	if metricsOn {
		body := scrapeOperatorMetrics(at.T)
		baseDrifted = counterSum(body, "cpu_pinning_pods_drifted_total", `workload_namespace="`+ns+`"`)
		basePDB = counterSum(body, "cpu_pinning_drift_evictions_total",
			`workload_namespace="`+ns+`"`, `outcome="pdb-blocked"`)
		baseAlertInput = driftAlertInput(body, ns)
	}

	// Read the singleton directly, not via clusterCPUAllocation (which fills in the
	// default), so cleanup can restore an absent field as absent.
	bccKey := client.ObjectKey{Name: "cluster"}
	cur := newUnstructured(bccGVK)
	if err := c.Get(ctx, bccKey, cur); err != nil {
		if apierrors.IsNotFound(err) {
			skipNoConfig(at, "BalloonsClusterConfig/cluster singleton is absent; nothing to flip for the fleet-wide drift trigger")
		}
		at.Fatalf("get BalloonsClusterConfig/cluster: %v", err)
	}

	// An absent field must be removed again on cleanup, not set to the default.
	rawAlloc, found, _ := unstructured.NestedString(cur.Object, "spec", "balloon", "cpuAllocation")
	origSet := found && rawAlloc != ""
	effectiveAlloc := rawAlloc
	if !origSet {
		effectiveAlloc = "physical-cores" // operator default when the field is absent
	}
	flippedAlloc := "hyperthreads"
	if effectiveAlloc == "hyperthreads" {
		flippedAlloc = "physical-cores"
	}
	t.Cleanup(func() {
		// A null member in a JSON merge patch deletes the field.
		var restore []byte
		if origSet {
			restore = []byte(`{"spec":{"balloon":{"cpuAllocation":"` + rawAlloc + `"}}}`)
		} else {
			restore = []byte(`{"spec":{"balloon":{"cpuAllocation":null}}}`)
		}
		cc := newUnstructured(bccGVK)
		if err := c.Get(context.Background(), bccKey, cc); err != nil {
			at.Errorf("cleanup: get BalloonsClusterConfig to restore cpuAllocation: %v", err)
			return
		}
		if err := c.Patch(context.Background(), cc, client.RawPatch(types.MergePatchType, restore)); err != nil {
			at.Errorf("cleanup: FAILED to restore cpuAllocation to its original state (cluster left in flipped state): %v", err)
		}
	})

	flip := []byte(`{"spec":{"balloon":{"cpuAllocation":"` + flippedAlloc + `"}}}`)
	if err := c.Patch(ctx, cur, client.RawPatch(types.MergePatchType, flip)); err != nil {
		at.Fatalf("flip cpuAllocation %s->%s to trigger fleet-wide drift: %v", effectiveAlloc, flippedAlloc, err)
	}
	at.Logf("flipped cluster cpuAllocation %s -> %s (fleet-wide drift trigger)", effectiveAlloc, flippedAlloc)

	// The drift reconciler watches Pods, not the BCC, and the index refreshes
	// asynchronously: wait, then nudge the pod to re-evaluate drift.
	time.Sleep(5 * time.Second)
	nudge := []byte(`{"metadata":{"annotations":{"cpu-pinning-operator.intel.com/e2e-chaos-nudge":"1"}}}`)
	_ = c.Patch(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: origName, Namespace: ns}},
		client.RawPatch(types.MergePatchType, nudge))

	if !waitForDriftedEvent(at.T, c, ns, origName) {
		at.Fatalf("no Warning \"Drifted\" event on pod %s/%s after the cluster-global cpuAllocation flip", ns, origName)
	}
	at.Logf("observed Drifted event on %s after the fleet-wide flip", origName)

	if evicted := waitForPodEvicted(at.T, c, ns, origName, 45*time.Second); evicted {
		at.Errorf("pod %s/%s was evicted despite a PodDisruptionBudget forbidding it", ns, origName)
	} else {
		at.Logf("pod %s correctly NOT evicted (PDB blocked) — drift rollout is stuck as expected", origName)
	}

	if !metricsOn {
		at.Log("E2E_METRICS unset — skipping the /metrics delta + alert-input assertions")
		return
	}

	// The reconciler requeues on PDB backoff, so poll for the deltas.
	deadline := time.Now().Add(2 * time.Minute)
	var drifted, pdbBlocked, alertInput float64
	for time.Now().Before(deadline) {
		after := scrapeOperatorMetrics(at.T)
		drifted = counterSum(after, "cpu_pinning_pods_drifted_total", `workload_namespace="`+ns+`"`)
		pdbBlocked = counterSum(after, "cpu_pinning_drift_evictions_total",
			`workload_namespace="`+ns+`"`, `outcome="pdb-blocked"`)
		alertInput = driftAlertInput(after, ns)
		if drifted > baseDrifted && pdbBlocked > basePDB {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if drifted <= baseDrifted {
		at.Errorf("cpu_pinning_pods_drifted_total{workload_namespace=%q} did not increment (was %v, now %v)", ns, baseDrifted, drifted)
	}
	if pdbBlocked <= basePDB {
		at.Errorf("cpu_pinning_drift_evictions_total{workload_namespace=%q,outcome=\"pdb-blocked\"} did not increment (was %v, now %v)",
			ns, basePDB, pdbBlocked)
	} else {
		at.Logf("drift_evictions_total{outcome=\"pdb-blocked\"} %v->%v, pods_drifted_total %v->%v",
			basePDB, pdbBlocked, baseDrifted, drifted)
	}
	// A positive alert numerator means the alert would fire once its `for` elapses.
	if alertInput <= baseAlertInput {
		at.Errorf("alert-input series cpu_pinning_drift_evictions_total{workload_namespace=%q,outcome=~\"pdb-blocked|error\"} did not increment "+
			"(was %v, now %v) — CPUPinningDriftEvictionsStuck would not fire", ns, baseAlertInput, alertInput)
	} else {
		at.Logf("alert-input (pdb-blocked|error) for %q %v->%v: CPUPinningDriftEvictionsStuck expression satisfied", ns, baseAlertInput, alertInput)
	}
}

// driftAlertInput sums the drift-eviction series matching the alert's
// outcome=~"pdb-blocked|error" for a namespace.
func driftAlertInput(body, ns string) float64 {
	nsMatch := `workload_namespace="` + ns + `"`
	return counterSum(body, "cpu_pinning_drift_evictions_total", nsMatch, `outcome="pdb-blocked"`) +
		counterSum(body, "cpu_pinning_drift_evictions_total", nsMatch, `outcome="error"`)
}
