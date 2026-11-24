// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/readiness"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

func labeledNode(name string, enabled bool, maxSpan string) *corev1.Node {
	labels := map[string]string{}
	if enabled {
		labels[controller.LabelBalloonsEnabled] = "true"
	}
	if maxSpan != "" {
		labels[controller.LabelMaxSpan] = maxSpan
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func newMutator(objs ...runtime.Object) *PodMutator {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &PodMutator{Client: c}
}

// TestNodeMaxSpanFromLabels verifies max-span comes from persisted Node labels.
func TestNodeMaxSpanFromLabels(t *testing.T) {
	m := newMutator(
		labeledNode("enabled-2", true, "2"),
		labeledNode("enabled-nolabel", true, ""), // enabled but missing max-span
		labeledNode("not-enabled", false, "8"),
	)
	ctx := context.Background()

	if maxSpan, enabled, labels, err := m.nodeMaxSpan(ctx, "enabled-2"); err != nil || !enabled || maxSpan != 2 ||
		labels[controller.LabelMaxSpan] != "2" {
		t.Errorf("enabled-2 => (%d,%v,%v,%v), want (2,true,the node's labels,nil)", maxSpan, enabled, labels, err)
	}
	// No max-span label reads the conservative floor of 1.
	if maxSpan, enabled, _, _ := m.nodeMaxSpan(ctx, "enabled-nolabel"); !enabled || maxSpan != 1 {
		t.Errorf("enabled-nolabel => (%d,%v), want (1,true)", maxSpan, enabled)
	}
	// Not enabled and missing nodes both report enabled=false and no labels.
	if _, enabled, labels, _ := m.nodeMaxSpan(ctx, "not-enabled"); enabled || labels != nil {
		t.Errorf("not-enabled => (enabled=%v,labels=%v), want (false,nil)", enabled, labels)
	}
	if _, enabled, labels, err := m.nodeMaxSpan(ctx, "ghost"); err != nil || enabled || labels != nil {
		t.Errorf("missing node => (enabled=%v,labels=%v,err=%v), want (false,nil,nil)", enabled, labels, err)
	}
}

func TestClusterMaxSpanFromLabels(t *testing.T) {
	ctx := context.Background()

	// No provisioned nodes → 0 (webhook then skips pinning).
	empty := newMutator(labeledNode("plain", false, ""))
	if got, err := empty.clusterMaxSpan(ctx); err != nil || got != 0 {
		t.Errorf("clusterMaxSpan with no enabled nodes = (%d,%v), want (0,nil)", got, err)
	}

	// Max over enabled nodes only; disabled node's higher value is ignored.
	m := newMutator(
		labeledNode("a", true, "2"),
		labeledNode("b", true, "4"),
		labeledNode("c", false, "8"),
	)
	if got, err := m.clusterMaxSpan(ctx); err != nil || got != 4 {
		t.Errorf("clusterMaxSpan = (%d,%v), want (4,nil)", got, err)
	}
}

// scopedReader mimics the label-scoped Node cache: only the given balloons-enabled nodes.
func scopedReader(t *testing.T, enabled ...*corev1.Node) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	objs := make([]client.Object, 0, len(enabled))
	for _, n := range enabled {
		objs = append(objs, n)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// TestAdmissionScansUseScopedNodeReader verifies every fleet-wide Node scan on the
// admission path (clusterMaxSpan, capacityFits, memoryCapacityCheck, admitWithHeadroom)
// reads NodeReader, never m.Client: m.Client also holds a "huge" node that would change
// each answer.
func TestAdmissionScansUseScopedNodeReader(t *testing.T) {
	ctx := context.Background()

	newSmall := func() *corev1.Node { return withMemoryFloor(topoNode("small", 4, 2), 10<<30) }
	// "huge" is wider than "small" on every axis a scan measures.
	newHuge := func() *corev1.Node { return withMemoryFloor(withMaxSpan(topoNode("huge", 128, 2), 8), 1000<<30) }

	t.Run("clusterMaxSpan", func(t *testing.T) {
		m := newMutator(newSmall(), newHuge())
		m.NodeReader = scopedReader(t, newSmall())
		got, err := m.clusterMaxSpan(ctx)
		if err != nil {
			t.Fatalf("clusterMaxSpan error: %v", err)
		}
		if got != 2 {
			t.Errorf("clusterMaxSpan = %d, want 2 (scoped reader sees only span-2 'small'; "+
				"got 8 would mean the scan read the unscoped m.Client's 'huge')", got)
		}
	})

	t.Run("capacityFits unscheduled", func(t *testing.T) {
		m := newMutator(newSmall(), newHuge())
		m.NodeReader = scopedReader(t, newSmall())
		// 20 cores at span 1 fits only 'huge'.
		pod := unscheduledPod("p", "20")
		fits, detail, err := m.capacityFits(ctx, pod, &pod.Spec.Containers[0], false, 1, "", "physical-cores", nil, nil)
		if err != nil {
			t.Fatalf("capacityFits error: %v", err)
		}
		if fits {
			t.Errorf("capacityFits = true, want false (only 'small' is in scope; a fit would mean "+
				"the scan read 'huge' in m.Client). detail=%q", detail)
		}
	})

	t.Run("memoryCapacityCheck unscheduled", func(t *testing.T) {
		m := newMutator(newSmall(), newHuge())
		m.NodeReader = scopedReader(t, newSmall())
		// 100 GiB fits only 'huge'.
		fits, detail, err := m.memoryCapacityCheck(ctx, unscheduledPod("p", "1"), 1, 100<<30)
		if err != nil {
			t.Fatalf("memoryCapacityCheck error: %v", err)
		}
		if fits {
			t.Errorf("memoryCapacityCheck = true, want false (only 'small' is in scope; a fit would "+
				"mean the scan read 'huge' in m.Client). detail=%q", detail)
		}
	})

	t.Run("admitWithHeadroom", func(t *testing.T) {
		// Only 'huge' has live data; a scoped scan must leave sawLive=false.
		topo := topology.NewCache()
		topo.Set("huge", simpleTopo())
		m := newMutatorWithTopo(topo, newSmall(), newHuge(), nrtObj("huge"))
		m.Reservations = NewReservationStore(0)
		m.NodeReader = scopedReader(t, newSmall())

		pod := unscheduledPod("p", "1")
		verdict := m.admitWithHeadroom(ctx, pod, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
		if verdict.sawLive {
			t.Error("admitWithHeadroom sawLive=true, want false ('small' has no live data; " +
				"sawLive would mean the scan read 'huge' in m.Client)")
		}
		if verdict.fits {
			t.Error("admitWithHeadroom fits=true, want false (no in-scope node has headroom)")
		}
		if req := affinityRequiredValues(pod); req != nil {
			t.Errorf("admitWithHeadroom injected REQUIRED affinity %v, want none (no candidate in scope)", req)
		}
		if h := affinityPreferredHostname(pod); h != "" {
			t.Errorf("admitWithHeadroom injected PREFERRED %q, want none", h)
		}
	})
}

// TestNodeReaderFallsBackToClient verifies Node reads use Client when NodeReader is unset.
func TestNodeReaderFallsBackToClient(t *testing.T) {
	ctx := context.Background()
	m := newMutator(labeledNode("a", true, "2"))
	if m.NodeReader != nil {
		t.Fatalf("precondition: NodeReader should be nil")
	}
	if got, err := m.clusterMaxSpan(ctx); err != nil || got != 2 {
		t.Errorf("clusterMaxSpan with nil NodeReader = (%d,%v), want (2,nil) via m.Client fallback", got, err)
	}
}

func hasRequiredKey(pod *corev1.Pod, key string) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return false
	}
	na := pod.Spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	for _, term := range na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, e := range term.MatchExpressions {
			if e.Key == key {
				return true
			}
		}
	}
	return false
}

func hasPreferredKey(pod *corev1.Pod, key string) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return false
	}
	na := pod.Spec.Affinity.NodeAffinity
	for _, p := range na.PreferredDuringSchedulingIgnoredDuringExecution {
		for _, e := range p.Preference.MatchExpressions {
			if e.Key == key {
				return true
			}
		}
	}
	return false
}

