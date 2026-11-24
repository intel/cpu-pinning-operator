// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runDensityModeAbsorbsSiblingsIntoReserved proves density mode drops the exclude-cpuset and names exactly the HT siblings in reservedResources.cpu.
func runDensityModeAbsorbsSiblingsIntoReserved(t *testing.T, c client.Client, node string) {
	siblings, siblingSet := densityBaselineSiblings(t, c, node)
	densityFlipToReserved(t, c)
	lastExcl, lastReserved := densityWaitRendered(t, c, node, siblingSet)

	if lastExcl != "" {
		t.Errorf("density mode still emitted availableResources.cpu = %q; the sibling exclusion should be dropped", lastExcl)
	}
	if !strings.HasPrefix(lastReserved, "cpuset:") {
		t.Fatalf("density mode reservedResources.cpu = %q, want an explicit cpuset naming the siblings", lastReserved)
	}
	reservedSet := parseCPUSet(strings.TrimPrefix(lastReserved, "cpuset:"))
	if !cpuSetsEqual(reservedSet, siblingSet) {
		t.Errorf("density mode reserved cpuset = %q (set %v), want the sibling set %q (%v)",
			lastReserved, sortedKeys(reservedSet), siblings, sortedKeys(siblingSet))
	}
	t.Logf("confirmed density mode on node %s: no exclude-cpuset, reservedResources.cpu=%q absorbs siblings %q",
		node, lastReserved, siblings)
}

// densityBaselineSiblings returns the node's exclude-mode sibling set, skipping unless the cluster is physical-cores with siblings.
func densityBaselineSiblings(t *testing.T, c client.Client, node string) (string, map[int]bool) {
	t.Helper()
	if alloc := clusterCPUAllocation(t, c); alloc != "physical-cores" {
		skipNoConfig(t, "cluster cpuAllocation is %q, not physical-cores; density mode is a no-op there", alloc)
	}
	bp := newUnstructured(bpGVK)
	key := client.ObjectKey{Name: "node." + node, Namespace: balloonsNS()}
	if err := c.Get(context.Background(), key, bp); err != nil {
		t.Fatalf("get BalloonsPolicy %s: %v", key.Name, err)
	}
	excl, _, _ := unstructured.NestedString(bp.Object, "spec", "availableResources", "cpu")
	if !strings.HasPrefix(excl, "exclude-cpuset:") {
		skipNoCapability(t, "availableResources.cpu = %q is not an exclude-cpuset (no HT siblings to absorb); nothing to test", excl)
	}
	siblings := strings.TrimPrefix(excl, "exclude-cpuset:")
	siblingSet := parseCPUSet(siblings)
	if len(siblingSet) == 0 {
		skipNoCapability(t, "exclude-cpuset is empty; no siblings to absorb")
	}
	return siblings, siblingSet
}

// densityFlipToReserved sets htSiblingPolicy=reserved and restores the prior value (unset = "exclude") on cleanup.
func densityFlipToReserved(t *testing.T, c client.Client) {
	t.Helper()
	bccKey := client.ObjectKey{Name: "cluster"}
	origPolicy := clusterHTSiblingPolicy(t, c)
	t.Cleanup(func() {
		restore := fmt.Sprintf(`{"spec":{"balloon":{"htSiblingPolicy":%q}}}`, origPolicy)
		cur := newUnstructured(bccGVK)
		if err := c.Get(context.Background(), bccKey, cur); err != nil {
			t.Errorf("cleanup: get BalloonsClusterConfig to restore: %v", err)
			return
		}
		if err := c.Patch(context.Background(), cur, client.RawPatch(types.MergePatchType, []byte(restore))); err != nil {
			t.Errorf("cleanup: FAILED to restore htSiblingPolicy to %q: %v", origPolicy, err)
		}
	})
	cur := newUnstructured(bccGVK)
	if err := c.Get(context.Background(), bccKey, cur); err != nil {
		t.Fatalf("get BalloonsClusterConfig/cluster: %v", err)
	}
	patch := []byte(`{"spec":{"balloon":{"htSiblingPolicy":"reserved"}}}`)
	if err := c.Patch(context.Background(), cur, client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatalf("patch htSiblingPolicy to reserved: %v", err)
	}
}

// densityWaitRendered polls the node BalloonsPolicy for the density render and returns the last values read.
func densityWaitRendered(t *testing.T, c client.Client, node string, siblingSet map[int]bool) (lastExcl, lastReserved string) {
	t.Helper()
	key := client.ObjectKey{Name: "node." + node, Namespace: balloonsNS()}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		re := newUnstructured(bpGVK)
		if err := c.Get(context.Background(), key, re); err != nil {
			t.Fatalf("poll BalloonsPolicy: %v", err)
		}
		lastExcl, _, _ = unstructured.NestedString(re.Object, "spec", "availableResources", "cpu")
		lastReserved, _, _ = unstructured.NestedString(re.Object, "spec", "reservedResources", "cpu")
		if lastExcl == "" && strings.HasPrefix(lastReserved, "cpuset:") &&
			cpuSetsEqual(parseCPUSet(strings.TrimPrefix(lastReserved, "cpuset:")), siblingSet) {
			return lastExcl, lastReserved
		}
		time.Sleep(3 * time.Second)
	}
	return lastExcl, lastReserved
}

