// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	admissionv1 "k8s.io/api/admission/v1"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// newHandleMutator builds a PodMutator for full Handle() admission with one rule pinning
// container "vllm" in namespace "inference". TopoCache is nil, so the unscheduled path
// takes the static fallback.
func newHandleMutator(t *testing.T, mode string, nodes ...runtime.Object) (*PodMutator, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	return newHandleMutatorFromClient(t, mode, c, nil)
}

// newHandleMutatorFromClient takes a pre-seeded client and optional topology cache; a
// non-nil topo also wires a fresh ReservationStore for the live steering path.
func newHandleMutatorFromClient(t *testing.T, mode string, c client.Client, topo *topology.Cache) (*PodMutator, *record.FakeRecorder) {
	t.Helper()
	return newHandleMutatorWithFeatures(t, mode, nil, c, topo)
}

// newHandleMutatorWithFeatures is newHandleMutatorFromClient with nodeFeatures on the rule.
func newHandleMutatorWithFeatures(t *testing.T, mode string, nodeFeatures map[string]cpopv1alpha1.NodeFeatureMode, c client.Client, topo *topology.Cache) (*PodMutator, *record.FakeRecorder) {
	t.Helper()
	return newHandleMutatorFull(t, mode, true, nodeFeatures, c, topo)
}

// newHandleMutatorEnforce is newHandleMutator with enforcePinning selectable.
func newHandleMutatorEnforce(t *testing.T, enforce bool, nodes ...runtime.Object) (*PodMutator, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	return newHandleMutatorFull(t, "warn", enforce, nil, c, nil)
}

// newHandleMutatorFull is the general builder.
func newHandleMutatorFull(t *testing.T, mode string, enforce bool, nodeFeatures map[string]cpopv1alpha1.NodeFeatureMode, c client.Client, topo *topology.Cache) (*PodMutator, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)

	idx := match.NewIndex()
	rules, err := match.CompileIntent(cpopv1alpha1.IntentSpec{
		Span: cpopv1alpha1.IntentSourceSpec{
			Annotation: "cpu-pinning-operator.intel.com/tp",
			Default:    1,
		},
	})
	if err != nil {
		t.Fatalf("compile intent: %v", err)
	}
	idx.Update([]match.PolicyRule{{
		Namespaces:        []string{"inference"},
		ByContainerName:   []string{"vllm"},
		EnforcePinning:    enforce,
		Intent:            rules,
		CapacityCheckMode: mode,
		NodeFeatures:      nodeFeatures,
	}})

	rec := record.NewFakeRecorder(16)
	m := &PodMutator{Client: c, PolicyIndex: idx, Recorder: rec}
	if topo != nil {
		m.TopoCache = topo
		m.Reservations = NewReservationStore(0)
	}
	if err := m.InjectDecoder(admission.NewDecoder(scheme)); err != nil {
		t.Fatalf("inject decoder: %v", err)
	}
	return m, rec
}

// admitPod runs a full CREATE admission for a whole-core "vllm" pod with a `-tp` span.
func admitPod(t *testing.T, m *PodMutator, name string, cores, tp string) admission.Response {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "inference",
			Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": tp},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "vllm",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cores)},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cores)},
			},
		}}},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	return m.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}),
		Operation: admissionv1.Create,
		Namespace: pod.Namespace, // as the apiserver sets it
		Object:    runtime.RawExtension{Raw: raw},
	}})
}

// admitPodMem is admitPod plus a memory request.
func admitPodMem(t *testing.T, m *PodMutator, name, cores, tp, mem string) admission.Response {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "inference",
			Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": tp},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "vllm",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cores),
					corev1.ResourceMemory: resource.MustParse(mem),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cores),
					corev1.ResourceMemory: resource.MustParse(mem),
				},
			},
		}}},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	return m.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}),
		Operation: admissionv1.Create,
		Namespace: pod.Namespace, // as the apiserver sets it
		Object:    runtime.RawExtension{Raw: raw},
	}})
}

