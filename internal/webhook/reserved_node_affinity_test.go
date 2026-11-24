// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/controller"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// preferAMX adds the weight-100 preferred term `nodeFeatures: amx: prefer` produces.
func preferAMX(pod *corev1.Pod) *corev1.Pod {
	injectNodeAffinity(pod, false, map[string]cpopv1alpha1.NodeFeatureMode{
		controller.LabelAMX: cpopv1alpha1.NodeFeaturePrefer,
	})
	return pod
}

// twoNodeFleet is a live-data fleet of "spacious" (empty) and "packed" (2 of 4 cores
// busy per NUMA). Spread ranks "spacious" first while the name tie-break would pick
// "packed", so asserting "spacious" measures room. "packed" carries the AMX label,
// inert unless a test passes it as a required feature.
func twoNodeFleet() *PodMutator {
	return twoNodeFleetWith(nil)
}

// twoNodeFleetWith builds twoNodeFleet, letting the caller adjust either node first.
func twoNodeFleetWith(decorate func(spacious, packed *corev1.Node)) *PodMutator {
	topo := topology.NewCache()
	topo.Set("spacious", fourCorePerNUMATopo())
	topo.Set("packed", fourCorePerNUMATopo())
	packed := topoNode("packed", 8, 2)
	packed.Labels[controller.LabelAMX] = "true"
	packed.Labels[hostnameLabel] = "packed"
	spacious := topoNode("spacious", 8, 2)
	spacious.Labels[hostnameLabel] = "spacious"
	if decorate != nil {
		decorate(spacious, packed)
	}
	m := newMutatorWithTopo(topo,
		spacious, packed,
		nrtObj("spacious"),
		nrtObj("packed", nrtZone(testContainerZoneType, "inference/x/x", "balloon-span2[0]", "0,1,4,5")))
	m.Reservations = NewReservationStore(0)
	return m
}

// TestAdmitWithHeadroomRequiresReservedNode proves a committed reservation REQUIRES the
// pod onto exactly that node (a soft term loses to a weight-100 feature preference),
// and the feature preference survives.
func TestAdmitWithHeadroomRequiresReservedNode(t *testing.T) {
	m := twoNodeFleet()

	pod := preferAMX(unscheduledPod("p", "1"))
	verdict := m.admitWithHeadroom(context.Background(), pod, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawLive || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=true", verdict)
	}

	if r := reservedCores(m.Reservations, "spacious"); len(r) == 0 {
		t.Fatalf("expected the reservation on 'spacious', got none")
	}
	if r := reservedCores(m.Reservations, "packed"); len(r) != 0 {
		t.Fatalf("'packed' must carry no reservation, got %v", r)
	}
	if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "spacious" {
		t.Errorf("REQUIRED hostnames = %v, want [spacious] — the pod must be pinned to the node the reservation was charged to", req)
	}
	if got := affinityPreferredHostname(pod); got != "" {
		t.Errorf("PREFERRED hostname = %q, want none — the committed node is REQUIRED, the weight-50 term is redundant", got)
	}
	// The AMX preference survives but cannot move the pod off the required node.
	if !hasPreferredFeature(pod, controller.LabelAMX, 100) {
		t.Errorf("the pod's weight-100 AMX preference must be preserved, got %+v",
			pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution)
	}
}

// TestAdmitWithHeadroomDryRunKeepsFittingSet proves a dry run, which reserves nothing,
// requires the whole fitting set and prefers the best node.
func TestAdmitWithHeadroomDryRunKeepsFittingSet(t *testing.T) {
	m := twoNodeFleet()

	pod := unscheduledPod("p", "1")
	verdict := m.admitWithHeadroom(context.Background(), pod, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{dryRun: true}, logr.Discard())
	if !verdict.fits {
		t.Fatalf("verdict = %+v, want fits=true", verdict)
	}
	req := affinityRequiredValues(pod)
	if len(req) != 2 || !slices.Contains(req, "spacious") || !slices.Contains(req, "packed") {
		t.Errorf("REQUIRED hostnames = %v, want both fitting nodes on a dry-run", req)
	}
	if got := affinityPreferredHostname(pod); got != "spacious" {
		t.Errorf("PREFERRED hostname = %q, want spacious (the local top choice)", got)
	}
	if r := reservedCores(m.Reservations, "spacious"); len(r) != 0 {
		t.Errorf("a dry-run must reserve nothing, got %v", r)
	}
}

// TestAdmitStaticHeadroomRequiresReservedNode is the cold-start twin: the committed node
// is the single required hostname.
func TestAdmitStaticHeadroomRequiresReservedNode(t *testing.T) {
	m, _ := coldStartMutator(t, "enforce", topoNode("n1", 8, 2), topoNode("n2", 8, 2))

	pod := preferAMX(unscheduledPod("p", "2"))
	verdict := m.admitStaticHeadroom(context.Background(), pod, 2, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawStatic || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawStatic=true fits=true", verdict)
	}
	req := affinityRequiredValues(pod)
	if len(req) != 1 {
		t.Fatalf("REQUIRED hostnames = %v, want exactly the reserved node", req)
	}
	if r := reservedCores(m.Reservations, req[0]); len(r) == 0 {
		t.Errorf("pod pinned to %q but that node carries no reservation", req[0])
	}
	if got := affinityPreferredHostname(pod); got != "" {
		t.Errorf("PREFERRED hostname = %q, want none", got)
	}
}

