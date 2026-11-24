// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ovLabelKey steers the override at one node; a throwaway domain so it never
	// collides with an operator-managed label.
	ovLabelKey = "ov-e2e.example.com/override-target"
	ovLabelVal = "true"
)

// runNodeConfigOverrideAppliesPerNode proves a nodeConfigOverrides entry changes the
// labeled node's cpuAllocation (rendered policy and status.perNode) while another
// node keeps the cluster default (needs >=2 balloons-enabled nodes).
func runNodeConfigOverrideAppliesPerNode(t *testing.T, c client.Client, node string) {
	ctx := context.Background()

	bccKey := client.ObjectKey{Name: "cluster"}
	bcc := newUnstructured(bccGVK)
	if err := c.Get(ctx, bccKey, bcc); err != nil {
		if apierrors.IsNotFound(err) {
			skipNoConfig(t, "BalloonsClusterConfig singleton absent; cannot add nodeConfigOverrides")
		}
		t.Fatalf("get BalloonsClusterConfig/cluster: %v", err)
	}

	clusterDefault := clusterCPUAllocation(t, c)
	overrideAlloc := ovOppositeAllocation(clusterDefault)
	t.Logf("cluster-default cpuAllocation=%q; override will set the chosen node to %q", clusterDefault, overrideAlloc)

	// A non-SMT node has no siblings to exclude, so the exclude-cpuset check is moot.
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		t.Fatalf("get node %s: %v", node, err)
	}
	threads, _ := strconv.Atoi(n.Labels[labelThreadsPerCore])

	// Registered first so it runs last (LIFO): the override is reverted before the label.
	t.Cleanup(func() {
		ovRemoveNodeLabel(t, c, node)
	})
	ovAddNodeLabel(t, c, node)

	// Capture presence too, so an empty list is restored empty and an absent field absent.
	origOverrides, origPresent, _ := unstructured.NestedSlice(bcc.Object, "spec", "nodeConfigOverrides")
	t.Cleanup(func() {
		ovPatchOverrides(context.Background(), t, c, bccKey, origOverrides, origPresent, true)
	})

	// Prepend: overrides are first-match-wins.
	ours := map[string]interface{}{
		"nodeSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{ovLabelKey: ovLabelVal},
		},
		"balloon": map[string]interface{}{
			"cpuAllocation": overrideAlloc,
		},
	}
	newOverrides := append([]interface{}{ours}, origOverrides...)
	ovPatchOverrides(ctx, t, c, bccKey, newOverrides, true, false)

	bpKey := client.ObjectKey{Name: "node." + node, Namespace: balloonsNS()}
	deadline := time.Now().Add(90 * time.Second)
	var gotAlloc, lastExclude string
	var haveStatus bool
	for time.Now().Before(deadline) {
		gotAlloc, haveStatus = ovPerNodeCPUAllocation(t, c, bccKey, node)
		lastExclude = balloonsPolicyAvailableCPU(t, c, bpKey)
		if haveStatus && gotAlloc == overrideAlloc && ovExcludeMatchesMode(lastExclude, overrideAlloc, threads) {
			break
		}
		time.Sleep(3 * time.Second)
	}

	if !haveStatus {
		t.Fatalf("status.perNode has no entry for node %s", node)
	}
	if gotAlloc != overrideAlloc {
		t.Errorf("status.perNode[%s].cpuAllocation = %q, want the override value %q", node, gotAlloc, overrideAlloc)
	}
	if gotAlloc == clusterDefault {
		t.Errorf("status.perNode[%s].cpuAllocation = %q equals the cluster default; the override did not take effect", node, gotAlloc)
	}

	switch overrideAlloc {
	case "hyperthreads":
		// hyperthreads never excludes siblings: no exclude-cpuset at all.
		if lastExclude != "" {
			t.Errorf("override to hyperthreads: node %s BalloonsPolicy still has availableResources.cpu = %q; the exclude-cpuset should be gone",
				node, lastExclude)
		}
	case "physical-cores":
		if threads >= 2 {
			if !strings.HasPrefix(lastExclude, "exclude-cpuset:") || strings.TrimPrefix(lastExclude, "exclude-cpuset:") == "" {
				t.Errorf("override to physical-cores on an SMT node (threads-per-core=%d): node %s availableResources.cpu = %q, want a non-empty exclude-cpuset",
					threads, node, lastExclude)
			}
		} else {
			t.Logf("node %s has threads-per-core=%d (no HT siblings); skipping the exclude-cpuset sub-check (indistinguishable)", node, threads)
		}
	}
	t.Logf("override applied on node %s: status cpuAllocation=%q, BalloonsPolicy availableResources.cpu=%q", node, gotAlloc, lastExclude)

	// Cross-node: a non-matching node keeps the cluster default.
	t.Run("non-matching node keeps cluster default", func(t *testing.T) {
		other := ovSecondBalloonsEnabledNode(t, c, node)
		if other == "" {
			skipNoCapability(t, "only one balloons-enabled node; the cross-node comparison needs >=2 nodes")
		}
		otherAlloc, ok := ovPerNodeCPUAllocation(t, c, bccKey, other)
		if !ok {
			t.Fatalf("status.perNode has no entry for the non-matching node %s", other)
		}
		if clusterDefault != "" && otherAlloc != clusterDefault {
			t.Errorf("non-matching node %s: status.perNode cpuAllocation = %q, want the untouched cluster default %q",
				other, otherAlloc, clusterDefault)
		}
		if otherAlloc == overrideAlloc {
			t.Errorf("non-matching node %s picked up the override cpuAllocation %q; the override leaked past its nodeSelector",
				other, overrideAlloc)
		}
		t.Logf("non-matching node %s kept cpuAllocation=%q (cluster default %q)", other, otherAlloc, clusterDefault)
	})
}

