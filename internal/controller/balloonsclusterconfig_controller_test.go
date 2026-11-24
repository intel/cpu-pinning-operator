// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/classification"
	"github.com/intel/cpu-pinning-operator/internal/match"
)

func intPtr(n int) *int { return &n }

func clusterConfigReconciler(objs ...runtime.Object) (*BalloonsClusterConfigReconciler, *match.Index) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&cpopv1alpha1.BalloonsClusterConfig{}).
		WithRuntimeObjects(objs...).
		Build()
	idx := match.NewIndex()
	return &BalloonsClusterConfigReconciler{
		Client:      c,
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: idx,
	}, idx
}

// TestClusterConfig_PublishesKnobs: cpuAllocation, reserved pool and node exclusion reach the Index.
func TestClusterConfig_PublishesKnobs(t *testing.T) {
	cfg := &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
		Spec: cpopv1alpha1.BalloonsClusterConfigSpec{
			Balloon: cpopv1alpha1.BalloonSpec{
				CPUAllocation: "hyperthreads",
				Reserved:      &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(18)},
			},
			ExcludeNodeSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"cpu-pinning-operator.intel.com/disabled": "true"},
			},
		},
	}
	r, idx := clusterConfigReconciler(cfg)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := idx.CPUAllocation(); got != "hyperthreads" {
		t.Errorf("CPUAllocation = %q, want hyperthreads", got)
	}
	got := idx.ReservedSizing()
	if got == nil || got.Cpus == nil || *got.Cpus != 18 {
		t.Errorf("ReservedSizing = %+v, want Cpus=18", got)
	}
	if !idx.NodeExcluded(map[string]string{"cpu-pinning-operator.intel.com/disabled": "true"}) {
		t.Error("excluded node should match the published excludeNodeSelector")
	}
}

// TestClusterConfig_PublishesNodeOverrides: a matching node resolves to the override,
// a non-matching one to the cluster default.
func TestClusterConfig_PublishesNodeOverrides(t *testing.T) {
	cfg := &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
		Spec: cpopv1alpha1.BalloonsClusterConfigSpec{
			Balloon: cpopv1alpha1.BalloonSpec{
				CPUAllocation: "hyperthreads",
				Reserved:      &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(12)},
			},
			NodeConfigOverrides: []cpopv1alpha1.NodeConfigOverride{{
				NodeSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"node-tier": "perf"},
				},
				Balloon: cpopv1alpha1.BalloonSpec{
					CPUAllocation: "physical-cores",
					Reserved:      &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(20)},
				},
			}},
		},
	}
	r, idx := clusterConfigReconciler(cfg)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	alloc, reserved := idx.ResolveNodeConfig(map[string]string{"node-tier": "perf"})
	if alloc != "physical-cores" {
		t.Errorf("matching node cpuAllocation = %q, want physical-cores (override)", alloc)
	}
	if reserved == nil || reserved.Cpus == nil || *reserved.Cpus != 20 {
		t.Errorf("matching node reserved = %+v, want Cpus=20 (override)", reserved)
	}

	alloc, reserved = idx.ResolveNodeConfig(map[string]string{"node-tier": "standard"})
	if alloc != "hyperthreads" {
		t.Errorf("non-matching node cpuAllocation = %q, want hyperthreads (cluster default)", alloc)
	}
	if reserved == nil || reserved.Cpus == nil || *reserved.Cpus != 12 {
		t.Errorf("non-matching node reserved = %+v, want Cpus=12 (cluster default)", reserved)
	}
}

// TestClusterConfig_InvalidNodeOverrideDropped: an override with a nil NodeSelector
// is dropped, not treated as match-all.
func TestClusterConfig_InvalidNodeOverrideDropped(t *testing.T) {
	cfg := &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
		Spec: cpopv1alpha1.BalloonsClusterConfigSpec{
			Balloon: cpopv1alpha1.BalloonSpec{CPUAllocation: "physical-cores"},
			NodeConfigOverrides: []cpopv1alpha1.NodeConfigOverride{{
				NodeSelector: nil, // dropped, not match-all
				Balloon:      cpopv1alpha1.BalloonSpec{CPUAllocation: "hyperthreads"},
			}},
		},
	}
	r, idx := clusterConfigReconciler(cfg)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	alloc, _ := idx.ResolveNodeConfig(map[string]string{"any": "label"})
	if alloc != "physical-cores" {
		t.Errorf("cpuAllocation = %q, want physical-cores (nil-selector override must be dropped)", alloc)
	}
}

