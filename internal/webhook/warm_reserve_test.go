// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// indexByPinnedBy mirrors the manager's PodPinnedByField index func so the fake
// client answers the warm-up's MatchingFields lookup.
func indexByPinnedBy(obj client.Object) []string {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Annotations[PinnedByAnnotation] == "" {
		return nil
	}
	return []string{pod.Annotations[PinnedByAnnotation]}
}

// pinnedPod is a pod a previous leader admitted and stamped; created drives the
// seeded expiry.
func pinnedPod(name, node, balloon, cores string, created time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "inference",
			CreationTimestamp: metav1.NewTime(created),
			Annotations: map[string]string{
				PinnedByAnnotation:             PinnedByValue,
				balloonAnnotationBase + "/pod": balloon,
			},
		},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name: "vllm",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cores)},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cores)},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// warmScheme is the fake-client scheme with the NodeResourceTopology GVK registered.
func warmScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return scheme
}

// warmMutator builds a mutator with an empty reservation store, as a new lease
// holder has, with the topology cache primed for both test nodes.
func warmMutator(t *testing.T, objs ...client.Object) *PodMutator {
	t.Helper()
	return warmMutatorCachedFor(t, []string{"a", "b"}, objs...)
}

// warmMutatorCachedFor primes the topology cache for the named nodes only, so the
// others fall back to static capacity labels.
func warmMutatorCachedFor(t *testing.T, cached []string, objs ...client.Object) *PodMutator {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(warmScheme()).
		WithIndex(&corev1.Pod{}, controller.PodPinnedByField, indexByPinnedBy).
		WithObjects(objs...).Build()
	topo := topology.NewCache()
	for _, node := range cached {
		topo.Set(node, simpleTopo())
	}
	m, _ := newHandleMutatorFromClient(t, "enforce", c, topo)
	return m
}

// publishedNRT is a node's CR with only the reserved-pool zone: no workload pins
// have landed yet.
func publishedNRT(node string) *unstructured.Unstructured {
	return nrtObj(node, nrtZone(testBalloonZoneType, "reserved[0]", "", ""))
}

// TestWarmReservationsNewLeaderSeesPreviousLeaderDemand proves a new leader seeds
// its store from pinned pods not yet in the CR, so the node has no room left for a
// further pod.
func TestWarmReservationsNewLeaderSeesPreviousLeaderDemand(t *testing.T) {
	now := time.Now()
	objs := []client.Object{
		topoNode("a", 4, 2),
		publishedNRT("a"),
		pinnedPod("prev-admitted", "a", "balloon-span2", "4", now),
	}

	// A cold successor admits the next pod.
	cold := warmMutator(t, objs...)
	if resp := admitPod(t, cold, "vllm-next", "2", "1"); !resp.Allowed {
		t.Fatalf("precondition: a cold successor must admit against its empty store, got denied: %q", resp.Result.Message)
	}

	warm := warmMutator(t, objs...)
	warm.WarmReservations(context.Background())

	if got, want := reservedCores(warm.Reservations, "a"), map[int]int{0: 2, 1: 2}; !sameCores(got, want) {
		t.Fatalf("warm-up derived reservedCores=%v, want %v (span-2 pod holding ceil(4/2) per NUMA)", got, want)
	}
	resp := admitPod(t, warm, "vllm-next", "2", "1")
	if resp.Allowed {
		t.Fatalf("a warmed successor must deny the next 2-core pod — node \"a\" has 2 cores/NUMA and the previous leader's pod holds both; got Allowed (the failover over-admission window is still open)")
	}
}

// TestWarmReservationsSkipsPodsAlreadyInNRT proves a pod already published in the
// CR is not seeded again (it would be charged twice).
func TestWarmReservationsSkipsPodsAlreadyInNRT(t *testing.T) {
	now := time.Now()
	m := warmMutator(t,
		topoNode("a", 4, 2),
		nrtObj("a", nrtZone(testContainerZoneType, "inference/prev-admitted/vllm", "balloon-span2[0]", "0,2")),
		pinnedPod("prev-admitted", "a", "balloon-span2", "4", now),
	)
	m.WarmReservations(context.Background())

	if got := reservedCores(m.Reservations, "a"); len(got) != 0 {
		t.Fatalf("warm-up seeded %v for a pod the CR already reflects, want nothing (double-counted headroom)", got)
	}
}