func TestInjectNodeAffinity(t *testing.T) {
	amxPrefer := map[string]cpopv1alpha1.NodeFeatureMode{amxNodeLabel: cpopv1alpha1.NodeFeaturePrefer}
	amxRequire := map[string]cpopv1alpha1.NodeFeatureMode{amxNodeLabel: cpopv1alpha1.NodeFeatureRequire}
	amxIgnore := map[string]cpopv1alpha1.NodeFeatureMode{amxNodeLabel: cpopv1alpha1.NodeFeatureIgnore}

	// enforce=true, amx=prefer → required balloons-enabled + preferred AMX.
	p := &corev1.Pod{}
	injectNodeAffinity(p, true, amxPrefer)
	if !hasRequiredKey(p, controller.LabelBalloonsEnabled) {
		t.Error("enforce should add required balloons-enabled term")
	}
	if !hasPreferredKey(p, amxNodeLabel) {
		t.Error("amx=prefer should add preferred AMX term")
	}
	if hasRequiredKey(p, amxNodeLabel) {
		t.Error("amx=prefer must NOT add a required AMX term")
	}

	// enforce=true, amx=require → both required.
	p = &corev1.Pod{}
	injectNodeAffinity(p, true, amxRequire)
	if !hasRequiredKey(p, controller.LabelBalloonsEnabled) || !hasRequiredKey(p, amxNodeLabel) {
		t.Error("enforce+require should add both required terms")
	}

	// enforce=false, amx=ignore → no affinity at all.
	p = &corev1.Pod{}
	injectNodeAffinity(p, false, amxIgnore)
	if p.Spec.Affinity != nil {
		t.Error("no enforce + ignore should not create affinity")
	}

	// Existing required terms are preserved and AND-ed with our requirement.
	p = &corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: "disktype", Operator: corev1.NodeSelectorOpIn, Values: []string{"ssd"}},
			}}},
		},
	}}}}
	injectNodeAffinity(p, true, amxIgnore)
	if !hasRequiredKey(p, "disktype") || !hasRequiredKey(p, controller.LabelBalloonsEnabled) {
		t.Error("existing required term must be preserved AND our requirement added")
	}

	// Mixed prefer and require entries, emitted in sorted order.
	p = &corev1.Pod{}
	injectNodeAffinity(p, false, map[string]cpopv1alpha1.NodeFeatureMode{
		"vendor.io/gpu-family": cpopv1alpha1.NodeFeatureRequire,
		amxNodeLabel:           cpopv1alpha1.NodeFeaturePrefer,
	})
	if !hasRequiredKey(p, "vendor.io/gpu-family") {
		t.Error("require entry must add a required term")
	}
	if !hasPreferredKey(p, amxNodeLabel) {
		t.Error("prefer entry must add a preferred term")
	}
}

func TestEventfEmitsWarning(t *testing.T) {
	rec := record.NewFakeRecorder(4)
	m := &PodMutator{Recorder: rec}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "inference"}}

	m.eventf(pod, "NotPinned", "reason %d", 42)
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "NotPinned") || !strings.Contains(e, "reason 42") {
			t.Errorf("event = %q, want NotPinned + formatted message", e)
		}
	default:
		t.Fatal("expected a Warning event, got none")
	}

	// Nil recorder must be a safe no-op.
	(&PodMutator{}).eventf(pod, "NotPinned", "x")
}

func TestPerNUMACPUShare(t *testing.T) {
	cases := []struct {
		cores, span, want int
	}{
		{0, 1, 0},
		{4, 1, 4},   // span 1 → full request
		{4, 2, 2},   // even split
		{3, 2, 2},   // odd → ceil: one NUMA carries the remainder
		{5, 2, 3},   // ceil(5/2)
		{64, 2, 32}, // large request, even split
		{7, 3, 3},   // ceil(7/3)
		{2, 0, 2},   // span clamps to 1 (never divide by zero / inflate)
		{2, -4, 2},  // negative span clamps to 1
		// Overflow guard for the ceil.
		{math.MaxInt, 2, math.MaxInt/2 + 1},
		{math.MaxInt, 1, math.MaxInt},
	}
	for _, c := range cases {
		if got := perNUMACPUShare(c.cores, c.span); got != c.want {
			t.Errorf("perNUMACPUShare(%d, %d) = %d, want %d", c.cores, c.span, got, c.want)
		}
	}
}

func TestNodeFits(t *testing.T) {
	// 64 physical / 2 NUMA = 32 per NUMA.
	labels := map[string]string{
		controller.LabelPhysicalCPUs:   "64",
		controller.LabelNumaNodes:      "2",
		controller.LabelThreadsPerCore: "2",
	}
	// nodeFits compares ceil(cores/span) against per-NUMA capacity (32 here).
	if fits, cap := nodeFits(labels, 64, 2, "physical-cores"); !fits || cap != 32 {
		t.Errorf("physical span2 64c => (fits=%v,cap=%d), want (true,32)", fits, cap)
	}

	if fits, cap := nodeFits(labels, 33, 1, "physical-cores"); fits || cap != 32 {
		t.Errorf("33c span1 => (fits=%v,cap=%d), want (false,32)", fits, cap)
	}

	if fits, cap := nodeFits(labels, 64, 1, "hyperthreads"); !fits || cap != 64 {
		t.Errorf("hyperthreads span1 64c => (fits=%v,cap=%d), want (true,64)", fits, cap)
	}
	// Odd requests round the per-NUMA share up: 63/2 fits, 65/2 does not.
	if fits, _ := nodeFits(labels, 63, 2, "physical-cores"); !fits {
		t.Error("63c span2 should fit (share ceil(63/2)=32 ≤ 32)")
	}
	if fits, _ := nodeFits(labels, 65, 2, "physical-cores"); fits {
		t.Error("65c span2 must NOT fit (share ceil(65/2)=33 > 32)")
	}
	// Missing topology labels: fits, capacity 0.
	if fits, cap := nodeFits(map[string]string{}, 1000, 8, "physical-cores"); !fits || cap != 0 {
		t.Errorf("no-topo => (fits=%v,cap=%d), want (true,0)", fits, cap)
	}
	// Invalid labels fold to unknown (fits, capacity 0).
	for _, bad := range []string{"0", "-64", "-1", "notanumber"} {
		l := map[string]string{
			controller.LabelPhysicalCPUs: bad,
			controller.LabelNumaNodes:    "2",
		}
		if fits, cap := nodeFits(l, 1000, 4, "physical-cores"); !fits || cap != 0 {
			t.Errorf("physical-cpus=%q => (fits=%v,cap=%d), want (true,0)", bad, fits, cap)
		}
		l = map[string]string{
			controller.LabelPhysicalCPUs: "64",
			controller.LabelNumaNodes:    bad,
		}
		if fits, cap := nodeFits(l, 1000, 4, "physical-cores"); !fits || cap != 0 {
			t.Errorf("numa-nodes=%q => (fits=%v,cap=%d), want (true,0)", bad, fits, cap)
		}
	}
	// Negative threads-per-core floors to 1.
	negThreads := map[string]string{
		controller.LabelPhysicalCPUs:   "64",
		controller.LabelNumaNodes:      "2",
		controller.LabelThreadsPerCore: "-2",
	}
	if fits, cap := nodeFits(negThreads, 32, 1, "hyperthreads"); !fits || cap != 32 {
		t.Errorf("neg threads hyperthreads span1 => (fits=%v,cap=%d), want (true,32)", fits, cap)
	}
}

func TestMemoryFits(t *testing.T) {
	// Per-NUMA floor of 100 GiB.
	const floor = int64(100) << 30
	labels := map[string]string{
		controller.LabelMemoryPerNUMABytes: strconv.FormatInt(floor, 10),
	}
	// span 1 → capacity = floor; a request at the floor fits, one byte over does not.
	if fits, cap := memoryFits(labels, floor, 1); !fits || cap != floor {
		t.Errorf("span1 at floor => (fits=%v,cap=%d), want (true,%d)", fits, cap, floor)
	}
	if fits, _ := memoryFits(labels, floor+1, 1); fits {
		t.Error("floor+1 must NOT fit span1")
	}
	// span multiplies the capacity, not the request.
	if fits, cap := memoryFits(labels, 2*floor, 2); !fits || cap != 2*floor {
		t.Errorf("span2 at 2×floor => (fits=%v,cap=%d), want (true,%d)", fits, cap, 2*floor)
	}
	if fits, _ := memoryFits(labels, 2*floor+1, 2); fits {
		t.Error("2×floor+1 must NOT fit span2")
	}

	if fits, cap := memoryFits(map[string]string{}, 1<<50, 8); !fits || cap != 0 {
		t.Errorf("no-floor => (fits=%v,cap=%d), want (true,0)", fits, cap)
	}
	// Invalid floor labels are unknown (fits, capacity 0).
	for _, bad := range []string{"0", "-1", "-100", "notanumber", ""} {
		l := map[string]string{controller.LabelMemoryPerNUMABytes: bad}
		if fits, cap := memoryFits(l, 1<<30, 2); !fits || cap != 0 {
			t.Errorf("floor=%q => (fits=%v,cap=%d), want (true,0)", bad, fits, cap)
		}
	}
	// floor×span overflow is unknown, not a wrapped capacity.
	huge := map[string]string{controller.LabelMemoryPerNUMABytes: strconv.FormatInt(math.MaxInt64, 10)}
	if fits, cap := memoryFits(huge, 1<<30, 8); !fits || cap != 0 {
		t.Errorf("overflow floor => (fits=%v,cap=%d), want (true,0)", fits, cap)
	}
}

func TestPodMemoryRequestBytes(t *testing.T) {
	c := &corev1.Container{Name: "vllm"}
	c.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		*c,
		{Name: "side", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}}},
	}}}

	if got := podMemoryRequestBytes(pod, c, false); got != int64(8)<<30 {
		t.Errorf("container-scoped = %d, want %d", got, int64(8)<<30)
	}
	// Pod-scoped sums across containers: 8Gi + 2Gi.
	wantSum := int64(10) << 30
	if got := podMemoryRequestBytes(pod, nil, true); got != wantSum {
		t.Errorf("pod-scoped = %d, want %d (sum, not max)", got, wantSum)
	}

	none := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "x"}}}}
	if got := podMemoryRequestBytes(none, nil, true); got != 0 {
		t.Errorf("no memory request = %d, want 0", got)
	}
}

// withMemoryFloor stamps the per-NUMA memory-floor label on a node.
func withMemoryFloor(n *corev1.Node, bytes int64) *corev1.Node {
	n.Labels[controller.LabelMemoryPerNUMABytes] = strconv.FormatInt(bytes, 10)
	return n
}

