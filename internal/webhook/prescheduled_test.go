// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// admitPrescheduled runs a CREATE admission for a whole-core "vllm" pod that already
// carries spec.nodeName, exercising handle()'s pre-scheduled branch. mem adds a memory
// request when non-empty.
func admitPrescheduled(t *testing.T, m *PodMutator, name, cores, tp, mem, node string, dryRun bool) admission.Response {
	t.Helper()
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cores)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cores)},
	}
	if mem != "" {
		res.Requests[corev1.ResourceMemory] = resource.MustParse(mem)
		res.Limits[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "inference",
			Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": tp},
		},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name:      "vllm",
				Resources: res,
			}},
		},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	return m.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}),
		Operation: admissionv1.Create,
		Namespace: pod.Namespace,
		DryRun:    &dryRun,
		Object:    runtime.RawExtension{Raw: raw},
	}})
}

// TestPrescheduledReservationBlocksUnscheduled proves a pre-scheduled pod's reservation
// blocks a later unscheduled pod: 4 cores/NUMA, each span-2 6-core pod needs 3/NUMA.
func TestPrescheduledReservationBlocksUnscheduled(t *testing.T) {
	m, rec := coldStartMutator(t, "enforce", topoNode("n1", 8, 2))

	first := admitPrescheduled(t, m, "daemon-a", "6", "2", "", "n1", false)
	if !first.Allowed {
		t.Fatalf("pre-scheduled pod must be admitted (3/NUMA fits a 4/NUMA node), got denied: %q", first.Result.Message)
	}

	second := admitPod(t, m, "vllm-b", "6", "2")
	if second.Allowed {
		t.Fatalf("unscheduled pod must be DENIED — the pre-scheduled pod's reservation leaves 1 core/NUMA and the request needs 3; got Allowed, so the pre-scheduled demand was never recorded")
	}
	events := drainEvents(rec)
	if !hasEventContaining(events, "CapacityCheckFailed") {
		t.Errorf("expected a CapacityCheckFailed event on the denied unscheduled pod, got %v", events)
	}
}

// TestPrescheduledDryRunRecordsNothing proves a dry-run pre-scheduled pod consumes no
// headroom (sideEffects: None).
func TestPrescheduledDryRunRecordsNothing(t *testing.T) {
	m, _ := coldStartMutator(t, "enforce", topoNode("n1", 8, 2))

	dry := admitPrescheduled(t, m, "daemon-a", "6", "2", "", "n1", true)
	if !dry.Allowed {
		t.Fatalf("dry-run pre-scheduled pod must be admitted, got denied: %q", dry.Result.Message)
	}

	real := admitPrescheduled(t, m, "daemon-b", "6", "2", "", "n1", false)
	if !real.Allowed {
		t.Fatalf("the dry-run must not have consumed headroom, but the following real pod was denied: %q", real.Result.Message)
	}
}

// TestPrescheduledFallsBackToLocalOnReserverError proves warn mode reserves locally when
// the reserver errors, since nothing later recovers a pre-scheduled pod's reservation.
func TestPrescheduledFallsBackToLocalOnReserverError(t *testing.T) {
	m, _ := coldStartMutator(t, "warn", topoNode("n1", 8, 2))
	m.Reservations = NewReservationStore(0)
	stub := &stubReserver{err: errors.New("leader unreachable: connection refused")}
	m.SetReserver(stub)

	resp := admitPrescheduled(t, m, "daemon-a", "6", "2", "", "n1", false)
	if !resp.Allowed {
		t.Fatalf("a reserver error must not deny a pre-scheduled pod under warn, got denied: %q", resp.Result.Message)
	}
	if stub.calls != 2 {
		t.Fatalf("reserver called %d times, want 2 (one retry on the same key before the local fallback)", stub.calls)
	}
	if r := reservedCores(m.Reservations, "n1"); len(r) == 0 {
		t.Fatal("no reservation on 'n1' after the reserver errored — the pre-scheduled demand was dropped instead of falling back to the local store")
	}
}

