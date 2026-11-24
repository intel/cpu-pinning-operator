// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nriv1alpha1 "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"
	nripolicy "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/balloons"
	"github.com/intel/cpu-pinning-operator/internal/classification"
	"github.com/intel/cpu-pinning-operator/internal/dashboard"
	"github.com/intel/cpu-pinning-operator/internal/match"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// twoNumaProbe is a probe transcript for a 2-socket, 2-NUMA HT node (maxSpan 2).
const twoNumaProbe = `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=1
CPU(s):                  64
Thread(s) per core:      2
Core(s) per socket:      16
Socket(s):               2
NUMA node(s):            2
NUMA node0 CPU(s):       0-15,32-47
NUMA node1 CPU(s):       16-31,48-63
AMX: false
`

// fourNumaProbe is the same node as 4 NUMA nodes (maxSpan 4), rendering a different policy.
const fourNumaProbe = `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=0
PKGNODE node=2 pkg=1
PKGNODE node=3 pkg=1
CPU(s):                  64
Thread(s) per core:      2
Core(s) per socket:      8
Socket(s):               2
NUMA node(s):            4
NUMA node0 CPU(s):       0-7,32-39
NUMA node1 CPU(s):       8-15,40-47
NUMA node2 CPU(s):       16-23,48-55
NUMA node3 CPU(s):       24-31,56-63
AMX: false
`

// threeSocketProbe has 3 packages of 2 NUMA nodes; no span-4 layout covers three
// packages, so the policy defines balloon-span{1,2} only.
const threeSocketProbe = `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=0
PKGNODE node=2 pkg=1
PKGNODE node=3 pkg=1
PKGNODE node=4 pkg=2
PKGNODE node=5 pkg=2
CPU(s):                  96
Thread(s) per core:      2
Core(s) per socket:      16
Socket(s):               3
NUMA node(s):            6
AMX: false
`

// eightSocketProbe has 8 single-NUMA packages: the policy defines balloon-span8 but
// no span-4 layout, leaving a hole at span 4.
const eightSocketProbe = `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=1
PKGNODE node=2 pkg=2
PKGNODE node=3 pkg=3
PKGNODE node=4 pkg=4
PKGNODE node=5 pkg=5
PKGNODE node=6 pkg=6
PKGNODE node=7 pkg=7
CPU(s):                  128
Thread(s) per core:      1
Core(s) per socket:      16
Socket(s):               8
NUMA node(s):            8
AMX: false
`

// twoNumaProbeWithCPUInfo is twoNumaProbe plus per-CPU CPUINFO lines: 64 CPUs, CPU i
// and i+32 are HT siblings; NUMA0 owns 0-15,32-47 and NUMA1 16-31,48-63.
var twoNumaProbeWithCPUInfo = buildTwoNumaProbeWithCPUInfo()

func buildTwoNumaProbeWithCPUInfo() string {
	var b strings.Builder
	b.WriteString(twoNumaProbe)
	numaOf := func(cpu int) int {
		low := cpu % 32 // 0-31 partition
		if low >= 16 {
			return 1
		}
		return 0
	}
	for cpu := 0; cpu < 64; cpu++ {
		core := cpu
		if cpu >= 32 {
			core = cpu - 32
		}
		fmt.Fprintf(&b, "CPUINFO cpu=%d numa=%d core=%d\n", cpu, numaOf(cpu), core)
	}
	return b.String()
}

// fakeLogsGetter returns a canned probe transcript or error instead of pod logs.
type fakeLogsGetter struct {
	output string
	err    error
}

func (f fakeLogsGetter) GetLogs(_ context.Context, _, _ string) (string, error) {
	return f.output, f.err
}

// succeedProbeOnCreate marks created probe pods Succeeded so discovery returns immediately.
func succeedProbeOnCreate() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if pod, ok := obj.(*corev1.Pod); ok && strings.HasPrefix(pod.Name, "cpu-topo-probe-") {
				pod.Status.Phase = corev1.PodSucceeded
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

const testProbeImage = "registry.example.com/cpu-pinning-operator@sha256:abc123"

func nodeReconciler(t *testing.T, logs topology.LogsGetter, objs ...runtime.Object) (*NodeReconciler, *record.FakeRecorder) {
	t.Helper()
	// Discovery refuses to probe without an image.
	t.Setenv("PROBE_IMAGE", testProbeImage)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	_ = nriv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithIndex(&corev1.Pod{}, podNodeNameField, func(obj client.Object) []string {
			pod, ok := obj.(*corev1.Pod)
			if !ok || pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}).
		WithInterceptorFuncs(succeedProbeOnCreate()).
		Build()
	rec := record.NewFakeRecorder(8)
	return &NodeReconciler{
		Client:      c,
		Scheme:      scheme,
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: match.NewIndex(),
		LogsGetter:  logs,
		Recorder:    rec,
	}, rec
}

func poolNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func reconcileNode(t *testing.T, r *NodeReconciler, name string) {
	t.Helper()
	req := ctrl.Request{}
	req.Name = name
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

// TestNode_StampsPackageTopologyLabel: twoNumaProbe (2 packages x 1 NUMA) stamps "1_1".
func TestNode_StampsPackageTopologyLabel(t *testing.T) {
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	reconcileNode(t, r, "n1")

	node := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got := node.Labels[LabelPackageTopology]; got != "1_1" {
		t.Errorf("package-topology label = %q, want %q (2 packages × 1 NUMA)", got, "1_1")
	}
}

// TestNode_MaxSpanLabel_MatchesEmittedSpanBalloons: 2+2+2 advertises 2, 2+2 keeps 4, and
// 8-socket stops at 2 despite balloon-span8.
func TestNode_MaxSpanLabel_MatchesEmittedSpanBalloons(t *testing.T) {
	cases := []struct {
		label    string
		probe    string
		wantSpan string
	}{
		{"2-socket 2+2 defines balloon-span4", fourNumaProbe, "4"},
		{"3-socket 2+2+2 defines no balloon-span4", threeSocketProbe, "2"},
		{"8-socket 1-NUMA-each defines balloon-span8 but no balloon-span4", eightSocketProbe, "2"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			n := poolNode("n1")
			r, rec := nodeReconciler(t, fakeLogsGetter{output: tc.probe}, n)
			reconcileNode(t, r, "n1")

			node := &corev1.Node{}
			if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, node); err != nil {
				t.Fatalf("get node: %v", err)
			}
			got := node.Labels[LabelMaxSpan]
			if got != tc.wantSpan {
				t.Errorf("max-span label = %q, want %q", got, tc.wantSpan)
			}
			// The Event reports the same span as the label.
			events := drainNodeEvents(rec)
			if len(events) != 1 || !strings.Contains(events[0], "maxSpan "+tc.wantSpan+",") {
				t.Errorf("events = %v, want one naming maxSpan %s", events, tc.wantSpan)
			}

			// The policy must define balloon-span<N> for the advertised span and every smaller
			// one; larger spans may exist unused.
			policy := &nriv1alpha1.BalloonsPolicy{}
			key := client.ObjectKey{Name: "node.n1", Namespace: "kube-system"}
			if err := r.Get(context.Background(), key, policy); err != nil {
				t.Fatalf("get policy: %v", err)
			}
			defined := map[string]bool{}
			for _, d := range policy.Spec.BalloonDefs {
				defined[d.Name] = true
			}
			advertised, err := strconv.Atoi(got)
			if err != nil {
				t.Fatalf("max-span label %q is not an int: %v", got, err)
			}
			for _, span := range []int{1, 2, 4, 8} {
				name := fmt.Sprintf("balloon-span%d", span)
				if span <= advertised && !defined[name] {
					t.Errorf("label advertises span %d but the policy defines no %s (defs: %v)",
						advertised, name, defNames(policy))
				}
			}
		})
	}
}