// TestClusterConfig_EmptyNodeOverrideDropped: an override setting no field is dropped
// at compile time so it cannot shadow a later matching override (runtime backstop to CEL).
func TestClusterConfig_EmptyNodeOverrideDropped(t *testing.T) {
	cfg := &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
		Spec: cpopv1alpha1.BalloonsClusterConfigSpec{
			Balloon: cpopv1alpha1.BalloonSpec{CPUAllocation: "physical-cores"},
			NodeConfigOverrides: []cpopv1alpha1.NodeConfigOverride{
				{
					// Matches the node but sets no honored field.
					NodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"node-tier": "perf"},
					},
					Balloon: cpopv1alpha1.BalloonSpec{},
				},
				{
					// Later override for the same node, setting a field.
					NodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"node-tier": "perf"},
					},
					Balloon: cpopv1alpha1.BalloonSpec{CPUAllocation: "hyperthreads"},
				},
			},
		},
	}
	r, idx := clusterConfigReconciler(cfg)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	alloc, _ := idx.ResolveNodeConfig(map[string]string{"node-tier": "perf"})
	if alloc != "hyperthreads" {
		t.Errorf("cpuAllocation = %q, want hyperthreads (empty override must not shadow the later one)", alloc)
	}
}

// TestClusterConfig_EmptySelectorNodeOverrideDropped: an empty nodeSelector compiles
// to labels.Everything() and would shadow every node, so it is dropped (runtime
// backstop to CEL).
func TestClusterConfig_EmptySelectorNodeOverrideDropped(t *testing.T) {
	cfg := &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
		Spec: cpopv1alpha1.BalloonsClusterConfigSpec{
			Balloon: cpopv1alpha1.BalloonSpec{CPUAllocation: "physical-cores"},
			NodeConfigOverrides: []cpopv1alpha1.NodeConfigOverride{
				{
					// Empty selector matches every node; must be dropped.
					NodeSelector: &metav1.LabelSelector{},
					Balloon:      cpopv1alpha1.BalloonSpec{CPUAllocation: "hyperthreads"},
				},
				{
					// Later override with a real selector, setting reserved.
					NodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"node-tier": "perf"},
					},
					Balloon: cpopv1alpha1.BalloonSpec{Reserved: &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(20)}},
				},
			},
		},
	}
	r, idx := clusterConfigReconciler(cfg)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	alloc, reserved := idx.ResolveNodeConfig(map[string]string{"node-tier": "standard"})
	if alloc != "physical-cores" {
		t.Errorf("unrelated node cpuAllocation = %q, want physical-cores (empty selector must be dropped, not fleet-wide)", alloc)
	}
	if reserved != nil {
		t.Errorf("unrelated node reserved = %+v, want nil cluster default", reserved)
	}

	// Reserved from the override; cpuAllocation falls back per field to the cluster default.
	alloc, reserved = idx.ResolveNodeConfig(map[string]string{"node-tier": "perf"})
	if alloc != "physical-cores" {
		t.Errorf("matching node cpuAllocation = %q, want physical-cores (cluster default; reserved-only override)", alloc)
	}
	if reserved == nil || reserved.Cpus == nil || *reserved.Cpus != 20 {
		t.Errorf("matching node reserved = %+v, want Cpus=20 (later override, not shadowed)", reserved)
	}
}

// TestClusterConfig_PackingIsClusterGlobalNotOverridable: packing is published at the
// cluster level (empty means spread), but a packing-only override sets nothing and is dropped.
func TestClusterConfig_PackingIsClusterGlobalNotOverridable(t *testing.T) {
	cfg := &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
		Spec: cpopv1alpha1.BalloonsClusterConfigSpec{
			Balloon: cpopv1alpha1.BalloonSpec{
				CPUAllocation: "physical-cores",
				Packing:       "binpack", // cluster-level packing is honored
			},
			NodeConfigOverrides: []cpopv1alpha1.NodeConfigOverride{
				{
					// Packing-only override: no resolved field, so dropped.
					NodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"node-tier": "perf"},
					},
					Balloon: cpopv1alpha1.BalloonSpec{Packing: "spread"},
				},
				{
					// Later override for the same node that DOES set a resolved field.
					NodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"node-tier": "perf"},
					},
					Balloon: cpopv1alpha1.BalloonSpec{CPUAllocation: "hyperthreads"},
				},
			},
		},
	}
	r, idx := clusterConfigReconciler(cfg)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := idx.Packing(); got != "binpack" {
		t.Errorf("Packing() = %q, want binpack (cluster-level packing honored)", got)
	}

	alloc, _ := idx.ResolveNodeConfig(map[string]string{"node-tier": "perf"})
	if alloc != "hyperthreads" {
		t.Errorf("cpuAllocation = %q, want hyperthreads (packing-only override must be dropped, not shadow the later one)", alloc)
	}
}