// TestWarmReservationsDropsPodsPastTheirTTL proves a seeded reservation expires one
// TTL after the pod's creation, not after warm-up.
func TestWarmReservationsDropsPodsPastTheirTTL(t *testing.T) {
	old := time.Now().Add(-2 * DefaultReservationTTL)
	m := warmMutator(t,
		topoNode("a", 4, 2),
		publishedNRT("a"),
		pinnedPod("long-running", "a", "balloon-span2", "4", old),
	)
	m.WarmReservations(context.Background())

	if got := reservedCores(m.Reservations, "a"); len(got) != 0 {
		t.Fatalf("warm-up seeded %v for a pod created %s ago, want nothing (its reservation had already expired)", got, 2*DefaultReservationTTL)
	}
	// The store refuses the expired entry outright.
	if m.Reservations.seed("a", []int{0}, 1, 0, time.Now().Add(-time.Second)) {
		t.Error("seed accepted an already-expired reservation")
	}
}

// TestWarmReservationsSkipsNodeWithMalformedNRT proves a node with unreadable zones
// is skipped rather than risking a double count.
func TestWarmReservationsSkipsNodeWithMalformedNRT(t *testing.T) {
	bad := nrtObj("a")
	bad.Object["zones"] = "not-a-zone-list"
	m := warmMutator(t,
		topoNode("a", 4, 2), bad,
		pinnedPod("prev-admitted", "a", "balloon-span2", "4", time.Now()),
	)
	m.WarmReservations(context.Background())

	if got := reservedCores(m.Reservations, "a"); len(got) != 0 {
		t.Fatalf("warm-up seeded %v from a malformed CR, want nothing (occupancy unknown)", got)
	}
}

// TestWarmReservationsSeedsWhenTheCRPublishesNoZones proves every pinned pod is
// seeded on a node whose CR reflects nothing.
func TestWarmReservationsSeedsWhenTheCRPublishesNoZones(t *testing.T) {
	m := warmMutator(t,
		topoNode("a", 4, 2),
		nrtObj("a"), // readable, zero zones
		pinnedPod("prev-admitted", "a", "balloon-span2", "4", time.Now()),
	)
	m.WarmReservations(context.Background())

	if got, want := reservedCores(m.Reservations, "a"), map[int]int{0: 2, 1: 2}; !sameCores(got, want) {
		t.Fatalf("warm-up derived reservedCores=%v, want %v — a zone-less CR reflects nothing", got, want)
	}
}

// TestWarmReservationsSkipsOnlyTheUnobservableNode proves a node with no CR is
// skipped without costing the rest of the fleet its warm-up.
func TestWarmReservationsSkipsOnlyTheUnobservableNode(t *testing.T) {
	now := time.Now()
	m := warmMutator(t,
		topoNode("a", 4, 2), publishedNRT("a"),
		topoNode("b", 4, 2), // no NodeResourceTopology CR for "b"
		pinnedPod("on-a", "a", "balloon-span2", "4", now),
		pinnedPod("on-b", "b", "balloon-span2", "4", now),
	)
	m.WarmReservations(context.Background())

	if got, want := reservedCores(m.Reservations, "a"), map[int]int{0: 2, 1: 2}; !sameCores(got, want) {
		t.Errorf("node \"a\" reservedCores=%v, want %v — a skipped peer must not suppress its warm-up", got, want)
	}
	if got := reservedCores(m.Reservations, "b"); len(got) != 0 {
		t.Errorf("node \"b\" reservedCores=%v, want nothing (no CR, so placement is unobservable)", got)
	}
}

// TestWarmReservationsSizesFromTheStampedSpan proves the share and NUMA count come
// from the pod's stamped span: a span-1 pod holds one NUMA only.
func TestWarmReservationsSizesFromTheStampedSpan(t *testing.T) {
	m := warmMutator(t,
		topoNode("b", 4, 2), publishedNRT("b"),
		pinnedPod("narrow", "b", "balloon-span1", "2", time.Now()),
	)
	m.WarmReservations(context.Background())

	if got, want := reservedCores(m.Reservations, "b"), map[int]int{0: 2}; !sameCores(got, want) {
		t.Errorf("span-1 pod seeded reservedCores=%v, want %v (one NUMA only)", got, want)
	}
}

