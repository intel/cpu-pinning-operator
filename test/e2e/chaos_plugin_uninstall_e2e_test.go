// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Chaos e2e guarding a known loss-of-pinning gap. A pinned pod re-created while
// the balloons plugin is gone is stamped pinned by the webhook but comes back
// unpinned, Running with the current hash. The drift reconciler's NRT check reads a
// topology the absent plugin no longer updates, so it may never fire. The test fails only
// when the pod is unpinned AND no Drifted event fires, which is the current
// behavior: a FAIL means the gap is still open. Do not weaken the assertion.
//
// The pod is deleted rather than SIGKILLed: an in-place restart on a fail-closed
// NRI runtime stalls with the plugin socket gone, while a fresh pod creates cleanly.
// Disruptive to every pinned workload on the node, so gated behind
// E2E_CHAOS_PLUGIN_UNINSTALL; the DaemonSet is always restored on cleanup.
package e2e

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// chaosSuspendLabel is a nodeSelector no node carries; adding it to the balloons
// DaemonSet template evicts every plugin pod, since a DaemonSet cannot scale to zero.
const chaosSuspendLabel = "cpu-pinning-operator.intel.com/chaos-suspend"

// TestChaosPluginUninstallRestartRepinnedOrDetected proves a pod re-created with the
// balloons plugin suspended is either still pinned or flagged by a Drifted event.
func TestChaosPluginUninstallRestartRepinnedOrDetected(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_CHAOS_PLUGIN_UNINSTALL") == "" {
		skipNotSelected(at, "set E2E_CHAOS_PLUGIN_UNINSTALL=1 to run the plugin-uninstall chaos test "+
			"(suspends the balloons DaemonSet — disruptive to every pinned workload on the node — then restores it)")
	}
	ctx := context.Background()
	c := newClient(t)
	balloonsEnabledNode(t, c) // skips cleanly when no node is provisioned

	dsName := balloonsDaemonSet()
	dsKey := client.ObjectKey{Namespace: balloonsNS(), Name: dsName}
	ds := &appsv1.DaemonSet{}
	if err := c.Get(ctx, dsKey, ds); err != nil {
		if apierrors.IsNotFound(err) {
			skipNoConfig(at, "balloons DaemonSet %s/%s not found; nothing to uninstall", balloonsNS(), dsName)
		}
		at.Fatalf("get balloons DaemonSet %s/%s: %v", balloonsNS(), dsName, err)
	}

	const ns = "cpu-pinning-e2e-chaos-uninstall"
	const appLabel = "chaos-uninstall"
	const policyName = "e2e-chaos-uninstall"
	const containerName = "chaos"

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	_ = c.Delete(ctx, nsObj) // best-effort clean of a leftover run
	if err := createEventuallyGone(c, nsObj); err != nil {
		at.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Cleanup(func() {
		e2eDelete(t, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	// enforcePinning keeps the pod on the balloons node; the default events-only
	// driftPolicy surfaces the loss as a Drifted event, not an eviction.
	policy := newUnstructured(bppGVK)
	policy.SetName(policyName)
	policy.Object["spec"] = map[string]interface{}{
		"match":      map[string]interface{}{"byContainerName": []interface{}{containerName}},
		"namespaces": []interface{}{ns},
		"intent":     map[string]interface{}{"span": map[string]interface{}{"default": int64(1)}},
		"scheduling": map[string]interface{}{"enforcePinning": true},
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

	// The webhook is CREATE-only, so wait until it pins in ns before creating the workload.
	if !waitForWebhookNamespace(t, c, ns, containerName) {
		skipNoConfig(at, "webhook never began pinning pods in %q (policy reconcile / selector patch did not take effect)", ns)
	}

	dep := chaosDeployment(ns, appLabel, containerName)
	if err := e2eCreate(ctx, c, dep); err != nil {
		at.Fatalf("create deployment: %v", err)
	}
	t.Cleanup(func() {
		e2eDelete(t, c, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: dep.Name, Namespace: ns}})
	})

	pod := waitForRunningPinnedPod(t, c, ns, appLabel)
	if pod == nil {
		skipNoConfig(at, "no pinned pod reached Running in %q within the deadline "+
			"(needs the node's balloons plugin so the wait-for-balloons init container clears)", ns)
	}
	origName := pod.Name
	// The Deployment is not node-bound: read the node the scheduler chose.
	node := pod.Spec.NodeName

	// Pinned vs unpinned is told by cpuset size, which only works when the node-default
	// set is larger than reqCores.
	reqCores := chaosReqCores
	if allocCPUs := nodeAllocatableCPUs(t, c, node); allocCPUs <= reqCores {
		skipNoCapability(at, "node %s has %d allocatable CPUs, not enough larger than reqCores=%d to distinguish "+
			"a pinned cpuset from the node-default cpuset by size", node, allocCPUs, reqCores)
	}

	var pinnedSet string
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		pinnedSet = containerCPUSet(t, c, node, ns, origName, containerName)
		if pinnedSet != "" && len(parseCPUSet(pinnedSet)) == reqCores {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if pinnedSet == "" || len(parseCPUSet(pinnedSet)) != reqCores {
		skipNoConfig(at, "container %s/%s/%s never showed a %d-core pinned cpuset in NRT (got %q); "+
			"NRI did not pin — cannot exercise the loss-of-pinning scenario",
			ns, origName, containerName, reqCores, pinnedSet)
	}
	at.Logf("container pinned to %d-core cpuset %q (ground truth)", reqCores, pinnedSet)

	restore := suspendBalloonsDaemonSet(t, c, dsKey)
	t.Cleanup(restore)
	if !waitForNoPluginPodOnNode(t, c, ds, node) {
		skipNoConfig(at, "balloons plugin pod did not leave the node after suspending the DaemonSet; "+
			"cannot guarantee the re-created pod comes up without a plugin")
	}
	at.Logf("balloons plugin suspended: no plugin pod on node %s", node)

	if err := c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: origName, Namespace: ns}}); err != nil {
		if !apierrors.IsNotFound(err) {
			at.Fatalf("delete pinned pod %s/%s to trigger re-creation: %v", ns, origName, err)
		}
	}
	at.Logf("deleted pinned pod %s; waiting for the ReplicaSet to re-create it while the plugin is down", origName)

	// The readiness gate may hold the replacement in Init, or a fail-closed runtime may
	// refuse it; either way the running-unpinned precondition is unavailable.
	repl := waitForReplacementRunningPinnedPod(t, c, ns, appLabel, origName)
	if repl == nil {
		skipNoCapability(at, "no replacement pod for %q reached Running within the deadline — the wait-for-balloons "+
			"readiness gate may be correctly holding the rescheduled pod in Init while the plugin is down, "+
			"or the runtime could not create its container without the plugin; the running-unpinned "+
			"precondition could not be established, so the loss-of-pinning scenario cannot be exercised here", origName)
	}
	replName := repl.Name
	at.Logf("replacement pod %s reached Running (hash=%s)", replName, repl.Annotations[pinnedHashAnnotation])

	// An unpinned container's NRT shape is runtime-dependent, so "still pinned" means a
	// request-sized dedicated cpuset.
	time.Sleep(15 * time.Second)
	// The replacement may land on another node; re-check the size signal there.
	replNode := repl.Spec.NodeName
	if replNode != node {
		if allocCPUs := nodeAllocatableCPUs(t, c, replNode); allocCPUs <= reqCores {
			skipNoCapability(at, "replacement landed on node %s with %d allocatable CPUs, not enough larger than reqCores=%d "+
				"to tell a pinned cpuset from the node-default one by size", replNode, allocCPUs, reqCores)
		}
	}
	afterSet := containerCPUSet(t, c, replNode, ns, replName, containerName)
	afterCores := len(parseCPUSet(afterSet))
	stillPinned := afterSet != "" && afterCores == reqCores
	if stillPinned {
		at.Logf("replacement container %s/%s/%s is pinned to a %d-core dedicated cpuset %q (NRT) "+
			"— pinning survived the plugin-uninstall + re-create", ns, replName, containerName, reqCores, afterSet)
	} else {
		at.Logf("replacement container %s/%s/%s is UNPINNED per NRT (cpuset=%q, cores=%d, want %d dedicated) "+
			"— the loss of pinning must be detected below", ns, replName, containerName, afterSet, afterCores, reqCores)
	}

	// Nudge the pod to force a reconcile, then wait for a Drifted event.
	nudge := []byte(`{"metadata":{"annotations":{"cpu-pinning-operator.intel.com/e2e-chaos-nudge":"1"}}}`)
	_ = c.Patch(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: replName, Namespace: ns}},
		client.RawPatch(types.MergePatchType, nudge))

	detected := waitForDriftedEventTimeout(t, c, ns, replName, 60*time.Second)
	if detected {
		at.Logf("PodDriftReconciler fired a Warning \"Drifted\" event on %s/%s — the loss of pinning "+
			"was detected", ns, replName)
	} else {
		at.Logf("no Drifted event on %s/%s within the sustained wait — the operator did not detect "+
			"the loss of pinning", ns, replName)
	}

	// Fail only on the silent state: unpinned and undetected. Expected to fail today.
	if !stillPinned && !detected {
		at.Errorf("SILENT LOSS OF PINNING: after plugin-uninstall + re-create, container %s/%s/%s came back "+
			"UNPINNED (NRT cpuset=%q, cores=%d, want %d dedicated) and NO Warning \"Drifted\" event fired. "+
			"The operator must either pin the re-created container or detect the drift; it did neither. "+
			"This failure is EXPECTED on the current operator (the drift reconciler ignores NRT ground truth / "+
			"plugin health) and confirms the loss-of-pinning gap — it becomes green once detection lands.",
			ns, replName, containerName, afterSet, afterCores, reqCores)
	}
}