// TestClusterConfig_PackingDefaultsToSpread: omitted packing normalizes to spread in
// code; the shared BalloonSpec has no CRD default so overrides are not auto-filled.
func TestClusterConfig_PackingDefaultsToSpread(t *testing.T) {
	cfg := &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
		Spec: cpopv1alpha1.BalloonsClusterConfigSpec{
			Balloon: cpopv1alpha1.BalloonSpec{CPUAllocation: "physical-cores"}, // packing omitted
		},
	}
	r, idx := clusterConfigReconciler(cfg)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := idx.Packing(); got != "spread" {
		t.Errorf("Packing() = %q, want spread (empty normalized in-code)", got)
	}
}

// TestClusterConfig_ReservedNamespacesThreadedThrough: reservedNamespaces is published
// verbatim, keeping nil (not configured), empty (route nothing extra) and scoped lists distinct.
func TestClusterConfig_ReservedNamespacesThreadedThrough(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		// wantNil asserts nil; otherwise want is the exact slice.
		wantNil bool
		want    []string
	}{
		{"unset is nil (defaulted downstream)", nil, true, nil},
		{"scoped list passed through", []string{"inference", "kube-system"}, false, []string{"inference", "kube-system"}},
		{"explicit empty is honored, not re-defaulted", []string{}, false, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &cpopv1alpha1.BalloonsClusterConfig{
				ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
				Spec: cpopv1alpha1.BalloonsClusterConfigSpec{
					Balloon: cpopv1alpha1.BalloonSpec{
						CPUAllocation:      "physical-cores",
						ReservedNamespaces: tc.in,
					},
				},
			}
			r, idx := clusterConfigReconciler(cfg)
			if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			got := idx.ReservedNamespaces()
			if tc.wantNil {
				if got != nil {
					t.Errorf("ReservedNamespaces() = %v, want nil (unset must stay nil so the Node reconciler applies the default)", got)
				}
				return
			}
			// A configured list must be non-nil, or [] would read as not configured.
			if got == nil {
				t.Fatalf("ReservedNamespaces() = nil, want non-nil %v (a configured list must stay non-nil to preserve the explicit-empty vs unset distinction)", tc.want)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ReservedNamespaces() = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("ReservedNamespaces()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestClusterConfig_Absent_ClearsIndex: no singleton resets the Index to defaults.
func TestClusterConfig_Absent_ClearsIndex(t *testing.T) {
	r, idx := clusterConfigReconciler()

	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{"x": "y"},
	})
	idx.SetClusterConfig(match.ClusterConfig{CPUAllocation: "hyperthreads", ExcludeNode: sel})

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := idx.CPUAllocation(); got != "" {
		t.Errorf("CPUAllocation = %q, want empty after clearing", got)
	}
	if idx.NodeExcluded(map[string]string{"x": "y"}) {
		t.Error("no node should be excluded after the singleton is gone")
	}
}

// TestClusterConfig_FailurePolicy_PreservesForeignNamespaceSelector: the failurePolicy
// read-modify-write leaves another writer's namespaceSelector intact.
func TestClusterConfig_FailurePolicy_PreservesForeignNamespaceSelector(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	_ = admissionregistrationv1.AddToScheme(scheme)

	cfg := &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
		Spec: cpopv1alpha1.BalloonsClusterConfigSpec{
			Webhook: cpopv1alpha1.WebhookSpec{FailurePolicy: string(admissionregistrationv1.Fail)},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&cpopv1alpha1.BalloonsClusterConfig{}).
		WithRuntimeObjects(cfg, webhookConfig()).
		Build()

	// A different field manager sets the selector first.
	foreignSel := &metav1.LabelSelector{MatchLabels: map[string]string{"pinning": "enabled"}}
	fp := admissionregistrationv1.Ignore // some other manager also set a policy; ours must take over
	foreign := &admissionregistrationv1.MutatingWebhookConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: admissionregistrationv1.SchemeGroupVersion.String(),
			Kind:       "MutatingWebhookConfiguration",
		},
		ObjectMeta: metav1.ObjectMeta{Name: webhookConfigName()},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{
			Name:              webhookName,
			NamespaceSelector: foreignSel,
			FailurePolicy:     &fp,
		}},
	}
	if err := c.Patch(context.Background(), foreign, client.Apply,
		client.FieldOwner("foreign-namespace-selector-owner"), client.ForceOwnership); err != nil {
		t.Fatalf("foreign apply: %v", err)
	}

	r := &BalloonsClusterConfigReconciler{Client: c, Log: ctrl.Log.WithName("test"), PolicyIndex: match.NewIndex()}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	sel, gotFP := webhookFieldOf(t, c)
	if gotFP == nil || *gotFP != admissionregistrationv1.Fail {
		t.Errorf("failurePolicy = %v, want Fail (the reconciler must take over and set it)", gotFP)
	}
	if sel == nil {
		t.Fatal("namespaceSelector is nil — the failurePolicy write clobbered the foreign field")
	}
	if sel.MatchLabels["pinning"] != "enabled" {
		t.Errorf("namespaceSelector = %+v, want the foreign {pinning: enabled} preserved", sel)
	}
}