// drainEvents collects any events emitted so far without blocking.
func drainEvents(rec *record.FakeRecorder) []string {
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

// TestHandleTopologyGateEnforceDenies proves enforce denies a span-4 pod on a max-span-2
// fleet with a CapacityCheckFailed event instead of pinning it at span 2.
func TestHandleTopologyGateEnforceDenies(t *testing.T) {
	m, rec := newHandleMutator(t, "enforce",
		labeledNode("small-a", true, "2"),
		labeledNode("small-b", true, "2"),
	)
	resp := admitPod(t, m, "vllm-wide", "4", "4")
	if resp.Allowed {
		t.Fatalf("span-4 pod on a max-span-2 fleet must be denied under enforce, got Allowed")
	}
	// Exact match: the client message carries only the pod's requested span, never the
	// fleet's max span.
	want := deniedCapacity("the requested span 4 is not supported").Result.Message
	if msg := resp.Result.Message; msg != want {
		t.Errorf("deny message = %q, want exactly %q", msg, want)
	}
	events := drainEvents(rec)
	if !hasEventContaining(events, "CapacityCheckFailed") {
		t.Errorf("expected a CapacityCheckFailed event, got %v", events)
	}
	if !hasEventContaining(events, "span 2") {
		t.Errorf("the CapacityCheckFailed event should carry the fleet detail, got %v", events)
	}
}

// TestHandleTopologyGateWarnPinsCapped proves warn mode pins at the capped span with a
// CapacityCheckWarning.
func TestHandleTopologyGateWarnPinsCapped(t *testing.T) {
	m, rec := newHandleMutator(t, "warn",
		labeledNode("small-a", true, "2"),
	)
	resp := admitPod(t, m, "vllm-wide", "4", "4")
	if !resp.Allowed {
		t.Fatalf("warn mode must admit, got denied: %q", resp.Result.Message)
	}
	events := drainEvents(rec)
	if !hasEventContaining(events, "CapacityCheckWarning") {
		t.Errorf("expected a CapacityCheckWarning event citing the capped span, got %v", events)
	}
}

// TestHandleTopologyGateFitsNoEvent proves a fitting span emits no event.
func TestHandleTopologyGateFitsNoEvent(t *testing.T) {
	m, rec := newHandleMutator(t, "enforce",
		labeledNode("small-a", true, "2"),
	)
	resp := admitPod(t, m, "vllm-ok", "4", "2")
	if !resp.Allowed {
		t.Fatalf("span-2 pod on a max-span-2 node must be admitted, got %q", resp.Result.Message)
	}
	events := drainEvents(rec)
	if hasEventContaining(events, "CapacityCheckFailed") || hasEventContaining(events, "CapacityCheckWarning") {
		t.Errorf("a topology-fitting pod must not emit a capacity-check event, got %v", events)
	}
}

// TestHandleTopologyGateLabelLessFleetDenies proves nodes without a parseable max-span
// label count as span 1, so a span-4 pod is denied rather than admitted and stranded.
func TestHandleTopologyGateLabelLessFleetDenies(t *testing.T) {
	m, rec := newHandleMutator(t, "enforce",
		labeledNode("nolabel-a", true, ""), // balloons-enabled, no max-span label
		labeledNode("nolabel-b", true, ""),
	)
	resp := admitPod(t, m, "vllm-wide", "4", "4")
	if resp.Allowed {
		t.Fatalf("span-4 pod on a label-less fleet must be denied (conservative span-1 floor), got Allowed")
	}
	events := drainEvents(rec)
	if !hasEventContaining(events, "CapacityCheckFailed") {
		t.Errorf("expected CapacityCheckFailed on the label-less fleet, got %v", events)
	}
}

// TestHandleMemoryGateEnforceDenies proves enforce denies a pod that fits CPU but not
// memory: 100 GiB/NUMA floor at span 1 against a 200 GiB request.
func TestHandleMemoryGateEnforceDenies(t *testing.T) {
	const floor = int64(100) << 30
	m, rec := newHandleMutator(t, "enforce",
		withMemoryFloor(topoNode("n1", 64, 2), floor),
	)
	resp := admitPodMem(t, m, "vllm-mem", "4", "1", "200Gi")
	if resp.Allowed {
		t.Fatalf("200GiB pod on a 100GiB/NUMA span-1 fleet must be denied under enforce, got Allowed")
	}
	if !strings.Contains(resp.Result.Message, "memory request") {
		t.Errorf("deny message should cite the memory request, got %q", resp.Result.Message)
	}
	if !hasEventContaining(drainEvents(rec), "CapacityCheckFailed") {
		t.Error("expected a CapacityCheckFailed event for the memory gate")
	}
}

// TestCapacityDenialDisclosesNothingAboutTheFleet proves the client-visible denial names
// only the pod's own request; node and fleet detail stay in the Event and log.
func TestCapacityDenialDisclosesNothingAboutTheFleet(t *testing.T) {
	const floor = int64(100) << 30
	const nodeName = "shared-worker-07"

	t.Run("unscheduled: fleet figures stay in the Event", func(t *testing.T) {
		m, rec := newHandleMutator(t, "enforce",
			withMemoryFloor(topoNode(nodeName, 64, 2), floor),
		)
		resp := admitPodMem(t, m, "vllm-mem", "4", "1", "200Gi")
		if resp.Allowed {
			t.Fatalf("200GiB pod on a 100GiB/NUMA fleet must be denied under enforce, got Allowed")
		}
		msg := resp.Result.Message
		for _, leaked := range []string{nodeName, "balloons-enabled", "candidate", "best per-NUMA"} {
			if strings.Contains(msg, leaked) {
				t.Errorf("deny message discloses %q: %q", leaked, msg)
			}
		}
		// The detail withheld from the client must reach the operator.
		if !hasEventContaining(drainEvents(rec), "balloons-enabled") {
			t.Error("the CapacityCheckFailed event should carry the fleet-level detail")
		}
	})

	t.Run("pre-scheduled: the node name stays in the Event", func(t *testing.T) {
		m, rec := newHandleMutator(t, "enforce",
			withMemoryFloor(topoNode(nodeName, 64, 2), floor),
		)
		resp := admitPrescheduled(t, m, "daemon-a", "4", "1", "200Gi", nodeName, false)
		if resp.Allowed {
			t.Fatalf("200GiB pre-scheduled pod on a 100GiB/NUMA node must be denied under enforce, got Allowed")
		}
		if strings.Contains(resp.Result.Message, nodeName) {
			t.Errorf("deny message discloses the node name: %q", resp.Result.Message)
		}
		if !hasEventContaining(drainEvents(rec), nodeName) {
			t.Error("the CapacityCheckFailed event should name the node for the operator")
		}
	})
}

// TestHandleMemoryGateWarnPins proves warn mode admits an over-capacity request with a warning.
func TestHandleMemoryGateWarnPins(t *testing.T) {
	const floor = int64(100) << 30
	m, rec := newHandleMutator(t, "warn",
		withMemoryFloor(topoNode("n1", 64, 2), floor),
	)
	resp := admitPodMem(t, m, "vllm-mem", "4", "1", "200Gi")
	if !resp.Allowed {
		t.Fatalf("warn mode must admit, got denied: %q", resp.Result.Message)
	}
	if !hasEventContaining(drainEvents(rec), "CapacityCheckWarning") {
		t.Error("expected a CapacityCheckWarning event citing the memory concern")
	}
}

// TestHandleMemoryGateFitsNoEvent proves a fitting request emits no event: 150 GiB within
// 2 x 100 GiB.
func TestHandleMemoryGateFitsNoEvent(t *testing.T) {
	const floor = int64(100) << 30
	m, rec := newHandleMutator(t, "enforce",
		withMemoryFloor(topoNode("n1", 64, 2), floor),
	)
	resp := admitPodMem(t, m, "vllm-mem", "4", "2", "150Gi")
	if !resp.Allowed {
		t.Fatalf("150GiB span-2 pod on a 100GiB/NUMA fleet must be admitted, got %q", resp.Result.Message)
	}
	if hasEventContaining(drainEvents(rec), "CapacityCheckFailed") {
		t.Error("a memory-fitting pod must not emit a CapacityCheckFailed event")
	}
}

// TestHandleMemoryGateNoRequestSkips proves a pod without a memory request skips the
// memory dimension.
func TestHandleMemoryGateNoRequestSkips(t *testing.T) {
	const floor = int64(1) << 30 // tiny floor so any request would fail if checked
	m, _ := newHandleMutator(t, "enforce",
		withMemoryFloor(topoNode("n1", 64, 2), floor),
	)
	resp := admitPod(t, m, "vllm-nomem", "4", "1") // admitPod sets no memory request
	if !resp.Allowed {
		t.Fatalf("a pod with no memory request must not be blocked by the memory gate, got %q", resp.Result.Message)
	}
}

// TestHandleMemoryGateWarnStillSteersCPU proves a failed memory gate in warn mode does not
// stop CPU headroom steering; a reservation is the proof, as the static fallback never
// reserves.
func TestHandleMemoryGateWarnStillSteersCPU(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	// Live CPU data but a 1 GiB memory floor, so the memory gate fails.
	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	topo.Set("b", simpleTopo())
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		withMemoryFloor(topoNode("a", 4, 2), 1<<30), nrtObj("a"),
		withMemoryFloor(topoNode("b", 4, 2), 1<<30), nrtObj("b"),
	).Build()
	m, rec := newHandleMutatorFromClient(t, "warn", c, topo)

	resp := admitPodMem(t, m, "vllm-mem", "1", "1", "50Gi")
	if !resp.Allowed {
		t.Fatalf("warn mode must admit, got denied: %q", resp.Result.Message)
	}
	if !hasEventContaining(drainEvents(rec), "CapacityCheckWarning") {
		t.Error("expected a memory CapacityCheckWarning")
	}

	if len(reservedCores(m.Reservations, "a")) == 0 && len(reservedCores(m.Reservations, "b")) == 0 {
		t.Error("expected a CPU headroom reservation on a or b — steering was dropped")
	}
}

