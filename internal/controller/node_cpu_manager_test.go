// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nriv1alpha1 "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"

	"github.com/intel/cpu-pinning-operator/internal/metrics"
)

const cpuManagerTestNode = "n1"

// kubeletConfigMapWith is kubeadm's kubelet-config ConfigMap advertising policy;
// omit it to model a cluster kubeadm did not provision.
func kubeletConfigMapWith(policy string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kubelet-config", Namespace: "kube-system"},
		Data: map[string]string{
			"kubelet": fmt.Sprintf("syncFrequency: 1m0s\ncpuManagerPolicy: %s\n", policy),
		},
	}
}

// kubeletConfigMapOmittingPolicy is a stock kubeadm ConfigMap whose
// KubeletConfiguration never sets cpuManagerPolicy, so the default applies.
func kubeletConfigMapOmittingPolicy() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kubelet-config", Namespace: "kube-system"},
		Data: map[string]string{
			"kubelet": "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nsyncFrequency: 1m0s\n",
		},
	}
}

// cpuManagerReconciler is nodeReconciler with the CPU Manager check reading the same fake client.
func cpuManagerReconciler(t *testing.T, objs ...runtime.Object) (*NodeReconciler, *record.FakeRecorder) {
	t.Helper()
	r, rec := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, objs...)
	r.KubeletConfigReader = r.Client
	return r, rec
}

// cpuManagerNode returns the reconciled pool node, failing the test if it is gone.
func cpuManagerNode(t *testing.T, r *NodeReconciler) *corev1.Node {
	t.Helper()
	node := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: cpuManagerTestNode}, node); err != nil {
		t.Fatalf("get node %s: %v", cpuManagerTestNode, err)
	}
	return node
}

// TestNode_CPUManagerStatic_FlaggedButStillProvisioned: static policy is flagged
// (label, gauge, Warning) but the node stays provisioned.
func TestNode_CPUManagerStatic_FlaggedButStillProvisioned(t *testing.T) {
	metrics.CPUManagerConflict.Reset()
	r, rec := cpuManagerReconciler(t, poolNode(cpuManagerTestNode), kubeletConfigMapWith("static"))
	reconcileNode(t, r, cpuManagerTestNode)

	node := cpuManagerNode(t, r)
	if got := node.Labels[LabelCPUManagerConflict]; got != "true" {
		t.Errorf("%s = %q, want %q", LabelCPUManagerConflict, got, "true")
	}
	if got := testutil.ToFloat64(metrics.CPUManagerConflict.WithLabelValues(cpuManagerTestNode)); got != 1 {
		t.Errorf("cpu_pinning_cpu_manager_conflict{node=\"n1\"} = %v, want 1", got)
	}

	// Exactly one Warning naming the reason an admin greps for.
	var conflicts []string
	for _, e := range drainNodeEvents(rec) {
		if strings.Contains(e, reasonCPUManagerConflict) {
			conflicts = append(conflicts, e)
		}
	}
	if len(conflicts) != 1 {
		t.Fatalf("events with reason %s = %v, want exactly 1", reasonCPUManagerConflict, conflicts)
	}
	if !strings.Contains(conflicts[0], "Warning") {
		t.Errorf("event = %q, want a Warning (a conflict is not informational)", conflicts[0])
	}

	// Flagging must not cost the node its pool label or BalloonsPolicy.
	if got := node.Labels[LabelBalloonsEnabled]; got != "true" {
		t.Errorf("%s = %q, want %q — a flagged node must stay in the pinning pool", LabelBalloonsEnabled, got, "true")
	}
	policy := &nriv1alpha1.BalloonsPolicy{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "node." + cpuManagerTestNode, Namespace: "kube-system"}, policy); err != nil {
		t.Errorf("get BalloonsPolicy node.%s: %v — the flag must not skip provisioning", cpuManagerTestNode, err)
	}
}