// chaosReqCores is the whole-core CPU request of the chaos workload container.
const chaosReqCores = 2

// chaosDeployment builds a single-replica pause Deployment whose container is a
// whole-core pinning target; deleting its pod triggers re-creation.
func chaosDeployment(ns, appLabel, containerName string) *appsv1.Deployment {
	replicas := int32(1)
	// chaosReqCores is the single source for both the request and the size assertions.
	q := resource.MustParse(strconv.Itoa(chaosReqCores))
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
						Image:   env("E2E_CHAOS_IMAGE", "registry.k8s.io/pause:3.9"),
						Command: []string{"/pause"},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: q},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: q},
						},
					}},
				},
			},
		},
	}
}

// nodeAllocatableCPUs returns the node's allocatable whole-CPU count, failing on a
// read error since it gates the size-based pinning signal.
func nodeAllocatableCPUs(t *testing.T, c client.Client, node string) int {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, &n); err != nil {
		t.Fatalf("get node %s for allocatable CPU count: %v", node, err)
	}
	cpu, ok := n.Status.Allocatable[corev1.ResourceCPU]
	if !ok {
		t.Fatalf("node %s reports no allocatable CPU quantity", node)
	}
	return int(cpu.Value())
}

// suspendBalloonsDaemonSet applies chaosSuspendLabel to the balloons DaemonSet and
// returns a function that removes it.
func suspendBalloonsDaemonSet(t *testing.T, c client.Client, key client.ObjectKey) func() {
	t.Helper()
	ctx := context.Background()

	patch := []byte(`{"spec":{"template":{"spec":{"nodeSelector":{"` + chaosSuspendLabel + `":"true"}}}}}`)
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
	if err := c.Patch(ctx, ds, client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatalf("suspend balloons DaemonSet %s/%s: %v", key.Namespace, key.Name, err)
	}
	return func() {

		restorePatch := []byte(`{"spec":{"template":{"spec":{"nodeSelector":{"` + chaosSuspendLabel + `":null}}}}}`)
		cur := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
		if err := c.Patch(context.Background(), cur, client.RawPatch(types.MergePatchType, restorePatch)); err != nil {
			t.Logf("WARNING: failed to restore balloons DaemonSet %s/%s (plugin may stay suspended): %v",
				key.Namespace, key.Name, err)
		}
	}
}