// TestClusterConfig_Status: the pool inventory from balloons-enabled nodes is reported on status.
func TestClusterConfig_Status(t *testing.T) {
	cfg := &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
	}
	n1 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "n1",
		Labels: map[string]string{
			LabelBalloonsEnabled: "true", LabelMaxSpan: "4", LabelNumaNodes: "2", LabelPhysicalCPUs: "64",
			// Per-node resolved config, projected into status.
			LabelReservedCPUs: "20", LabelCPUAllocation: "hyperthreads",
		},
	}}
	n2 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}} // not balloons-enabled
	r, _ := clusterConfigReconciler(cfg, n1, n2)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &cpopv1alpha1.BalloonsClusterConfig{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: cpopv1alpha1.ClusterConfigName}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.ProvisionedNodes != 1 {
		t.Errorf("ProvisionedNodes = %d, want 1", got.Status.ProvisionedNodes)
	}
	if len(got.Status.PerNode) != 1 || got.Status.PerNode[0].Name != "n1" {
		t.Fatalf("PerNode = %+v, want single entry for n1", got.Status.PerNode)
	}
	if got.Status.PerNode[0].ReservedCPUs != 20 {
		t.Errorf("PerNode[0].ReservedCPUs = %d, want 20 (projected from node label)", got.Status.PerNode[0].ReservedCPUs)
	}
	if got.Status.PerNode[0].CPUAllocation != "hyperthreads" {
		t.Errorf("PerNode[0].CPUAllocation = %q, want hyperthreads (projected from node label)", got.Status.PerNode[0].CPUAllocation)
	}
}

// TestClusterConfig_DeletionNeedsNoFinalizer: reconciling a deleting singleton is a no-op;
// GC via ownerReferences and the post-delete hook do the cleanup, so no finalizer.
func TestClusterConfig_DeletionNeedsNoFinalizer(t *testing.T) {
	now := metav1.Now()
	cfg := &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:              cpopv1alpha1.ClusterConfigName,
			DeletionTimestamp: &now,
			Finalizers:        []string{"external.example.com/keep-alive"},
		},
	}
	r, idx := clusterConfigReconciler(cfg)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile of deleting singleton: %v", err)
	}

	// Only the unrelated external finalizer remains, untouched.
	got := &cpopv1alpha1.BalloonsClusterConfig{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: cpopv1alpha1.ClusterConfigName}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != "external.example.com/keep-alive" {
		t.Errorf("finalizers = %v, want only the untouched external finalizer (no cleanup finalizer added)", got.Finalizers)
	}
	// The object still exists, so its config is still published.
	_ = idx
}