func defNames(policy *nriv1alpha1.BalloonsPolicy) []string {
	var names []string
	for _, d := range policy.Spec.BalloonDefs {
		names = append(names, d.Name)
	}
	return names
}

// TestNode_StampsTopologyCacheAnnotation: the leader publishes per-CPU placement and
// per-NUMA memory as a Node annotation for followers, decoding to the probed CPU map.
func TestNode_StampsTopologyCacheAnnotation(t *testing.T) {
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbeWithCPUInfo}, poolNode("n1"))
	reconcileNode(t, r, "n1")

	node := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	payload, ok := node.Annotations[AnnotationTopologyCache]
	if !ok || payload == "" {
		t.Fatalf("node n1 has no %s annotation, want the probed topology", AnnotationTopologyCache)
	}
	cpus, _, err := topology.DecodeNodeCache(payload)
	if err != nil {
		t.Fatalf("decode topology-cache annotation: %v", err)
	}
	// Every one of the 64 CPUs must be carried.
	if len(cpus) != 64 {
		t.Fatalf("decoded topology carries %d CPUs, want 64", len(cpus))
	}
	// CPU 0 and sibling 32 share core 0 on NUMA 0; CPU 16 is on NUMA 1.
	if got := cpus[0]; got.NUMA != 0 || got.Core != 0 {
		t.Errorf("cpu 0 = %+v, want NUMA 0 core 0", got)
	}
	if got := cpus[32]; got.NUMA != 0 || got.Core != 0 {
		t.Errorf("cpu 32 (sibling of 0) = %+v, want NUMA 0 core 0", got)
	}
	if got := cpus[16]; got.NUMA != 1 {
		t.Errorf("cpu 16 = %+v, want NUMA 1", got)
	}
}

// TestNode_Provisioning_EmitsProvisionedEvent: first apply emits a Normal Provisioned event.
func TestNode_Provisioning_EmitsProvisionedEvent(t *testing.T) {
	before := testutil.ToFloat64(metrics.NodeProvisioning.WithLabelValues("n1", metrics.NodeProvisioned))
	r, rec := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	reconcileNode(t, r, "n1")

	events := drainNodeEvents(rec)
	if len(events) != 1 {
		t.Fatalf("events = %v, want exactly one Provisioned event", events)
	}
	if !strings.Contains(events[0], "Normal") || !strings.Contains(events[0], reasonProvisioned) {
		t.Errorf("event = %q, want Normal %s", events[0], reasonProvisioned)
	}

	if after := testutil.ToFloat64(metrics.NodeProvisioning.WithLabelValues("n1", metrics.NodeProvisioned)); after != before+1 {
		t.Errorf("cpu_pinning_node_provisioning_total{node=\"n1\",outcome=\"provisioned\"} = %v, want %v", after, before+1)
	}
}

// TestNode_FreshUnlabelledNode_GetsProvisioned: a node with none of the operator's
// labels is probed and labelled balloons-enabled=true, which requires the reconciler
// to read the unscoped Node cache.
func TestNode_FreshUnlabelledNode_GetsProvisioned(t *testing.T) {
	fresh := poolNode("brand-new")
	if fresh.Labels[LabelBalloonsEnabled] != "" {
		t.Fatalf("precondition: fresh node must start without the balloons-enabled label")
	}
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, fresh)
	reconcileNode(t, r, "brand-new")

	node := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "brand-new"}, node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if node.Labels[LabelBalloonsEnabled] != "true" {
		t.Errorf("balloons-enabled label = %q, want %q: a fresh unlabelled node was not provisioned",
			node.Labels[LabelBalloonsEnabled], "true")
	}
}

// TestNode_BalloonsPolicy_DefaultsToKubeSystem: with BALLOONS_NAMESPACE unset the CR
// lands in kube-system.
func TestNode_BalloonsPolicy_DefaultsToKubeSystem(t *testing.T) {
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	reconcileNode(t, r, "n1")

	policy := &nriv1alpha1.BalloonsPolicy{}
	key := client.ObjectKey{Name: "node.n1", Namespace: balloons.DefaultBalloonsNamespace}
	if err := r.Get(context.Background(), key, policy); err != nil {
		t.Fatalf("BalloonsPolicy not created in %s: %v", balloons.DefaultBalloonsNamespace, err)
	}
	// And nowhere else.
	stray := &nriv1alpha1.BalloonsPolicy{}
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "node.n1", Namespace: "cpu-pinning-operator-system"}, stray); !apierrors.IsNotFound(err) {
		t.Errorf("unexpected BalloonsPolicy in release namespace: err = %v", err)
	}
}

// TestNode_BalloonsPolicy_UsesResolvedReleaseNamespace: a resolved balloons namespace
// (self-install) receives the CR, where the plugin reads it.
func TestNode_BalloonsPolicy_UsesResolvedReleaseNamespace(t *testing.T) {
	const releaseNS = "cpu-pinning-operator-system"
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	r.BalloonsNamespace = releaseNS
	reconcileNode(t, r, "n1")

	policy := &nriv1alpha1.BalloonsPolicy{}
	key := client.ObjectKey{Name: "node.n1", Namespace: releaseNS}
	if err := r.Get(context.Background(), key, policy); err != nil {
		t.Fatalf("BalloonsPolicy not created in resolved namespace %s: %v", releaseNS, err)
	}

	stray := &nriv1alpha1.BalloonsPolicy{}
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "node.n1", Namespace: balloons.DefaultBalloonsNamespace}, stray); !apierrors.IsNotFound(err) {
		t.Errorf("unexpected BalloonsPolicy in kube-system: err = %v", err)
	}
}

// TestNode_Excluded_PrunesFromResolvedNamespace: prune deletes from the namespace it writes to.
func TestNode_Excluded_PrunesFromResolvedNamespace(t *testing.T) {
	const releaseNS = "cpu-pinning-operator-system"
	node := poolNode("n1")
	node.Labels = map[string]string{LabelDisabled: "true", LabelBalloonsEnabled: "true"}
	existing := &nriv1alpha1.BalloonsPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "node.n1", Namespace: releaseNS},
	}
	r, rec := nodeReconciler(t, fakeLogsGetter{}, node, existing)
	r.BalloonsNamespace = releaseNS
	reconcileNode(t, r, "n1")

	events := drainNodeEvents(rec)
	if len(events) != 1 || !strings.Contains(events[0], reasonDeprovisioned) {
		t.Fatalf("events = %v, want exactly one Deprovisioned event", events)
	}
	gone := &nriv1alpha1.BalloonsPolicy{}
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "node.n1", Namespace: releaseNS}, gone); !apierrors.IsNotFound(err) {
		t.Errorf("BalloonsPolicy not pruned from %s: err = %v", releaseNS, err)
	}
}

