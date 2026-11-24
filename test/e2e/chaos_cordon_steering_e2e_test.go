// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Chaos e2e: headroom steering must never inject a required hostname affinity for a
// cordoned node, or the pod is admitted and then stays Pending. Gated behind
// E2E_CHAOS_CORDON; needs two balloons-enabled nodes and restores the original
// cordon state on cleanup. Asserted at admission, so no NRI dependency.
package e2e

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// e2eChaosCordonGate opts the cordon-steering chaos test in.
const e2eChaosCordonGate = "E2E_CHAOS_CORDON"

// balloonsEnabledNodes returns every node labelled balloons-enabled=true.
func balloonsEnabledNodes(t *testing.T, c client.Client) []string {
	t.Helper()
	var nodes corev1.NodeList
	if err := c.List(context.Background(), &nodes); err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	var names []string
	for i := range nodes.Items {
		if nodes.Items[i].Labels[labelBalloonsEnabled] == "true" {
			names = append(names, nodes.Items[i].Name)
		}
	}
	return names
}

// TestChaosCordonNotSteered proves headroom steering never requires a cordoned node.
func TestChaosCordonNotSteered(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(e2eChaosCordonGate) == "" {
		skipNotSelected(at, "set %s=1 to run the cordon-steering chaos test (it cordons a real node and restores its original cordon state in cleanup)", e2eChaosCordonGate)
	}
	c := newClient(t) // skips cleanly when no kubeconfig

	nodes := balloonsEnabledNodes(t, c)
	if len(nodes) < 2 {
		skipNoCapability(at, "cordon-steering needs >= 2 balloons-enabled nodes to prove steering avoids the cordoned one; found %d", len(nodes))
	}
	cordoned := nodes[0]

	ns := useTargetNamespace(t, c)

	// Restore the original state, so a node under real maintenance stays cordoned.
	// Register before cordoning so an interrupted run still restores it.
	wasUnschedulable := nodeUnschedulable(t, c, cordoned)
	t.Cleanup(func() { restoreUnschedulable(t, c, cordoned, wasUnschedulable) })
	cordonNode(t, c, cordoned)

	pod := vllmPod("chaos-cordon", ns, "1", "1", 1)
	got := createAndCleanup(t, c, pod)

	// Steering only happens with live NRT data.
	req := requiredHostnames(got)
	if len(req) == 0 {
		skipNoConfig(at, "webhook injected no REQUIRED hostname affinity (no live headroom data); nothing to assert about steering")
	}
	// The committed node is the pod's only candidate, so the set is a single node.
	if len(req) != 1 {
		at.Errorf("REQUIRED hostnames = %v, want exactly the node the reservation was charged to", req)
	}
	for _, h := range req {
		if h == cordoned {
			at.Errorf("headroom steering REQUIRED the cordoned node %q (required set %v); a cordoned node must be excluded", cordoned, req)
		}
	}
}

// nodeUnschedulable returns the node's Spec.Unschedulable.
func nodeUnschedulable(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	node := &corev1.Node{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, node); err != nil {
		t.Fatalf("get node %q to read cordon state: %v", name, err)
	}
	return node.Spec.Unschedulable
}

// cordonNode cordons the node, failing the test on error.
func cordonNode(t *testing.T, c client.Client, name string) {
	t.Helper()
	if err := setUnschedulable(c, name, true); err != nil {
		t.Fatalf("cordon node %q: %v", name, err)
	}
}

// restoreUnschedulable sets the node back to original, logging rather than failing
// on error so cleanup does not mask the result.
func restoreUnschedulable(t *testing.T, c client.Client, name string, original bool) {
	t.Helper()
	if err := setUnschedulable(c, name, original); err != nil {
		verb := "uncordon"
		if original {
			verb = "re-cordon"
		}
		t.Logf("cleanup: %s node %q FAILED: %v — restore its original cordon state manually (kubectl %s %s)", verb, name, err, verb, name)
	}
}

// setUnschedulable sets node.Spec.Unschedulable, a no-op when already set.
func setUnschedulable(c client.Client, name string, unschedulable bool) error {
	node := &corev1.Node{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, node); err != nil {
		return err
	}
	if node.Spec.Unschedulable == unschedulable {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = unschedulable
	return c.Patch(context.Background(), node, patch)
}

// requiredHostnames returns every value of the required kubernetes.io/hostname In terms.
func requiredHostnames(pod *corev1.Pod) []string {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return nil
	}
	sel := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if sel == nil {
		return nil
	}
	var out []string
	for _, term := range sel.NodeSelectorTerms {
		for _, req := range term.MatchExpressions {
			if req.Key == "kubernetes.io/hostname" && req.Operator == corev1.NodeSelectorOpIn {
				out = append(out, req.Values...)
			}
		}
	}
	return out
}