// TestHandleEnforceUnscheduledLiveMemoryExhausted proves enforce denies a pod that passes
// the static memory and CPU checks but not live per-NUMA memory.
func TestHandleEnforceUnscheduledLiveMemoryExhausted(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})

	const gib = int64(1) << 30
	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	topo.SetMemory("a", map[int]int64{0: 250 * gib, 1: 250 * gib})
	// An incumbent pins 240Gi/NUMA, leaving 10Gi live free; CPU and the 250Gi static
	// floor stay roomy.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		withMemoryFloor(withMaxSpan(topoNode("a", 4, 2), 2), 250*gib),
		nrtObj("a", nrtZoneMem("inference/incumbent/vllm", "balloon-span2[0]", "0", "0-1")),
		pinnedMemPod("incumbent", 480),
	).Build()
	m, rec := newHandleMutatorFromClient(t, "enforce", c, topo)

	// 50Gi/NUMA share against 10Gi live free.
	resp := admitPodMem(t, m, "vllm-new", "2", "2", "100Gi")
	if resp.Allowed {
		t.Fatalf("enforce must DENY a pod that fits CPU + static memory but not live per-NUMA memory; got allowed")
	}
	if !hasEventContaining(drainEvents(rec), "CapacityCheckFailed") {
		t.Error("expected a CapacityCheckFailed event for the live memory rejection")
	}
}