// runDensityModeAdmitsPinnedPodUnderEnforce proves a fitting pinned pod is admitted under enforce on a density-mode node.
func runDensityModeAdmitsPinnedPodUnderEnforce(t *testing.T, c client.Client, node string) {
	_, siblingSet := densityBaselineSiblings(t, c, node)
	ns := useTargetNamespace(t, c)
	policy := installedPolicyName(t, c)

	densityFlipToReserved(t, c)
	if lastExcl, lastReserved := densityWaitRendered(t, c, node, siblingSet); lastExcl != "" || !strings.HasPrefix(lastReserved, "cpuset:") {
		t.Fatalf("node %s was not re-rendered in density mode (availableResources.cpu=%q reservedResources.cpu=%q)",
			node, lastExcl, lastReserved)
	}
	// Enforce reads live capacity, so the reserved pool must be the sibling set in NRT too.
	if !densityWaitNRTReserved(t, c, node, siblingSet) {
		skipNoCapability(t, "node %s NodeResourceTopology never published the sibling set as the reserved balloon; the live capacity path is not exercised", node)
	}

	ccSetMode(t, c, policy, "enforce")

	// Retry past reconcile lag; a denial that outlives it is the defect.
	balloonKey := balloonAnnotationBase + "/container.vllm"
	var lastErr error
	var name string
	pinned := false
	deadline := time.Now().Add(ccReconcileTimeout)
	for i := 0; ; i++ {
		name = fmt.Sprintf("e2e-density-enforce-%d", i)
		// Bind to this node: an unbound pod passes enforce if ANY node fits.
		pod := vllmPod(name, ns, "2", "2", 1)
		pod.Spec.NodeName = node
		lastErr = ccCreate(t, c, pod)
		if lastErr == nil {
			got := &corev1.Pod{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, got); err == nil {
				pinned = got.Annotations[balloonKey] != "" && got.Annotations[pinnedByAnnotation] == "cpu-pinning-operator"
			}
			if pinned {
				break
			}
			// Delete by name before retrying, or its reservation narrows the node for the next attempt.
			_ = c.Delete(context.Background(), &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			}, client.GracePeriodSeconds(0))
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}

	if lastErr != nil {
		t.Fatalf("a 2-core pinned pod was DENIED on density-mode node %s under capacityCheck.mode=enforce: %v", node, lastErr)
	}
	if !pinned {
		t.Fatalf("pod %s was admitted on density-mode node %s but not pinned", name, node)
	}
	if ccPodHasEvent(t, c, ns, name, ccReasonDenied) {
		t.Errorf("admitted pod %s still carries a %s event", name, ccReasonDenied)
	}
	t.Logf("density-mode node %s admitted a pinned pod under enforce", node)
}

// densityWaitNRTReserved polls until the node's NRT reserved balloon zone holds exactly the sibling set.
func densityWaitNRTReserved(t *testing.T, c client.Client, node string, siblingSet map[int]bool) bool {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if cpuSetsEqual(parseCPUSet(nrtReservedCPUSet(t, c, node)), siblingSet) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(3 * time.Second)
	}
}

// nrtReservedCPUSet returns the NRT reserved balloon zone's cpuset, or "". The plugin suffixes the instance ("reserved[0]").
func nrtReservedCPUSet(t *testing.T, c client.Client, node string) string {
	t.Helper()
	nrt := newUnstructured(nrtGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, nrt); err != nil {
		return ""
	}
	zones, _, _ := unstructured.NestedSlice(nrt.Object, "zones")
	for _, z := range zones {
		zone, ok := z.(map[string]interface{})
		if !ok {
			continue
		}
		if zoneType, _, _ := unstructured.NestedString(zone, "type"); zoneType != "balloon" {
			continue
		}
		if name, _, _ := unstructured.NestedString(zone, "name"); !strings.HasPrefix(name, "reserved") {
			continue
		}
		attrs, _, _ := unstructured.NestedSlice(zone, "attributes")
		for _, a := range attrs {
			attr, ok := a.(map[string]interface{})
			if !ok {
				continue
			}
			if an, _, _ := unstructured.NestedString(attr, "name"); an == "cpuset" {
				v, _, _ := unstructured.NestedString(attr, "value")
				return v
			}
		}
	}
	return ""
}

// clusterHTSiblingPolicy returns the singleton's htSiblingPolicy, defaulting to "exclude".
// Other read errors are fatal: the value is used to restore shared state.
func clusterHTSiblingPolicy(t *testing.T, c client.Client) string {
	t.Helper()
	bcc := newUnstructured(bccGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "cluster"}, bcc); err != nil {
		if apierrors.IsNotFound(err) {
			return "exclude" // operator default when the singleton is absent
		}
		t.Fatalf("get BalloonsClusterConfig/cluster to capture htSiblingPolicy: %v", err)
	}
	p, _, _ := unstructured.NestedString(bcc.Object, "spec", "balloon", "htSiblingPolicy")
	if p == "" {
		return "exclude"
	}
	return p
}

func cpuSetsEqual(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for cpu := range a {
		if !b[cpu] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