// TestBurstReservationsPinToTheirNode proves each of K admissions is pinned to the node
// its own reservation was charged to, the split is even, and admission K+1 finds no room.
func TestBurstReservationsPinToTheirNode(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	topo.Set("b", simpleTopo())
	m := newMutatorWithTopo(topo, topoNode("a", 4, 2), topoNode("b", 4, 2), nrtObj("a"), nrtObj("b"))
	m.Reservations = NewReservationStore(0)

	// Every pod carries a weight-100 feature preference that must not pull it off its node.
	pinned := map[string]int{}
	for i := 0; i < 4; i++ {
		pod := preferAMX(unscheduledPod("p", "2"))
		v := m.admitWithHeadroom(ctx, pod, 2, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
		if !v.fits {
			t.Fatalf("burst pod %d must fit (2 nodes × 2 NUMAs): verdict = %+v", i, v)
		}
		req := affinityRequiredValues(pod)
		if len(req) != 1 {
			t.Fatalf("burst pod %d REQUIRED = %v, want exactly one node", i, req)
		}
		pinned[req[0]]++
	}

	// Each node accounts exactly for its pinned pods: one per NUMA, 2 cores each.
	for _, node := range []string{"a", "b"} {
		if pinned[node] != 2 {
			t.Errorf("%d of 4 burst pods pinned to %q, want 2 — the reservations did not split the burst", pinned[node], node)
		}
		r := reservedCores(m.Reservations, node)
		if len(r) != pinned[node] {
			t.Errorf("%q has reservations on %d NUMAs (%v) but %d pods were pinned to it", node, len(r), r, pinned[node])
		}
		for numa, cores := range r {
			if cores != 2 {
				t.Errorf("%q NUMA %d reserved %d cores, want 2 (one pod's whole per-NUMA share)", node, numa, cores)
			}
		}
	}

	extra := preferAMX(unscheduledPod("p", "2"))
	v := m.admitWithHeadroom(ctx, extra, 2, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if v.fits {
		t.Errorf("admission K+1 = fits, want no-fit: both nodes are fully reserved by the burst (verdict %+v)", v)
	}
	if extra.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		t.Errorf("a no-fit admission must inject no hostname affinity, got %+v",
			extra.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
	}
}

// TestInjectReservedNodeAffinityPreservesExistingTerms proves the hostname requirement is
// AND-ed into every existing OR-ed NodeSelectorTerm and other affinity is untouched.
func TestInjectReservedNodeAffinityPreservesExistingTerms(t *testing.T) {
	pod := unscheduledPod("p", "1")

	pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"east"},
				}}},
				{MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"west"},
				}}},
			},
		},
	}}
	preferAMX(pod)

	injectReservedNodeAffinity(pod, "node-7")

	terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 2 {
		t.Fatalf("got %d required terms, want the 2 pre-existing ones", len(terms))
	}
	for i, term := range terms {
		if len(term.MatchExpressions) != 2 {
			t.Fatalf("term %d = %+v, want the original expression plus the hostname requirement", i, term.MatchExpressions)
		}
		last := term.MatchExpressions[1]
		if last.Key != hostnameLabel || len(last.Values) != 1 || last.Values[0] != "node-7" {
			t.Errorf("term %d hostname requirement = %+v, want %s In [node-7]", i, last, hostnameLabel)
		}
		if term.MatchExpressions[0].Key != "zone" {
			t.Errorf("term %d dropped its original expression: %+v", i, term.MatchExpressions)
		}
	}
	if !hasPreferredFeature(pod, controller.LabelAMX, 100) {
		t.Errorf("the pre-existing preferred term must be untouched, got %+v",
			pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution)
	}
}

// hasPreferredFeature reports whether the pod prefers key at the given weight.
func hasPreferredFeature(pod *corev1.Pod, key string, weight int32) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return false
	}
	for _, p := range pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		if p.Weight != weight {
			continue
		}
		for _, e := range p.Preference.MatchExpressions {
			if e.Key == key {
				return true
			}
		}
	}
	return false
}

// TestAdmitStaticHeadroomSkipsCordonedNode is the cold-start twin of
// TestAdmitWithHeadroomSkipsCordonedNode: the roomiest node is cordoned and must be skipped.
func TestAdmitStaticHeadroomSkipsCordonedNode(t *testing.T) {
	m, _ := coldStartMutator(t, "enforce",
		withCordon(topoNode("cordoned", 16, 2)), topoNode("open", 8, 2))

	pod := unscheduledPod("p", "2")
	verdict := m.admitStaticHeadroom(context.Background(), pod, 2, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawStatic || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawStatic=true fits=true", verdict)
	}
	if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "open" {
		t.Errorf("REQUIRED hostnames = %v, want [open] (the cordoned node is roomier but unschedulable)", req)
	}
	if r := reservedCores(m.Reservations, "cordoned"); len(r) != 0 {
		t.Errorf("cordoned node must not receive a reservation, got %v", r)
	}
}

// TestAdmitWithHeadroomSkipsNodeMissingRequiredFeature proves a node without a
// require-mode feature label is skipped, since the pin would make the spec unsatisfiable.
func TestAdmitWithHeadroomSkipsNodeMissingRequiredFeature(t *testing.T) {
	m := twoNodeFleet() // "spacious" (roomier, no AMX) and "packed" (AMX)

	pod := unscheduledPod("p", "1")
	verdict := m.admitWithHeadroom(context.Background(), pod, 1, 1, 0, true, "", "physical-cores", packingSpread,
		[]string{controller.LabelAMX}, reservePolicy{}, logr.Discard())
	if !verdict.sawLive || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=true", verdict)
	}
	if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "packed" {
		t.Errorf("REQUIRED hostnames = %v, want [packed] (the only node carrying the required feature label)", req)
	}
	if r := reservedCores(m.Reservations, "spacious"); len(r) != 0 {
		t.Errorf("the unlabeled node must not receive a reservation, got %v", r)
	}
}

// TestAdmitWithHeadroomHonorsPodNodeSelector proves a node excluded by the pod's own
// nodeSelector is not a candidate.
func TestAdmitWithHeadroomHonorsPodNodeSelector(t *testing.T) {
	m := twoNodeFleet()

	pod := unscheduledPod("p", "1")

	pod.Spec.NodeSelector = map[string]string{hostnameLabel: "packed"}
	verdict := m.admitWithHeadroom(context.Background(), pod, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawLive || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=true", verdict)
	}
	if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "packed" {
		t.Errorf("REQUIRED hostnames = %v, want [packed] (the node the pod's nodeSelector allows)", req)
	}
	if r := reservedCores(m.Reservations, "spacious"); len(r) != 0 {
		t.Errorf("a node the pod's nodeSelector excludes must not receive a reservation, got %v", r)
	}
}