// TestPrescheduledDeniesWhenTheCommitCannotBeAccounted is the enforce half: an
// unaccountable commit denies.
func TestPrescheduledDeniesWhenTheCommitCannotBeAccounted(t *testing.T) {
	const ns = "inference"
	reason := func(r string) float64 {
		return testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues("n1", ns, r, "enforce"))
	}
	unaccBefore, capBefore := reason(reasonUnaccountable), reason("capacity")

	m, rec := coldStartMutator(t, "enforce", topoNode("n1", 8, 2))
	m.Reservations = NewReservationStore(0)
	m.SetReserver(&stubReserver{err: errors.New("leader unreachable: connection refused")})

	resp := admitPrescheduled(t, m, "daemon-a", "6", "2", "", "n1", false)
	if resp.Allowed {
		t.Fatal("an unaccountable commit under enforce must deny a pre-scheduled pod too")
	}
	if r := reservedCores(m.Reservations, "n1"); len(r) != 0 {
		t.Errorf("local store = %v, want empty — enforce must not book what the fleet cannot see", r)
	}
	if got := reason(reasonUnaccountable) - unaccBefore; got != 1 {
		t.Errorf("reason=%s rose by %v, want 1", reasonUnaccountable, got)
	}
	if got := reason("capacity") - capBefore; got != 0 {
		t.Errorf("reason=capacity rose by %v; a broken authority is not a full node", got)
	}
	if events := drainEvents(rec); !hasEventContaining(events, "ReserveUnaccountable") {
		t.Errorf("events = %v, want ReserveUnaccountable", events)
	}
}

// TestPrescheduledBlocksSecondPrescheduled proves the capacity gate reads reservations:
// 4 cores/NUMA, each span-2 6-core pod needs 3/NUMA, so the second is denied.
func TestPrescheduledBlocksSecondPrescheduled(t *testing.T) {
	m, rec := coldStartMutator(t, "enforce", topoNode("n1", 8, 2))

	first := admitPrescheduled(t, m, "daemon-a", "6", "2", "", "n1", false)
	if !first.Allowed {
		t.Fatalf("first pre-scheduled pod must be admitted (3/NUMA fits 4/NUMA), got denied: %q", first.Result.Message)
	}
	second := admitPrescheduled(t, m, "daemon-b", "6", "2", "", "n1", false)
	if second.Allowed {
		t.Fatalf("second pre-scheduled pod must be DENIED — the first reserved 3 of 4 cores/NUMA and this needs 3 more; got Allowed, so capacityFits gated against room the first pod already holds")
	}
	events := drainEvents(rec)
	if !hasEventContaining(events, "CapacityCheckFailed") {
		t.Errorf("expected a CapacityCheckFailed event on the denied second pod, got %v", events)
	}
}

// TestPrescheduledBlocksSecondPrescheduledOnMemory is the memory twin: 100Gi/NUMA static
// floor, each 120Gi span-2 pod takes 60Gi/NUMA, so the second is denied.
func TestPrescheduledBlocksSecondPrescheduledOnMemory(t *testing.T) {
	node := withMemoryFloor(topoNode("n1", 64, 2), 100*gib)
	m, rec := coldStartMutator(t, "enforce", node)

	first := admitPrescheduled(t, m, "daemon-a", "4", "2", "120Gi", "n1", false)
	if !first.Allowed {
		t.Fatalf("first pre-scheduled pod must be admitted (60Gi/NUMA fits the 100Gi floor), got denied: %q", first.Result.Message)
	}
	second := admitPrescheduled(t, m, "daemon-b", "4", "2", "120Gi", "n1", false)
	if second.Allowed {
		t.Fatalf("second pre-scheduled pod must be DENIED — the first reserved 60Gi of the 100Gi/NUMA floor and this needs 60Gi more; got Allowed, so memoryCapacityCheck ignored the outstanding reservation")
	}
	if !hasEventContaining(drainEvents(rec), "CapacityCheckFailed") {
		t.Error("expected a CapacityCheckFailed event on the denied second pod")
	}
}