// dashboardNamespace is where these tests write dashboard ConfigMaps.
const dashboardNamespace = "monitoring"

// dashboardWriter returns an enabled dashboard.Writer on the reconciler's client
// writing into dashboardNamespace.
func dashboardWriter(r *NodeReconciler) *dashboard.Writer {
	return &dashboard.Writer{
		Client: r.Client,
		Config: dashboard.Config{
			Enabled:      true,
			Namespace:    dashboardNamespace,
			InstanceName: "cpop",
		},
	}
}

// dashboardConfigMap reports whether n1's dashboard ConfigMap exists.
func dashboardConfigMap(t *testing.T, r *NodeReconciler) bool {
	t.Helper()
	cm := &corev1.ConfigMap{}
	err := r.Get(context.Background(),
		client.ObjectKey{Name: "cpop-canvas-n1", Namespace: "monitoring"}, cm)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("get dashboard ConfigMap for n1: %v", err)
	}
	return true
}

// TestNode_Provisioned_WritesDashboard: a provisioned node gets its dashboard ConfigMap.
func TestNode_Provisioned_WritesDashboard(t *testing.T) {
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbeWithCPUInfo}, poolNode("n1"))
	r.Dashboards = dashboardWriter(r)
	reconcileNode(t, r, "n1")

	if !dashboardConfigMap(t, r) {
		t.Fatalf("provisioned node n1 has no dashboard ConfigMap, want one")
	}
}

// TestNode_NotBalloonsEnabled_NoDashboard: an excluded node gets no dashboard ConfigMap.
func TestNode_NotBalloonsEnabled_NoDashboard(t *testing.T) {
	node := poolNode("n1")
	node.Labels = map[string]string{LabelDisabled: "true"}
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbeWithCPUInfo}, node)
	r.Dashboards = dashboardWriter(r)
	reconcileNode(t, r, "n1")

	if dashboardConfigMap(t, r) {
		t.Fatalf("excluded (non-balloons-enabled) node n1 got a dashboard ConfigMap, want none")
	}
}

// TestNode_LosesBalloonsEnabled_PrunesDashboard: excluding a provisioned node prunes its dashboard.
func TestNode_LosesBalloonsEnabled_PrunesDashboard(t *testing.T) {
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbeWithCPUInfo}, poolNode("n1"))
	r.Dashboards = dashboardWriter(r)
	reconcileNode(t, r, "n1")
	if !dashboardConfigMap(t, r) {
		t.Fatalf("provisioned node n1 has no dashboard ConfigMap after first reconcile")
	}

	node := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, node); err != nil {
		t.Fatalf("get node n1: %v", err)
	}
	node.Labels[LabelDisabled] = "true"
	if err := r.Update(context.Background(), node); err != nil {
		t.Fatalf("update node n1: %v", err)
	}
	reconcileNode(t, r, "n1")

	if dashboardConfigMap(t, r) {
		t.Fatalf("dashboard ConfigMap for excluded node n1 was not pruned")
	}
}

// TestNode_SteadyStateResync_NoEvent: re-rendering an identical policy is silent.
func TestNode_SteadyStateResync_NoEvent(t *testing.T) {
	r, rec := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	reconcileNode(t, r, "n1")
	_ = drainNodeEvents(rec) // discard the first-provision event

	reconcileNode(t, r, "n1")
	if events := drainNodeEvents(rec); len(events) != 0 {
		t.Errorf("steady-state resync emitted events %v, want none", events)
	}
}

// TestNode_ServerDefaultedSpecField_NoSpuriousUpdate: CRD-defaulted fields (turboDomain,
// controllableInterrupts) are not a change, or every resync would report an update.
// The fake client does not default, so the field is seeded under a foreign manager.
func TestNode_ServerDefaultedSpecField_NoSpuriousUpdate(t *testing.T) {
	r, rec := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	reconcileNode(t, r, "n1")
	_ = drainNodeEvents(rec) // discard the first-provision event

	defaulted := &unstructured.Unstructured{}
	defaulted.SetGroupVersionKind(schema.GroupVersionKind{Group: "config.nri", Version: "v1alpha1", Kind: "BalloonsPolicy"})
	defaulted.SetName("node.n1")
	defaulted.SetNamespace("kube-system")
	if err := unstructured.SetNestedField(defaulted.Object, "package", "spec", "turboDomain"); err != nil {
		t.Fatalf("seed turboDomain: %v", err)
	}
	if err := r.Patch(context.Background(), defaulted, client.Apply,
		client.FieldOwner("crd-defaulting")); err != nil {
		t.Fatalf("seed defaulted field: %v", err)
	}

	before := testutil.ToFloat64(metrics.NodeProvisioning.WithLabelValues("n1", metrics.NodeUpdated))

	// Identical topology; only the defaulted field differs.
	reconcileNode(t, r, "n1")

	if events := drainNodeEvents(rec); len(events) != 0 {
		t.Errorf("resync of an unchanged node emitted events %v, want none", events)
	}
	if after := testutil.ToFloat64(metrics.NodeProvisioning.WithLabelValues("n1", metrics.NodeUpdated)); after != before {
		t.Errorf("cpu_pinning_node_provisioning_total{node=\"n1\",outcome=\"updated\"} = %v after a resync that wrote nothing, want %v",
			after, before)
	}

	// The defaulted field must survive, not be clobbered to pass.
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{Group: "config.nri", Version: "v1alpha1", Kind: "BalloonsPolicy"})
	if err := r.Get(context.Background(), client.ObjectKey{Name: "node.n1", Namespace: "kube-system"}, got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	domain, found, err := unstructured.NestedString(got.Object, "spec", "turboDomain")
	if err != nil || !found || domain != "package" {
		t.Errorf("spec.turboDomain = %q (found=%v, err=%v), want it preserved", domain, found, err)
	}
}

// TestNode_PolicyChange_EmitsUpdatedEvent: a changed policy emits BalloonsPolicyUpdated
// (not Provisioned) and is counted.
func TestNode_PolicyChange_EmitsUpdatedEvent(t *testing.T) {
	logs := &fakeLogsGetter{output: twoNumaProbe}
	r, rec := nodeReconciler(t, logs, poolNode("n1"))
	reconcileNode(t, r, "n1")
	_ = drainNodeEvents(rec) // discard the first-provision event

	before := testutil.ToFloat64(metrics.NodeProvisioning.WithLabelValues("n1", metrics.NodeUpdated))

	logs.output = fourNumaProbe // topology changed → policy re-renders
	reconcileNode(t, r, "n1")

	if after := testutil.ToFloat64(metrics.NodeProvisioning.WithLabelValues("n1", metrics.NodeUpdated)); after != before+1 {
		t.Errorf("cpu_pinning_node_provisioning_total{node=\"n1\",outcome=\"updated\"} = %v after a real configuration change, want %v",
			after, before+1)
	}

	events := drainNodeEvents(rec)
	if len(events) != 1 {
		t.Fatalf("events = %v, want exactly one policy-change event", events)
	}
	if !strings.Contains(events[0], "Normal") || !strings.Contains(events[0], reasonBalloonsPolicyUpdated) {
		t.Errorf("event = %q, want Normal %s", events[0], reasonBalloonsPolicyUpdated)
	}
	// 2+2 defines balloon-span4, so the event reports span 4.
	if !strings.Contains(events[0], "maxSpan 4,") {
		t.Errorf("event = %q, want it to name maxSpan 4", events[0])
	}
}