// TestHandleOffModeAccountsMemoryEndToEnd proves off mode, derived in handle(), still
// records memory in the reservation and does not filter candidates on memory.
func TestHandleOffModeAccountsMemoryEndToEnd(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})

	const gib = int64(1) << 30
	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	// Memory far too small: off mode must not filter on it.
	topo.SetMemory("a", map[int]int64{0: 10 * gib, 1: 10 * gib})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		withMemoryFloor(withMaxSpan(topoNode("a", 4, 2), 2), 10*gib),
		nrtObj("a"),
	).Build()
	m, _ := newHandleMutatorFromClient(t, "off", c, topo)

	resp := admitPodMem(t, m, "batch-pod", "2", "2", "100Gi")
	if !resp.Allowed {
		t.Fatalf("off mode must admit, got denied: %q", resp.Result.Message)
	}

	mem := reservedMem(m.Reservations, "a")
	if len(mem) == 0 {
		t.Fatalf("off mode recorded no memory on 'a' — a 100Gi pod booked a CPU-only reservation, so the next admission is sized against memory already committed")
	}
	for numa, got := range mem {
		if got != 50*gib {
			t.Errorf("reserved memory on NUMA %d = %d, want %d (ceil(100Gi/span 2))", numa, got, 50*gib)
		}
	}
	if len(reservedCores(m.Reservations, "a")) == 0 {
		t.Error("expected a CPU reservation as well")
	}
}