// ovOppositeAllocation returns the other cpuAllocation mode; anything but
// "hyperthreads" counts as physical-cores, as in clusterCPUAllocation.
func ovOppositeAllocation(current string) string {
	if current == "hyperthreads" {
		return "physical-cores"
	}
	return "hyperthreads"
}

// ovExcludeMatchesMode reports whether exclude-cpuset fits the mode: hyperthreads emits
// none, physical-cores emits one only on an SMT node.
func ovExcludeMatchesMode(excludeCPU, alloc string, threads int) bool {
	switch alloc {
	case "hyperthreads":
		return excludeCPU == ""
	case "physical-cores":
		if threads >= 2 {
			return strings.HasPrefix(excludeCPU, "exclude-cpuset:") && strings.TrimPrefix(excludeCPU, "exclude-cpuset:") != ""
		}
		return true // non-SMT: no exclude-cpuset either way, so any state is "settled"
	default:
		return true
	}
}

// ovAddNodeLabel stamps the override-target label on the node via a merge patch.
func ovAddNodeLabel(t *testing.T, c client.Client, node string) {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, &n); err != nil {
		t.Fatalf("get node %s to label: %v", node, err)
	}
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, ovLabelKey, ovLabelVal)
	if err := c.Patch(context.Background(), &n, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatalf("label node %s with %s: %v", node, ovLabelKey, err)
	}
}

// ovRemoveNodeLabel deletes the override-target label (merge-patch null).
func ovRemoveNodeLabel(t *testing.T, c client.Client, node string) {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, &n); err != nil {
		t.Errorf("cleanup: get node %s to unlabel: %v", node, err)
		return
	}
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:null}}}`, ovLabelKey)
	if err := c.Patch(context.Background(), &n, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Errorf("cleanup: FAILED to remove label %s from node %s: %v", ovLabelKey, node, err)
	}
}

// ovPatchOverrides merge-patches spec.nodeConfigOverrides: verbatim when present
// (empty stays []), JSON null when absent. isCleanup makes failures non-fatal.
func ovPatchOverrides(ctx context.Context, t *testing.T, c client.Client, key client.ObjectKey, overrides []interface{}, present, isCleanup bool) {
	t.Helper()
	var value interface{}
	if present {
		// A nil slice would marshal to null and clear the field.
		if overrides == nil {
			overrides = []interface{}{}
		}
		value = overrides
	} // else nil -> marshals to null, clearing the field to restore an absent field
	body, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"nodeConfigOverrides": value},
	})
	if err != nil {
		if isCleanup {
			t.Errorf("cleanup: marshal nodeConfigOverrides restore patch: %v", err)
		} else {
			t.Fatalf("marshal nodeConfigOverrides patch: %v", err)
		}
		return
	}
	patchBCC(ctx, c, body, func(verb string, err error) {
		if isCleanup {
			t.Errorf("cleanup: FAILED to restore nodeConfigOverrides (%s): %v", verb, err)
		} else {
			t.Fatalf("%s for nodeConfigOverrides patch: %v", verb, err)
		}
	})
}

// ovPerNodeCPUAllocation returns the resolved cpuAllocation the singleton's
// status.perNode reports for the node, and whether an entry was found.
func ovPerNodeCPUAllocation(t *testing.T, c client.Client, bccKey client.ObjectKey, node string) (string, bool) {
	t.Helper()
	bcc := newUnstructured(bccGVK)
	if err := c.Get(context.Background(), bccKey, bcc); err != nil {
		t.Fatalf("get BalloonsClusterConfig/cluster for status: %v", err)
	}
	perNode, _, _ := unstructured.NestedSlice(bcc.Object, "status", "perNode")
	for _, entry := range perNode {
		m, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(m, "name")
		if name != node {
			continue
		}
		alloc, _, _ := unstructured.NestedString(m, "cpuAllocation")
		return alloc, true
	}
	return "", false
}

// ovSecondBalloonsEnabledNode returns another balloons-enabled node without the
// override label, or "" if there is none.
func ovSecondBalloonsEnabledNode(t *testing.T, c client.Client, exclude string) string {
	t.Helper()
	var nodes corev1.NodeList
	if err := c.List(context.Background(), &nodes); err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Name == exclude {
			continue
		}
		if n.Labels[labelBalloonsEnabled] != "true" {
			continue
		}
		if n.Labels[ovLabelKey] == ovLabelVal {
			continue // must genuinely not match our override
		}
		return n.Name
	}
	return ""
}