// TestWarmReservationsSkipsNodeWithNoUsableCapacity proves a node with no live map
// and no topology labels is skipped while a readable peer is warmed.
func TestWarmReservationsSkipsNodeWithNoUsableCapacity(t *testing.T) {
	now := time.Now()
	m := warmMutatorCachedFor(t, []string{"a"}, // only "a" is hydrated
		topoNode("a", 4, 2), publishedNRT("a"),
		withoutTopologyLabels(topoNode("b", 4, 2)), publishedNRT("b"),
		pinnedPod("on-a", "a", "balloon-span2", "4", now),
		pinnedPod("on-b", "b", "balloon-span2", "4", now),
	)
	m.WarmReservations(context.Background())

	if got, want := reservedCores(m.Reservations, "a"), map[int]int{0: 2, 1: 2}; !sameCores(got, want) {
		t.Errorf("node \"a\" reservedCores=%v, want %v", got, want)
	}
	if got := reservedCores(m.Reservations, "b"); len(got) != 0 {
		t.Errorf("node \"b\" reservedCores=%v, want nothing (no live map and no capacity labels)", got)
	}
}

// TestWarmReservationsListFailureServesEmptyStore proves a failing pod list leaves
// an empty store instead of panicking or blocking Start.
func TestWarmReservationsListFailureServesEmptyStore(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(warmScheme()).
		WithIndex(&corev1.Pod{}, controller.PodPinnedByField, indexByPinnedBy).
		WithObjects(topoNode("a", 4, 2), publishedNRT("a"),
			pinnedPod("prev-admitted", "a", "balloon-span2", "4", time.Now())).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				if _, ok := list.(*corev1.PodList); ok {
					return fmt.Errorf("apiserver unavailable")
				}
				return nil
			},
		}).Build()
	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	m, _ := newHandleMutatorFromClient(t, "enforce", c, topo)

	m.WarmReservations(context.Background())

	if got := reservedCores(m.Reservations, "a"); len(got) != 0 {
		t.Fatalf("warm-up recorded %v despite a failed pod list, want nothing", got)
	}
}

// TestWarmReservationsPublishesLiveGauge proves the live gauge reports what warm-up
// derived per node.
func TestWarmReservationsPublishesLiveGauge(t *testing.T) {
	const node = "a"
	metrics.ReservationsLive.DeleteLabelValues(node)
	t.Cleanup(func() { metrics.ReservationsLive.DeleteLabelValues(node) })

	m := warmMutator(t,
		topoNode(node, 4, 2),
		publishedNRT(node),
		pinnedPod("prev-admitted", node, "balloon-span2", "4", time.Now()),
	)
	m.WarmReservations(context.Background())

	if got := testutil.ToFloat64(metrics.ReservationsLive.WithLabelValues(node)); got != 1 {
		t.Errorf("webhook_reservations_live{node=%s} = %v after warm-up, want 1", node, got)
	}
}

// TestWarmReservationsGaugeReportsZeroForAnEmptyDerivation proves an examined node
// with nothing derived publishes 0, not an absent series.
func TestWarmReservationsGaugeReportsZeroForAnEmptyDerivation(t *testing.T) {
	const node = "a"
	metrics.ReservationsLive.WithLabelValues(node).Set(7)
	t.Cleanup(func() { metrics.ReservationsLive.DeleteLabelValues(node) })

	m := warmMutator(t,
		topoNode(node, 4, 2),
		nrtObj(node, nrtZone(testContainerZoneType, "inference/prev-admitted/vllm", "balloon-span2[0]", "0,2")),
		pinnedPod("prev-admitted", node, "balloon-span2", "4", time.Now()),
	)
	m.WarmReservations(context.Background())

	if got := testutil.ToFloat64(metrics.ReservationsLive.WithLabelValues(node)); got != 0 {
		t.Errorf("webhook_reservations_live{node=%s} = %v, want 0 for a node examined with nothing to seed", node, got)
	}
}

