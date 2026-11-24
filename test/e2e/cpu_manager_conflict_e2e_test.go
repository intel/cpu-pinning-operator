// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Both cases hold on a policy=none cluster and on a static one; switching a cluster to
// static needs a drain and kubelet restart, so no run assumes it.

package e2e

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// labelCPUManagerConflict is "true" (static), "false" (none or defaulted) or
	// absent (unread or unrecognised policy, never "compatible").
	labelCPUManagerConflict = "cpu-pinning-operator.intel.com/cpu-manager-conflict"
	// cpuManagerCondType is the BalloonsClusterConfig condition summarising the pool.
	cpuManagerCondType = "CPUManagerCompatible"
)

// TestCPUManagerConflictMetricExported proves the per-node conflict gauge is exported
// and agrees with the node label and the BalloonsClusterConfig condition.
func TestCPUManagerConflictMetricExported(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_METRICS") == "" {
		skipNotSelected(at, "set E2E_METRICS=1 to scrape the operator metrics endpoint via the apiserver proxy")
	}
	c := newClient(at)
	node := balloonsEnabledNode(at, c)

	// The gauge is set per node reconcile; a node mid-reprovision can lack it.
	waitForNodeSettled(t, c, node)

	// A just-started manager serves no per-node series yet; wait so a rollout does not
	// read as an unknown policy.
	const metric = "cpu_pinning_cpu_manager_conflict"
	body := scrapeOperatorMetricsForNode(t, node, metric)

	if !metricPresentForNode(body, metric, node) {
		// An omitted cpuManagerPolicy still exports 0, so absence means
		// kube-system/kubelet-config is missing, malformed, or names an unknown policy.
		skipNoCapability(at, "%s absent for node %q — the cluster's cpuManagerPolicy is unknown: "+
			"kube-system/kubelet-config is absent, carries no %q key, does not parse, carries a "+
			"non-string policy, omits it without declaring kind: KubeletConfiguration, or names a "+
			"policy the operator does not recognise "+
			"(an omitted cpuManagerPolicy in a KubeletConfiguration would read as none and export 0)",
			metric, node, "kubelet")
	}
	v, ok := metricValueForNode(body, metric, node)
	if !ok {
		at.Fatalf("could not parse %s sample for node %q", metric, node)
	}
	if v != 0 && v != 1 {
		at.Fatalf("%s = %v for node %q, want an indicator value (0 = kubelet leaves cpusets to the "+
			"plugin, 1 = static policy)", metric, v, node)
	}

	// A 0 gauge needs an explicit "false" label: absent means unread, not compatible.
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, &n); err != nil {
		at.Fatalf("get node %s: %v", node, err)
	}
	wantLabel := "false"
	if v == 1 {
		wantLabel = "true"
	}
	if got := n.Labels[labelCPUManagerConflict]; got != wantLabel {
		at.Errorf("%s = %v for node %q but label %s=%q, want %q: the gauge and the label must report "+
			"the same policy read, and a published verdict must be published in both",
			metric, v, node, labelCPUManagerConflict, got, wantLabel)
	}
	if v == 1 {
		at.Logf("the cluster's kubelets run the CPU Manager static policy alongside the balloons "+
			"plugin: placement on node %q is unpredictable and the node stays flagged, not skipped", node)
	}
}

// TestCPUManagerConditionMatchesNodeLabels proves CPUManagerCompatible follows the
// conflict labels across all provisioned nodes (False if any flagged, Unknown if any
// unread, else True), and that the labels agree fleet-wide.
func TestCPUManagerConditionMatchesNodeLabels(t *testing.T) {
	at := allureCase(t)
	ctx := context.Background()
	c := newClient(at)
	node := balloonsEnabledNode(at, c)
	waitForNodeSettled(t, c, node)

	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes, client.MatchingLabels{labelBalloonsEnabled: "true"}); err != nil {
		at.Fatalf("list balloons-enabled nodes: %v", err)
	}
	var flagged, unread []string
	for i := range nodes.Items {
		switch nodes.Items[i].Labels[labelCPUManagerConflict] {
		case "true":
			flagged = append(flagged, nodes.Items[i].Name)
		case "false":
		default:
			unread = append(unread, nodes.Items[i].Name)
		}
	}

	if len(flagged) > 0 && len(flagged) != len(nodes.Items) {
		at.Errorf("%d of %d balloons-enabled node(s) carry %s=true (%v): the verdict comes from one "+
			"cluster-wide kubelet configuration, so the whole pool must agree",
			len(flagged), len(nodes.Items), labelCPUManagerConflict, flagged)
	}

	bcc, found, err := getBCC(ctx, c)
	if err != nil {
		at.Fatalf("get BalloonsClusterConfig/cluster: %v", err)
	}
	if !found {
		skipNoConfig(at, "BalloonsClusterConfig/%s not found — the operator writes the %s condition on the singleton",
			bccName, cpuManagerCondType)
	}

	status, reason, present := bccConditionStatus(at, bcc, cpuManagerCondType)
	if !present {
		skipNoConfig(at, "BalloonsClusterConfig/%s has no %s condition yet — the status is written on "+
			"singleton reconcile", bccName, cpuManagerCondType)
	}

	// Precedence matches the singleton: conflict > unread > compatible.
	want := "True"
	switch {
	case len(flagged) > 0:
		want = "False"
	case len(unread) > 0:
		want = "Unknown"
	}
	if status != want {
		at.Errorf("%s = %s (reason %s), want %s: of %d balloons-enabled node(s), %d carry %s=true (%v) and %d carry no verdict "+
			"at all (%v). The condition and the node labels must report one reading of the fleet",
			cpuManagerCondType, status, reason, want, len(nodes.Items), len(flagged), labelCPUManagerConflict, flagged,
			len(unread), unread)
	}

	// A flagged node is surfaced, not withdrawn from the pinning pool.
	if len(flagged) > 0 {
		for _, name := range flagged {
			if !nodeInPerNode(at, bcc, name) {
				at.Errorf("node %q is flagged %s=true but is absent from status.perNode: a conflicting "+
					"node is flagged, not skipped", name, labelCPUManagerConflict)
			}
		}
	}
}

// bccConditionStatus returns a singleton condition's status and reason, false if absent.
func bccConditionStatus(t testing.TB, bcc *unstructured.Unstructured, condType string) (status, reason string, present bool) {
	t.Helper()
	conds, _, err := unstructured.NestedSlice(bcc.Object, "status", "conditions")
	if err != nil {
		t.Fatalf("read status.conditions: %v", err)
	}
	for _, raw := range conds {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] != condType {
			continue
		}
		s, _ := m["status"].(string)
		r, _ := m["reason"].(string)
		return s, r, true
	}
	return "", "", false
}

// nodeInPerNode reports whether the singleton's status.perNode lists the node.
func nodeInPerNode(t testing.TB, bcc *unstructured.Unstructured, node string) bool {
	t.Helper()
	perNode, _, err := unstructured.NestedSlice(bcc.Object, "status", "perNode")
	if err != nil {
		t.Fatalf("read status.perNode: %v", err)
	}
	for _, raw := range perNode {
		if m, ok := raw.(map[string]any); ok && m["name"] == node {
			return true
		}
	}
	return false
}