// TestHandleWarnUnscheduledLiveMemoryExhausted is the warn twin: admitted with a memory
// warning, and CPU steering still records a reservation.
func TestHandleWarnUnscheduledLiveMemoryExhausted(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})

	const gib = int64(1) << 30
	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	topo.SetMemory("a", map[int]int64{0: 250 * gib, 1: 250 * gib})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		withMemoryFloor(withMaxSpan(topoNode("a", 4, 2), 2), 250*gib),
		nrtObj("a", nrtZoneMem("inference/incumbent/vllm", "balloon-span2[0]", "0", "0-1")),
		pinnedMemPod("incumbent", 480),
	).Build()
	m, rec := newHandleMutatorFromClient(t, "warn", c, topo)

	resp := admitPodMem(t, m, "vllm-new", "2", "2", "100Gi")
	if !resp.Allowed {
		t.Fatalf("warn mode must admit, got denied: %q", resp.Result.Message)
	}
	if !hasEventContaining(drainEvents(rec), "CapacityCheckWarning") {
		t.Error("expected a memory CapacityCheckWarning in warn mode")
	}
	// memGateFailed disables the memory filter, not accounting.
	if len(reservedCores(m.Reservations, "a")) == 0 {
		t.Error("expected a CPU headroom reservation — best-effort steering was dropped")
	}
	// It must still reserve the real memory demand, 50Gi per NUMA.
	if mem := reservedMem(m.Reservations, "a"); len(mem) == 0 {
		t.Error("expected the reservation to record memory demand even after memGateFailed; got none")
	} else {
		for numa, b := range mem {
			if b != 50*gib {
				t.Errorf("reservedMem[%d] = %d, want %d (ceil(100Gi/2))", numa, b, 50*gib)
			}
		}
	}
}

// admitPodDryRun is admitPod with AdmissionRequest.DryRun set.
func admitPodDryRun(t *testing.T, m *PodMutator, name, cores, tp string, dryRun bool) admission.Response {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "inference",
			Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": tp},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "vllm",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cores)},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cores)},
			},
		}}},
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

// TestHandleDryRunDoesNotConsumeReservation proves a dry-run CREATE is still mutated but
// records no reservation, while the identical real admission does.
func TestHandleDryRunDoesNotConsumeReservation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})

	topo := topology.NewCache()
	topo.Set("a", simpleTopo())
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		topoNode("a", 4, 2), nrtObj("a"),
	).Build()
	m, _ := newHandleMutatorFromClient(t, "enforce", c, topo)

	resp := admitPodDryRun(t, m, "vllm-dry", "2", "1", true)
	if !resp.Allowed {
		t.Fatalf("dry-run admission must be allowed, got denied: %q", resp.Result.Message)
	}
	if stampedBalloon(resp) == "" {
		t.Error("dry-run response must still stamp the balloon annotation so the preview is accurate")
	}
	if got := reservedCores(m.Reservations, "a"); len(got) != 0 {
		t.Fatalf("dry-run consumed a reservation slot: reservedCores=%v, want none (sideEffects: None violated)", got)
	}

	resp = admitPodDryRun(t, m, "vllm-real", "2", "1", false)
	if !resp.Allowed {
		t.Fatalf("non-dry-run admission must be allowed, got denied: %q", resp.Result.Message)
	}
	if got := reservedCores(m.Reservations, "a"); len(got) == 0 {
		t.Fatalf("non-dry-run admission did not reserve: reservedCores=%v, want a per-NUMA reservation", got)
	}
}