func TestMemoryCapacityCheckScheduled(t *testing.T) {
	const floor = int64(100) << 30 // 100 GiB per NUMA
	ctx := context.Background()
	m := newMutator(withMemoryFloor(topoNode("n1", 64, 2), floor))

	pod := scheduledPod("n1", "8")
	fits, _, err := m.memoryCapacityCheck(ctx, pod, 2, 150<<30)
	if err != nil || !fits {
		t.Errorf("150GiB span2 on 100GiB×2 floor => (fits=%v,err=%v), want (true,nil)", fits, err)
	}
	fits, detail, err := m.memoryCapacityCheck(ctx, pod, 2, 250<<30)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if fits {
		t.Error("250GiB span2 must NOT fit a 100GiB×2 floor")
	}
	if !strings.Contains(detail, "n1") || !strings.Contains(detail, "span 2") {
		t.Errorf("detail should cite the node and span, got %q", detail)
	}
}

func TestMemoryCapacityCheckScheduledNoFloorLabelSkips(t *testing.T) {
	ctx := context.Background()

	m := newMutator(topoNode("n1", 64, 2))
	fits, _, err := m.memoryCapacityCheck(ctx, scheduledPod("n1", "8"), 2, 1<<50)
	if err != nil || !fits {
		t.Errorf("missing floor label must not block => (fits=%v,err=%v), want (true,nil)", fits, err)
	}
}

// TestMemoryCapacityCheckScheduledNodeNotFoundSkips: a missing target node is unknown
// (skip), never a denial.
func TestMemoryCapacityCheckScheduledNodeNotFoundSkips(t *testing.T) {
	ctx := context.Background()
	m := newMutator() // no nodes seeded → Get returns NotFound
	fits, detail, err := m.memoryCapacityCheck(ctx, scheduledPod("ghost", "8"), 2, 1<<50)
	if err != nil {
		t.Fatalf("NotFound must not surface as an error, got %v", err)
	}
	if !fits {
		t.Errorf("missing node must skip (fits=true), got fits=false detail=%q", detail)
	}
}

// TestCapacityFitsScheduledNodeNotFoundSkips: a bound pod with unknown node labels skips
// the check.
func TestCapacityFitsScheduledNodeNotFoundSkips(t *testing.T) {
	ctx := context.Background()
	m := newMutator() // no nodes seeded
	pod := scheduledPod("ghost", "8")
	fits, detail, err := m.capacityFits(ctx, pod, &pod.Spec.Containers[0], false, 1, "", "physical-cores", nil, nil)
	if err != nil {
		t.Fatalf("NotFound must not surface as an error, got %v", err)
	}
	if !fits {
		t.Errorf("missing node must skip (fits=true), got fits=false detail=%q", detail)
	}
}

func TestMemoryCapacityCheckUnscheduledAnyNodeFits(t *testing.T) {
	const floor = int64(100) << 30
	ctx := context.Background()
	m := newMutator(
		withMemoryFloor(topoNode("small", 64, 2), 50<<30), // 50 GiB × 2 = 100 GiB
		withMemoryFloor(topoNode("big", 64, 2), floor),    // 100 GiB × 2 = 200 GiB
	)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "inference"}}

	if fits, _, err := m.memoryCapacityCheck(ctx, pod, 2, 150<<30); err != nil || !fits {
		t.Errorf("150GiB should fit the big node => (fits=%v,err=%v), want (true,nil)", fits, err)
	}

	fits, detail, err := m.memoryCapacityCheck(ctx, pod, 2, 250<<30)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if fits {
		t.Error("250GiB must exceed every node's per-span memory")
	}
	if !strings.Contains(detail, "every balloons-enabled node") {
		t.Errorf("detail should be fleet-wide, got %q", detail)
	}
}

// TestMemoryCapacityCheckUnscheduledSkipsSpanIneligibleNode: a roomy node whose max-span
// is below the requested span must not produce a pass.
func TestMemoryCapacityCheckUnscheduledSkipsSpanIneligibleNode(t *testing.T) {
	ctx := context.Background()
	m := newMutator(

		withMaxSpan(withMemoryFloor(topoNode("roomy-narrow", 64, 2), 500<<30), 1),

		withMemoryFloor(topoNode("span2-small", 64, 2), 40<<30),
	)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "inference"}}

	fits, detail, err := m.memoryCapacityCheck(ctx, pod, 2, 150<<30)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if fits {
		t.Errorf("span-ineligible roomy node must not yield a false pass; got fits=true (detail %q)", detail)
	}
}

// TestCapacityFitsUnscheduledSkipsSpanIneligibleNode is the CPU twin of the above.
func TestCapacityFitsUnscheduledSkipsSpanIneligibleNode(t *testing.T) {
	ctx := context.Background()
	m := newMutator(

		withMaxSpan(topoNode("roomy-narrow", 64, 2), 1),

		topoNode("span2-small", 4, 2),
	)
	pod := scheduledPod("", "8") // unscheduled, 8 cores
	fits, _, err := m.capacityFits(ctx, pod, &pod.Spec.Containers[0], false, 2, "", "physical-cores", nil, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if fits {
		t.Error("span-ineligible roomy node must not yield a false pass in the CPU static fallback")
	}
}

func TestPodCPURequestCores(t *testing.T) {
	c := &corev1.Container{Name: "vllm"}
	c.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		*c,
		{Name: "side", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}}},
	}}}

	if got := podCPURequestCores(pod, c, false); got != 8 {
		t.Errorf("container-scoped = %d, want 8", got)
	}

	if got := podCPURequestCores(pod, nil, true); got != 10 {
		t.Errorf("pod-scoped = %d, want 10 (sum, not max)", got)
	}
	// Fractional requests sum in milli first: 500m + 500m = 1 core.
	frac := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "a", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}}},
		{Name: "b", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}}},
	}}}
	if got := podCPURequestCores(frac, nil, true); got != 1 {
		t.Errorf("pod-scoped fractional = %d, want 1 (sum milli)", got)
	}
	// Whole core plus fractional sidecar rounds up to 2 cores.
	ceil := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "main", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}},
		{Name: "side", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}}},
	}}}
	if got := podCPURequestCores(ceil, nil, true); got != 2 {
		t.Errorf("pod-scoped 1 core + 500m = %d, want 2 (ceil to whole cores)", got)
	}
}

// NRT zone types published by the NRI balloons plugin.
const (
	testContainerZoneType = "allocation for container"
	testBalloonZoneType   = "balloon"
)

func nrtZone(zoneType, name, parent, cpuset string) map[string]interface{} {
	return map[string]interface{}{
		"type":   zoneType,
		"name":   name,
		"parent": parent,
		"attributes": []interface{}{
			map[string]interface{}{"name": "cpuset", "value": cpuset},
		},
	}
}

func nrtObj(node string, zones ...map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(nrtpkg.GVK)
	u.SetName(node)
	zs := make([]interface{}, len(zones))
	for i, z := range zones {
		zs[i] = z
	}
	u.Object["zones"] = zs
	return u
}

// topoNode is a balloons-enabled node with the static topology labels and a max-span
// label defaulting to the NUMA count. Use withMaxSpan or withoutMaxSpan to change it.
func topoNode(name string, physical, numa int) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
		controller.LabelBalloonsEnabled: "true",
		controller.LabelPhysicalCPUs:    strconv.Itoa(physical),
		controller.LabelNumaNodes:       strconv.Itoa(numa),
		controller.LabelMaxSpan:         strconv.Itoa(numa),
	}}}
}

// withMaxSpan overrides a node's max-span label.
func withMaxSpan(n *corev1.Node, maxSpan int) *corev1.Node {
	n.Labels[controller.LabelMaxSpan] = strconv.Itoa(maxSpan)
	return n
}

// withCordon marks a node unschedulable.
func withCordon(n *corev1.Node) *corev1.Node {
	n.Spec.Unschedulable = true
	return n
}

// withTaint adds a taint to a node, leaving Spec.Unschedulable false.
func withTaint(n *corev1.Node, key, value string, effect corev1.TaintEffect) *corev1.Node {
	n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: key, Value: value, Effect: effect})
	return n
}

// withReady sets the node's Ready condition. topoNode sets none, which reads as unknown.
func withReady(n *corev1.Node, status corev1.ConditionStatus) *corev1.Node {
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == corev1.NodeReady {
			n.Status.Conditions[i].Status = status
			return n
		}
	}
	n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{
		Type: corev1.NodeReady, Status: status,
	})
	return n
}

// testDevice is the extended resource the candidacy tests request.
const testDevice = "vendor.com/gpu"

// withAllocatable advertises an extended resource on a node.
func withAllocatable(n *corev1.Node, name, quantity string) *corev1.Node {
	if n.Status.Allocatable == nil {
		n.Status.Allocatable = corev1.ResourceList{}
	}
	n.Status.Allocatable[corev1.ResourceName(name)] = resource.MustParse(quantity)
	return n
}

// withoutTopologyLabels strips physical-cpus and numa-nodes, keeping balloons-enabled and
// max-span: a valid candidate with unknown per-NUMA capacity.
func withoutTopologyLabels(n *corev1.Node) *corev1.Node {
	delete(n.Labels, controller.LabelPhysicalCPUs)
	delete(n.Labels, controller.LabelNumaNodes)
	return n
}

// withoutMaxSpan removes the max-span label.
func withoutMaxSpan(n *corev1.Node) *corev1.Node {
	delete(n.Labels, controller.LabelMaxSpan)
	return n
}

func scheduledPod(node, cores string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "inference"},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name: "vllm",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cores)},
				},
			}},
		},
	}
}