// TestNode_PolicyUpdate_PreservesForeignLabels: SSA touches only operator-owned label
// keys, so foreign labels survive an update.
func TestNode_PolicyUpdate_PreservesForeignLabels(t *testing.T) {
	logs := &fakeLogsGetter{output: twoNumaProbe}
	r, _ := nodeReconciler(t, logs, poolNode("n1"))
	reconcileNode(t, r, "n1") // first provision creates the CR

	policy := &nriv1alpha1.BalloonsPolicy{}
	key := client.ObjectKey{Name: "node.n1", Namespace: "kube-system"}
	if err := r.Get(context.Background(), key, policy); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if policy.Labels == nil {
		policy.Labels = map[string]string{}
	}
	policy.Labels["example.com/owner"] = "team-a"
	if err := r.Update(context.Background(), policy); err != nil {
		t.Fatalf("seed foreign label: %v", err)
	}

	logs.output = fourNumaProbe // topology changed → policy re-renders and updates
	reconcileNode(t, r, "n1")

	updated := &nriv1alpha1.BalloonsPolicy{}
	if err := r.Get(context.Background(), key, updated); err != nil {
		t.Fatalf("get updated policy: %v", err)
	}
	if updated.Labels["example.com/owner"] != "team-a" {
		t.Errorf("foreign label lost after update: labels = %v", updated.Labels)
	}

	if updated.Labels[cpopv1alpha1.LabelPrefix+"node"] != "n1" {
		t.Errorf("managed label missing after update: labels = %v", updated.Labels)
	}
}

// TestNode_SSA_ForeignFieldManagerLabelPersists: a label applied by a foreign field
// manager persists across the operator's apply.
func TestNode_SSA_ForeignFieldManagerLabelPersists(t *testing.T) {
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	reconcileNode(t, r, "n1") // operator creates the CR via SSA

	key := client.ObjectKey{Name: "node.n1", Namespace: "kube-system"}

	// Applied under its own field manager, not a plain Update.
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(schema.GroupVersionKind{Group: "config.nri", Version: "v1alpha1", Kind: "BalloonsPolicy"})
	foreign.SetName("node.n1")
	foreign.SetNamespace("kube-system")
	foreign.SetLabels(map[string]string{"example.com/owner": "team-a"})
	if err := r.Patch(context.Background(), foreign, client.Apply,
		client.FieldOwner("foreign-controller")); err != nil {
		t.Fatalf("foreign SSA apply: %v", err)
	}

	// Steady state: a no-op apply.
	reconcileNode(t, r, "n1")

	got := &nriv1alpha1.BalloonsPolicy{}
	if err := r.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if got.Labels["example.com/owner"] != "team-a" {
		t.Errorf("foreign-manager label lost after operator SSA reconcile: labels = %v", got.Labels)
	}

	if got.Labels[cpopv1alpha1.LabelPrefix+"node"] != "n1" {
		t.Errorf("operator managed label missing: labels = %v", got.Labels)
	}
}

// TestNode_ForeignLabelOnly_NoSpuriousUpdate: a foreign label alone is not drift.
func TestNode_ForeignLabelOnly_NoSpuriousUpdate(t *testing.T) {
	r, rec := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	reconcileNode(t, r, "n1")
	_ = drainNodeEvents(rec) // discard the first-provision event

	policy := &nriv1alpha1.BalloonsPolicy{}
	key := client.ObjectKey{Name: "node.n1", Namespace: "kube-system"}
	if err := r.Get(context.Background(), key, policy); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if policy.Labels == nil {
		policy.Labels = map[string]string{}
	}
	policy.Labels["example.com/owner"] = "team-a"
	if err := r.Update(context.Background(), policy); err != nil {
		t.Fatalf("seed foreign label: %v", err)
	}

	reconcileNode(t, r, "n1") // identical topology → only the foreign label differs
	if events := drainNodeEvents(rec); len(events) != 0 {
		t.Errorf("foreign label triggered spurious update events %v, want none", events)
	}
}

// TestNode_ForeignPrunedSpecField_NoSpuriousUpdate: a foreign-set spec block the
// operator prunes from its apply (control/log/instrumentation) is not drift.
func TestNode_ForeignPrunedSpecField_NoSpuriousUpdate(t *testing.T) {
	r, rec := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	reconcileNode(t, r, "n1")
	_ = drainNodeEvents(rec) // discard the first-provision event

	key := client.ObjectKey{Name: "node.n1", Namespace: "kube-system"}

	// Instrumentation set under a foreign field manager.
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(schema.GroupVersionKind{Group: "config.nri", Version: "v1alpha1", Kind: "BalloonsPolicy"})
	foreign.SetName("node.n1")
	foreign.SetNamespace("kube-system")
	if err := unstructured.SetNestedField(foreign.Object, "localhost:8080", "spec", "instrumentation", "httpEndpoint"); err != nil {
		t.Fatalf("set foreign instrumentation: %v", err)
	}
	if err := r.Patch(context.Background(), foreign, client.Apply,
		client.FieldOwner("foreign-controller")); err != nil {
		t.Fatalf("foreign SSA apply: %v", err)
	}

	// Only the foreign instrumentation block differs; it must not count as drift.
	reconcileNode(t, r, "n1")
	if events := drainNodeEvents(rec); len(events) != 0 {
		t.Errorf("foreign pruned-spec field triggered spurious update events %v, want none", events)
	}

	// The foreign field must survive, not be clobbered to pass.
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{Group: "config.nri", Version: "v1alpha1", Kind: "BalloonsPolicy"})
	if err := r.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	endpoint, found, err := unstructured.NestedString(got.Object, "spec", "instrumentation", "httpEndpoint")
	if err != nil || !found || endpoint != "localhost:8080" {
		t.Errorf("foreign instrumentation endpoint = %q (found=%v, err=%v), want it preserved", endpoint, found, err)
	}
}

// TestNode_Excluded_EmitsDeprovisionedEvent: excluding a provisioned node removes the
// CR and emits Deprovisioned.
func TestNode_Excluded_EmitsDeprovisionedEvent(t *testing.T) {
	node := poolNode("n1")
	node.Labels = map[string]string{LabelDisabled: "true", LabelBalloonsEnabled: "true"}
	existing := &nriv1alpha1.BalloonsPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "node.n1", Namespace: "kube-system"},
	}
	r, rec := nodeReconciler(t, fakeLogsGetter{}, node, existing)
	reconcileNode(t, r, "n1")

	events := drainNodeEvents(rec)
	if len(events) != 1 {
		t.Fatalf("events = %v, want exactly one Deprovisioned event", events)
	}
	if !strings.Contains(events[0], "Normal") || !strings.Contains(events[0], reasonDeprovisioned) {
		t.Errorf("event = %q, want Normal %s", events[0], reasonDeprovisioned)
	}
}