// TestClusterConfig_FreshReconcileAddsNoFinalizer: a normal reconcile adds no finalizer.
func TestClusterConfig_FreshReconcileAddsNoFinalizer(t *testing.T) {
	cfg := &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
	}
	r, _ := clusterConfigReconciler(cfg)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &cpopv1alpha1.BalloonsClusterConfig{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: cpopv1alpha1.ClusterConfigName}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want none (ownerRef-GC model uses no finalizer)", got.Finalizers)
	}
}

// cpuManagerFleetNode builds a pool node with the CPU Manager label "true", "false",
// or "" (unread, label omitted).
func cpuManagerFleetNode(name string, provisioned bool, verdict string) *corev1.Node {
	labels := map[string]string{}
	if provisioned {
		labels[LabelBalloonsEnabled] = "true"
	}
	if verdict != "" {
		labels[LabelCPUManagerConflict] = verdict
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// cpuManagerConditionOf reconciles the singleton and returns its CPUManagerCompatible condition.
func cpuManagerConditionOf(t *testing.T, nodes ...runtime.Object) *metav1.Condition {
	t.Helper()
	cfg := &cpopv1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName},
	}
	r, _ := clusterConfigReconciler(append([]runtime.Object{cfg}, nodes...)...)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &cpopv1alpha1.BalloonsClusterConfig{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: cpopv1alpha1.ClusterConfigName}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, "CPUManagerCompatible")
	if c == nil {
		t.Fatalf("no CPUManagerCompatible condition on the singleton; conditions = %+v", got.Status.Conditions)
	}
	return c
}

// TestClusterConfig_CPUManagerCondition_NoConflict: True/NoConflictDetected needs an
// explicit "false" verdict on every provisioned node.
func TestClusterConfig_CPUManagerCondition_NoConflict(t *testing.T) {
	c := cpuManagerConditionOf(t,
		cpuManagerFleetNode("n1", true, "false"),
		cpuManagerFleetNode("n2", true, "false"),
	)
	if c.Status != metav1.ConditionTrue || c.Reason != "NoConflictDetected" {
		t.Errorf("condition = %v/%q, want True/NoConflictDetected (%q)", c.Status, c.Reason, c.Message)
	}
}

// TestClusterConfig_CPUManagerCondition_UnreadIsUnknown: an unread kubelet config is
// Unknown, never True (the common case without kube-system/kubelet-config).
func TestClusterConfig_CPUManagerCondition_UnreadIsUnknown(t *testing.T) {
	c := cpuManagerConditionOf(t,
		cpuManagerFleetNode("n1", true, ""),
		cpuManagerFleetNode("n2", true, ""),
	)
	if c.Status != metav1.ConditionUnknown || c.Reason != "PolicyUnread" {
		t.Fatalf("condition = %v/%q, want Unknown/PolicyUnread — an unread kubelet must not report compatible (%q)",
			c.Status, c.Reason, c.Message)
	}
	if !strings.Contains(c.Message, "2 provisioned node") {
		t.Errorf("message = %q, want the count of unread nodes", c.Message)
	}
	if !strings.Contains(c.Message, "kubelet-config") {
		t.Errorf("message = %q, want it to name the ConfigMap the reading comes from", c.Message)
	}
}

// TestClusterConfig_CPUManagerCondition_PartiallyReadIsUnknown: one unread node makes the pool Unknown.
func TestClusterConfig_CPUManagerCondition_PartiallyReadIsUnknown(t *testing.T) {
	c := cpuManagerConditionOf(t,
		cpuManagerFleetNode("checked-a", true, "false"),
		cpuManagerFleetNode("unread", true, ""),
		cpuManagerFleetNode("checked-b", true, "false"),
	)
	if c.Status != metav1.ConditionUnknown || c.Reason != "PolicyUnread" {
		t.Fatalf("condition = %v/%q, want Unknown/PolicyUnread (%q)", c.Status, c.Reason, c.Message)
	}
	if !strings.Contains(c.Message, "1 provisioned node") {
		t.Errorf("message = %q, want the count 1 (only the unread node counts)", c.Message)
	}
}