// TestNode_CPUManagerNone_NotFlagged: policy none publishes label "false" and gauge 0,
// no Warning; a missing label would count as unread in the singleton's condition.
func TestNode_CPUManagerNone_NotFlagged(t *testing.T) {
	metrics.CPUManagerConflict.Reset()
	r, rec := cpuManagerReconciler(t, poolNode(cpuManagerTestNode), kubeletConfigMapWith("none"))
	reconcileNode(t, r, cpuManagerTestNode)

	node := cpuManagerNode(t, r)
	if got := node.Labels[LabelCPUManagerConflict]; got != "false" {
		t.Errorf("%s = %q, want %q on a node whose kubelet was read and runs the none policy", LabelCPUManagerConflict, got, "false")
	}
	if got := testutil.ToFloat64(metrics.CPUManagerConflict.WithLabelValues(cpuManagerTestNode)); got != 0 {
		t.Errorf("cpu_pinning_cpu_manager_conflict{node=\"n1\"} = %v, want 0", got)
	}
	for _, e := range drainNodeEvents(rec) {
		if strings.Contains(e, reasonCPUManagerConflict) {
			t.Errorf("event %q emitted on a compatible node, want none", e)
		}
	}
}

// TestNode_CPUManagerPolicyOmitted_ReadsAsCompatible: an omitted cpuManagerPolicy
// (stock kubeadm) reads as compatible: label "false", gauge 0, no Warning.
func TestNode_CPUManagerPolicyOmitted_ReadsAsCompatible(t *testing.T) {
	metrics.CPUManagerConflict.Reset()
	r, rec := cpuManagerReconciler(t, poolNode(cpuManagerTestNode), kubeletConfigMapOmittingPolicy())
	reconcileNode(t, r, cpuManagerTestNode)

	node := cpuManagerNode(t, r)
	if got := node.Labels[LabelCPUManagerConflict]; got != "false" {
		t.Errorf("%s = %q, want %q on a node whose KubeletConfiguration omits cpuManagerPolicy (the default is none)",
			LabelCPUManagerConflict, got, "false")
	}
	if n := testutil.CollectAndCount(metrics.CPUManagerConflict); n != 1 {
		t.Errorf("cpu_pinning_cpu_manager_conflict series count = %d, want 1 (the verdict was read, so it is published)", n)
	}
	if got := testutil.ToFloat64(metrics.CPUManagerConflict.WithLabelValues(cpuManagerTestNode)); got != 0 {
		t.Errorf("cpu_pinning_cpu_manager_conflict{node=\"n1\"} = %v, want 0", got)
	}
	for _, e := range drainNodeEvents(rec) {
		if strings.Contains(e, reasonCPUManagerConflict) {
			t.Errorf("event %q emitted on a compatible node, want none", e)
		}
	}
}

// TestNode_CPUManagerUnreadable_IsUnknownNotCompatible: an unreadable kubelet clears
// the label and gauge series and emits no event, even when previously flagged.
func TestNode_CPUManagerUnreadable_IsUnknownNotCompatible(t *testing.T) {
	metrics.CPUManagerConflict.Reset()
	metrics.CPUManagerConflict.WithLabelValues(cpuManagerTestNode).Set(1) // stale verdict from an earlier pass
	flagged := poolNode(cpuManagerTestNode)
	flagged.Labels = map[string]string{LabelBalloonsEnabled: "true", LabelCPUManagerConflict: "true"}

	// No kubelet-config ConfigMap: the read returns NotFound.
	r, rec := cpuManagerReconciler(t, flagged)
	reconcileNode(t, r, cpuManagerTestNode)

	node := cpuManagerNode(t, r)
	if _, ok := node.Labels[LabelCPUManagerConflict]; ok {
		t.Errorf("%s survived an unreadable configuration, want it dropped (the label claims an OBSERVED conflict)", LabelCPUManagerConflict)
	}
	if n := testutil.CollectAndCount(metrics.CPUManagerConflict); n != 0 {
		t.Errorf("cpu_pinning_cpu_manager_conflict series count = %d, want 0 (unknown is an absent series, never 0)", n)
	}
	for _, e := range drainNodeEvents(rec) {
		if strings.Contains(e, reasonCPUManagerConflict) {
			t.Errorf("event %q emitted for an unreadable configuration, want none", e)
		}
	}
}