// newMutatorWithTopo builds a mutator whose fake client also serves the NRT CR.
func newMutatorWithTopo(topo *topology.Cache, objs ...client.Object) *PodMutator {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PodMutator{Client: c, TopoCache: topo}
}

// simpleTopo is a 4-CPU node: 2 NUMA nodes, 2 single-thread cores each.
func simpleTopo() map[int]topology.CPUInfo {
	return map[int]topology.CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0}, 1: {CPU: 1, NUMA: 0, Core: 1},
		2: {CPU: 2, NUMA: 1, Core: 2}, 3: {CPU: 3, NUMA: 1, Core: 3},
	}
}

// TestCapacityFitsLiveOccupancy: a request that fits statically is rejected when live NRT
// occupancy leaves too little room.
func TestCapacityFitsLiveOccupancy(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("n1", simpleTopo())
	// cpu0 and cpu2 are claimed: 1 free core per NUMA.
	nrt := nrtObj("n1", nrtZone(testContainerZoneType, "inference/other/other", "balloon-span2[0]", "0,2"))
	m := newMutatorWithTopo(topo, topoNode("n1", 4, 2), nrt)
	pod := scheduledPod("n1", "2") // 2 cores, span 1

	if fits, _ := nodeFits(topoNode("n1", 4, 2).Labels, 2, 1, "physical-cores"); !fits {
		t.Fatal("precondition: request should fit the static formula")
	}

	fits, detail, err := m.capacityFits(ctx, pod, &pod.Spec.Containers[0], false, 1, "", "physical-cores", topoNode("n1", 4, 2).Labels, nil)
	if err != nil {
		t.Fatalf("capacityFits error: %v", err)
	}
	if fits {
		t.Error("request must NOT fit live occupancy (each NUMA has 1 free core, needs 2)")
	}
	if !strings.Contains(detail, "live") {
		t.Errorf("failure detail should identify the live check, got %q", detail)
	}
}

// TestCapacityCheckStaticLiveAgreeOnSpan: static and live checks reach the same verdict for
// the same (cores, span, node).
func TestCapacityCheckStaticLiveAgreeOnSpan(t *testing.T) {
	ctx := context.Background()

	node := withMaxSpan(topoNode("n1", 8, 2), 2)

	// Span-2, 4 cores: per-NUMA share 2 fits statically.
	if fits, _ := nodeFits(node.Labels, 4, 2, "physical-cores"); !fits {
		t.Fatal("static: span2 req4 should fit (share 2 ≤ 4 per NUMA)")
	}

	// With 2 free per NUMA the share still fits live.
	topo := topology.NewCache()
	topo.Set("n1", map[int]topology.CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0}, 1: {CPU: 1, NUMA: 0, Core: 1},
		2: {CPU: 2, NUMA: 0, Core: 2}, 3: {CPU: 3, NUMA: 0, Core: 3},
		4: {CPU: 4, NUMA: 1, Core: 4}, 5: {CPU: 5, NUMA: 1, Core: 5},
		6: {CPU: 6, NUMA: 1, Core: 6}, 7: {CPU: 7, NUMA: 1, Core: 7},
	})

	nrt := nrtObj("n1", nrtZone(testContainerZoneType, "inference/x/x", "balloon-span2[0]", "0,2,4,6"))
	m := newMutatorWithTopo(topo, node, nrt)
	fits, free, ok := m.liveFits(ctx, "n1", 4, 2, "physical-cores", false, nil)
	if !ok {
		t.Fatal("live check should have data")
	}
	if free != 2 {
		t.Fatalf("precondition: expected 2 free cores per NUMA, got least-free %d", free)
	}
	if !fits {
		t.Errorf("live: span2 req4 with 2 free/NUMA should fit (share 2 ≤ free 2) — static fits, so live must agree, not diverge")
	}
}

// TestCapacityFitsFallbackWhenNRTAbsent: no NRT CR falls back to the static formula.
func TestCapacityFitsFallbackWhenNRTAbsent(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("n1", simpleTopo())

	m := newMutatorWithTopo(topo, topoNode("n1", 4, 2))
	pod := scheduledPod("n1", "2")

	fits, _, err := m.capacityFits(ctx, pod, &pod.Spec.Containers[0], false, 1, "", "physical-cores", topoNode("n1", 4, 2).Labels, nil)
	if err != nil {
		t.Fatalf("capacityFits error: %v", err)
	}
	if !fits {
		t.Error("with NRT absent, static fallback should fit (cap 2 >= 2)")
	}
}

// TestCapacityFitsFallbackWhenNodeNotCached: an uncached topology falls back to static.
func TestCapacityFitsFallbackWhenNodeNotCached(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache() // empty: n1 not cached
	nrt := nrtObj("n1", nrtZone(testContainerZoneType, "inference/other/other", "balloon-span2[0]", "0,2"))
	m := newMutatorWithTopo(topo, topoNode("n1", 4, 2), nrt)
	pod := scheduledPod("n1", "2")

	fits, _, err := m.capacityFits(ctx, pod, &pod.Spec.Containers[0], false, 1, "", "physical-cores", topoNode("n1", 4, 2).Labels, nil)
	if err != nil {
		t.Fatalf("capacityFits error: %v", err)
	}
	if !fits {
		t.Error("with node uncached, static fallback should fit (cap 2 >= 2)")
	}
}

// TestCapacityFitsUnscheduledUnchanged: an unscheduled pod uses the static scan and ignores
// live occupancy.
func TestCapacityFitsUnscheduledUnchanged(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("n1", simpleTopo())

	nrt := nrtObj("n1", nrtZone(testContainerZoneType, "inference/other/other", "balloon-span2[0]", "0,2"))
	m := newMutatorWithTopo(topo, topoNode("n1", 4, 2), nrt)
	pod := scheduledPod("", "2") // no nodeName

	fits, _, err := m.capacityFits(ctx, pod, &pod.Spec.Containers[0], false, 1, "", "physical-cores", nil, nil)
	if err != nil {
		t.Fatalf("capacityFits error: %v", err)
	}
	if !fits {
		t.Error("unscheduled pod should fit statically (cap 2 >= 2), ignoring live occupancy")
	}
}

// affinityRequiredValues returns the hostnames a pod is required to land on.
func affinityRequiredValues(pod *corev1.Pod) []string {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil ||
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return nil
	}
	for _, term := range pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, e := range term.MatchExpressions {
			if e.Key == hostnameLabel {
				return e.Values
			}
		}
	}
	return nil
}

// affinityPreferredHostname returns the hostname a pod is preferred toward.
func affinityPreferredHostname(pod *corev1.Pod) string {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return ""
	}
	for _, p := range pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		for _, e := range p.Preference.MatchExpressions {
			if e.Key == hostnameLabel && len(e.Values) == 1 {
				return e.Values[0]
			}
		}
	}
	return ""
}

func unscheduledPod(name, cores string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "inference"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "vllm",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cores)},
				},
			}},
		},
	}
}

// tolerating adds a toleration matching a `<key>=<value>:<effect>` taint.
func tolerating(pod *corev1.Pod, key, value string, effect corev1.TaintEffect) *corev1.Pod {
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{
		Key: key, Operator: corev1.TolerationOpEqual, Value: value, Effect: effect,
	})
	return pod
}

// toleratingKey adds a valueless `operator: Exists` toleration, as DaemonSet pods carry.
func toleratingKey(pod *corev1.Pod, key string, effect corev1.TaintEffect) *corev1.Pod {
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{
		Key: key, Operator: corev1.TolerationOpExists, Effect: effect,
	})
	return pod
}

func requesting(pod *corev1.Pod, name, quantity string) *corev1.Pod {
	c := &pod.Spec.Containers[0]
	if c.Resources.Requests == nil {
		c.Resources.Requests = corev1.ResourceList{}
	}
	c.Resources.Requests[corev1.ResourceName(name)] = resource.MustParse(quantity)
	return pod
}

// TestAdmitWithHeadroomRestrictsToFittingNodes: only the node with live headroom is
// required and reserved.
func TestAdmitWithHeadroomRestrictsToFittingNodes(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("full", simpleTopo())  // 4 CPUs, 2 NUMA, 2 cores each
	topo.Set("empty", simpleTopo()) // same layout, empty NRT below
	// Every cpu on "full" is occupied.
	nrtFull := nrtObj("full",
		nrtZone(testContainerZoneType, "inference/a/a", "balloon-span2[0]", "0-3"))
	nrtEmpty := nrtObj("empty") // no occupancy
	m := newMutatorWithTopo(topo, topoNode("full", 4, 2), topoNode("empty", 4, 2), nrtFull, nrtEmpty)
	m.Reservations = NewReservationStore(0)

	pod := unscheduledPod("p", "1")
	verdict := m.admitWithHeadroom(ctx, pod, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawLive || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=true", verdict)
	}

	req := affinityRequiredValues(pod)
	if len(req) != 1 || req[0] != "empty" {
		t.Errorf("REQUIRED hostnames = %v, want [empty]", req)
	}
	if got := affinityPreferredHostname(pod); got != "" {
		t.Errorf("PREFERRED hostname = %q, want none (the reserved node is REQUIRED)", got)
	}

	if r := reservedCores(m.Reservations, "empty"); len(r) == 0 {
		t.Error("expected reservation on 'empty' after admit, got none")
	}
}