// waitForNoPluginPodOnNode polls until no live balloons pod remains on the node.
func waitForNoPluginPodOnNode(t *testing.T, c client.Client, ds *appsv1.DaemonSet, node string) bool {
	t.Helper()
	sel, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
	if err != nil {
		t.Fatalf("balloons DaemonSet has an invalid pod selector: %v", err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		var pods corev1.PodList
		if err := c.List(context.Background(), &pods,
			client.InNamespace(ds.Namespace), client.MatchingLabelsSelector{Selector: sel}); err == nil {
			present := false
			for i := range pods.Items {
				p := &pods.Items[i]
				if p.Spec.NodeName == node && p.DeletionTimestamp == nil &&
					p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
					present = true
					break
				}
			}
			if !present {
				return true
			}
		}
		time.Sleep(3 * time.Second)
	}
	return false
}

// waitForReplacementRunningPinnedPod returns a stamped, Running pod with the app
// label other than origName, or nil if none appears in time.
func waitForReplacementRunningPinnedPod(t *testing.T, c client.Client, ns, appLabel, origName string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		var pods corev1.PodList
		if err := c.List(context.Background(), &pods,
			client.InNamespace(ns), client.MatchingLabels{"app": appLabel}); err == nil {
			for i := range pods.Items {
				p := &pods.Items[i]
				if p.Name == origName || p.DeletionTimestamp != nil {
					continue
				}
				if p.Annotations[pinnedByAnnotation] == "cpu-pinning-operator" &&
					p.Status.Phase == corev1.PodRunning {
					return p.DeepCopy()
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
	return nil
}

// waitForDriftedEventTimeout reports whether a Warning Drifted event appears on the
// pod within timeout.
func waitForDriftedEventTimeout(t *testing.T, c client.Client, ns, pod string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var events corev1.EventList
		if err := c.List(context.Background(), &events, client.InNamespace(ns)); err == nil {
			for i := range events.Items {
				e := &events.Items[i]
				if e.Reason == "Drifted" &&
					e.Type == corev1.EventTypeWarning &&
					e.InvolvedObject.Kind == "Pod" &&
					e.InvolvedObject.Name == pod {
					return true
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	return false
}