// TestReservationsLiveTracksAdmissionAndExpiry proves the gauge rises on an
// admission and falls once the entry expires.
func TestReservationsLiveTracksAdmissionAndExpiry(t *testing.T) {
	const node = "a"
	metrics.ReservationsLive.DeleteLabelValues(node)
	t.Cleanup(func() { metrics.ReservationsLive.DeleteLabelValues(node) })

	topo := topology.NewCache()
	topo.Set(node, simpleTopo())
	c := fake.NewClientBuilder().WithScheme(warmScheme()).
		WithObjects(topoNode(node, 4, 2), publishedNRT(node)).Build()
	m, _ := newHandleMutatorFromClient(t, "enforce", c, topo)

	if resp := admitPod(t, m, "vllm-a", "2", "1"); !resp.Allowed {
		t.Fatalf("admission must be allowed, got denied: %q", resp.Result.Message)
	}
	if got := testutil.ToFloat64(metrics.ReservationsLive.WithLabelValues(node)); got != 1 {
		t.Fatalf("webhook_reservations_live{node=%s} = %v after one admission, want 1", node, got)
	}

	// Age past the TTL; the next read prunes and republishes.
	m.Reservations.now = func() time.Time { return time.Now().Add(2 * DefaultReservationTTL) }
	reservedCores(m.Reservations, node)
	if got := testutil.ToFloat64(metrics.ReservationsLive.WithLabelValues(node)); got != 0 {
		t.Errorf("webhook_reservations_live{node=%s} = %v after the reservation expired, want 0", node, got)
	}
}

// TestPinnedBalloonReadsTheStampedScope covers both stamp shapes: pod-scoped (cores
// summed across containers) and container-scoped (that container alone).
func TestPinnedBalloonReadsTheStampedScope(t *testing.T) {
	pod := pinnedPod("p", "a", "balloon-span2", "4", time.Now())
	balloon, container, podScoped, ok := pinnedBalloon(pod)
	if !ok || balloon != "balloon-span2" || container != nil || !podScoped {
		t.Fatalf("pod-scoped stamp read as (%q, %v, %v, %v), want (balloon-span2, nil, true, true)", balloon, container, podScoped, ok)
	}

	delete(pod.Annotations, balloonAnnotationBase+"/pod")
	pod.Annotations[balloonAnnotationBase+"/container.vllm"] = "balloon-span1"
	balloon, container, podScoped, ok = pinnedBalloon(pod)
	if !ok || balloon != "balloon-span1" || container == nil || container.Name != "vllm" || podScoped {
		t.Fatalf("container-scoped stamp read as (%q, %v, %v, %v), want (balloon-span1, vllm, false, true)", balloon, container, podScoped, ok)
	}

	// Both keys present: the pod-scoped stamp wins.
	pod.Annotations[balloonAnnotationBase+"/pod"] = "balloon-span2"
	balloon, container, podScoped, ok = pinnedBalloon(pod)
	if !ok || balloon != "balloon-span2" || container != nil || !podScoped {
		t.Errorf("with both keys set, read (%q, %v, %v, %v), want the pod-scoped stamp", balloon, container, podScoped, ok)
	}

	pod.Annotations = nil
	if _, _, _, ok := pinnedBalloon(pod); ok {
		t.Error("a pod with no balloon annotation must not report a stamp")
	}
}