// fourCorePerNUMATopo is an 8-CPU node with 2 NUMA nodes of 4 single-thread cores.
func fourCorePerNUMATopo() map[int]topology.CPUInfo {
	info := map[int]topology.CPUInfo{}
	cpu := 0
	for numa := 0; numa < 2; numa++ {
		for i := 0; i < 4; i++ {
			info[cpu] = topology.CPUInfo{CPU: cpu, NUMA: numa, Core: cpu}
			cpu++
		}
	}
	return info
}

// TestAdmitWithHeadroomPackingSteersOppositeNodes: with two fitting nodes, spread picks
// the emptier and binpack the fuller.
func TestAdmitWithHeadroomPackingSteersOppositeNodes(t *testing.T) {
	ctx := context.Background()
	// "fuller" has 2 busy cores per NUMA, "emptier" none.
	fleet := func() *PodMutator {
		topo := topology.NewCache()
		topo.Set("fuller", fourCorePerNUMATopo())
		topo.Set("emptier", fourCorePerNUMATopo())
		nrtFuller := nrtObj("fuller",
			nrtZone(testContainerZoneType, "inference/x/x", "balloon-span2[0]", "0,1,4,5"))
		nrtEmptier := nrtObj("emptier")
		m := newMutatorWithTopo(topo,
			topoNode("fuller", 8, 2), topoNode("emptier", 8, 2), nrtFuller, nrtEmptier)
		m.Reservations = NewReservationStore(0)
		return m
	}

	mSpread := fleet()
	pSpread := unscheduledPod("p", "2")
	vs := mSpread.admitWithHeadroom(ctx, pSpread, 2, 2, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !vs.fits {
		t.Fatalf("spread: verdict=%+v, want fits", vs)
	}
	if req := affinityRequiredValues(pSpread); len(req) != 1 || req[0] != "emptier" {
		t.Errorf("spread REQUIRED = %v, want [emptier] (roomiest node)", req)
	}

	mBin := fleet()
	pBin := unscheduledPod("p", "2")
	vb := mBin.admitWithHeadroom(ctx, pBin, 2, 2, 0, true, "", "physical-cores", packingBinpack, nil, reservePolicy{}, logr.Discard())
	if !vb.fits {
		t.Fatalf("binpack: verdict=%+v, want fits", vb)
	}
	if req := affinityRequiredValues(pBin); len(req) != 1 || req[0] != "fuller" {
		t.Errorf("binpack REQUIRED = %v, want [fuller] (fills partially-used node first)", req)
	}
}

// TestAdmitWithHeadroomMemoryFiltersSteering: when both nodes fit CPU but only one fits
// memory, steering requires the memory-fitting node.
func TestAdmitWithHeadroomMemoryFiltersSteering(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("bigmem", simpleTopo())   // 4 CPUs, 2 NUMA — CPU-fits a 1-core span-1 pod
	topo.Set("smallmem", simpleTopo()) // same CPU layout, also CPU-fits
	nrtBig := nrtObj("bigmem")         // empty occupancy → CPU headroom on both
	nrtSmall := nrtObj("smallmem")

	m := newMutatorWithTopo(topo,
		withMemoryFloor(topoNode("bigmem", 4, 2), 100<<30),
		withMemoryFloor(topoNode("smallmem", 4, 2), 1<<30),
		nrtBig, nrtSmall)
	m.Reservations = NewReservationStore(0)

	pod := unscheduledPod("p", "1")

	verdict := m.admitWithHeadroom(ctx, pod, 1, 1, 50<<30, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawLive || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=true", verdict)
	}
	req := affinityRequiredValues(pod)
	if len(req) != 1 || req[0] != "bigmem" {
		t.Errorf("REQUIRED hostnames = %v, want [bigmem] (smallmem must be memory-filtered out)", req)
	}
}

// TestAdmitWithHeadroomHeterogeneousFleetOneWinner: the span, memory and live CPU filters
// compose; each of three nodes fails a different one and only "winner" is steered to.
func TestAdmitWithHeadroomHeterogeneousFleetOneWinner(t *testing.T) {
	ctx := context.Background()
	const bigFloor = int64(100) << 30
	topo := topology.NewCache()
	topo.Set("too-narrow", simpleTopo())
	topo.Set("mem-too-small", simpleTopo())
	topo.Set("cpu-full", simpleTopo())
	topo.Set("winner", simpleTopo())

	m := newMutatorWithTopo(topo,

		withMaxSpan(withMemoryFloor(topoNode("too-narrow", 4, 2), bigFloor), 1), nrtObj("too-narrow"),

		withMemoryFloor(topoNode("mem-too-small", 4, 2), 1<<30), nrtObj("mem-too-small"),

		withMemoryFloor(topoNode("cpu-full", 4, 2), bigFloor),
		nrtObj("cpu-full", nrtZone(testContainerZoneType, "inference/x/x", "balloon-span2[0]", "0-3")),

		withMemoryFloor(topoNode("winner", 4, 2), bigFloor), nrtObj("winner"),
	)
	m.Reservations = NewReservationStore(0)

	pod := unscheduledPod("p", "1")

	verdict := m.admitWithHeadroom(ctx, pod, 1, 2, 50<<30, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawLive || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=true", verdict)
	}
	req := affinityRequiredValues(pod)
	if len(req) != 1 || req[0] != "winner" {
		t.Errorf("REQUIRED hostnames = %v, want [winner] (each other node fails a different filter)", req)
	}
	if got := affinityPreferredHostname(pod); got != "" {
		t.Errorf("PREFERRED hostname = %q, want none (the reserved node is REQUIRED)", got)
	}
}

// TestAdmitWithHeadroomSkipsCordonedNode: steering never requires or reserves a cordoned
// node, even the roomiest.
func TestAdmitWithHeadroomSkipsCordonedNode(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("cordoned", fourCorePerNUMATopo())
	topo.Set("open", fourCorePerNUMATopo())
	// "cordoned" would win under spread if the cordon filter were missing.
	nrtCordoned := nrtObj("cordoned")
	nrtOpen := nrtObj("open",
		nrtZone(testContainerZoneType, "inference/x/x", "balloon-span2[0]", "0,1,4,5"))
	m := newMutatorWithTopo(topo,
		withCordon(topoNode("cordoned", 8, 2)), topoNode("open", 8, 2),
		nrtCordoned, nrtOpen)
	m.Reservations = NewReservationStore(0)

	pod := unscheduledPod("p", "2")
	verdict := m.admitWithHeadroom(ctx, pod, 2, 2, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawLive || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=true", verdict)
	}
	req := affinityRequiredValues(pod)
	if len(req) != 1 || req[0] != "open" {
		t.Errorf("REQUIRED hostnames = %v, want [open] (cordoned node must be excluded)", req)
	}
	if got := affinityPreferredHostname(pod); got != "" {
		t.Errorf("PREFERRED hostname = %q, want none (the reserved node is REQUIRED)", got)
	}
	if r := reservedCores(m.Reservations, "cordoned"); len(r) != 0 {
		t.Errorf("cordoned node must not receive a reservation, got %v", r)
	}
}

// TestAdmitWithHeadroomAllCordonedFallsBack: with every node cordoned, sawLive is false
// and no affinity is injected.
func TestAdmitWithHeadroomAllCordonedFallsBack(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("only", simpleTopo())
	m := newMutatorWithTopo(topo, withCordon(topoNode("only", 4, 2)), nrtObj("only"))
	m.Reservations = NewReservationStore(0)

	pod := unscheduledPod("p", "1")
	verdict := m.admitWithHeadroom(ctx, pod, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if verdict.sawLive {
		t.Errorf("every candidate cordoned → sawLive must be false, got %+v", verdict)
	}
	if pod.Spec.Affinity != nil {
		t.Errorf("no candidate → must not inject affinity, got %+v", pod.Spec.Affinity)
	}
}

// TestAdmitWithHeadroomBurstSplits: two back-to-back pods that each fill a node land on
// different nodes because of the first one's reservation.
func TestAdmitWithHeadroomBurstSplits(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	topo.Set("b", simpleTopo())

	nrtA := nrtObj("a")
	nrtB := nrtObj("b")
	m := newMutatorWithTopo(topo, topoNode("a", 4, 2), topoNode("b", 4, 2), nrtA, nrtB)
	m.Reservations = NewReservationStore(0)

	p1 := unscheduledPod("p1", "2")
	p2 := unscheduledPod("p2", "2")
	v1 := m.admitWithHeadroom(ctx, p1, 2, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	v2 := m.admitWithHeadroom(ctx, p2, 2, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !v1.fits || !v2.fits {
		t.Fatalf("both burst pods should fit (one per node): v1=%+v v2=%+v", v1, v2)
	}

	req1 := affinityRequiredValues(p1)
	req2 := affinityRequiredValues(p2)
	if len(req1) != 1 || len(req2) != 1 {
		t.Fatalf("expected a single REQUIRED node per pod, got %v and %v", req1, req2)
	}
	if req1[0] == req2[0] {
		t.Errorf("burst pods steered to same node %q — reservation did not split the burst", req1[0])
	}
}

// TestAdmitWithHeadroomNoLiveDataFallback: without live data the affinity is untouched
// and sawLive is false.
func TestAdmitWithHeadroomNoLiveDataFallback(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache() // empty: no nodes cached
	m := newMutatorWithTopo(topo, topoNode("a", 4, 2), topoNode("b", 4, 2))
	m.Reservations = NewReservationStore(0)

	p := unscheduledPod("p", "2")
	verdict := m.admitWithHeadroom(ctx, p, 2, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if verdict.sawLive {
		t.Errorf("no cached topology → sawLive must be false, got %+v", verdict)
	}
	if p.Spec.Affinity != nil {
		t.Errorf("no live data must not add affinity, got %+v", p.Spec.Affinity)
	}
	if got := reservedCores(m.Reservations, "a"); len(got) != 0 {
		t.Errorf("no live data must not reserve, got %v", got)
	}
}

// TestAdmitWithHeadroomNoFittingNodeVerdict: live data with no fitting node returns
// sawLive=true, fits=false and a detail, without affinity or reservation.
func TestAdmitWithHeadroomNoFittingNodeVerdict(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("a", simpleTopo())

	nrt := nrtObj("a", nrtZone(testContainerZoneType, "inference/x/x", "balloon-span2[0]", "0-3"))
	m := newMutatorWithTopo(topo, topoNode("a", 4, 2), nrt)
	m.Reservations = NewReservationStore(0)

	p := unscheduledPod("p", "2")
	verdict := m.admitWithHeadroom(ctx, p, 2, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawLive {
		t.Errorf("live NRT present → sawLive must be true, got %+v", verdict)
	}
	if verdict.fits {
		t.Errorf("no room on any node → fits must be false, got %+v", verdict)
	}
	if verdict.detail == "" {
		t.Error("verdict.detail should carry a human-readable reason for the caller's event")
	}
	if p.Spec.Affinity != nil {
		t.Errorf("no fitting node must not add affinity, got %+v", p.Spec.Affinity)
	}
	if got := reservedCores(m.Reservations, "a"); len(got) != 0 {
		t.Errorf("no fitting node must not reserve, got %v", got)
	}
}

// TestAdmitWithHeadroomNilStoreNoOp: with Reservations unset, admitWithHeadroom returns
// an empty verdict.
func TestAdmitWithHeadroomNilStoreNoOp(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	m := newMutatorWithTopo(topo, topoNode("a", 4, 2), nrtObj("a"))

	p := unscheduledPod("p", "1")
	verdict := m.admitWithHeadroom(ctx, p, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if verdict.sawLive || verdict.fits {
		t.Errorf("nil Reservations must return empty verdict, got %+v", verdict)
	}
	if p.Spec.Affinity != nil {
		t.Errorf("nil Reservations must be a no-op, got affinity %+v", p.Spec.Affinity)
	}
}

// TestLiveFits exercises the live per-NUMA computation in both unit modes and its fallbacks.
func TestLiveFits(t *testing.T) {
	ctx := context.Background()
	// 2 threads per core; cpu0 busy makes core0 busy on NUMA0.
	topo := topology.NewCache()
	topo.Set("n1", map[int]topology.CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0}, 1: {CPU: 1, NUMA: 0, Core: 0},
		2: {CPU: 2, NUMA: 0, Core: 2}, 3: {CPU: 3, NUMA: 0, Core: 2},
		4: {CPU: 4, NUMA: 1, Core: 4}, 5: {CPU: 5, NUMA: 1, Core: 4},
		6: {CPU: 6, NUMA: 1, Core: 6}, 7: {CPU: 7, NUMA: 1, Core: 6},
	})
	nrt := nrtObj("n1", nrtZone(testBalloonZoneType, "reserved[0]", "", "0"))
	m := newMutatorWithTopo(topo, nrt)

	// physical-cores: NUMA0 has 1 free core, NUMA1 has 2.
	if fits, free, ok := m.liveFits(ctx, "n1", 2, 1, "physical-cores", false, nil); !ok || !fits || free != 2 {
		t.Errorf("physical span1 req2 => (fits=%v,free=%d,ok=%v), want (true,2,true)", fits, free, ok)
	}
	// span 2: share ceil(2/2)=1 fits the least-free NUMA's 1.
	if fits, free, ok := m.liveFits(ctx, "n1", 2, 2, "physical-cores", false, nil); !ok || !fits || free != 1 {
		t.Errorf("physical span2 req2 => (fits=%v,free=%d,ok=%v), want (true,1,true)", fits, free, ok)
	}
	// span 2, 3 cores: share 2 > 1, no fit.
	if fits, free, ok := m.liveFits(ctx, "n1", 3, 2, "physical-cores", false, nil); !ok || fits || free != 1 {
		t.Errorf("physical span2 req3 => (fits=%v,free=%d,ok=%v), want (false,1,true)", fits, free, ok)
	}
	// hyperthreads: NUMA0 has 3 free logical cpus, NUMA1 has 4.
	if fits, free, ok := m.liveFits(ctx, "n1", 4, 1, "hyperthreads", false, nil); !ok || !fits || free != 4 {
		t.Errorf("ht span1 req4 => (fits=%v,free=%d,ok=%v), want (true,4,true)", fits, free, ok)
	}

	if _, _, ok := m.liveFits(ctx, "ghost", 1, 1, "physical-cores", false, nil); ok {
		t.Error("uncached node should signal ok=false")
	}

	if _, _, ok := (&PodMutator{Client: m.Client}).liveFits(ctx, "n1", 1, 1, "physical-cores", false, nil); ok {
		t.Error("nil topo cache should signal ok=false")
	}
}

// TestAdmitWithHeadroomBurstRejectsOversubscription: three concurrent admissions with
// room for two yield exactly two fits and one no-fit (sawLive=true). Run with -race.
func TestAdmitWithHeadroomBurstRejectsOversubscription(t *testing.T) {
	ctx := context.Background()
	// A span-2 4-core pod fills a simpleTopo node, so two nodes hold two such pods.
	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	topo.Set("b", simpleTopo())
	m := newMutatorWithTopo(topo, topoNode("a", 4, 2), topoNode("b", 4, 2), nrtObj("a"), nrtObj("b"))
	m.Reservations = NewReservationStore(0)

	const N = 3
	pods := make([]*corev1.Pod, N)
	verdicts := make([]headroomVerdict, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		pods[i] = unscheduledPod("p"+strconv.Itoa(i), "4")
		go func() {
			defer wg.Done()
			verdicts[i] = m.admitWithHeadroom(ctx, pods[i], 4, 2, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
		}()
	}
	wg.Wait()

	for i, v := range verdicts {
		if !v.sawLive {
			t.Errorf("pod %d: sawLive=false, expected all three admissions to see live data (verdict=%+v)", i, v)
		}
	}

	fitCount, rejectCount := 0, 0
	for i, v := range verdicts {
		switch v.fits {
		case true:
			fitCount++
		case false:
			rejectCount++
			if v.detail == "" {
				t.Errorf("pod %d: rejected but detail is empty — caller needs a message for CapacityCheckFailed event", i)
			}
		}
	}
	if fitCount != 2 || rejectCount != 1 {
		t.Errorf("burst = (%d fit, %d reject), want (2 fit, 1 reject) — reservation did not oversubscribe-reject the burst", fitCount, rejectCount)
	}
}

// TestAdmitWithHeadroomNRTReadsOutsideLock verifies the NRT reads run outside the
// ReservationStore lock: TryLock succeeds from inside the intercepted Get.
func TestAdmitWithHeadroomNRTReadsOutsideLock(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	topo.Set("b", simpleTopo())
	topo.SetMemory("a", map[int]int64{0: 250 * gib, 1: 250 * gib})
	topo.SetMemory("b", map[int]int64{0: 250 * gib, 1: 250 * gib})

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})

	store := NewReservationStore(0)
	var nrtGets, lockFreeGets int32
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		withMemoryFloor(topoNode("a", 4, 2), 250*gib),
		withMemoryFloor(topoNode("b", 4, 2), 250*gib),
		nrtObj("a"), nrtObj("b"),
	).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			// Only the NRT reads matter here.
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == nrtpkg.GVK {
				atomic.AddInt32(&nrtGets, 1)

				if store.mu.TryLock() {
					atomic.AddInt32(&lockFreeGets, 1)
					store.mu.Unlock()
				}
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	m := &PodMutator{Client: c, TopoCache: topo, Reservations: store}

	// A memory request makes the memory path read the NRT too.
	pod := unscheduledPod("p", "2")
	v := m.admitWithHeadroom(ctx, pod, 2, 2, 50*gib, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !v.sawLive || !v.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
	}
	if atomic.LoadInt32(&nrtGets) == 0 {
		t.Fatal("expected at least one NRT Get — the read phase did not run")
	}
	if got, total := atomic.LoadInt32(&lockFreeGets), atomic.LoadInt32(&nrtGets); got != total {
		t.Errorf("NRT reads under the lock: %d of %d Gets were NOT lock-free — the critical section still spans NRT reads", total-got, total)
	}
}

// TestAdmitWithHeadroomConcurrentNoDoubleAdmission: under many racing admissions, exactly
// capacity-many pods fit and no NUMA is over-reserved. Run with -race.
func TestAdmitWithHeadroomConcurrentNoDoubleAdmission(t *testing.T) {
	ctx := context.Background()
	// One span-2 4-core pod fills a node, so 4 nodes fit exactly 4.
	const nodes = 4
	topo := topology.NewCache()
	objs := make([]client.Object, 0, 2*nodes)
	for i := 0; i < nodes; i++ {
		name := "n" + strconv.Itoa(i)
		topo.Set(name, simpleTopo())
		objs = append(objs, topoNode(name, 4, 2), nrtObj(name))
	}
	m := newMutatorWithTopo(topo, objs...)
	m.Reservations = NewReservationStore(0)

	const N = 40 // 10× oversubscribed
	verdicts := make([]headroomVerdict, N)
	pods := make([]*corev1.Pod, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		pods[i] = unscheduledPod("p"+strconv.Itoa(i), "4")
		go func() {
			defer wg.Done()
			verdicts[i] = m.admitWithHeadroom(ctx, pods[i], 4, 2, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
		}()
	}
	wg.Wait()

	fits := 0
	for _, v := range verdicts {
		if !v.sawLive {
			t.Fatalf("every admission should see live data, got %+v", v)
		}
		if v.fits {
			fits++
		}
	}

	if fits != nodes {
		t.Errorf("fits = %d, want %d (one per node) — double-admission or lost fit under concurrency", fits, nodes)
	}
	// No NUMA may hold more than its 2 free cores.
	for i := 0; i < nodes; i++ {
		name := "n" + strconv.Itoa(i)
		for numa, c := range reservedCores(m.Reservations, name) {
			if c > 2 {
				t.Errorf("node %s NUMA %d reserved %d cores, exceeds capacity 2 — headroom double-reserved", name, numa, c)
			}
		}
	}
}

// TestAdmitWithHeadroomReservesPerNUMAShare: a span-N reservation records ceil(cores/span)
// per NUMA, never the full request.
func TestAdmitWithHeadroomReservesPerNUMAShare(t *testing.T) {
	ctx := context.Background()

	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	m := newMutatorWithTopo(topo, topoNode("a", 4, 2), nrtObj("a"))
	m.Reservations = NewReservationStore(0)

	p := unscheduledPod("p", "3")
	v := m.admitWithHeadroom(ctx, p, 3, 2, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !v.fits {
		t.Fatalf("span2 req3 (share 2 ≤ 2 free/NUMA) should fit, verdict=%+v", v)
	}
	reserved := reservedCores(m.Reservations, "a")

	if len(reserved) != 2 {
		t.Fatalf("expected reservation on 2 NUMAs, got %v", reserved)
	}
	for numa, cores := range reserved {
		if cores != 2 {
			t.Errorf("NUMA %d reserved %d cores, want 2 (ceil(3/2)); full-request-per-NUMA would be 3 — inflation", numa, cores)
		}
	}
}

// TestAdmitWithHeadroomDryRunSkipsReservation: a dry-run admission scans and reports a fit
// but reserves nothing (sideEffects: None); a real one still reserves.
func TestAdmitWithHeadroomDryRunSkipsReservation(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	m := newMutatorWithTopo(topo, topoNode("a", 4, 2), nrtObj("a"))
	m.Reservations = NewReservationStore(0)

	pDry := unscheduledPod("dry", "3")
	vDry := m.admitWithHeadroom(ctx, pDry, 3, 2, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{dryRun: true}, logr.Discard())
	if !vDry.sawLive || !vDry.fits {
		t.Fatalf("dry-run verdict = %+v, want sawLive=true fits=true (scan must still run for an accurate preview)", vDry)
	}
	if got := reservedCores(m.Reservations, "a"); len(got) != 0 {
		t.Fatalf("dry-run consumed a reservation slot: reservedCores=%v, want none (sideEffects: None violated)", got)
	}

	pReal := unscheduledPod("real", "3")
	vReal := m.admitWithHeadroom(ctx, pReal, 3, 2, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !vReal.fits {
		t.Fatalf("non-dry-run verdict = %+v, want fits=true", vReal)
	}
	if got := reservedCores(m.Reservations, "a"); len(got) == 0 {
		t.Fatalf("non-dry-run did not reserve: reservedCores=%v, want a per-NUMA reservation", got)
	}
}

// TestAdmitWithHeadroomIncrementsReservationMetric: a real reservation bumps
// reservations_recorded_total for the node by one; a dry-run leaves it unchanged.
func TestAdmitWithHeadroomIncrementsReservationMetric(t *testing.T) {
	ctx := context.Background()

	// The CounterVec is process-wide, so the test uses its own node label and resets it.
	const node = "ha-metric-node"
	metrics.ReservationsRecorded.DeleteLabelValues(node)
	t.Cleanup(func() { metrics.ReservationsRecorded.DeleteLabelValues(node) })

	topo := topology.NewCache()
	topo.Set(node, simpleTopo())
	m := newMutatorWithTopo(topo, topoNode(node, 4, 2), nrtObj(node))
	m.Reservations = NewReservationStore(0)

	before := testutil.ToFloat64(metrics.ReservationsRecorded.WithLabelValues(node))

	pDry := unscheduledPod("dry", "3")
	if v := m.admitWithHeadroom(ctx, pDry, 3, 2, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{dryRun: true}, logr.Discard()); !v.fits {
		t.Fatalf("dry-run verdict = %+v, want fits=true", v)
	}
	if got := testutil.ToFloat64(metrics.ReservationsRecorded.WithLabelValues(node)); got != before {
		t.Fatalf("reservations_recorded_total{node=%s} = %v after dry-run, want %v unchanged (sideEffects: None)", node, got, before)
	}

	pReal := unscheduledPod("real", "3")
	if v := m.admitWithHeadroom(ctx, pReal, 3, 2, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard()); !v.fits {
		t.Fatalf("real admission verdict = %+v, want fits=true", v)
	}
	if got := testutil.ToFloat64(metrics.ReservationsRecorded.WithLabelValues(node)); got != before+1 {
		t.Fatalf("reservations_recorded_total{node=%s} = %v, want %v (one reservation recorded)", node, got, before+1)
	}
}

// wideTopo is a 4-CPU node of 4 single-core NUMA nodes, hosting span up to 4.
func wideTopo() map[int]topology.CPUInfo {
	return map[int]topology.CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0},
		1: {CPU: 1, NUMA: 1, Core: 1},
		2: {CPU: 2, NUMA: 2, Core: 2},
		3: {CPU: 3, NUMA: 3, Core: 3},
	}
}

// TestAdmitWithHeadroomSkipsNodesBelowSpan: a span-4 pod lands only on the node whose
// max-span hosts it, even when a narrower node has more room.
func TestAdmitWithHeadroomSkipsNodesBelowSpan(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("wide", wideTopo())     // 4 NUMA → max-span 4
	topo.Set("narrow", simpleTopo()) // 2 NUMA → max-span 2
	m := newMutatorWithTopo(topo,
		withMaxSpan(topoNode("wide", 4, 4), 4), nrtObj("wide"),
		withMaxSpan(topoNode("narrow", 4, 2), 2), nrtObj("narrow"),
	)
	m.Reservations = NewReservationStore(0)

	pod := unscheduledPod("p", "1")
	verdict := m.admitWithHeadroom(ctx, pod, 1, 4, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawLive || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=true (wide node fits span 4)", verdict)
	}
	req := affinityRequiredValues(pod)
	if len(req) != 1 || req[0] != "wide" {
		t.Errorf("REQUIRED hostnames = %v, want [wide] — narrow node must be excluded for span 4", req)
	}

	if r := reservedCores(m.Reservations, "narrow"); len(r) != 0 {
		t.Errorf("narrow node must not be reserved for a span-4 pod, got %v", r)
	}
}

// TestAdmitWithHeadroomNoNodeSupportsSpan: when no node's max-span hosts the span,
// sawLive stays false and no fit is fabricated.
func TestAdmitWithHeadroomNoNodeSupportsSpan(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("narrow", simpleTopo()) // 2 NUMA → max-span 2
	m := newMutatorWithTopo(topo, withMaxSpan(topoNode("narrow", 4, 2), 2), nrtObj("narrow"))
	m.Reservations = NewReservationStore(0)

	pod := unscheduledPod("p", "1")
	verdict := m.admitWithHeadroom(ctx, pod, 1, 4, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if verdict.sawLive || verdict.fits {
		t.Errorf("no node supports span 4 → verdict must be empty (fallback), got %+v", verdict)
	}
	if pod.Spec.Affinity != nil {
		t.Errorf("no candidate node must not add affinity, got %+v", pod.Spec.Affinity)
	}
}

// TestNodeSupportsSpan: span<=1 fits anything; span>1 needs a parseable label >= span.
func TestNodeSupportsSpan(t *testing.T) {
	lbl := func(v string) map[string]string {
		if v == "" {
			return map[string]string{}
		}
		return map[string]string{controller.LabelMaxSpan: v}
	}
	cases := []struct {
		name   string
		labels map[string]string
		span   int
		want   bool
	}{
		{"span1 fits label-less", lbl(""), 1, true},
		{"span1 fits any", lbl("2"), 1, true},
		{"span2 needs label — missing", lbl(""), 2, false},
		{"span2 needs label — garbage", lbl("wide"), 2, false},
		{"span2 on max-span2 ok", lbl("2"), 2, true},
		{"span4 on max-span2 no", lbl("2"), 4, false},
		{"span4 on max-span8 ok", lbl("8"), 4, true},
		{"span2 on zero label no", lbl("0"), 2, false},
		// A bogus label still floors to span 1.
		{"span1 on zero label — floors to 1", lbl("0"), 1, true},
		{"span1 on negative label — floors to 1", lbl("-3"), 1, true},
	}
	for _, c := range cases {
		if got := nodeSupportsSpan(c.labels, c.span); got != c.want {
			t.Errorf("%s: nodeSupportsSpan(%v, %d) = %v, want %v", c.name, c.labels, c.span, got, c.want)
		}
	}
}

// TestAdmitWithHeadroomMissingMaxSpanLabelNotCandidate: a node without a max-span label is
// not a candidate for span > 1, matching the injected `max-span Gt` affinity.
func TestAdmitWithHeadroomMissingMaxSpanLabelNotCandidate(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("nolabel", wideTopo()) // physically 4 NUMA, but max-span label stripped
	m := newMutatorWithTopo(topo, withoutMaxSpan(topoNode("nolabel", 4, 4)), nrtObj("nolabel"))
	m.Reservations = NewReservationStore(0)

	pod := unscheduledPod("p", "1")
	verdict := m.admitWithHeadroom(ctx, pod, 1, 2, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if verdict.sawLive || verdict.fits {
		t.Errorf("label-less node must not be a span-2 candidate → empty verdict, got %+v", verdict)
	}
	if pod.Spec.Affinity != nil {
		t.Errorf("no candidate → no hostname affinity, got %+v", pod.Spec.Affinity)
	}
	if r := reservedCores(m.Reservations, "nolabel"); len(r) != 0 {
		t.Errorf("label-less node must not be reserved, got %v", r)
	}

	// The same node is a candidate for span 1.
	pod1 := unscheduledPod("p1", "1")
	v1 := m.admitWithHeadroom(ctx, pod1, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !v1.sawLive || !v1.fits {
		t.Errorf("span-1 pod must still fit a label-less node, got %+v", v1)
	}
}

// TestInjectMaxSpanAffinity checks the Gt term keeps a pod off nodes whose max-span is
// below its span.
func TestInjectMaxSpanAffinity(t *testing.T) {
	pod := &corev1.Pod{}
	injectMaxSpanAffinity(pod, 4)

	na := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if na == nil || len(na.NodeSelectorTerms) != 1 {
		t.Fatalf("expected one required nodeSelectorTerm, got %+v", na)
	}
	me := na.NodeSelectorTerms[0].MatchExpressions
	if len(me) != 1 {
		t.Fatalf("expected one match expression, got %+v", me)
	}
	got := me[0]
	if got.Key != controller.LabelMaxSpan || got.Operator != corev1.NodeSelectorOpGt {
		t.Errorf("term = {%s %s %v}, want {%s Gt [3]}", got.Key, got.Operator, got.Values, controller.LabelMaxSpan)
	}

	if len(got.Values) != 1 || got.Values[0] != "3" {
		t.Errorf("Gt value = %v, want [3] (max-span >= span)", got.Values)
	}

	pod = &corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: "disktype", Operator: corev1.NodeSelectorOpIn, Values: []string{"ssd"}},
			}}},
		},
	}}}}
	injectMaxSpanAffinity(pod, 2)
	if !hasRequiredKey(pod, "disktype") || !hasRequiredKey(pod, controller.LabelMaxSpan) {
		t.Error("existing required term must be preserved AND our max-span requirement added")
	}
}