// TestPrescheduledDeniedWhenReserveCommitRejects proves enforce mode denies the loser of
// a commit race. The stub models the loser deterministically; a real race is usually
// caught by the gate instead.
func TestPrescheduledDeniedWhenReserveCommitRejects(t *testing.T) {
	m, rec := coldStartMutator(t, "enforce", topoNode("n1", 8, 2))

	m.SetReserver(&stubReserver{accepted: false})

	resp := admitPrescheduled(t, m, "daemon-a", "6", "2", "", "n1", false)
	if resp.Allowed {
		t.Fatal("a pod that lost the reserve commit must be DENIED in enforce mode; got Allowed, so it stays pinned to room another pod already holds")
	}
	if !hasEventContaining(drainEvents(rec), "CapacityCheckFailed") {
		t.Error("expected a CapacityCheckFailed event when the reserve commit was rejected")
	}
}

// TestPrescheduledConcurrentNotBothAdmitted proves under real concurrency that exactly
// one of two contending pods is admitted, whichever mechanism denies the other.
func TestPrescheduledConcurrentNotBothAdmitted(t *testing.T) {
	m, _ := coldStartMutator(t, "enforce", topoNode("n1", 8, 2))

	var wg sync.WaitGroup
	resps := make([]admission.Response, 2)
	names := []string{"daemon-a", "daemon-b"}
	wg.Add(2)
	for i := range names {
		go func(i int) {
			defer wg.Done()
			resps[i] = admitPrescheduled(t, m, names[i], "6", "2", "", "n1", false)
		}(i)
	}
	wg.Wait()

	admitted := 0
	for _, r := range resps {
		if r.Allowed {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("exactly one concurrent pre-scheduled admission may pass (each needs 3 of 4 cores/NUMA); got %d admitted — the loser of the reserve commit was not denied", admitted)
	}
}

// TestPrescheduledContendedWarnModeAdmits proves warn mode admits a pod whose commit
// rejects, and emits the warning exactly once although the gate and commit both see it.
func TestPrescheduledContendedWarnModeAdmits(t *testing.T) {
	const ns = "inference"
	warnCount := func() float64 {
		return testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues("n1", ns, "capacity", "warn"))
	}
	before := warnCount()

	m, rec := coldStartMutator(t, "warn", topoNode("n1", 8, 2))
	m.Reservations = NewReservationStore(0)
	if _, _, ok := m.Reservations.authoritativeReserveBest("", []ReserveRequest{{
		Node: "n1", PerNUMACores: 4, Span: 2,
		RawFree: map[int]int{0: 4, 1: 4},
	}}); !ok {
		t.Fatal("seeding a full-node reservation failed")
	}

	resp := admitPrescheduled(t, m, "daemon-a", "6", "2", "", "n1", false)
	if !resp.Allowed {
		t.Fatalf("warn mode must never deny, got denied: %q", resp.Result.Message)
	}

	events := drainEvents(rec)
	warnings := 0
	for _, e := range events {
		if strings.Contains(e, "CapacityCheckWarning") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("got %d CapacityCheckWarning events, want exactly 1 — the gate and the reserve commit must not both report one pod's shortfall; events: %v", warnings, events)
	}
	if got := warnCount() - before; got != 1 {
		t.Errorf("capacity_check_failures{result=warn} rose by %v, want 1 — a double increment doubles the alerting counter", got)
	}
}