// TestClusterConfig_CPUManagerCondition_ConflictOutranksUnread: False wins over Unknown.
func TestClusterConfig_CPUManagerCondition_ConflictOutranksUnread(t *testing.T) {
	c := cpuManagerConditionOf(t,
		cpuManagerFleetNode("static-node", true, "true"),
		cpuManagerFleetNode("unread", true, ""),
	)
	if c.Status != metav1.ConditionFalse || c.Reason != "CPUManagerStatic" {
		t.Fatalf("condition = %v/%q, want False/CPUManagerStatic (%q)", c.Status, c.Reason, c.Message)
	}
	if !strings.Contains(c.Message, "static-node") {
		t.Errorf("message = %q, want it to name static-node", c.Message)
	}
}

// TestClusterConfig_CPUManagerCondition_OneFlaggedAmongHealthy: one flagged node flips
// the condition and is named.
func TestClusterConfig_CPUManagerCondition_OneFlaggedAmongHealthy(t *testing.T) {
	c := cpuManagerConditionOf(t,
		cpuManagerFleetNode("healthy-a", true, "false"),
		cpuManagerFleetNode("static-node", true, "true"),
		cpuManagerFleetNode("healthy-b", true, "false"),
	)
	if c.Status != metav1.ConditionFalse || c.Reason != "CPUManagerStatic" {
		t.Fatalf("condition = %v/%q, want False/CPUManagerStatic (%q)", c.Status, c.Reason, c.Message)
	}
	if !strings.Contains(c.Message, "static-node") {
		t.Errorf("message = %q, want it to name static-node", c.Message)
	}
	if !strings.Contains(c.Message, "1 provisioned node") {
		t.Errorf("message = %q, want the count 1 (only the flagged node counts)", c.Message)
	}
	for _, healthy := range []string{"healthy-a", "healthy-b"} {
		if strings.Contains(c.Message, healthy) {
			t.Errorf("message = %q, must not name the unflagged node %s", c.Message, healthy)
		}
	}
}

// TestClusterConfig_CPUManagerCondition_TruncatesExamples: the full count is reported
// with at most conflictExampleLimit examples, the lexicographically first, so the
// message neither grows with the fleet nor churns.
func TestClusterConfig_CPUManagerCondition_TruncatesExamples(t *testing.T) {
	// Seeded out of order; this pins the rendered order, not the sort itself, since List
	// already returns Nodes by name.
	c := cpuManagerConditionOf(t,
		cpuManagerFleetNode("node-e", true, "true"),
		cpuManagerFleetNode("node-c", true, "true"),
		cpuManagerFleetNode("node-a", true, "true"),
		cpuManagerFleetNode("node-d", true, "true"),
		cpuManagerFleetNode("node-b", true, "true"),
	)
	if c.Status != metav1.ConditionFalse || c.Reason != "CPUManagerStatic" {
		t.Fatalf("condition = %v/%q, want False/CPUManagerStatic", c.Status, c.Reason)
	}
	if !strings.Contains(c.Message, "5 provisioned node") {
		t.Errorf("message = %q, want the full count 5 even though the examples are truncated", c.Message)
	}

	if !strings.Contains(c.Message, "node-a, node-b, node-c") {
		t.Errorf("message = %q, want the sorted first %d examples \"node-a, node-b, node-c\"", c.Message, conflictExampleLimit)
	}
	for _, beyond := range []string{"node-d", "node-e"} {
		if strings.Contains(c.Message, beyond) {
			t.Errorf("message = %q, must list at most %d examples (found %s)", c.Message, conflictExampleLimit, beyond)
		}
	}
}

// TestClusterConfig_CPUManagerCondition_IgnoresNodesOutsidePool: a flagged node outside
// the pinning pool is not counted.
func TestClusterConfig_CPUManagerCondition_IgnoresNodesOutsidePool(t *testing.T) {
	c := cpuManagerConditionOf(t,
		cpuManagerFleetNode("in-pool", true, "false"),
		cpuManagerFleetNode("outside-pool", false, "true"),
	)
	if c.Status != metav1.ConditionTrue || c.Reason != "NoConflictDetected" {
		t.Errorf("condition = %v/%q, want True/NoConflictDetected — a flagged node outside the pinning pool is not a conflict (%q)",
			c.Status, c.Reason, c.Message)
	}
	if strings.Contains(c.Message, "outside-pool") {
		t.Errorf("message = %q, must not name a node outside the pinning pool", c.Message)
	}
}