// TestReserveAuthorityWarmsBeforeItServes proves Start warms the store before the
// authority answers a reserve. The hook runs synchronously inside the warm-up
// window and probes with a full handshake, since a bare connect proves nothing
// (see freeLoopbackAddr).
func TestReserveAuthorityWarmsBeforeItServes(t *testing.T) {
	certDir := writeReserveCerts(t)
	store := NewReservationStore(0)
	addr := freeLoopbackAddr(t)
	probe := authorityProbe(t, certDir)

	var servingDuringWarmup bool
	warmed := make(chan struct{})
	a := &ReserveAuthority{
		Addr: addr, CertDir: certDir, Store: store, Log: logr.Discard(),
		Warm: func(ctx context.Context) {
			// Bounded, so a slow warm-up cannot hold the lease open indefinitely.
			if _, hasDeadline := ctx.Deadline(); !hasDeadline {
				t.Error("the warm-up context carries no deadline; a hung read would stall the reserve endpoint for the life of the lease")
			}
			servingDuringWarmup = authorityServing(probe, addr)
			store.seed("n1", []int{0, 1}, 2, 0, time.Now().Add(time.Minute))
			close(warmed)
		},
	}
	runAuthority(t, a)

	select {
	case <-warmed:
	case <-time.After(5 * time.Second):
		t.Fatal("Start never ran the warm-up hook, so a successor would serve from an empty store")
	}
	if servingDuringWarmup {
		t.Error("the reserve authority was already answering handshakes during warm-up; a reserve could be answered before the store is warm")
	}

	awaitAuthorityTLS(t, certDir, addr)
	host, portStr, _ := net.SplitHostPort(addr)
	c, err := newLeaderReserveClient(certDir, atoiOrFatal(t, portStr), NewReservationStore(0),
		func(context.Context) (string, bool, error) { return host, false, nil }, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	// The warm-up holds both cores per NUMA, so the first reserve must be rejected.
	req := ReserveRequest{Node: "n1", PerNUMACores: 2, Span: 2, RawFree: map[int]int{0: 2, 1: 2}}
	node, _, accepted, err := c.reserveBest(context.Background(), "", []ReserveRequest{req})
	if err != nil {
		t.Fatalf("reserve RPC failed: %v", err)
	}
	if accepted {
		t.Fatalf("the authority's FIRST reserve was accepted on %q; the warm-up's outstanding reservation was not consulted", node)
	}
}

// TestWarmReservationsSeedsTheMemoryShare proves memory is seeded as the per-NUMA
// share ceil(bytes/span), like admission records, not the total.
func TestWarmReservationsSeedsTheMemoryShare(t *testing.T) {
	pod := pinnedPod("prev-admitted", "a", "balloon-span2", "4", time.Now())
	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("8Gi")
	pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("8Gi")
	m := warmMutator(t, topoNode("a", 4, 2), publishedNRT("a"), pod)
	m.WarmReservations(context.Background())

	want := perNUMAMemShare(8<<30, 2) // 4Gi on each of the span's two NUMAs
	got := reservedMem(m.Reservations, "a")
	if len(got) != 2 || got[0] != want || got[1] != want {
		t.Fatalf("warm-up seeded reservedMem=%v, want %d on each of NUMA 0 and 1", got, want)
	}
}

// TestWarmReservationsSkipsPodsWithNothingToAccount proves unbound, terminating,
// terminal, unpinned and unreadably-stamped pods are not seeded.
func TestWarmReservationsSkipsPodsWithNothingToAccount(t *testing.T) {
	now := time.Now()
	unscheduled := pinnedPod("unscheduled", "", "balloon-span2", "4", now)
	terminating := pinnedPod("terminating", "a", "balloon-span2", "4", now)
	terminating.DeletionTimestamp = &metav1.Time{Time: now}
	terminating.Finalizers = []string{"test/keep"} // the fake client rejects a bare deletionTimestamp
	succeeded := pinnedPod("succeeded", "a", "balloon-span2", "4", now)
	succeeded.Status.Phase = corev1.PodSucceeded
	failed := pinnedPod("failed", "a", "balloon-span2", "4", now)
	failed.Status.Phase = corev1.PodFailed
	unpinned := pinnedPod("unpinned", "a", "balloon-span2", "4", now)
	delete(unpinned.Annotations, PinnedByAnnotation)
	unknownBalloon := pinnedPod("unknown-balloon", "a", "balloon-span3", "4", now)
	noCPU := pinnedPod("no-cpu", "a", "balloon-span2", "4", now)
	noCPU.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
	noStamp := pinnedPod("no-stamp", "a", "balloon-span2", "4", now)
	delete(noStamp.Annotations, balloonAnnotationBase+"/pod")

	m := warmMutator(t, topoNode("a", 4, 2), publishedNRT("a"),
		unscheduled, terminating, succeeded, failed, unpinned, unknownBalloon, noCPU, noStamp)
	m.WarmReservations(context.Background())

	if got := reservedCores(m.Reservations, "a"); len(got) != 0 {
		t.Fatalf("warm-up seeded %v, want nothing — none of these pods holds accountable room", got)
	}
}

// TestWarmReservationsSpreadsSuccessivePodsAcrossNUMAs proves seeding places each
// pod against room net of earlier seeds: two span-1 pods take one NUMA apiece.
func TestWarmReservationsSpreadsSuccessivePodsAcrossNUMAs(t *testing.T) {
	now := time.Now()
	m := warmMutator(t, topoNode("a", 4, 2), publishedNRT("a"),
		pinnedPod("first", "a", "balloon-span1", "2", now),
		pinnedPod("second", "a", "balloon-span1", "2", now),
	)
	m.WarmReservations(context.Background())

	if got, want := reservedCores(m.Reservations, "a"), map[int]int{0: 2, 1: 2}; !sameCores(got, want) {
		t.Fatalf("two span-1 pods seeded reservedCores=%v, want %v (one NUMA each)", got, want)
	}
}

// TestWarmReservationsFallsBackToStaticCapacity proves a node without a hydrated
// live map is seeded from its topology labels.
func TestWarmReservationsFallsBackToStaticCapacity(t *testing.T) {
	m := warmMutatorCachedFor(t, nil, // topology cache empty for every node
		topoNode("a", 4, 2), publishedNRT("a"),
		pinnedPod("prev-admitted", "a", "balloon-span2", "4", time.Now()),
	)
	m.WarmReservations(context.Background())

	if got, want := reservedCores(m.Reservations, "a"), map[int]int{0: 2, 1: 2}; !sameCores(got, want) {
		t.Fatalf("warm-up derived reservedCores=%v, want %v from the static capacity labels", got, want)
	}
}

// TestWarmReservationsContainerScopedStampIgnoresSiblingZones proves a sibling
// container's zone does not mark a container-scoped pod as already reflected.
func TestWarmReservationsContainerScopedStampIgnoresSiblingZones(t *testing.T) {
	pod := pinnedPod("split", "a", "balloon-span1", "2", time.Now())
	delete(pod.Annotations, balloonAnnotationBase+"/pod")
	pod.Annotations[balloonAnnotationBase+"/container.vllm"] = "balloon-span1"
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "sidecar"})

	m := warmMutator(t,
		topoNode("a", 4, 2),
		nrtObj("a", nrtZone(testContainerZoneType, "inference/split/sidecar", "balloon-span1[0]", "3")),
		pod,
	)
	m.WarmReservations(context.Background())

	if got, want := reservedCores(m.Reservations, "a"), map[int]int{0: 2}; !sameCores(got, want) {
		t.Fatalf("warm-up derived reservedCores=%v, want %v — only the stamped container's zone proves its pin landed", got, want)
	}
}

// TestWarmReservationsTreatsAnInitContainerZoneAsReflected proves a pod-scoped pod
// whose injected init container's zone is published is not seeded again.
func TestWarmReservationsTreatsAnInitContainerZoneAsReflected(t *testing.T) {
	pod := pinnedPod("starting", "a", "balloon-span2", "4", time.Now())
	pod.Spec.InitContainers = []corev1.Container{{Name: defaultInitContainerName}}

	m := warmMutator(t,
		topoNode("a", 4, 2),
		nrtObj("a", nrtZone(testContainerZoneType,
			"inference/starting/"+defaultInitContainerName, "balloon-span2[0]", "0,2")),
		pod,
	)
	m.WarmReservations(context.Background())

	if got := reservedCores(m.Reservations, "a"); len(got) != 0 {
		t.Fatalf("warm-up seeded %v for a pod whose init-container zone is already published, want nothing", got)
	}
}

// sameCores compares two per-NUMA reservation maps.
func sameCores(got, want map[int]int) bool {
	if len(got) != len(want) {
		return false
	}
	for numa, n := range want {
		if got[numa] != n {
			return false
		}
	}
	return true
}