// TestPrescheduledConcurrentMemoryNotBothAdmitted is the memory twin of
// TestPrescheduledConcurrentNotBothAdmitted: 100Gi/NUMA floor, two 120Gi span-2 pods.
func TestPrescheduledConcurrentMemoryNotBothAdmitted(t *testing.T) {
	node := withMemoryFloor(topoNode("n1", 64, 2), 100*gib)
	m, _ := coldStartMutator(t, "enforce", node)

	var wg sync.WaitGroup
	resps := make([]admission.Response, 2)
	names := []string{"daemon-a", "daemon-b"}
	wg.Add(2)
	for i := range names {
		go func(i int) {
			defer wg.Done()
			resps[i] = admitPrescheduled(t, m, names[i], "4", "2", "120Gi", "n1", false)
		}(i)
	}
	wg.Wait()

	admitted := 0
	for _, r := range resps {
		if r.Allowed {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("exactly one of two concurrent 60Gi/NUMA pre-scheduled pods may pass a 100Gi/NUMA floor; got %d admitted — the reserve commit skipped its memory re-check because MemChecked was not set from the static floor", admitted)
	}
}

// TestPrescheduledWarnCountsMemoryShortfallOnce proves a memory shortfall seen by the
// gate is reported once and the commit (no MemChecked) still records CPU and bytes.
func TestPrescheduledWarnCountsMemoryShortfallOnce(t *testing.T) {
	const ns = "inference"
	memWarn := func() float64 {
		return testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues("n1", ns, "memory", "warn"))
	}
	before := memWarn()

	m, rec := coldStartMutator(t, "warn", withMemoryFloor(topoNode("n1", 64, 2), 100*gib))
	if first := admitPrescheduled(t, m, "daemon-a", "4", "2", "120Gi", "n1", false); !first.Allowed {
		t.Fatalf("first pod must be admitted, got denied: %q", first.Result.Message)
	}
	drainEvents(rec) // discard the first pod's events; only the second is under test

	second := admitPrescheduled(t, m, "daemon-b", "4", "2", "120Gi", "n1", false)
	if !second.Allowed {
		t.Fatalf("warn mode must never deny, got denied: %q", second.Result.Message)
	}

	events := drainEvents(rec)
	warnings := 0
	for _, e := range events {
		if strings.Contains(e, "CapacityCheckWarning") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("got %d CapacityCheckWarning events, want exactly 1 — the memory gate and the reserve commit both reported one memory shortfall; events: %v", warnings, events)
	}
	if got := memWarn() - before; got != 1 {
		t.Errorf("capacity_check_failures{reason=memory,result=warn} rose by %v, want 1", got)
	}
}

// seedingReserver commits a competing reservation on its first call, then delegates to
// the real store, so the commit rejects after the gate saw room.
type seedingReserver struct {
	store *ReservationStore
	seed  ReserveRequest
	done  bool
}

func (s *seedingReserver) reserveBest(_ context.Context, _ string, reqs []ReserveRequest) (string, []int, bool, error) {
	if !s.done {
		s.done = true
		if _, _, ok := s.store.authoritativeReserveBest("", []ReserveRequest{s.seed}); !ok {
			panic("seedingReserver: seed reservation did not fit")
		}
	}
	node, numas, ok := s.store.authoritativeReserveBest("", reqs)
	return node, numas, ok, nil
}

// TestPrescheduledCommitRejectsOnMemoryAndSaysSo proves that on a cold-start node (static
// memory floor, no live data) the commit re-checks memory and attributes the reject to
// memory, not CPU. The seed takes 60Gi/NUMA of 100Gi between gate and commit.
func TestPrescheduledCommitRejectsOnMemoryAndSaysSo(t *testing.T) {
	const ns = "inference"
	memBefore := testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues("n1", ns, "memory", "enforce"))
	cpuBefore := testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues("n1", ns, "capacity", "enforce"))

	m, _ := coldStartMutator(t, "enforce", withMemoryFloor(topoNode("n1", 64, 2), 100*gib))
	m.Reservations = NewReservationStore(0)
	m.SetReserver(&seedingReserver{store: m.Reservations, seed: ReserveRequest{
		Node: "n1", PerNUMACores: 1, PerNUMAMem: 60 * gib,
		Span: 2, MemBytes: 120 * gib, RawFree: map[int]int{0: 32, 1: 32},
		MemChecked: true, MemFree: map[int]int64{0: 100 * gib, 1: 100 * gib},
	}})

	resp := admitPrescheduled(t, m, "daemon-a", "4", "2", "120Gi", "n1", false)
	if resp.Allowed {
		t.Fatal("pod must be DENIED: 60Gi/NUMA does not fit the 40Gi left after the competing reservation — got Allowed, so the authority skipped its memory re-check (MemChecked unset for want of the static-floor fallback)")
	}
	if !strings.Contains(resp.Result.Message, "memory request") {
		t.Errorf("denial must name memory as the cause, got %q", resp.Result.Message)
	}
	if got := testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues("n1", ns, "memory", "enforce")) - memBefore; got != 1 {
		t.Errorf("capacity_check_failures{reason=memory} rose by %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues("n1", ns, "capacity", "enforce")) - cpuBefore; got != 0 {
		t.Errorf("capacity_check_failures{reason=capacity} rose by %v, want 0 — a memory denial was attributed to CPU", got)
	}
}