// admitDevicePod is admitPod plus one unit of deviceResource.
func admitDevicePod(t *testing.T, m *PodMutator, name, cores, tp, deviceResource string) admission.Response {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "inference",
			Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": tp},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "vllm",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:                  resource.MustParse(cores),
					corev1.ResourceName(deviceResource): resource.MustParse("1"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:                  resource.MustParse(cores),
					corev1.ResourceName(deviceResource): resource.MustParse("1"),
				},
			},
		}}},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	return m.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}),
		Operation: admissionv1.Create,
		Namespace: pod.Namespace, // as the apiserver sets it
		Object:    runtime.RawExtension{Raw: raw},
	}})
}

// stampedBalloon returns the balloon type the response's patch stamped, or "".
func stampedBalloon(resp admission.Response) string {
	for _, p := range resp.Patches {
		if strings.Contains(p.Path, "balloon.balloons.resource-policy.nri.io") {
			if s, ok := p.Value.(string); ok {
				return s
			}
		}
	}
	return ""
}

// TestHandleDevicePodRoutesToDeviceBalloon proves a GPU pod goes to balloon-device,
// ignoring its tp-4 annotation.
func TestHandleDevicePodRoutesToDeviceBalloon(t *testing.T) {
	m, _ := newHandleMutator(t, "enforce",
		labeledNode("wide", true, "8"),
	)
	resp := admitDevicePod(t, m, "vllm-gpu", "4", "4", "nvidia.com/gpu")
	if !resp.Allowed {
		t.Fatalf("device pod must be admitted, got denied: %q", resp.Result.Message)
	}
	if got := stampedBalloon(resp); got != "balloon-device" {
		t.Errorf("device pod stamped %q, want balloon-device (intent tp=4 must be ignored)", got)
	}
}

// TestHandleNonGPUDevicePodRoutesToDeviceBalloon proves the same for a non-GPU (FPGA)
// extended resource.
func TestHandleNonGPUDevicePodRoutesToDeviceBalloon(t *testing.T) {
	m, _ := newHandleMutator(t, "enforce",
		labeledNode("wide", true, "8"),
	)
	resp := admitDevicePod(t, m, "vllm-fpga", "4", "4", "example.com/fpga")
	if !resp.Allowed {
		t.Fatalf("device pod must be admitted, got denied: %q", resp.Result.Message)
	}
	if got := stampedBalloon(resp); got != "balloon-device" {
		t.Errorf("non-gpu device pod stamped %q, want balloon-device (intent tp=4 must be ignored)", got)
	}
}

// TestHandleNonDevicePodKeepsIntent proves a non-device tp-4 pod keeps its span-4 balloon.
func TestHandleNonDevicePodKeepsIntent(t *testing.T) {
	m, _ := newHandleMutator(t, "enforce",
		labeledNode("wide", true, "8"),
	)
	resp := admitPod(t, m, "vllm-cpu", "4", "4")
	if !resp.Allowed {
		t.Fatalf("non-device pod must be admitted, got denied: %q", resp.Result.Message)
	}
	if got := stampedBalloon(resp); got != "balloon-span4" {
		t.Errorf("non-device tp=4 pod stamped %q, want balloon-span4 (intent must still apply)", got)
	}
}