// affinityAdmitsNode evaluates the pod's required nodeAffinity against node labels with
// the scheduler's helper.
func affinityAdmitsNode(pod *corev1.Pod, labels map[string]string) bool {
	return nodeMatchesPodRequirements(podRequiredNodeMatcher(pod),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: labels}})
}

// TestNodeSupportsSpanMirrorsInjectedAffinity: candidacy keeps exactly the nodes the
// injected `max-span Gt (span-1)` term accepts.
func TestNodeSupportsSpanMirrorsInjectedAffinity(t *testing.T) {
	for _, span := range []int{2, 4, 8} {
		for _, v := range []string{
			"", "0", "1", "2", "4", "8", "-3", "wide", "+4", "4.0", " 4", "004",
			"9223372036854775807", "9223372036854775808",
		} {
			labels := map[string]string{}
			if v != "" {
				labels[controller.LabelMaxSpan] = v
			}
			pod := &corev1.Pod{}
			injectMaxSpanAffinity(pod, span)
			if got, want := affinityAdmitsNode(pod, labels), nodeSupportsSpan(labels, span); got != want {
				t.Errorf("span %d, max-span %q: candidacy says %v, the injected affinity says %v",
					span, v, want, got)
			}
		}
	}
}

// TestNodeHasFeaturesMirrorsInjectedAffinity: candidacy accepts a require-mode feature
// label only when it reads "true".
func TestNodeHasFeaturesMirrorsInjectedAffinity(t *testing.T) {
	const key = "feature.node.kubernetes.io/cpu-cpuid.AMX"
	features := map[string]cpopv1alpha1.NodeFeatureMode{key: cpopv1alpha1.NodeFeatureRequire}
	for _, v := range []string{"", "true", "false", "0", "1", "yes", "TRUE"} {
		labels := map[string]string{}
		if v != "" {
			labels[key] = v
		}
		pod := &corev1.Pod{}
		injectNodeAffinity(pod, false, features)
		got := affinityAdmitsNode(pod, labels)
		want := nodeHasFeatures(labels, requiredFeatureLabels(features))
		if got != want {
			t.Errorf("feature label %q: candidacy says %v, the injected affinity says %v", v, want, got)
		}
	}
}