// TestPrescheduledPicksNUMAsNetOfReservations proves NUMAs are chosen from free room net
// of reservations: NUMA 0 is fully reserved, so a span-1 pod must land on NUMA 1.
func TestPrescheduledPicksNUMAsNetOfReservations(t *testing.T) {
	m, _ := coldStartMutator(t, "enforce", topoNode("n1", 8, 2))
	m.Reservations = NewReservationStore(0)

	if _, _, ok := m.Reservations.authoritativeReserveBest("", []ReserveRequest{{
		Node:         "n1",
		PerNUMACores: 4,
		Span:         1,
		RawFree:      map[int]int{0: 4},
	}}); !ok {
		t.Fatal("seeding a reservation on NUMA 0 failed")
	}

	resp := admitPrescheduled(t, m, "daemon-a", "3", "1", "", "n1", false)
	if !resp.Allowed {
		t.Fatalf("pre-scheduled pod must be admitted, got denied: %q", resp.Result.Message)
	}

	got := reservedCores(m.Reservations, "n1")
	if got[1] != 3 {
		t.Fatalf("reserved = %v, want 3 cores on the un-reserved NUMA 1 — NUMAs were picked from raw free room, so the reserve landed on (and failed against) the contended NUMA 0", got)
	}
	if got[0] != 4 {
		t.Errorf("reserved = %v, want NUMA 0 left at the seeded 4", got)
	}
}

// TestPrescheduledNodeWithoutCapacityDataSkipsReserve proves a node with no live topology
// and no capacity labels skips the reserve and logs it instead of panicking.
func TestPrescheduledNodeWithoutCapacityDataSkipsReserve(t *testing.T) {
	// Balloons-enabled and max-span only: no capacity labels.
	bare := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "bare", Labels: map[string]string{
		controller.LabelBalloonsEnabled: "true",
		controller.LabelMaxSpan:         "2",
	}}}
	m, _ := coldStartMutator(t, "enforce", bare)
	var logged []string
	m.Log = logr.New(funcr.NewJSON(func(line string) { logged = append(logged, line) }, funcr.Options{}).GetSink())

	resp := admitPrescheduled(t, m, "daemon-a", "6", "2", "", "bare", false)
	if !resp.Allowed {
		t.Fatalf("a pre-scheduled pod on a node with no capacity data must still be admitted, got denied: %q", resp.Result.Message)
	}

	if r := reservedCores(m.Reservations, "bare"); len(r) != 0 {
		t.Errorf("expected no reservation on a node with no per-NUMA data, got %v", r)
	}

	if !hasLogContaining(logged, "no per-NUMA capacity resolved") {
		t.Errorf("the skipped reservation must be logged, got %v", logged)
	}
}