// TestCompileClassification: in-code defaults (override label, cpu-amx threshold) are
// filled and selectors resolved once.
func TestCompileClassification(t *testing.T) {
	log := ctrl.Log.WithName("test")

	t.Run("empty spec uses in-code defaults", func(t *testing.T) {
		got := compileClassification(cpopv1alpha1.ClassificationSpec{}, log)
		if got.OverrideLabel != classification.DefaultOverrideLabel {
			t.Errorf("overrideLabel = %q, want default", got.OverrideLabel)
		}
		if got.CPUAMXMinLogicalCPUs != classification.DefaultCPUAMXMinLogicalCPUs {
			t.Errorf("cpuAMXMinLogicalCPUs = %d, want %d", got.CPUAMXMinLogicalCPUs, classification.DefaultCPUAMXMinLogicalCPUs)
		}
		if got.AMXSelector != nil || got.GPUOnlySelector != nil {
			t.Errorf("selectors should be nil when unset")
		}
	})

	t.Run("explicit values kept and selectors compiled", func(t *testing.T) {
		spec := cpopv1alpha1.ClassificationSpec{
			OverrideLabel:        "example.com/class",
			CPUAMXMinLogicalCPUs: 48,
			AMXNodeSelector:      &metav1.LabelSelector{MatchLabels: map[string]string{"cpu-inference": "true"}},
			GPUOnlyNodeSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"gpu-only": "true"}},
		}
		got := compileClassification(spec, log)
		if got.OverrideLabel != "example.com/class" || got.CPUAMXMinLogicalCPUs != 48 {
			t.Errorf("explicit values not preserved: %+v", got)
		}
		if got.AMXSelector == nil || !got.AMXSelector.Matches(labels.Set{"cpu-inference": "true"}) {
			t.Errorf("amxSelector not compiled/matching")
		}
		if got.GPUOnlySelector == nil || !got.GPUOnlySelector.Matches(labels.Set{"gpu-only": "true"}) {
			t.Errorf("gpuOnlySelector not compiled/matching")
		}
	})
}

// TestCompileClassProfiles: class specs reduce to cpuAllocation and reserved only.
func TestCompileClassProfiles(t *testing.T) {
	if compileClassProfiles(nil) != nil {
		t.Error("nil input should return nil")
	}
	profiles := map[string]cpopv1alpha1.BalloonSpec{
		"cpu-amx": {
			CPUAllocation: "hyperthreads",
			Reserved:      &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(16)},

			HTSiblingPolicy: "reserved",
			Packing:         "binpack",
		},
	}
	got := compileClassProfiles(profiles)
	p, ok := got["cpu-amx"]
	if !ok {
		t.Fatal("cpu-amx profile missing")
	}
	if p.CPUAllocation != "hyperthreads" {
		t.Errorf("cpuAllocation = %q, want hyperthreads", p.CPUAllocation)
	}
	if p.Reserved == nil || p.Reserved.Cpus == nil || *p.Reserved.Cpus != 16 {
		t.Errorf("reserved = %+v, want Cpus=16", p.Reserved)
	}
}

// TestExcludeSelectorCondition: an invalid excludeNodeSelector is dropped fail-open,
// so the reconciler sets ExcludeNodeSelectorValid=False.
func TestExcludeSelectorCondition(t *testing.T) {
	valid := &cpopv1alpha1.BalloonsClusterConfig{
		Spec: cpopv1alpha1.BalloonsClusterConfigSpec{
			ExcludeNodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
		},
	}
	if c := excludeSelectorCondition(valid); c.Status != metav1.ConditionTrue {
		t.Errorf("valid selector: Status = %v, want True (%q)", c.Status, c.Message)
	}

	if c := excludeSelectorCondition(&cpopv1alpha1.BalloonsClusterConfig{}); c.Status != metav1.ConditionTrue {
		t.Errorf("nil selector: Status = %v, want True", c.Status)
	}

	invalid := &cpopv1alpha1.BalloonsClusterConfig{
		Spec: cpopv1alpha1.BalloonsClusterConfigSpec{
			ExcludeNodeSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "a", Operator: "NotAnOperator",
			}}},
		},
	}
	c := excludeSelectorCondition(invalid)
	if c.Status != metav1.ConditionFalse || c.Reason != "InvalidSelector" {
		t.Errorf("invalid selector: got Status=%v Reason=%q, want False/InvalidSelector", c.Status, c.Reason)
	}
}