// TestAdmitWithHeadroomHonorsPodRequiredNodeAffinity proves a node rejected by the pod's
// own required nodeAffinity is not a candidate.
func TestAdmitWithHeadroomHonorsPodRequiredNodeAffinity(t *testing.T) {
	m := twoNodeFleet()

	pod := unscheduledPod("p", "1")
	pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key: controller.LabelAMX, Operator: corev1.NodeSelectorOpIn, Values: []string{"true"},
			}}}},
		},
	}}
	verdict := m.admitWithHeadroom(context.Background(), pod, 1, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !verdict.sawLive || !verdict.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=true", verdict)
	}
	if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "packed" {
		t.Errorf("REQUIRED hostnames = %v, want [packed] (the only node the pod's own nodeAffinity accepts)", req)
	}
	if r := reservedCores(m.Reservations, "spacious"); len(r) != 0 {
		t.Errorf("a node the pod's own nodeAffinity rejects must not receive a reservation, got %v", r)
	}
}

// TestAdmitStaticHeadroomAppliesPodAndFeatureCandidacy is the cold-start twin of the
// feature and nodeSelector candidacy tests.
func TestAdmitStaticHeadroomAppliesPodAndFeatureCandidacy(t *testing.T) {
	// "roomier" has twice the capacity, so spread ranks it first unless candidacy drops it.
	fleet := func(t *testing.T) *PodMutator {
		labeled := topoNode("labeled", 8, 2)
		labeled.Labels[controller.LabelAMX] = "true"
		labeled.Labels[hostnameLabel] = "labeled"
		roomier := topoNode("roomier", 32, 2)
		roomier.Labels[hostnameLabel] = "roomier"
		m, _ := coldStartMutator(t, "enforce", labeled, roomier)
		return m
	}

	t.Run("require-mode nodeFeatures", func(t *testing.T) {
		m := fleet(t)
		pod := unscheduledPod("p", "2")
		v := m.admitStaticHeadroom(context.Background(), pod, 2, 1, 0, true, "", "physical-cores", packingSpread,
			[]string{controller.LabelAMX}, reservePolicy{}, logr.Discard())
		if !v.sawStatic || !v.fits {
			t.Fatalf("verdict = %+v, want sawStatic=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "labeled" {
			t.Errorf("REQUIRED hostnames = %v, want [labeled] (the roomier node lacks the required feature)", req)
		}
		if r := reservedCores(m.Reservations, "roomier"); len(r) != 0 {
			t.Errorf("the unlabeled node must not receive a reservation, got %v", r)
		}
	})

	t.Run("pod nodeSelector", func(t *testing.T) {
		m := fleet(t)
		pod := unscheduledPod("p", "2")
		pod.Spec.NodeSelector = map[string]string{hostnameLabel: "labeled"}
		v := m.admitStaticHeadroom(context.Background(), pod, 2, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
		if !v.sawStatic || !v.fits {
			t.Fatalf("verdict = %+v, want sawStatic=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "labeled" {
			t.Errorf("REQUIRED hostnames = %v, want [labeled] (the node the pod's nodeSelector allows)", req)
		}
		if r := reservedCores(m.Reservations, "roomier"); len(r) != 0 {
			t.Errorf("a node the pod's nodeSelector excludes must not receive a reservation, got %v", r)
		}
	})
}

// TestHandleWiresRequireFeaturesIntoCandidacy proves nodeFeatures reach the headroom scan
// through handle(): the only AMX node is full and the roomier one lacks AMX, so enforce
// denies.
func TestHandleWiresRequireFeaturesIntoCandidacy(t *testing.T) {
	topo := topology.NewCache()
	topo.Set("amx-full", simpleTopo())
	topo.Set("plain-free", fourCorePerNUMATopo())
	amxFull := topoNode("amx-full", 4, 2)
	amxFull.Labels[controller.LabelAMX] = "true"
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		amxFull, topoNode("plain-free", 8, 2),
		// Every CPU on the AMX node is claimed; the plain node is empty.
		nrtObj("amx-full", nrtZone(testContainerZoneType, "inference/x/x", "balloon-span2[0]", "0-3")),
		nrtObj("plain-free"),
	).Build()
	m, _ := newHandleMutatorWithFeatures(t, "enforce",
		map[string]cpopv1alpha1.NodeFeatureMode{controller.LabelAMX: cpopv1alpha1.NodeFeatureRequire}, c, topo)

	resp := admitPod(t, m, "vllm-amx", "2", "1")
	if resp.Allowed {
		t.Error("pod was admitted: the only node carrying the required AMX label is full, so admission must deny rather than reserve on the unlabeled node")
	}
}

// TestRequiredFeatureLabels proves only require-mode entries become candidacy filters.
func TestRequiredFeatureLabels(t *testing.T) {
	got := requiredFeatureLabels(map[string]cpopv1alpha1.NodeFeatureMode{
		"vendor.io/gpu":     cpopv1alpha1.NodeFeatureRequire,
		"vendor.io/nic":     cpopv1alpha1.NodeFeatureRequire,
		controller.LabelAMX: cpopv1alpha1.NodeFeaturePrefer,
		"vendor.io/off":     cpopv1alpha1.NodeFeatureIgnore,
	})
	if len(got) != 2 || !slices.Contains(got, "vendor.io/gpu") || !slices.Contains(got, "vendor.io/nic") {
		t.Errorf("requiredFeatureLabels = %v, want the two require-mode keys only", got)
	}
	if got := requiredFeatureLabels(nil); len(got) != 0 {
		t.Errorf("requiredFeatureLabels(nil) = %v, want empty", got)
	}
}

// TestNoFitDetailReportsCandidateScope proves the no-fit event reports how many nodes were
// candidates and how many of those had live data, counted before the NRT read.
func TestNoFitDetailReportsCandidateScope(t *testing.T) {
	topo := topology.NewCache()
	topo.Set("chosen", simpleTopo())
	topo.Set("nodata", simpleTopo())
	topo.Set("other", simpleTopo())
	zoned := func(name, zone string) *corev1.Node {
		n := topoNode(name, 4, 2)
		n.Labels["topology.kubernetes.io/zone"] = zone
		return n
	}
	m := newMutatorWithTopo(topo,
		zoned("chosen", "a"), zoned("nodata", "a"), zoned("other", "b"),
		// "nodata" is a candidate without an NRT CR.
		nrtObj("chosen"))
	m.Reservations = NewReservationStore(0)

	pod := unscheduledPod("p", "4")
	pod.Spec.NodeSelector = map[string]string{"topology.kubernetes.io/zone": "a"}

	v := m.admitWithHeadroom(context.Background(), pod, 4, 1, 0, true, "", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !v.sawLive || v.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=false", v)
	}

	if want := "2 of 3 balloons-enabled nodes were candidates for this pod, 1 with live topology data"; !strings.Contains(v.detail, want) {
		t.Errorf("detail = %q, want it to contain %q", v.detail, want)
	}
}

// TestAdmitWithHeadroomSkipsUntoleratedTaintedNode proves a node with an untolerated taint
// (cordon does not cover it) leaves candidacy, and returns once the pod tolerates it.
func TestAdmitWithHeadroomSkipsUntoleratedTaintedNode(t *testing.T) {
	// The tainted node is the roomier one, so asserting "packed" measures the taint.
	fleet := func(effect corev1.TaintEffect) *PodMutator {
		return twoNodeFleetWith(func(spacious, _ *corev1.Node) {
			withTaint(spacious, "dedicated", "inference", effect)
		})
	}
	admit := func(m *PodMutator, pod *corev1.Pod) headroomVerdict {
		return m.admitWithHeadroom(context.Background(), pod, 1, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
	}

	for _, effect := range []corev1.TaintEffect{corev1.TaintEffectNoSchedule, corev1.TaintEffectNoExecute} {
		t.Run(string(effect)+"/untolerated", func(t *testing.T) {
			m := fleet(effect)
			pod := unscheduledPod("p", "1")
			if v := admit(m, pod); !v.sawLive || !v.fits {
				t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
			}
			if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "packed" {
				t.Errorf("REQUIRED hostnames = %v, want [packed] (the tainted node is roomier but the pod does not tolerate it)", req)
			}
			if r := reservedCores(m.Reservations, "spacious"); len(r) != 0 {
				t.Errorf("a node with an untolerated %s taint must not receive a reservation, got %v", effect, r)
			}
		})

		t.Run(string(effect)+"/tolerated", func(t *testing.T) {
			m := fleet(effect)
			pod := tolerating(unscheduledPod("p", "1"), "dedicated", "inference", effect)
			if v := admit(m, pod); !v.sawLive || !v.fits {
				t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
			}
			if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "spacious" {
				t.Errorf("REQUIRED hostnames = %v, want [spacious] — the pod tolerates the taint, so the node stays a candidate", req)
			}
			if r := reservedCores(m.Reservations, "spacious"); len(r) == 0 {
				t.Error("the tolerated node must carry the reservation, got none")
			}
		})
	}

	// A non-matching toleration must not rescue the node.
	for _, tc := range []struct {
		name   string
		key    string
		value  string
		effect corev1.TaintEffect
	}{
		{"wrong key", "reserved", "inference", corev1.TaintEffectNoSchedule},
		{"wrong value", "dedicated", "training", corev1.TaintEffectNoSchedule},
		{"wrong effect", "dedicated", "inference", corev1.TaintEffectPreferNoSchedule},
	} {
		t.Run("mismatching toleration/"+tc.name, func(t *testing.T) {
			m := fleet(corev1.TaintEffectNoSchedule)
			pod := tolerating(unscheduledPod("p", "1"), tc.key, tc.value, tc.effect)
			if v := admit(m, pod); !v.sawLive || !v.fits {
				t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
			}
			if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "packed" {
				t.Errorf("REQUIRED hostnames = %v, want [packed] — the pod's toleration does not match the node's taint", req)
			}
			if r := reservedCores(m.Reservations, "spacious"); len(r) != 0 {
				t.Errorf("a toleration that does not match the taint must not make the node a candidate, got %v", r)
			}
		})
	}

	t.Run("wildcard toleration", func(t *testing.T) {
		m := fleet(corev1.TaintEffectNoSchedule)
		pod := unscheduledPod("p", "1")
		// Keyless `operator: Exists` tolerates every taint.
		pod.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
		if v := admit(m, pod); !v.sawLive || !v.fits {
			t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "spacious" {
			t.Errorf("REQUIRED hostnames = %v, want [spacious] — a wildcard toleration keeps the tainted node a candidate", req)
		}
	})

	t.Run("PreferNoSchedule", func(t *testing.T) {
		m := fleet(corev1.TaintEffectPreferNoSchedule)
		pod := unscheduledPod("p", "1")
		if v := admit(m, pod); !v.sawLive || !v.fits {
			t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "spacious" {
			t.Errorf("REQUIRED hostnames = %v, want [spacious] — PreferNoSchedule is a soft effect and must not shrink candidacy", req)
		}
		if r := reservedCores(m.Reservations, "spacious"); len(r) == 0 {
			t.Error("a PreferNoSchedule node must still be reservable, got no reservation")
		}
	})
}

// TestNoFitDetailCountsTaintedNodeOutsideCandidacy proves a tainted node counts as a
// non-candidate, so the denial does not read as "the fleet is full".
func TestNoFitDetailCountsTaintedNodeOutsideCandidacy(t *testing.T) {
	m := twoNodeFleetWith(func(spacious, _ *corev1.Node) {
		withTaint(spacious, "dedicated", "inference", corev1.TaintEffectNoSchedule)
	})

	pod := unscheduledPod("p", "3")
	v := m.admitWithHeadroom(context.Background(), pod, 3, 1, 0, true, "", "physical-cores",
		packingSpread, nil, reservePolicy{}, logr.Discard())
	if !v.sawLive || v.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=false (the only untainted node has 2 free cores per NUMA)", v)
	}
	if want := "1 of 2 balloons-enabled nodes were candidates for this pod, 1 with live topology data"; !strings.Contains(v.detail, want) {
		t.Errorf("detail = %q, want it to contain %q", v.detail, want)
	}
}

// TestAdmitStaticHeadroomSkipsUntoleratedTaintedNode is the cold-start twin.
func TestAdmitStaticHeadroomSkipsUntoleratedTaintedNode(t *testing.T) {
	// "tainted" has twice the capacity, so spread ranks it first unless filtered.
	fleet := func(t *testing.T, effect corev1.TaintEffect) *PodMutator {
		m, _ := coldStartMutator(t, "enforce",
			withTaint(topoNode("tainted", 16, 2), "dedicated", "inference", effect),
			topoNode("open", 8, 2))
		return m
	}
	admit := func(m *PodMutator, pod *corev1.Pod) staticVerdict {
		return m.admitStaticHeadroom(context.Background(), pod, 2, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
	}

	t.Run("untolerated", func(t *testing.T) {
		m := fleet(t, corev1.TaintEffectNoSchedule)
		pod := unscheduledPod("p", "2")
		if v := admit(m, pod); !v.sawStatic || !v.fits {
			t.Fatalf("verdict = %+v, want sawStatic=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "open" {
			t.Errorf("REQUIRED hostnames = %v, want [open] (the tainted node is roomier but untolerated)", req)
		}
		if r := reservedCores(m.Reservations, "tainted"); len(r) != 0 {
			t.Errorf("a node with an untolerated taint must not receive a reservation, got %v", r)
		}
	})

	t.Run("tolerated", func(t *testing.T) {
		m := fleet(t, corev1.TaintEffectNoSchedule)
		pod := tolerating(unscheduledPod("p", "2"), "dedicated", "inference", corev1.TaintEffectNoSchedule)
		if v := admit(m, pod); !v.sawStatic || !v.fits {
			t.Fatalf("verdict = %+v, want sawStatic=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "tainted" {
			t.Errorf("REQUIRED hostnames = %v, want [tainted] — the pod tolerates the taint", req)
		}
		if r := reservedCores(m.Reservations, "tainted"); len(r) == 0 {
			t.Error("the tolerated node must carry the reservation, got none")
		}
	})

	t.Run("PreferNoSchedule", func(t *testing.T) {
		m := fleet(t, corev1.TaintEffectPreferNoSchedule)
		pod := unscheduledPod("p", "2")
		if v := admit(m, pod); !v.sawStatic || !v.fits {
			t.Fatalf("verdict = %+v, want sawStatic=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "tainted" {
			t.Errorf("REQUIRED hostnames = %v, want [tainted] — PreferNoSchedule must not shrink candidacy", req)
		}
	})
}

// TestAllCandidatesTaintedFallsBack proves that when taints leave no candidate, neither
// twin denies: the caller drops to the read-only capacity check with nothing reserved
// or injected. The live fleet uses the lifecycle controller's not-ready taint.
func TestAllCandidatesTaintedFallsBack(t *testing.T) {
	taintBoth := func(nodes ...*corev1.Node) {
		for _, n := range nodes {
			withTaint(n, "node.kubernetes.io/not-ready", "", corev1.TaintEffectNoSchedule)
		}
	}

	t.Run("live", func(t *testing.T) {
		m := twoNodeFleetWith(func(spacious, packed *corev1.Node) { taintBoth(spacious, packed) })
		pod := unscheduledPod("p", "1")
		v := m.admitWithHeadroom(context.Background(), pod, 1, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
		if v.sawLive || v.fits {
			t.Errorf("verdict = %+v, want the empty verdict: every candidate was tainted out", v)
		}
		if pod.Spec.Affinity != nil {
			t.Errorf("no candidate → must not inject affinity, got %+v", pod.Spec.Affinity)
		}
		for _, node := range []string{"spacious", "packed"} {
			if r := reservedCores(m.Reservations, node); len(r) != 0 {
				t.Errorf("%q must carry no reservation, got %v", node, r)
			}
		}
	})

	t.Run("cold start", func(t *testing.T) {
		m, _ := coldStartMutator(t, "enforce",
			withTaint(topoNode("n1", 8, 2), "dedicated", "inference", corev1.TaintEffectNoSchedule),
			withTaint(topoNode("n2", 8, 2), "dedicated", "inference", corev1.TaintEffectNoSchedule))
		pod := unscheduledPod("p", "2")
		v := m.admitStaticHeadroom(context.Background(), pod, 2, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
		if v.sawStatic || v.fits {
			t.Errorf("verdict = %+v, want the empty verdict: every candidate was tainted out", v)
		}
		if pod.Spec.Affinity != nil {
			t.Errorf("no candidate → must not inject affinity, got %+v", pod.Spec.Affinity)
		}
		for _, node := range []string{"n1", "n2"} {
			if r := reservedCores(m.Reservations, node); len(r) != 0 {
				t.Errorf("%q must carry no reservation, got %v", node, r)
			}
		}
	})
}

// TestAdmitWithHeadroomSkipsNotReadyNode proves a node whose Ready condition is False or
// Unknown is dropped (the pin is immutable, so it would stay Pending), while True or
// absent keeps it. The NotReady node is the roomier one throughout.
func TestAdmitWithHeadroomSkipsNotReadyNode(t *testing.T) {
	admit := func(m *PodMutator, pod *corev1.Pod) headroomVerdict {
		return m.admitWithHeadroom(context.Background(), pod, 1, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
	}

	for _, status := range []corev1.ConditionStatus{corev1.ConditionFalse, corev1.ConditionUnknown} {
		t.Run("Ready="+string(status), func(t *testing.T) {
			m := twoNodeFleetWith(func(spacious, _ *corev1.Node) { withReady(spacious, status) })
			pod := unscheduledPod("p", "1")
			if v := admit(m, pod); !v.sawLive || !v.fits {
				t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
			}
			if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "packed" {
				t.Errorf("REQUIRED hostnames = %v, want [packed] (the roomier node reports Ready=%s)", req, status)
			}
			if r := reservedCores(m.Reservations, "spacious"); len(r) != 0 {
				t.Errorf("a node reporting Ready=%s must not receive a reservation, got %v", status, r)
			}
		})
	}

	for _, tc := range []struct {
		name  string
		fleet func() *PodMutator
	}{
		{"Ready=True", func() *PodMutator {
			return twoNodeFleetWith(func(spacious, _ *corev1.Node) { withReady(spacious, corev1.ConditionTrue) })
		}},
		{"no Ready condition", twoNodeFleet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.fleet()
			pod := unscheduledPod("p", "1")
			if v := admit(m, pod); !v.sawLive || !v.fits {
				t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
			}
			if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "spacious" {
				t.Errorf("REQUIRED hostnames = %v, want [spacious] — the readiness filter must not exclude it", req)
			}
		})
	}

	// State taints are stamped NoSchedule: every pod gets an Exists/NoExecute toleration
	// for these keys (DefaultTolerationSeconds), so NoExecute would make the filter a no-op.
	t.Run("NoExecute toleration does not excuse a NotReady node", func(t *testing.T) {
		for _, status := range []corev1.ConditionStatus{corev1.ConditionFalse, corev1.ConditionUnknown} {
			m := twoNodeFleetWith(func(spacious, _ *corev1.Node) { withReady(spacious, status) })
			pod := toleratingKey(
				toleratingKey(unscheduledPod("p", "1"), corev1.TaintNodeNotReady, corev1.TaintEffectNoExecute),
				corev1.TaintNodeUnreachable, corev1.TaintEffectNoExecute)
			if v := admit(m, pod); !v.sawLive || !v.fits {
				t.Fatalf("Ready=%s: verdict = %+v, want sawLive=true fits=true", status, v)
			}
			if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "packed" {
				t.Errorf("Ready=%s: REQUIRED hostnames = %v, want [packed] — the NoExecute toleration every pod carries must not match the NoSchedule state taint", status, req)
			}
		}
	})

	// A matching NoSchedule toleration keeps the node, which pins the taint keys.
	for _, tc := range []struct {
		status corev1.ConditionStatus
		key    string
	}{
		{corev1.ConditionFalse, corev1.TaintNodeNotReady},
		{corev1.ConditionUnknown, corev1.TaintNodeUnreachable},
	} {
		t.Run("tolerating "+tc.key, func(t *testing.T) {
			m := twoNodeFleetWith(func(spacious, _ *corev1.Node) { withReady(spacious, tc.status) })
			pod := toleratingKey(unscheduledPod("p", "1"), tc.key, corev1.TaintEffectNoSchedule)
			if v := admit(m, pod); !v.sawLive || !v.fits {
				t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
			}
			if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "spacious" {
				t.Errorf("REQUIRED hostnames = %v, want [spacious] — the pod tolerates %s, so Ready=%s must not drop the node", req, tc.key, tc.status)
			}
		})
	}

	// A blanket Exists toleration overrides both state predicates, as for the scheduler.
	t.Run("blanket Exists toleration", func(t *testing.T) {
		m := twoNodeFleetWith(func(spacious, _ *corev1.Node) {
			withReady(withCordon(spacious), corev1.ConditionFalse)
		})
		pod := unscheduledPod("p", "1")
		pod.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
		if v := admit(m, pod); !v.sawLive || !v.fits {
			t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "spacious" {
			t.Errorf("REQUIRED hostnames = %v, want [spacious] — a wildcard toleration keeps a cordoned, NotReady node a candidate", req)
		}
	})
}

// TestNodeTaintsToleratedDoesNotMutateNode proves state taints are matched from a copy:
// appending onto a cached Node's Spec.Taints would race other readers.
func TestNodeTaintsToleratedDoesNotMutateNode(t *testing.T) {
	node := withReady(withCordon(withTaint(topoNode("n", 8, 2), "dedicated", "inference", corev1.TaintEffectNoSchedule)), corev1.ConditionFalse)
	// Spare capacity is what an in-place append would write into.
	node.Spec.Taints = append(make([]corev1.Taint, 0, 4), node.Spec.Taints...)
	backing := node.Spec.Taints[:cap(node.Spec.Taints)]
	before := append([]corev1.Taint(nil), backing...)

	if nodeTaintsTolerated(nil, node) {
		t.Fatal("precondition: an untolerated taint must make this false")
	}
	if len(node.Spec.Taints) != 1 || node.Spec.Taints[0].Key != "dedicated" {
		t.Errorf("node.Spec.Taints = %+v, want the one taint it started with", node.Spec.Taints)
	}
	for i := range before {
		if backing[i] != before[i] {
			t.Errorf("taint backing array slot %d = %+v, want %+v — the state taints must not be appended in place", i, backing[i], before[i])
		}
	}
}

// TestAdmitStaticHeadroomSkipsNotReadyNode is the cold-start twin.
func TestAdmitStaticHeadroomSkipsNotReadyNode(t *testing.T) {
	fleet := func(t *testing.T, decorate func(*corev1.Node)) *PodMutator {
		notReady := topoNode("roomier", 16, 2)
		decorate(notReady)
		m, _ := coldStartMutator(t, "enforce", notReady, topoNode("open", 8, 2))
		return m
	}
	admit := func(m *PodMutator, pod *corev1.Pod) staticVerdict {
		return m.admitStaticHeadroom(context.Background(), pod, 2, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
	}

	for _, status := range []corev1.ConditionStatus{corev1.ConditionFalse, corev1.ConditionUnknown} {
		t.Run("Ready="+string(status), func(t *testing.T) {
			m := fleet(t, func(n *corev1.Node) { withReady(n, status) })
			pod := unscheduledPod("p", "2")
			if v := admit(m, pod); !v.sawStatic || !v.fits {
				t.Fatalf("verdict = %+v, want sawStatic=true fits=true", v)
			}
			if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "open" {
				t.Errorf("REQUIRED hostnames = %v, want [open] (the roomier node reports Ready=%s)", req, status)
			}
			if r := reservedCores(m.Reservations, "roomier"); len(r) != 0 {
				t.Errorf("a node reporting Ready=%s must not receive a reservation, got %v", status, r)
			}
		})
	}

	t.Run("no Ready condition", func(t *testing.T) {
		m := fleet(t, func(*corev1.Node) {})
		pod := unscheduledPod("p", "2")
		if v := admit(m, pod); !v.sawStatic || !v.fits {
			t.Fatalf("verdict = %+v, want sawStatic=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "roomier" {
			t.Errorf("REQUIRED hostnames = %v, want [roomier] — an absent Ready condition is unknown, not NotReady", req)
		}
	})
}

// TestAllCandidatesNotReadyFallsBack proves readiness leaving no candidate falls back to
// the read-only capacity check, like TestAllCandidatesTaintedFallsBack.
func TestAllCandidatesNotReadyFallsBack(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		m := twoNodeFleetWith(func(spacious, packed *corev1.Node) {
			withReady(spacious, corev1.ConditionFalse)
			withReady(packed, corev1.ConditionUnknown)
		})
		pod := unscheduledPod("p", "1")
		v := m.admitWithHeadroom(context.Background(), pod, 1, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
		if v.sawLive || v.fits {
			t.Errorf("verdict = %+v, want the empty verdict: no candidate was Ready", v)
		}
		if pod.Spec.Affinity != nil {
			t.Errorf("no candidate → must not inject affinity, got %+v", pod.Spec.Affinity)
		}
	})

	t.Run("cold start", func(t *testing.T) {
		m, _ := coldStartMutator(t, "enforce",
			withReady(topoNode("n1", 8, 2), corev1.ConditionFalse),
			withReady(topoNode("n2", 8, 2), corev1.ConditionFalse))
		pod := unscheduledPod("p", "2")
		v := m.admitStaticHeadroom(context.Background(), pod, 2, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
		if v.sawStatic || v.fits {
			t.Errorf("verdict = %+v, want the empty verdict: no candidate was Ready", v)
		}
		if pod.Spec.Affinity != nil {
			t.Errorf("no candidate → must not inject affinity, got %+v", pod.Spec.Affinity)
		}
	})
}

// TestCordonedNodeHonorsUnschedulableToleration proves a pod tolerating the cordon taint
// (e.g. a DaemonSet pod) keeps a cordoned node, but that does not excuse NotReady.
func TestCordonedNodeHonorsUnschedulableToleration(t *testing.T) {
	daemonSetPod := func() *corev1.Pod {
		return toleratingKey(unscheduledPod("p", "1"), corev1.TaintNodeUnschedulable, corev1.TaintEffectNoSchedule)
	}

	t.Run("live", func(t *testing.T) {
		m := twoNodeFleetWith(func(spacious, _ *corev1.Node) { withCordon(spacious) })
		pod := daemonSetPod()
		v := m.admitWithHeadroom(context.Background(), pod, 1, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
		if !v.sawLive || !v.fits {
			t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "spacious" {
			t.Errorf("REQUIRED hostnames = %v, want [spacious] — the pod tolerates the cordon", req)
		}
		if r := reservedCores(m.Reservations, "spacious"); len(r) == 0 {
			t.Error("a cordoned node the pod tolerates must carry the reservation, got none")
		}
	})

	t.Run("cold start", func(t *testing.T) {
		m, _ := coldStartMutator(t, "enforce",
			withCordon(topoNode("cordoned", 16, 2)), topoNode("open", 8, 2))
		pod := toleratingKey(unscheduledPod("p", "2"), corev1.TaintNodeUnschedulable, corev1.TaintEffectNoSchedule)
		v := m.admitStaticHeadroom(context.Background(), pod, 2, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
		if !v.sawStatic || !v.fits {
			t.Fatalf("verdict = %+v, want sawStatic=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "cordoned" {
			t.Errorf("REQUIRED hostnames = %v, want [cordoned] — the pod tolerates the cordon", req)
		}
	})

	t.Run("does not excuse NotReady", func(t *testing.T) {
		m := twoNodeFleetWith(func(spacious, _ *corev1.Node) {
			withReady(withCordon(spacious), corev1.ConditionFalse)
		})
		pod := daemonSetPod()
		v := m.admitWithHeadroom(context.Background(), pod, 1, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
		if !v.sawLive || !v.fits {
			t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "packed" {
			t.Errorf("REQUIRED hostnames = %v, want [packed] — the cordon toleration must not carry over to NotReady", req)
		}
	})
}

// TestAdmitWithHeadroomSkipsNodeMissingRequestedDevice proves a node whose allocatable
// lacks the requested device is skipped, while a merely busy node stays a candidate.
func TestAdmitWithHeadroomSkipsNodeMissingRequestedDevice(t *testing.T) {
	admit := func(m *PodMutator, pod *corev1.Pod) headroomVerdict {
		return m.admitWithHeadroom(context.Background(), pod, 1, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
	}

	t.Run("not advertised", func(t *testing.T) {
		m := twoNodeFleetWith(func(_, packed *corev1.Node) { withAllocatable(packed, testDevice, "1") })
		pod := requesting(unscheduledPod("p", "1"), testDevice, "1")
		if v := admit(m, pod); !v.sawLive || !v.fits {
			t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "packed" {
			t.Errorf("REQUIRED hostnames = %v, want [packed] (the roomier node advertises no %s)", req, testDevice)
		}
		if r := reservedCores(m.Reservations, "spacious"); len(r) != 0 {
			t.Errorf("a node advertising no %s must not receive a reservation, got %v", testDevice, r)
		}
	})

	t.Run("advertised below the request", func(t *testing.T) {
		m := twoNodeFleetWith(func(spacious, packed *corev1.Node) {
			withAllocatable(spacious, testDevice, "1")
			withAllocatable(packed, testDevice, "2")
		})
		pod := requesting(unscheduledPod("p", "1"), testDevice, "2")
		if v := admit(m, pod); !v.sawLive || !v.fits {
			t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "packed" {
			t.Errorf("REQUIRED hostnames = %v, want [packed] (the roomier node advertises 1 of the 2 requested)", req)
		}
	})

	t.Run("advertised and sufficient", func(t *testing.T) {
		m := twoNodeFleetWith(func(spacious, _ *corev1.Node) { withAllocatable(spacious, testDevice, "1") })
		pod := requesting(unscheduledPod("p", "1"), testDevice, "1")
		if v := admit(m, pod); !v.sawLive || !v.fits {
			t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "spacious" {
			t.Errorf("REQUIRED hostnames = %v, want [spacious] — its allocatable covers the request", req)
		}
	})

	// A kubernetes.io vendor subdomain is still a device; the domain is a prefix check.
	t.Run("vendor subdomain of kubernetes.io", func(t *testing.T) {
		const subdomainDevice = "vendor.kubernetes.io/gpu"
		m := twoNodeFleetWith(func(_, packed *corev1.Node) { withAllocatable(packed, subdomainDevice, "1") })
		pod := requesting(unscheduledPod("p", "1"), subdomainDevice, "1")
		if v := admit(m, pod); !v.sawLive || !v.fits {
			t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "packed" {
			t.Errorf("REQUIRED hostnames = %v, want [packed] (the roomier node advertises no %s)", req, subdomainDevice)
		}
	})

	// Native resources are not filtered.
	t.Run("native request does not filter", func(t *testing.T) {
		m := twoNodeFleet()
		pod := requesting(unscheduledPod("p", "1"), "hugepages-2Mi", "2Mi")
		if v := admit(m, pod); !v.sawLive || !v.fits {
			t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
		}
		if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "spacious" {
			t.Errorf("REQUIRED hostnames = %v, want [spacious] — neither node advertises hugepages, and native names are not filtered", req)
		}
	})
}

// TestAdmitStaticHeadroomSkipsNodeMissingRequestedDevice is the cold-start twin.
func TestAdmitStaticHeadroomSkipsNodeMissingRequestedDevice(t *testing.T) {
	// "roomier" has twice the capacity, so spread ranks it first unless filtered.
	m, _ := coldStartMutator(t, "enforce",
		topoNode("roomier", 16, 2), withAllocatable(topoNode("gpu", 8, 2), testDevice, "1"))

	pod := requesting(unscheduledPod("p", "2"), testDevice, "1")
	v := m.admitStaticHeadroom(context.Background(), pod, 2, 1, 0, true, "", "physical-cores",
		packingSpread, nil, reservePolicy{}, logr.Discard())
	if !v.sawStatic || !v.fits {
		t.Fatalf("verdict = %+v, want sawStatic=true fits=true", v)
	}
	if req := affinityRequiredValues(pod); len(req) != 1 || req[0] != "gpu" {
		t.Errorf("REQUIRED hostnames = %v, want [gpu] (the roomier node advertises no %s)", req, testDevice)
	}
	if r := reservedCores(m.Reservations, "roomier"); len(r) != 0 {
		t.Errorf("a node advertising no %s must not receive a reservation, got %v", testDevice, r)
	}
}

// TestNoNodeAdvertisesDeviceFallsBack proves that when no node advertises the device yet,
// both twins fall back rather than enforce denying every device pod.
func TestNoNodeAdvertisesDeviceFallsBack(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		m := twoNodeFleet()
		pod := requesting(unscheduledPod("p", "1"), testDevice, "1")
		v := m.admitWithHeadroom(context.Background(), pod, 1, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
		if v.sawLive || v.fits {
			t.Errorf("verdict = %+v, want the empty verdict: no candidate advertises the device", v)
		}
		if pod.Spec.Affinity != nil {
			t.Errorf("no candidate → must not inject affinity, got %+v", pod.Spec.Affinity)
		}
		for _, node := range []string{"spacious", "packed"} {
			if r := reservedCores(m.Reservations, node); len(r) != 0 {
				t.Errorf("%q must carry no reservation, got %v", node, r)
			}
		}
	})

	t.Run("cold start", func(t *testing.T) {
		m, _ := coldStartMutator(t, "enforce", topoNode("n1", 8, 2), topoNode("n2", 8, 2))
		pod := requesting(unscheduledPod("p", "2"), testDevice, "1")
		v := m.admitStaticHeadroom(context.Background(), pod, 2, 1, 0, true, "", "physical-cores",
			packingSpread, nil, reservePolicy{}, logr.Discard())
		if v.sawStatic || v.fits {
			t.Errorf("verdict = %+v, want the empty verdict: no candidate advertises the device", v)
		}
		if pod.Spec.Affinity != nil {
			t.Errorf("no candidate → must not inject affinity, got %+v", pod.Spec.Affinity)
		}
		for _, node := range []string{"n1", "n2"} {
			if r := reservedCores(m.Reservations, node); len(r) != 0 {
				t.Errorf("%q must carry no reservation, got %v", node, r)
			}
		}
	})
}

// TestNoFitDetailCountsDeviceLessNodeOutsideCandidacy proves a device-less node counts as
// a non-candidate in the no-fit detail.
func TestNoFitDetailCountsDeviceLessNodeOutsideCandidacy(t *testing.T) {
	m := twoNodeFleetWith(func(_, packed *corev1.Node) { withAllocatable(packed, testDevice, "1") })

	pod := requesting(unscheduledPod("p", "3"), testDevice, "1")
	v := m.admitWithHeadroom(context.Background(), pod, 3, 1, 0, true, "", "physical-cores",
		packingSpread, nil, reservePolicy{}, logr.Discard())
	if !v.sawLive || v.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=false (the only GPU node has 2 free cores per NUMA)", v)
	}
	if want := "1 of 2 balloons-enabled nodes were candidates for this pod, 1 with live topology data"; !strings.Contains(v.detail, want) {
		t.Errorf("detail = %q, want it to contain %q", v.detail, want)
	}
}

// TestExtendedResourceName proves device-plugin names are filtered and native ones are
// not, including the `*.kubernetes.io/` and `notkubernetes.io/` cases.
func TestExtendedResourceName(t *testing.T) {
	for _, name := range []string{"vendor.com/gpu", "example.org/fpga", "qat.intel.com/generic",
		"vendor.kubernetes.io/gpu", "notkubernetes.io/gpu"} {
		if !extendedResourceName(corev1.ResourceName(name)) {
			t.Errorf("extendedResourceName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"cpu", "memory", "ephemeral-storage", "hugepages-2Mi",
		"attachable-volumes-ebs", "kubernetes.io/foo"} {
		if extendedResourceName(corev1.ResourceName(name)) {
			t.Errorf("extendedResourceName(%q) = true, want false", name)
		}
	}
}

// TestPodExtendedRequests proves the scheduler's aggregation: sidecars add to the sum,
// plain init containers only raise the floor, and zero or native requests drop out.
func TestPodExtendedRequests(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	pod := requesting(unscheduledPod("p", "1"), testDevice, "1")
	pod.Spec.Containers[0].Resources.Requests["vendor.com/nic"] = resource.MustParse("0")
	pod.Spec.InitContainers = []corev1.Container{
		{Name: "sidecar", RestartPolicy: &always, Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{testDevice: resource.MustParse("2")},
		}},
		// Its floor is 2+4=6, above the running phase's 1+2=3; this pins the floor half.
		{Name: "init", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{testDevice: resource.MustParse("4")},
		}},
	}

	got := podExtendedRequests(pod)
	if len(got) != 1 {
		t.Fatalf("podExtendedRequests = %v, want only the positive extended request", got)
	}
	if q := got[testDevice]; q.Value() != 6 {
		t.Errorf("%s = %s, want 6 — max(container 1 + sidecar 2, sidecar 2 + plain init 4)", testDevice, q.String())
	}
	if got := podExtendedRequests(unscheduledPod("p", "1")); got != nil {
		t.Errorf("a pod requesting only cpu must yield no extended requests, got %v", got)
	}
}