// TestNode_ExcludedWithoutPolicy_NoEvent: excluding a never-provisioned node is silent.
func TestNode_ExcludedWithoutPolicy_NoEvent(t *testing.T) {
	node := poolNode("n1")
	node.Labels = map[string]string{LabelDisabled: "true"}
	r, rec := nodeReconciler(t, fakeLogsGetter{}, node)
	reconcileNode(t, r, "n1")

	if events := drainNodeEvents(rec); len(events) != 0 {
		t.Errorf("excluding an unprovisioned node emitted events %v, want none", events)
	}
}

// TestPruneNode_ConcurrentDelete_NotFoundStillReportsRemoval: a NotFound on Delete
// after a successful Get counts as removal.
func TestPruneNode_ConcurrentDelete_NotFoundStillReportsRemoval(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	_ = nriv1alpha1.AddToScheme(scheme)
	existing := &nriv1alpha1.BalloonsPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "node.n1", Namespace: "kube-system"},
	}

	deleteNotFound := interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			if _, ok := obj.(*nriv1alpha1.BalloonsPolicy); ok {
				return apierrors.NewNotFound(schema.GroupResource{Group: "config.nri", Resource: "balloonspolicies"}, obj.GetName())
			}
			return nil
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(existing).
		WithInterceptorFuncs(deleteNotFound).
		Build()
	r := &NodeReconciler{
		Client:      c,
		Scheme:      scheme,
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: match.NewIndex(),
	}

	removed, err := r.pruneNode(context.Background(), "n1")
	if err != nil {
		t.Fatalf("pruneNode returned error on concurrent NotFound delete: %v", err)
	}
	if !removed {
		t.Errorf("pruneNode removed = false, want true (policy is absent, deprovision event must still fire)")
	}
}

// TestPruneNode_DropsPerNodeGauges: a departed node's gauges are deleted, since the
// reservation gauge only republishes on change.
func TestPruneNode_DropsPerNodeGauges(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	_ = nriv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &NodeReconciler{Client: c, Scheme: scheme, Log: ctrl.Log.WithName("test"), PolicyIndex: match.NewIndex()}

	// Process-wide vecs, and the assertion counts series.
	metrics.ReservationsLive.Reset()
	metrics.NodeReservedCPUs.Reset()
	t.Cleanup(func() {
		metrics.ReservationsLive.Reset()
		metrics.NodeReservedCPUs.Reset()
	})
	metrics.ReservationsLive.WithLabelValues("n1").Set(3)
	metrics.NodeReservedCPUs.WithLabelValues("n1").Set(4)

	if _, err := r.pruneNode(context.Background(), "n1"); err != nil {
		t.Fatalf("pruneNode: %v", err)
	}
	if n := testutil.CollectAndCount(metrics.ReservationsLive); n != 0 {
		t.Errorf("cpu_pinning_webhook_reservations_live still has %d series after pruning the node, want 0", n)
	}
	if n := testutil.CollectAndCount(metrics.NodeReservedCPUs); n != 0 {
		t.Errorf("cpu_pinning_node_reserved_cpus still has %d series after pruning the node, want 0", n)
	}
}

// fourNumaHTProbe is 4 NUMA, 128 cores x 2 threads: core i on NUMA i/32, sibling
// i+128. Default reserved resolves to 6 cores, spread per NUMA to 8.
func fourNumaHTProbe() string {
	var b strings.Builder
	b.WriteString(`PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=0
PKGNODE node=2 pkg=1
PKGNODE node=3 pkg=1
CPU(s):                  256
Thread(s) per core:      2
Core(s) per socket:      64
Socket(s):               2
NUMA node(s):            4
AMX: false
`)
	for cpu := 0; cpu < 256; cpu++ {
		core := cpu % 128
		fmt.Fprintf(&b, "CPUINFO cpu=%d numa=%d core=%d\n", cpu, core/32, core)
	}
	return b.String()
}

// TestNode_ReservedCPUsMetric_ReportsTheAppliedPool: the gauge and utilization use the
// applied pool (8), not the pre-spread size (6); a 2-CPU pod tells 2/8 from 2/6.
func TestNode_ReservedCPUsMetric_ReportsTheAppliedPool(t *testing.T) {
	demand := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "unpinned", Namespace: "default"},
		Spec: corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("2"),
			}},
		}}},
	}
	r, _ := nodeReconciler(t, fakeLogsGetter{output: fourNumaHTProbe()}, poolNode("n1"), demand)
	reconcileNode(t, r, "n1")

	node := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, node); err != nil {
		t.Fatalf("get node n1: %v", err)
	}
	if got := node.Labels[LabelReservedCPUs]; got != "6" {
		t.Fatalf("%s = %q, want the resolved 6 (12 logical ÷ 2 threads); the fixture no longer exercises the spread",
			LabelReservedCPUs, got)
	}
	applied := reservedCPUCount(t, getBalloonsPolicy(t, r, "n1"))
	if applied != 8 {
		t.Fatalf("rendered reserved pool = %d CPUs, want 8 (6 spread over 4 NUMA nodes)", applied)
	}
	if got := testutil.ToFloat64(metrics.NodeReservedCPUs.WithLabelValues("n1")); got != float64(applied) {
		t.Errorf("cpu_pinning_node_reserved_cpus = %v, want the applied %d", got, applied)
	}
	if got, want := testutil.ToFloat64(metrics.ReservedPoolUtilizationRatio.WithLabelValues("n1")), 2.0/float64(applied); got != want {
		t.Errorf("cpu_pinning_reserved_pool_utilization_ratio = %v, want %v (2 CPUs of demand over the applied %d)", got, want, applied)
	}
}

// TestNode_ProbeFailure_EmitsProvisioningFailedWarning: a discovery failure emits a
// ProvisioningFailed Warning and requeues.
func TestNode_ProbeFailure_EmitsProvisioningFailedWarning(t *testing.T) {
	before := testutil.ToFloat64(metrics.NodeProvisioning.WithLabelValues("n1", metrics.NodeProvisionFailed))
	r, rec := nodeReconciler(t, fakeLogsGetter{err: context.DeadlineExceeded}, poolNode("n1"))
	// Reconcile swallows the discovery error, so call it directly.
	req := ctrl.Request{}
	req.Name = "n1"
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	events := drainNodeEvents(rec)
	if len(events) != 1 {
		t.Fatalf("events = %v, want exactly one ProvisioningFailed event", events)
	}
	if !strings.Contains(events[0], "Warning") || !strings.Contains(events[0], reasonProvisioningFailed) {
		t.Errorf("event = %q, want Warning %s", events[0], reasonProvisioningFailed)
	}
	if after := testutil.ToFloat64(metrics.NodeProvisioning.WithLabelValues("n1", metrics.NodeProvisionFailed)); after != before+1 {
		t.Errorf("cpu_pinning_node_provisioning_total{node=\"n1\",outcome=\"failed\"} = %v, want %v", after, before+1)
	}
}