func hasEventContaining(events []string, substr string) bool {
	for _, e := range events {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

// patchesTouchInitContainers reports whether the patch touches /spec/initContainers,
// i.e. the wait-for-balloons init container was injected.
func patchesTouchInitContainers(resp admission.Response) bool {
	for _, p := range resp.Patches {
		if strings.HasPrefix(p.Path, "/spec/initContainers") {
			return true
		}
	}
	return false
}

// testInitImage stands in for INIT_CONTAINER_IMAGE (the manager's own image).
const testInitImage = "registry.example.com/cpu-pinning-operator:test"

// TestHandleInjectsInitContainerWhenEndpointSet proves the init container is injected with
// the configured endpoint and image.
func TestHandleInjectsInitContainerWhenEndpointSet(t *testing.T) {
	t.Setenv("BALLOONS_READY_ENDPOINT", "http://mgr-balloons-ready.op.svc:8082")
	t.Setenv("INIT_CONTAINER_IMAGE", testInitImage)

	m, _ := newHandleMutator(t, "warn",
		labeledNode("n1", true, "2"),
	)
	resp := admitPod(t, m, "vllm-ok", "2", "1")
	if !resp.Allowed || len(resp.Patches) == 0 {
		t.Fatalf("expected a pinned (patched, allowed) response, got allowed=%v patches=%d", resp.Allowed, len(resp.Patches))
	}
	if !patchesTouchInitContainers(resp) {
		t.Fatalf("expected the wait-for-balloons init container to be injected, got patches %+v", resp.Patches)
	}

	patched, err := json.Marshal(resp.Patches)
	if err != nil {
		t.Fatalf("marshal patches: %v", err)
	}
	for _, want := range []string{"--endpoint=http://mgr-balloons-ready.op.svc:8082", testInitImage} {
		if !strings.Contains(string(patched), want) {
			t.Errorf("the injected init container does not carry %q: %s", want, patched)
		}
	}
}

// TestHandleSkipsInitContainerWhenEndpointEmpty proves an empty endpoint still pins the pod
// but skips the init container, which would otherwise wedge in Init.
func TestHandleSkipsInitContainerWhenEndpointEmpty(t *testing.T) {
	t.Setenv("BALLOONS_READY_ENDPOINT", "")
	// Image set, so the empty endpoint is the only reason to drop.
	t.Setenv("INIT_CONTAINER_IMAGE", testInitImage)

	m, _ := newHandleMutator(t, "warn",
		labeledNode("n1", true, "2"),
	)
	resp := admitPod(t, m, "vllm-ok", "2", "1")
	if !resp.Allowed {
		t.Fatalf("empty endpoint must still admit the pod, got denied: %q", resp.Result.Message)
	}

	if len(resp.Patches) == 0 {
		t.Fatalf("expected the pod to still be pinned (annotation patch), got no patches")
	}

	if patchesTouchInitContainers(resp) {
		t.Errorf("empty endpoint must NOT inject the wait-for-balloons init container, got patches %+v", resp.Patches)
	}
}

// TestHandleSkipsInitContainerWhenImageEmpty proves an empty image skips the init
// container rather than guess one.
func TestHandleSkipsInitContainerWhenImageEmpty(t *testing.T) {
	// Endpoint set, so the empty image is the only reason to drop.
	t.Setenv("BALLOONS_READY_ENDPOINT", "http://mgr-balloons-ready.op.svc:8082")
	t.Setenv("INIT_CONTAINER_IMAGE", "")

	m, _ := newHandleMutator(t, "warn",
		labeledNode("n1", true, "2"),
	)
	resp := admitPod(t, m, "vllm-ok", "2", "1")
	if !resp.Allowed {
		t.Fatalf("an empty image must still admit the pod, got denied: %q", resp.Result.Message)
	}
	if stampedBalloon(resp) == "" {
		t.Error("the pod must still be pinned: no image is a reason to drop the gate, not the annotation")
	}
	if patchesTouchInitContainers(resp) {
		t.Errorf("an empty image must NOT inject the wait-for-balloons init container, got patches %+v", resp.Patches)
	}
}