// TestPrescheduledOffModeAccountsAsItsSteeredTwin proves off mode records the same
// reservation as the steered path when memory does not fit (it opted out of memory).
func TestPrescheduledOffModeAccountsAsItsSteeredTwin(t *testing.T) {
	bound, _ := liveMutatorFor(t, "off", withMemoryFloor(topoNode("n1", 8, 2), 4*gib))
	if resp := admitPrescheduled(t, bound, "daemon-a", "6", "2", "100Gi", "n1", false); !resp.Allowed {
		t.Fatalf("off mode must never deny the bound pod, got %q", resp.Result.Message)
	}
	steered, _ := liveMutatorFor(t, "off", withMemoryFloor(topoNode("n1", 8, 2), 4*gib))
	if resp := admitPodMem(t, steered, "vllm-a", "6", "2", "100Gi"); !resp.Allowed {
		t.Fatalf("off mode must never deny the steered twin, got %q", resp.Result.Message)
	}

	wantCores, wantMem := reservedCores(steered.Reservations, "n1"), reservedMem(steered.Reservations, "n1")
	if len(wantCores) == 0 || len(wantMem) == 0 {
		t.Fatalf("the steered twin booked nothing (cores %v, memory %v), so there is nothing to compare against", wantCores, wantMem)
	}
	if got := reservedCores(bound.Reservations, "n1"); !maps.Equal(got, wantCores) {
		t.Errorf("bound pod reserved cores %v, its steered twin %v", got, wantCores)
	}
	if got := reservedMem(bound.Reservations, "n1"); !maps.Equal(got, wantMem) {
		t.Errorf("bound pod reserved memory %v, its steered twin %v", got, wantMem)
	}
}

// liveMutatorFor is liveHandleMutator with the node supplied.
func liveMutatorFor(t *testing.T, mode string, node *corev1.Node) (*PodMutator, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	topo := topology.NewCache()
	topo.Set(node.Name, fourCorePerNUMATopo())
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, nrtObj(node.Name)).Build()
	return newHandleMutatorFromClient(t, mode, c, topo)
}

// TestPrescheduledWarnModeAccountsAfterMemoryShortfall proves warn mode still records the
// reservation after reporting a memory shortfall.
func TestPrescheduledWarnModeAccountsAfterMemoryShortfall(t *testing.T) {
	m, rec := coldStartMutator(t, "warn", withMemoryFloor(topoNode("n1", 8, 2), 4*gib))

	if resp := admitPrescheduled(t, m, "daemon-a", "6", "2", "100Gi", "n1", false); !resp.Allowed {
		t.Fatalf("warn mode must never deny, got %q", resp.Result.Message)
	}
	if !hasEventContaining(drainEvents(rec), "CapacityCheckWarning") {
		t.Error("expected the memory gate's CapacityCheckWarning event")
	}
	if got := reservedCores(m.Reservations, "n1")[0]; got != 3 {
		t.Errorf("NUMA 0 reserved cores = %d, want 3 (the warning must not cost the accounting)", got)
	}
}

func hasLogContaining(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// TestPrescheduledWarnCommitRejectsOnMemoryReportsIt proves warn mode reports, as memory,
// a shortfall that only the commit observed.
func TestPrescheduledWarnCommitRejectsOnMemoryReportsIt(t *testing.T) {
	const ns = "inference"
	memBefore := testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues("n1", ns, "memory", "warn"))
	cpuBefore := testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues("n1", ns, "capacity", "warn"))

	m, rec := coldStartMutator(t, "warn", withMemoryFloor(topoNode("n1", 64, 2), 100*gib))
	m.Reservations = NewReservationStore(0)
	m.SetReserver(&seedingReserver{store: m.Reservations, seed: ReserveRequest{
		Node: "n1", PerNUMACores: 1, PerNUMAMem: 60 * gib,
		Span: 2, MemBytes: 120 * gib, RawFree: map[int]int{0: 32, 1: 32},
		MemChecked: true, MemFree: map[int]int64{0: 100 * gib, 1: 100 * gib},
	}})

	if resp := admitPrescheduled(t, m, "daemon-a", "4", "2", "120Gi", "n1", false); !resp.Allowed {
		t.Fatalf("warn mode must never deny, got %q", resp.Result.Message)
	}
	if got := testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues("n1", ns, "memory", "warn")) - memBefore; got != 1 {
		t.Errorf("capacity_check_failures{reason=memory,result=warn} rose by %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues("n1", ns, "capacity", "warn")) - cpuBefore; got != 0 {
		t.Errorf("capacity_check_failures{reason=capacity,result=warn} rose by %v, want 0 — a memory shortfall attributed to CPU", got)
	}
	if !hasEventContaining(drainEvents(rec), "memory concern") {
		t.Error("expected a CapacityCheckWarning naming memory")
	}
}