// TestNode_NoProbeImage_LeavesNodeUnknown: without a probe image the node gets no
// policy, labels or cached topology, plus a Warning naming the variable, so it reads
// as unknown rather than provisioned or zero-capacity.
func TestNode_NoProbeImage_LeavesNodeUnknown(t *testing.T) {
	r, rec := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	r.TopoCache = topology.NewCache()
	// Image cleared; the transcript would otherwise provision the node.
	t.Setenv("PROBE_IMAGE", "")

	req := ctrl.Request{}
	req.Name = "n1"
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile returned error %v, want nil (a discovery failure requeues)", err)
	}
	if res.RequeueAfter != time.Minute {
		t.Errorf("RequeueAfter = %v, want %v (the ordinary discovery-failure retry)", res.RequeueAfter, time.Minute)
	}

	policy := &nriv1alpha1.BalloonsPolicy{}
	key := client.ObjectKey{Name: "node.n1", Namespace: balloons.DefaultBalloonsNamespace}
	if err := r.Get(context.Background(), key, policy); !apierrors.IsNotFound(err) {
		t.Errorf("BalloonsPolicy for an unprobed node: err = %v, want NotFound", err)
	}

	node := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	for _, l := range ManagedNodeLabels {
		if v, ok := node.Labels[l]; ok {
			t.Errorf("unprobed node carries %s = %q, want the label absent", l, v)
		}
	}
	if v, ok := node.Annotations[AnnotationTopologyCache]; ok {
		t.Errorf("unprobed node carries a topology-cache annotation %q, want none", v)
	}
	if _, ok := r.TopoCache.Get("n1"); ok {
		t.Error("an unprobed node published a topology to the cache, want no entry")
	}

	events := drainNodeEvents(rec)
	if len(events) != 1 || !strings.Contains(events[0], "Warning") ||
		!strings.Contains(events[0], reasonProvisioningFailed) {
		t.Fatalf("events = %v, want one Warning %s", events, reasonProvisioningFailed)
	}
	// The event must say what to set.
	if !strings.Contains(events[0], "PROBE_IMAGE") {
		t.Errorf("event %q does not name the missing variable", events[0])
	}
}

// TestNode_PerNodeOverride_RendersOverrideConfig: on identical topology, the node
// matching a NodeConfigOverride renders the override, the other the cluster default.
func TestNode_PerNodeOverride_RendersOverrideConfig(t *testing.T) {
	// n1 matches the override; n2 does not.
	n1 := poolNode("n1")
	n1.Labels = map[string]string{"node-tier": "perf"}
	n2 := poolNode("n2")
	n2.Labels = map[string]string{"node-tier": "standard"}

	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbeWithCPUInfo}, n1, n2)

	// Default: physical-cores, reserved 4 here. Override (node-tier=perf): hyperthreads, reserved 6.
	sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{"node-tier": "perf"},
	})
	if err != nil {
		t.Fatalf("compile selector: %v", err)
	}
	sixCPUs := 6
	r.PolicyIndex.SetClusterConfig(match.ClusterConfig{
		CPUAllocation: "physical-cores",
		NodeOverrides: []match.NodeConfigOverride{{
			Selector:      sel,
			CPUAllocation: "hyperthreads",
			Reserved:      &cpopv1alpha1.ReservedSizingSpec{Cpus: &sixCPUs},
		}},
	})

	reconcileNode(t, r, "n1")
	reconcileNode(t, r, "n2")

	// n1: hyperthreads, reserved 6, no exclude-cpuset.
	n1Got := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, n1Got); err != nil {
		t.Fatalf("get n1: %v", err)
	}
	if got := n1Got.Labels[LabelCPUAllocation]; got != "hyperthreads" {
		t.Errorf("n1 %s = %q, want hyperthreads (override)", LabelCPUAllocation, got)
	}
	if got := n1Got.Labels[LabelReservedCPUs]; got != "6" {
		t.Errorf("n1 %s = %q, want 6 (override)", LabelReservedCPUs, got)
	}
	n1Policy := getBalloonsPolicy(t, r, "n1")
	if len(n1Policy.Spec.Config.AvailableResources) != 0 {
		t.Errorf("n1 policy AvailableResources = %+v, want empty (hyperthreads mode has no exclude-cpuset)",
			n1Policy.Spec.Config.AvailableResources)
	}

	if got := reservedCPUCount(t, n1Policy); got != 6 {
		t.Errorf("n1 policy reserved CPU count = %d, want 6 (override reserved)", got)
	}

	// n2: physical-cores, reserved 4, exclude-cpuset with the HT siblings.
	n2Got := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "n2"}, n2Got); err != nil {
		t.Fatalf("get n2: %v", err)
	}
	if got := n2Got.Labels[LabelCPUAllocation]; got != "physical-cores" {
		t.Errorf("n2 %s = %q, want physical-cores (cluster default)", LabelCPUAllocation, got)
	}
	if got := n2Got.Labels[LabelReservedCPUs]; got != "4" {
		t.Errorf("n2 %s = %q, want 4 (cluster default reserved on this topology)", LabelReservedCPUs, got)
	}
	n2Policy := getBalloonsPolicy(t, r, "n2")
	if len(n2Policy.Spec.Config.AvailableResources) == 0 {
		t.Errorf("n2 policy AvailableResources is empty, want an exclude-cpuset (physical-cores mode)")
	}
	if got := reservedCPUCount(t, n2Policy); got != 4 {
		t.Errorf("n2 policy reserved CPU count = %d, want 4 (cluster default reserved)", got)
	}
}

// TestNode_EmitsNodeClassLabel_CPUOnly: a node without GPU or AMX is labelled cpu-only.
func TestNode_EmitsNodeClassLabel_CPUOnly(t *testing.T) {
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, poolNode("n1"))
	reconcileNode(t, r, "n1")

	node := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got := node.Labels[LabelNodeClass]; got != classification.ClassCPUOnly {
		t.Errorf("node-class label = %q, want %q", got, classification.ClassCPUOnly)
	}
}

// TestNode_ClassProfile_UsedAsResolutionBase: an AMX node classifies to cpu-amx and
// its classProfile's cpuAllocation is the base; reserved size is clamped, so it is
// not asserted.
func TestNode_ClassProfile_UsedAsResolutionBase(t *testing.T) {
	node := poolNode("n1")
	// AMX via the NFD CPUID label (the probe reports AMX: false).
	node.Labels = map[string]string{"feature.node.kubernetes.io/cpu-cpuid.AMX_TILE": "true"}
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbeWithCPUInfo}, node)

	// Profile sets hyperthreads against the physical-cores default.
	r.PolicyIndex.SetClusterConfig(match.ClusterConfig{
		CPUAllocation: "physical-cores",
		ClassProfiles: map[string]match.ClassProfile{
			classification.ClassCPUAMX: {CPUAllocation: "hyperthreads"},
		},
	})

	reconcileNode(t, r, "n1")

	got := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if c := got.Labels[LabelNodeClass]; c != classification.ClassCPUAMX {
		t.Fatalf("node-class = %q, want cpu-amx", c)
	}

	if a := got.Labels[LabelCPUAllocation]; a != "hyperthreads" {
		t.Errorf("cpu-allocation = %q, want hyperthreads (from cpu-amx class profile)", a)
	}
}