// TestSpanOneInjectsNoMaxSpanTerm: a span-1 pod gets no max-span term, which a label-less
// node would fail.
func TestSpanOneInjectsNoMaxSpanTerm(t *testing.T) {
	m, _ := newHandleMutator(t, "warn", labeledNode("nolabel", true, ""))
	resp := admitPod(t, m, "p", "4", "1")
	if !resp.Allowed {
		t.Fatalf("span-1 pod on a label-less node must be admitted, got %q", resp.Result.Message)
	}
	raw, err := json.Marshal(resp.Patches)
	if err != nil {
		t.Fatalf("marshalling the patches: %v", err)
	}
	// Without the affinity injection the check below proves nothing.
	if !strings.Contains(string(raw), controller.LabelBalloonsEnabled) {
		t.Fatalf("pod was not steered, so a missing max-span term says nothing: %s", raw)
	}
	if strings.Contains(string(raw), controller.LabelMaxSpan) {
		t.Errorf("a span-1 pod carries a max-span requirement, excluding the label-less node it was steered to: %s", raw)
	}
}

// TestCreateInitContainerPollsEndpointNoKubectl: the init container runs the manager's
// wait-for-balloons subcommand with the node from the downward API, no kubectl or shell.
func TestCreateInitContainerPollsEndpointNoKubectl(t *testing.T) {
	t.Setenv("WAIT_TIMEOUT", "120")

	ic := createInitContainer("http://mgr-balloons-ready.op.svc:8082", testInitImage)

	if ic.Name != defaultInitContainerName {
		t.Errorf("name = %q, want %q", ic.Name, defaultInitContainerName)
	}

	if ic.Image != testInitImage {
		t.Errorf("image = %q, want the supplied %q", ic.Image, testInitImage)
	}
	if len(ic.Command) != 1 || ic.Command[0] != "/manager" {
		t.Errorf("command = %v, want [/manager]", ic.Command)
	}

	joined := strings.Join(append(ic.Command, ic.Args...), " ")
	if !strings.Contains(joined, readiness.SubcommandName) {
		t.Errorf("args %v do not invoke the %q subcommand", ic.Args, readiness.SubcommandName)
	}
	for _, banned := range []string{"kubectl", "/bin/sh", "/bin/bash"} {
		if strings.Contains(joined, banned) {
			t.Errorf("init container invokes %q — it must run neither a shell nor kubectl (%q)", banned, joined)
		}
	}
	if !strings.Contains(joined, "http://mgr-balloons-ready.op.svc:8082") {
		t.Errorf("endpoint not wired into args: %q", joined)
	}
	if !strings.Contains(joined, "--timeout=120s") {
		t.Errorf("timeout not wired into args: %q", joined)
	}

	if !strings.Contains(joined, "$(NODE_NAME)") {
		t.Errorf("node not sourced from the downward API env var: %q", joined)
	}
	var hasNodeEnv bool
	for _, e := range ic.Env {
		if e.Name == "NODE_NAME" && e.ValueFrom != nil && e.ValueFrom.FieldRef != nil &&
			e.ValueFrom.FieldRef.FieldPath == "spec.nodeName" {
			hasNodeEnv = true
		}
	}
	if !hasNodeEnv {
		t.Error("NODE_NAME env must come from fieldRef spec.nodeName")
	}

	sc := ic.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be false")
	}
	if sc == nil || sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Error("all capabilities must be dropped")
	}
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("RunAsNonRoot must be true")
	}
	// RunAsUser stays nil: a hard-coded UID breaks OpenShift's restricted-v2 SCC.
	if sc != nil && sc.RunAsUser != nil {
		t.Errorf("RunAsUser must be unset for OpenShift restricted-v2 compatibility, got %d", *sc.RunAsUser)
	}
}

// TestCreateInitContainerWaitTimeout: WAIT_TIMEOUT accepts bare seconds ("s" appended) or a
// full Go duration verbatim.
func TestCreateInitContainerWaitTimeout(t *testing.T) {
	cases := map[string]string{
		"120":   "--timeout=120s",
		"90":    "--timeout=90s",
		"5m":    "--timeout=5m",
		"120s":  "--timeout=120s",
		"2h30m": "--timeout=2h30m",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			t.Setenv("WAIT_TIMEOUT", in)
			ic := createInitContainer("http://mgr-balloons-ready.op.svc:8082", testInitImage)
			joined := strings.Join(ic.Args, " ")
			if !strings.Contains(joined, want) {
				t.Errorf("WAIT_TIMEOUT=%q → args %q, want %q", in, joined, want)
			}
		})
	}
}
