// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Chaos e2e: `helm uninstall` must exit cleanly and leave no per-node state.
// Per-node BalloonsPolicy CRs are reaped by GC through their ownerReference to the
// BalloonsClusterConfig singleton, and a post-delete hook strips the node labels
// (post-delete so the manager cannot re-label). The injected wait-for-balloons init
// container on a live pod is the one exception: pod specs are immutable, so it stays.
//
// Destructive (it uninstalls the operator), so gated behind E2E_HELM_UNINSTALL_CLEANUP.
// Cleanup re-runs the prune but cannot reinstall the operator.
package e2e

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// operatorManagedNodeLabels is the always-applied subset of the managed labels, the
// ones safe to assert present before uninstall; pruneLingeringState strips the full set.
var operatorManagedNodeLabels = []string{
	labelBalloonsEnabled,
	labelMaxSpan,
	labelNumaNodes,
	labelPhysicalCPUs,
	labelThreadsPerCore,
}

// TestHelmUninstallCleansUpResources proves helm uninstall removes the per-node
// BalloonsPolicy and operator node labels, while the immutable init container stays.
func TestHelmUninstallCleansUpResources(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_HELM_UNINSTALL_CLEANUP") == "" {
		skipNotSelected(at, "set E2E_HELM_UNINSTALL_CLEANUP=1 to run the destructive helm-uninstall cleanup chaos test "+
			"(it UNINSTALLS the operator; run only in a throwaway cluster or before a deliberate reinstall)")
	}

	helmBin := env("E2E_HELM_BIN", "helm")
	if _, err := exec.LookPath(helmBin); err != nil {
		skipNoConfig(at, "helm binary %q not on PATH (set E2E_HELM_BIN): %v", helmBin, err)
	}

	c := newClient(t)
	ctx := context.Background()
	node := balloonsEnabledNode(t, c)

	// The uninstall deletes the singleton the balloons namespace derives from, so find
	// the CR's real namespace now.
	bpName := "node." + node
	bpNamespace, found := findBalloonsPolicyNamespace(t, c, bpName)
	if !found {
		skipNoConfig(at, "no BalloonsPolicy %q found in any namespace before uninstall; operator not provisioning %s", bpName, node)
	}
	at.Logf("baseline: BalloonsPolicy %s/%s present; node %s carries operator labels (expect GC to reap the CR and the post-delete hook to strip the labels on uninstall)", bpNamespace, bpName, node)

	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		at.Fatalf("get node %s: %v", node, err)
	}
	for _, l := range operatorManagedNodeLabels {
		if _, ok := n.Labels[l]; !ok {
			at.Fatalf("baseline: node %s missing operator label %s; cannot assert it lingers", node, l)
		}
	}

	// The webhook fails open without a readiness endpoint and init image, so the init
	// container check runs only if it was injected. The pod must exist before uninstall.
	ns := useTargetNamespace(t, c)
	pod := createAndCleanup(t, c, vllmPod("e2e-helm-uninstall-linger", ns, "2", "2", 2))
	assertInit := hasInitContainer(pod, waitForBalloonsInit)
	if assertInit {
		at.Logf("baseline: pod %s/%s carries the %s init container", ns, pod.Name, waitForBalloonsInit)
	} else {
		at.Logf("pod %s/%s has no %s init container (fail-open profile: the readiness gate is not configured) — skipping the init-container lingering assertion",
			ns, pod.Name, waitForBalloonsInit)
	}

	t.Cleanup(func() { pruneLingeringState(t, c, bpName, bpNamespace, node) })

	// Release and Deployment names are independent; resolve each from its own env var.
	release := env("E2E_RELEASE_NAME", operatorDeployment)
	deployment := env("E2E_OPERATOR_DEPLOYMENT", operatorDeployment)
	helmNS := env("E2E_OPERATOR_NAMESPACE", "cpu-pinning-operator-system")
	at.Logf("running: %s uninstall %s -n %s --wait", helmBin, release, helmNS)

	helmCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(helmCtx, helmBin, "uninstall", release, "-n", helmNS, "--wait")
	// The test was explicitly selected, so an uninstall failure is a failure, not a skip.
	if out, err := cmd.CombinedOutput(); err != nil {
		at.Fatalf("helm uninstall %s -n %s failed (release/namespace mismatch? set E2E_RELEASE_NAME / E2E_OPERATOR_NAMESPACE): %v\n%s",
			release, helmNS, err, string(out))
	}

	if !waitForDeploymentGone(t, c, helmNS, deployment) {
		at.Fatalf("operator Deployment %s/%s still present after helm uninstall --wait", helmNS, deployment)
	}
	at.Logf("operator uninstalled: Deployment %s/%s gone", helmNS, deployment)

	// GC and the post-delete hook can lag the uninstall return, so poll.

	if waitForBalloonsPolicyGone(t, c, bpName, bpNamespace) {
		at.Logf("cleaned up: BalloonsPolicy %s/%s garbage-collected on uninstall", bpNamespace, bpName)
	} else {
		at.Errorf("BalloonsPolicy %s/%s still exists after helm uninstall — GC did not reap the per-node CR (ownerReference missing? lingering regression)",
			bpNamespace, bpName)
	}

	// The hook runs after the manager is gone, so a lingering label means the sweep failed.
	var after corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: node}, &after); err != nil {
		at.Fatalf("get node %s after uninstall: %v", node, err)
	}
	for _, l := range operatorManagedNodeLabels {
		if _, ok := after.Labels[l]; ok {
			at.Errorf("node label %s still present after helm uninstall — the post-delete hook failed to strip operator-managed labels (lingering regression)", l)
		}
	}
	at.Logf("cleaned up: operator-managed labels stripped from node %s by the post-delete hook on uninstall", node)

	// Inherent: a running pod's spec is immutable.
	if assertInit {
		live := &corev1.Pod{}
		if err := c.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: ns}, live); err != nil {
			if apierrors.IsNotFound(err) {
				at.Logf("pod %s/%s no longer exists after uninstall — cannot assert init-container lingering (pod was evicted/deleted)", ns, pod.Name)
			} else {
				at.Fatalf("get pod %s/%s after uninstall: %v", ns, pod.Name, err)
			}
		} else if !hasInitContainer(live, waitForBalloonsInit) {
			at.Errorf("pod %s/%s lost its %s init container after uninstall — a pod spec is immutable, this should always linger",
				ns, pod.Name, waitForBalloonsInit)
		} else {
			at.Logf("lingering (inherent, immutable pod spec): pod %s/%s still carries the %s init container", ns, pod.Name, waitForBalloonsInit)
		}
	}
}