// TestNode_ClassOverrideRejected_EmitsWarning: an AMX class override on a non-AMX node
// is ignored with a Warning.
func TestNode_ClassOverrideRejected_EmitsWarning(t *testing.T) {
	node := poolNode("n1")
	node.Labels = map[string]string{classification.DefaultOverrideLabel: classification.ClassCPUAMX}
	r, rec := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbe}, node)

	r.PolicyIndex.SetClusterConfig(match.ClusterConfig{
		CPUAllocation:  "physical-cores",
		Classification: classification.Spec{OverrideLabel: classification.DefaultOverrideLabel},
	})

	reconcileNode(t, r, "n1")

	got := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if c := got.Labels[LabelNodeClass]; c != classification.ClassCPUOnly {
		t.Errorf("node-class = %q, want cpu-only (rejected override falls back to auto)", c)
	}
	events := drainNodeEvents(rec)
	var found bool
	for _, e := range events {
		if strings.Contains(e, "Warning") && strings.Contains(e, reasonClassificationOverrideRejected) {
			found = true
		}
	}
	if !found {
		t.Errorf("events = %v, want a Warning %s event", events, reasonClassificationOverrideRejected)
	}
}

// reservedCPUCount returns the policy's reserved CPU count from ReservedResources.cpu,
// either a bare count or a "cpuset:<list>".
func reservedCPUCount(t *testing.T, policy *nriv1alpha1.BalloonsPolicy) int {
	t.Helper()
	amount := string(policy.Spec.Config.ReservedResources[nripolicy.CPU])
	if amount == "" {
		return 0
	}
	if strings.HasPrefix(amount, "cpuset:") {
		list := strings.TrimPrefix(amount, "cpuset:")
		if list == "" {
			return 0
		}
		count := 0
		for _, part := range strings.Split(list, ",") {
			if lo, hi, ok := strings.Cut(part, "-"); ok {
				a, err1 := strconv.Atoi(lo)
				b, err2 := strconv.Atoi(hi)
				if err1 != nil || err2 != nil {
					t.Fatalf("bad cpuset range %q in %q", part, amount)
				}
				count += b - a + 1
			} else {
				count++
			}
		}
		return count
	}
	n, err := strconv.Atoi(amount)
	if err != nil {
		t.Fatalf("bad reserved amount %q: %v", amount, err)
	}
	return n
}

// getBalloonsPolicy fetches the "node.<node>" BalloonsPolicy from the balloons namespace.
func getBalloonsPolicy(t *testing.T, r *NodeReconciler, nodeName string) *nriv1alpha1.BalloonsPolicy {
	t.Helper()
	policy := &nriv1alpha1.BalloonsPolicy{}
	key := client.ObjectKey{Name: "node." + nodeName, Namespace: r.balloonsNamespace()}
	if err := r.Get(context.Background(), key, policy); err != nil {
		t.Fatalf("get BalloonsPolicy for %s: %v", nodeName, err)
	}
	return policy
}

func drainNodeEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestNode_Dashboard_BakesTheNodesOwnFacts: the dashboard bakes the node's stamped
// labels (class, package topology, mode), so swapped fields would describe the wrong machine.
func TestNode_Dashboard_BakesTheNodesOwnFacts(t *testing.T) {
	r, _ := nodeReconciler(t, fakeLogsGetter{output: twoNumaProbeWithCPUInfo}, poolNode("n1"))
	r.Dashboards = dashboardWriter(r)
	reconcileNode(t, r, "n1")

	node := &corev1.Node{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, node); err != nil {
		t.Fatalf("get node n1: %v", err)
	}
	cm := &corev1.ConfigMap{}
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "cpop-canvas-n1", Namespace: dashboardNamespace}, cm); err != nil {
		t.Fatalf("get dashboard ConfigMap for n1: %v", err)
	}
	page := cm.Data["cpu-pinning-canvas.json"]
	if page == "" {
		t.Fatal("dashboard ConfigMap carries no page")
	}

	for label, want := range map[string]string{
		LabelNodeClass:       node.Labels[LabelNodeClass],
		LabelPackageTopology: node.Labels[LabelPackageTopology],
		LabelCPUAllocation:   node.Labels[LabelCPUAllocation],
	} {
		if want == "" {
			t.Errorf("node n1 carries no %s, so the page's copy of it cannot be checked", label)
			continue
		}
		if !strings.Contains(page, want) {
			t.Errorf("the page does not state %s=%q that the reconciler stamped", label, want)
		}
	}

	if numa := node.Labels[LabelNumaNodes]; numa != "" &&
		!strings.Contains(page, "**NUMA nodes** | "+numa) {
		t.Errorf("the page's NUMA-node count disagrees with the label %q", numa)
	}
	// Reserved pool size is baked from the same value as the label.
	if reserved := node.Labels[LabelReservedCPUs]; reserved == "" {
		t.Errorf("node n1 carries no %s, so the pool listings have no denominator", LabelReservedCPUs)
	} else if !strings.Contains(page, "("+reserved+" of them)") {
		t.Errorf("the pool listing does not name the reserved pool of %q CPUs the reconciler "+
			"stamped", reserved)
	}
}

// TestNode_PublishesThePinnablePool: the reconciler publishes the pinnable pool for
// the NRT metrics; unpublished, metrics count the hardware set. With
// htSiblingPolicy=reserved the siblings are the reserved pool, so the pool is the
// whole machine. htProbe: 8 CPUs, 0-3 representatives, 4-7 siblings.
const htProbe = `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=1
CPU(s):                  8
Thread(s) per core:      2
Core(s) per socket:      2
Socket(s):               2
NUMA node(s):            2
NUMA node0 CPU(s):       0-1,4-5
NUMA node1 CPU(s):       2-3,6-7
CPUINFO cpu=0 numa=0 core=0
CPUINFO cpu=4 numa=0 core=0
CPUINFO cpu=1 numa=0 core=1
CPUINFO cpu=5 numa=0 core=1
CPUINFO cpu=2 numa=1 core=2
CPUINFO cpu=6 numa=1 core=2
CPUINFO cpu=3 numa=1 core=3
CPUINFO cpu=7 numa=1 core=3
AMX: false
`