// TestNode_CPUManagerUnrecognizedPolicy_IsUnknown: an unrecognised policy is treated
// as unknown: no label, no series, no event.
func TestNode_CPUManagerUnrecognizedPolicy_IsUnknown(t *testing.T) {
	for _, policy := range []string{"future-policy", "Static"} {
		t.Run(policy, func(t *testing.T) {
			metrics.CPUManagerConflict.Reset()
			r, rec := cpuManagerReconciler(t, poolNode(cpuManagerTestNode), kubeletConfigMapWith(policy))
			reconcileNode(t, r, cpuManagerTestNode)

			node := cpuManagerNode(t, r)
			if got, ok := node.Labels[LabelCPUManagerConflict]; ok {
				t.Errorf("%s = %q for policy %q, want the label absent (unrecognised is unknown, not compatible)",
					LabelCPUManagerConflict, got, policy)
			}
			if n := testutil.CollectAndCount(metrics.CPUManagerConflict); n != 0 {
				t.Errorf("cpu_pinning_cpu_manager_conflict series count = %d for policy %q, want 0 (unknown is an absent series, never 0)", n, policy)
			}
			for _, e := range drainNodeEvents(rec) {
				if strings.Contains(e, reasonCPUManagerConflict) {
					t.Errorf("event %q emitted for policy %q, want none (only an observed static policy warns)", e, policy)
				}
			}
			if node.Labels[LabelBalloonsEnabled] != "true" {
				t.Errorf("node not provisioned under policy %q, want provisioned as before", policy)
			}
		})
	}
}

// TestNode_CPUManagerConflictClearedWhenPolicyChanges: once the kubelet reads none,
// the label becomes "false" and the gauge drops to 0.
func TestNode_CPUManagerConflictClearedWhenPolicyChanges(t *testing.T) {
	metrics.CPUManagerConflict.Reset()
	flagged := poolNode(cpuManagerTestNode)
	flagged.Labels = map[string]string{LabelBalloonsEnabled: "true", LabelCPUManagerConflict: "true"}

	r, _ := cpuManagerReconciler(t, flagged, kubeletConfigMapWith("none"))
	reconcileNode(t, r, cpuManagerTestNode)

	node := cpuManagerNode(t, r)
	if got := node.Labels[LabelCPUManagerConflict]; got != "false" {
		t.Errorf("%s = %q after a kubelet re-read as none, want %q (the conflict claim withdrawn, the node still checked)",
			LabelCPUManagerConflict, got, "false")
	}
	if got := testutil.ToFloat64(metrics.CPUManagerConflict.WithLabelValues(cpuManagerTestNode)); got != 0 {
		t.Errorf("cpu_pinning_cpu_manager_conflict{node=\"n1\"} = %v, want 0 after the policy changed", got)
	}
}

// TestNode_CPUManagerCheckUnwired: a nil KubeletConfigReader behaves like an
// unreadable configuration instead of panicking.
func TestNode_CPUManagerCheckUnwired(t *testing.T) {
	metrics.CPUManagerConflict.Reset()
	// Reader nil but ConfigMap seeded, so the check is off rather than unable to read.
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe},
		poolNode(cpuManagerTestNode), kubeletConfigMapWith("static"))
	reconcileNode(t, r, cpuManagerTestNode)

	node := cpuManagerNode(t, r)
	if _, ok := node.Labels[LabelCPUManagerConflict]; ok {
		t.Errorf("%s stamped with no KubeletConfigReader wired, want absent", LabelCPUManagerConflict)
	}
	if node.Labels[LabelBalloonsEnabled] != "true" {
		t.Error("node not provisioned with the CPU Manager check unwired, want provisioned as before")
	}
	if n := testutil.CollectAndCount(metrics.CPUManagerConflict); n != 0 {
		t.Errorf("cpu_pinning_cpu_manager_conflict series count = %d, want 0 (unwired is unknown)", n)
	}
}

// TestNode_PruneDropsCPUManagerSeries: a deleted node's conflict series is removed.
func TestNode_PruneDropsCPUManagerSeries(t *testing.T) {
	metrics.CPUManagerConflict.Reset()
	metrics.CPUManagerConflict.WithLabelValues("gone").Set(1)
	metrics.CPUManagerConflict.WithLabelValues("stays").Set(1)

	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe})
	if _, err := r.pruneNode(context.Background(), "gone"); err != nil {
		t.Fatalf("pruneNode: %v", err)
	}
	if got := testutil.ToFloat64(metrics.CPUManagerConflict.WithLabelValues("stays")); got != 1 {
		t.Errorf("cpu_pinning_cpu_manager_conflict{node=\"stays\"} = %v, want 1 (prune must not touch other nodes)", got)
	}
	if n := testutil.CollectAndCount(metrics.CPUManagerConflict); n != 1 {
		t.Errorf("cpu_pinning_cpu_manager_conflict series count = %d, want 1 (the pruned node's series dropped)", n)
	}
}