// findBalloonsPolicyNamespace returns the namespace of the BalloonsPolicy named bpName.
func findBalloonsPolicyNamespace(t *testing.T, c client.Client, bpName string) (string, bool) {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(bpGVK)
	if err := c.List(context.Background(), list); err != nil {
		// Fallback for when RBAC denies a cluster-wide list.
		bp := newUnstructured(bpGVK)
		if err2 := c.Get(context.Background(), client.ObjectKey{Name: bpName, Namespace: balloonsNS()}, bp); err2 == nil {
			return balloonsNS(), true
		}
		t.Logf("list BalloonsPolicy cluster-wide failed (%v); default-namespace probe found nothing either", err)
		return "", false
	}
	for i := range list.Items {
		if list.Items[i].GetName() == bpName {
			return list.Items[i].GetNamespace(), true
		}
	}
	return "", false
}

// waitForDeploymentGone polls until the Deployment is NotFound. Any other Get error
// is fatal so it is not misreported as a timeout.
func waitForDeploymentGone(t *testing.T, c client.Client, ns, name string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, &appsv1.Deployment{})
		if apierrors.IsNotFound(err) {
			return true
		}
		if err != nil {
			t.Fatalf("get Deployment %s/%s while waiting for it to be removed: %v", ns, name, err)
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(3 * time.Second)
	}
}

// waitForBalloonsPolicyGone polls until the BalloonsPolicy is NotFound. Any other Get
// error is fatal so it is not misreported as a timeout.
func waitForBalloonsPolicyGone(t *testing.T, c client.Client, name, namespace string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: namespace}, newUnstructured(bpGVK))
		if apierrors.IsNotFound(err) {
			return true
		}
		if err != nil {
			t.Fatalf("get BalloonsPolicy %s/%s while waiting for GC to reap it: %v", namespace, name, err)
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(3 * time.Second)
	}
}

// pruneLingeringState is a best-effort cleanup net that deletes the per-node
// BalloonsPolicy and strips the operator labels; it only logs errors.
func pruneLingeringState(t *testing.T, c client.Client, bpName, bpNamespace, node string) {
	ctx := context.Background()

	bp := newUnstructured(bpGVK)
	bp.SetName(bpName)
	bp.SetNamespace(bpNamespace)
	if err := c.Delete(ctx, bp); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("cleanup: could not delete lingering BalloonsPolicy %s/%s: %v (manual: kubectl delete balloonspolicy -n %s %s)",
			bpNamespace, bpName, err, bpNamespace, bpName)
	}

	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		t.Logf("cleanup: could not get node %s to strip operator labels: %v", node, err)
		return
	}
	if n.Labels == nil {
		return
	}
	patch := client.MergeFrom(n.DeepCopy())
	// Full label set, matching NodeReconciler.unlabelNode.
	for _, l := range []string{
		labelBalloonsEnabled, labelMaxSpan, labelNumaNodes, labelPhysicalCPUs, labelThreadsPerCore,
		"cpu-pinning-operator.intel.com/memory-per-numa-bytes",
		"cpu-pinning-operator.intel.com/package-topology",
		"cpu-pinning-operator.intel.com/amx",
		"cpu-pinning-operator.intel.com/reserved-cpus",
		"cpu-pinning-operator.intel.com/cpu-allocation",
	} {
		delete(n.Labels, l)
	}
	if err := c.Patch(ctx, &n, patch); err != nil {
		t.Logf("cleanup: could not strip operator labels from node %s: %v (manual: kubectl label node %s <label>-)", node, err, node)
	}
}