func TestNode_PublishesThePinnablePool(t *testing.T) {
	cases := []struct {
		name            string
		htSiblingPolicy string
		wantPinnable    int
		wantReserved    string
	}{
		{"exclude drops the siblings from the pool", balloons.HTSiblingPolicyExclude, 4, ""},
		{"density keeps them as the reserved pool", balloons.HTSiblingPolicyReserved, 8, "4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := nodeReconciler(t, fakeLogsGetter{output: htProbe}, poolNode("n1"))
			r.PolicyIndex.SetClusterConfig(match.ClusterConfig{HTSiblingPolicy: tc.htSiblingPolicy})
			cache := topology.NewCache()
			r.TopoCache = cache
			reconcileNode(t, r, "n1")

			pinnable, ok := cache.GetPinnable("n1")
			if !ok {
				t.Fatal("the reconciler published no pinnable pool, so the metrics have nothing " +
					"to count free capacity against and fall back to the hardware total")
			}
			if len(pinnable) != tc.wantPinnable {
				t.Errorf("pinnable pool holds %d CPUs, want %d", len(pinnable), tc.wantPinnable)
			}
			// Named CPUs: the right count of the wrong threads still contradicts the policy.
			for _, cpu := range []int{0, 3} {
				if !pinnable[cpu] {
					t.Errorf("CPU %d is a representative thread and must be in the pool", cpu)
				}
			}
			if got := pinnable[4]; got != (tc.htSiblingPolicy == balloons.HTSiblingPolicyReserved) {
				t.Errorf("sibling CPU 4 in pool = %v, want %v for htSiblingPolicy=%q",
					got, !got, tc.htSiblingPolicy)
			}

			if tc.wantReserved == "" {
				return
			}
			node := &corev1.Node{}
			if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, node); err != nil {
				t.Fatalf("get node: %v", err)
			}
			if got := node.Labels[LabelReservedCPUs]; got != tc.wantReserved {
				t.Errorf("%s = %q, want %q (in density mode the reserved pool IS the sibling set)",
					LabelReservedCPUs, got, tc.wantReserved)
			}
		})
	}
}

// noHTProbe is htProbe's machine without hyperthreading: 4 CPUs, one thread per core.
const noHTProbe = `PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=1
CPU(s):                  4
Thread(s) per core:      1
Core(s) per socket:      2
Socket(s):               2
NUMA node(s):            2
NUMA node0 CPU(s):       0-1
NUMA node1 CPU(s):       2-3
CPUINFO cpu=0 numa=0 core=0
CPUINFO cpu=1 numa=0 core=1
CPUINFO cpu=2 numa=1 core=2
CPUINFO cpu=3 numa=1 core=3
AMX: false
`

// wantOutcomes is the published label set, written out so dropping a value from
// balloons.SiblingOutcomes fails the count.
var wantOutcomes = []string{
	balloons.SiblingsHeldBack,
	balloons.SiblingsReservedPool,
	balloons.SiblingsAllocatable,
	balloons.SiblingsNone,
}

// TestNode_SiblingOutcomeMetric_ReportsWhatBecameOfTheSiblings: the gauge reports
// what happened to the siblings, distinguishing hyperthreads (allocatable) from no HT.
func TestNode_SiblingOutcomeMetric_ReportsWhatBecameOfTheSiblings(t *testing.T) {
	cases := []struct {
		name            string
		probe           string
		cpuAllocation   string
		htSiblingPolicy string
		want            string
	}{
		{
			"physical cores with exclude holds the siblings out",
			htProbe, balloons.CPUAllocationPhysicalCores, balloons.HTSiblingPolicyExclude,
			balloons.SiblingsHeldBack,
		},
		{
			"physical cores with reserved makes them the reserved pool",
			htProbe, balloons.CPUAllocationPhysicalCores, balloons.HTSiblingPolicyReserved,
			balloons.SiblingsReservedPool,
		},
		{
			"hyperthreads leaves the siblings allocatable, whatever the policy says",
			htProbe, cpuAllocationHyperthreads, balloons.HTSiblingPolicyExclude,
			balloons.SiblingsAllocatable,
		},
		{
			"a machine without hyperthreading has no sibling at all",
			noHTProbe, balloons.CPUAllocationPhysicalCores, balloons.HTSiblingPolicyExclude,
			balloons.SiblingsNone,
		},
		{
			"a machine without hyperthreading reads the same under the reserved policy",
			noHTProbe, balloons.CPUAllocationPhysicalCores, balloons.HTSiblingPolicyReserved,
			balloons.SiblingsNone,
		},
		{
			"hyperthreads on a machine without hyperthreading is still no sibling",
			noHTProbe, cpuAllocationHyperthreads, balloons.HTSiblingPolicyExclude,
			balloons.SiblingsNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics.NodeHTSiblingPolicy.Reset()
			r, _ := nodeReconciler(t, fakeLogsGetter{output: tc.probe}, poolNode("n1"))
			r.PolicyIndex.SetClusterConfig(match.ClusterConfig{
				CPUAllocation:   tc.cpuAllocation,
				HTSiblingPolicy: tc.htSiblingPolicy,
			})
			reconcileNode(t, r, "n1")

			// Count before reading, since reading creates the series.
			if n := testutil.CollectAndCount(metrics.NodeHTSiblingPolicy); n != len(wantOutcomes) {
				t.Errorf("cpu_pinning_ht_sibling_policy has %d series, want one per outcome (%d)",
					n, len(wantOutcomes))
			}

			// All four asserted, zeros included: the panel shows whichever series is 1.
			for _, outcome := range wantOutcomes {
				want := 0.0
				if outcome == tc.want {
					want = 1.0
				}
				got := testutil.ToFloat64(metrics.NodeHTSiblingPolicy.WithLabelValues("n1", outcome))
				if got != want {
					t.Errorf("cpu_pinning_ht_sibling_policy{policy=%q} = %v, want %v (cpuAllocation=%q, htSiblingPolicy=%q)",
						outcome, got, want, tc.cpuAllocation, tc.htSiblingPolicy)
				}
			}
		})
	}
}

// TestNode_SiblingOutcomeMetric_MixedFleet: an override changing cpuAllocation on one
// machine yields per-machine outcomes, not the cluster setting.
func TestNode_SiblingOutcomeMetric_MixedFleet(t *testing.T) {
	metrics.NodeHTSiblingPolicy.Reset()
	n1 := poolNode("n1")
	n1.Labels = map[string]string{"node-tier": "perf"}
	n2 := poolNode("n2")

	r, _ := nodeReconciler(t, fakeLogsGetter{output: htProbe}, n1, n2)
	sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{"node-tier": "perf"},
	})
	if err != nil {
		t.Fatalf("compile selector: %v", err)
	}
	r.PolicyIndex.SetClusterConfig(match.ClusterConfig{
		CPUAllocation:   balloons.CPUAllocationPhysicalCores,
		HTSiblingPolicy: balloons.HTSiblingPolicyExclude,
		NodeOverrides: []match.NodeConfigOverride{{
			Selector:      sel,
			CPUAllocation: cpuAllocationHyperthreads,
		}},
	})
	reconcileNode(t, r, "n1")
	reconcileNode(t, r, "n2")

	// Count before WithLabelValues, which creates series.
	if n, want := testutil.CollectAndCount(metrics.NodeHTSiblingPolicy), 2*len(wantOutcomes); n != want {
		t.Errorf("cpu_pinning_ht_sibling_policy has %d series over two machines, want %d "+
			"(every outcome on each)", n, want)
	}

	for node, want := range map[string]string{
		"n1": balloons.SiblingsAllocatable,
		"n2": balloons.SiblingsHeldBack,
	} {
		for _, outcome := range wantOutcomes {
			wantValue := 0.0
			if outcome == want {
				wantValue = 1.0
			}
			got := testutil.ToFloat64(metrics.NodeHTSiblingPolicy.WithLabelValues(node, outcome))
			if got != wantValue {
				t.Errorf("cpu_pinning_ht_sibling_policy{node=%q, policy=%q} = %v, want %v",
					node, outcome, got, wantValue)
			}
		}
	}
}
