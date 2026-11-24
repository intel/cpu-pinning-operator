// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/intel/cpu-pinning-operator/internal/balloons"
	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/match"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// allocationNode is a balloons-enabled node in the given mode with 8 cores x 2
// threads on one NUMA: capacity 8 in physical-cores mode, 16 in hyperthreads.
func allocationNode(name, mode string) *corev1.Node {
	n := topoNode(name, 8, 1)
	n.Labels[controller.LabelThreadsPerCore] = "2"
	n.Labels[controller.LabelCPUAllocation] = mode
	return n
}

// patchedAnnotation returns the value the admission response stamps on one annotation.
func patchedAnnotation(t *testing.T, resp admission.Response, key string) string {
	t.Helper()
	path := "/metadata/annotations/" + strings.ReplaceAll(key, "/", "~1")
	for _, p := range resp.Patches {
		if p.Path != path {
			continue
		}
		value, ok := p.Value.(string)
		if !ok {
			t.Fatalf("annotation %q patched with %T, want a string", key, p.Value)
		}
		return value
	}
	t.Fatalf("no patch stamps annotation %q: %v", key, resp.Patches)
	return ""
}

// assertHashRecordsMode checks the stamped drift hash encodes mode for the stamped
// balloon, by comparing against that balloon's expected hash.
func assertHashRecordsMode(t *testing.T, resp admission.Response, mode, what string) string {
	t.Helper()
	balloon := patchedAnnotation(t, resp, balloonAnnotationBase+"/container.vllm")
	got := patchedAnnotation(t, resp, PinnedHashAnnotation)
	if want := match.PinnedConfigHash(balloon, mode); got != want {
		t.Errorf("drift hash %s records a mode other than %s (%q, want %q for balloon %q)",
			what, describeMode(mode), got, want, balloon)
	}
	return got
}

// respMessage is the response's message, empty for a response that carries none.
func respMessage(resp admission.Response) string {
	if resp.Result == nil {
		return ""
	}
	return resp.Result.Message
}

func describeMode(mode string) string {
	if mode == "" {
		return "an unresolved mode"
	}
	return mode
}

// TestBoundPodJudgedUnderItsOwnNodeMode proves a bound pod's capacity is judged in
// its node's mode, not the cluster default: 12 cores fit hyperthreads (16), not
// physical-cores (8).
func TestBoundPodJudgedUnderItsOwnNodeMode(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cluster      string
		nodeMode     string
		wantAdmitted bool
	}{
		{
			name:         "a hyperthreads node in a physical-cores cluster has the room",
			cluster:      balloons.CPUAllocationPhysicalCores,
			nodeMode:     hyperthreadsAllocation,
			wantAdmitted: true,
		},
		{
			name:         "a physical-cores node in a hyperthreads cluster does not",
			cluster:      hyperthreadsAllocation,
			nodeMode:     balloons.CPUAllocationPhysicalCores,
			wantAdmitted: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newHandleMutator(t, "enforce", allocationNode("n1", tc.nodeMode))
			m.PolicyIndex.SetClusterConfig(match.ClusterConfig{CPUAllocation: tc.cluster})

			resp := admitPrescheduled(t, m, "vllm-a", "12", "1", "", "n1", false)
			if resp.Allowed != tc.wantAdmitted {
				t.Fatalf("12 cores on a %s node under a %s cluster: allowed = %v, want %v (%s)",
					tc.nodeMode, tc.cluster, resp.Allowed, tc.wantAdmitted, respMessage(resp))
			}
			if tc.wantAdmitted {
				assertHashRecordsMode(t, resp, tc.nodeMode, "of a bound pod")
			}
		})
	}
}

// TestMixedModeFleetHashesEachPodUnderItsOwnNode proves each bound pod is hashed
// under its own node's mode.
func TestMixedModeFleetHashesEachPodUnderItsOwnNode(t *testing.T) {
	m, _ := newHandleMutator(t, "enforce",
		allocationNode("n-physical", balloons.CPUAllocationPhysicalCores),
		allocationNode("n-hyperthreads", hyperthreadsAllocation))
	m.PolicyIndex.SetClusterConfig(match.ClusterConfig{CPUAllocation: balloons.CPUAllocationPhysicalCores})

	hashes := map[string]string{}
	for _, tc := range []struct{ node, mode string }{
		{"n-physical", balloons.CPUAllocationPhysicalCores},
		{"n-hyperthreads", hyperthreadsAllocation},
	} {
		resp := admitPrescheduled(t, m, "vllm-"+tc.node, "4", "1", "", tc.node, false)
		if !resp.Allowed {
			t.Fatalf("4 cores on %s must be admitted under either mode, got %q", tc.node, respMessage(resp))
		}
		hashes[tc.node] = assertHashRecordsMode(t, resp, tc.mode, "on "+tc.node)
	}
	if hashes["n-physical"] == hashes["n-hyperthreads"] {
		t.Errorf("both pods carry hash %q, so the fleet's two modes are indistinguishable to drift detection",
			hashes["n-physical"])
	}
}

// TestUnboundPodRecordsAModeOnlyWhenTheFleetLeavesOne proves an unbound pod records a
// mode only on a single-mode fleet; a wrong mode would report drift on every pass.
func TestUnboundPodRecordsAModeOnlyWhenTheFleetLeavesOne(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nodes []*corev1.Node
		want  string
	}{
		{
			name:  "a fleet in the cluster default mode",
			nodes: []*corev1.Node{allocationNode("n1", balloons.CPUAllocationPhysicalCores)},
			want:  balloons.CPUAllocationPhysicalCores,
		},
		{
			name:  "a fleet entirely under one override",
			nodes: []*corev1.Node{allocationNode("n1", hyperthreadsAllocation), allocationNode("n2", hyperthreadsAllocation)},
			want:  hyperthreadsAllocation,
		},
		{
			name:  "a fleet under two modes",
			nodes: []*corev1.Node{allocationNode("n1", balloons.CPUAllocationPhysicalCores), allocationNode("n2", hyperthreadsAllocation)},
			want:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tc.nodes))
			for _, n := range tc.nodes {
				objs = append(objs, n)
			}
			m, _ := newHandleMutator(t, "warn", objs...)
			m.PolicyIndex.SetClusterConfig(match.ClusterConfig{CPUAllocation: balloons.CPUAllocationPhysicalCores})

			resp := admitPod(t, m, "vllm-a", "4", "1")
			if !resp.Allowed {
				t.Fatalf("unbound pod must be admitted, got %q", respMessage(resp))
			}
			assertHashRecordsMode(t, resp, tc.want, "of an unbound pod")
		})
	}
}

// htTopoNode is a hyperthreaded node with 4 cores x 2 threads on one NUMA: room 4
// in physical-cores mode, 8 in hyperthreads.
func htTopoNode(name, mode string) *corev1.Node {
	n := topoNode(name, 4, 1)
	n.Labels[controller.LabelThreadsPerCore] = "2"
	n.Labels[controller.LabelCPUAllocation] = mode
	return n
}

// htTopology is that node's topology: CPUs 0-3 are cores 0-3, CPUs 4-7 their siblings.
func htTopology() map[int]topology.CPUInfo {
	info := map[int]topology.CPUInfo{}
	for cpu := 0; cpu < 8; cpu++ {
		info[cpu] = topology.CPUInfo{CPU: cpu, NUMA: 0, Core: cpu % 4}
	}
	return info
}

// steeredTo reports whether the response's injected affinity names this node.
func steeredTo(t *testing.T, resp admission.Response, node string) bool {
	t.Helper()
	raw, err := json.Marshal(resp.Patches)
	if err != nil {
		t.Fatalf("marshalling the patches: %v", err)
	}
	return strings.Contains(string(raw), `"`+node+`"`)
}

// assertSteeredToTheRoomierMode checks an unbound pod was admitted, steered to the
// node whose mode has room, and hashed under that node's mode.
func assertSteeredToTheRoomierMode(t *testing.T, resp admission.Response) {
	t.Helper()
	if !resp.Allowed {
		t.Fatalf("a request that fits the hyperthreads node was refused: %q", respMessage(resp))
	}
	if !steeredTo(t, resp, "n-hyperthreads") {
		t.Errorf("pod was not steered to the node with the room: %v", resp.Patches)
	}
	if steeredTo(t, resp, "n-physical") {
		t.Errorf("pod was steered to a node whose mode leaves it no room: %v", resp.Patches)
	}
	assertHashRecordsMode(t, resp, hyperthreadsAllocation, "of a steered pod")
}

// mixedModeFleet is one node in the cluster default mode and one under an override.
func mixedModeFleet() []runtime.Object {
	return []runtime.Object{
		htTopoNode("n-physical", balloons.CPUAllocationPhysicalCores),
		htTopoNode("n-hyperthreads", hyperthreadsAllocation),
	}
}

// TestLiveSteeringCountsEachCandidateInItsOwnMode proves live candidate filtering
// counts each node in its own mode: with one core claimed, 5 cores fit only the
// hyperthreads node (7 logical CPUs free vs 3 cores).
func TestLiveSteeringCountsEachCandidateInItsOwnMode(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})

	objs := mixedModeFleet()
	topo := topology.NewCache()
	for _, node := range []string{"n-physical", "n-hyperthreads"} {
		topo.Set(node, htTopology())
		// One core claimed by another workload.
		objs = append(objs, nrtObj(node, nrtZone(testContainerZoneType, "inference/other/other", "balloon-span1[0]", "0")))
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	m, _ := newHandleMutatorFromClient(t, "enforce", c, topo)
	m.PolicyIndex.SetClusterConfig(match.ClusterConfig{CPUAllocation: balloons.CPUAllocationPhysicalCores})

	assertSteeredToTheRoomierMode(t, admitPod(t, m, "vllm-a", "5", "1"))
}

// TestColdStartSteeringCountsEachCandidateInItsOwnMode is the cold-start twin, with
// capacity from static labels.
func TestColdStartSteeringCountsEachCandidateInItsOwnMode(t *testing.T) {
	m, _ := coldStartMutator(t, "enforce", mixedModeFleet()...)
	m.PolicyIndex.SetClusterConfig(match.ClusterConfig{CPUAllocation: balloons.CPUAllocationPhysicalCores})

	assertSteeredToTheRoomierMode(t, admitPod(t, m, "vllm-a", "5", "1"))
}

// splitTopology is a node whose NUMAs rank oppositely per mode: 1 free core on NUMA 0
// vs 0 on NUMA 1, but 2 free logical CPUs vs 3.
func splitTopology() map[int]topology.CPUInfo {
	info := map[int]topology.CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0},
		4: {CPU: 4, NUMA: 0, Core: 0},
	}
	for core := 1; core <= 3; core++ {
		info[core] = topology.CPUInfo{CPU: core, NUMA: 1, Core: core}
		info[core+4] = topology.CPUInfo{CPU: core + 4, NUMA: 1, Core: core}
	}
	return info
}

// TestWarmUpSeedsInTheNodesOwnMode proves warm-up seeds in the node's own mode: on
// splitTopology the share lands on NUMA 1 only under hyperthreads.
func TestWarmUpSeedsInTheNodesOwnMode(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "b", Labels: map[string]string{
		controller.LabelBalloonsEnabled: "true",
		controller.LabelCPUAllocation:   hyperthreadsAllocation,
	}}}
	// Another workload holds NUMA 1's representative threads; this pod has no zone, so
	// it is seeded.
	occupied := nrtObj("b", nrtZone(testContainerZoneType, "inference/other/other", "balloon-span1[0]", "1-3"))
	pod := pinnedPod("vllm-a", "b", "balloon-span1", "2", time.Now())

	m := warmMutatorCachedFor(t, nil, node, occupied, pod)
	m.TopoCache.Set("b", splitTopology())
	m.PolicyIndex.SetClusterConfig(match.ClusterConfig{CPUAllocation: balloons.CPUAllocationPhysicalCores})

	m.WarmReservations(context.Background())

	reserved := reservedCores(m.Reservations, "b")
	if reserved[1] != 2 {
		t.Errorf("seeded reservation %v, want 2 cores on NUMA 1: the node runs hyperthreads, where NUMA 1 is the roomier one", reserved)
	}
	if reserved[0] != 0 {
		t.Errorf("seeded reservation %v, want nothing on NUMA 0: that NUMA node is only the roomier one under the cluster default", reserved)
	}
}

// TestPreScheduledReservationCountsInTheNodesOwnMode proves a bound pod's booking is
// counted in its node's mode: on splitTopology it lands on NUMA 1 only under
// hyperthreads.
func TestPreScheduledReservationCountsInTheNodesOwnMode(t *testing.T) {
	node := htTopoNode("n1", hyperthreadsAllocation)
	// NUMA 1 representative threads claimed (see splitTopology).
	occupied := nrtObj("n1", nrtZone(testContainerZoneType, "inference/other/other", "balloon-span1[0]", "1-3"))

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(node, occupied).Build()

	topo := topology.NewCache()
	topo.Set("n1", splitTopology())
	m, _ := newHandleMutatorFromClient(t, "warn", c, topo)
	m.PolicyIndex.SetClusterConfig(match.ClusterConfig{CPUAllocation: balloons.CPUAllocationPhysicalCores})

	if resp := admitPrescheduled(t, m, "vllm-a", "2", "1", "", "n1", false); !resp.Allowed {
		t.Fatalf("bound pod must be admitted in warn mode, got %q", respMessage(resp))
	}

	reserved := reservedCores(m.Reservations, "n1")
	if reserved[1] != 2 {
		t.Errorf("booked %v, want 2 cores on NUMA 1: the node runs hyperthreads, where NUMA 1 is the roomier one", reserved)
	}
	if reserved[0] != 0 {
		t.Errorf("booked %v, want nothing on NUMA 0: that NUMA node is only the roomier one under the cluster default", reserved)
	}
}

// TestSharedModeNamesOneModeOnly proves a denial names a mode only when every listed
// candidate was counted in it.
func TestSharedModeNamesOneModeOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		modes map[string]string
		want  string
	}{
		{"one node", map[string]string{"a": hyperthreadsAllocation}, ", " + hyperthreadsAllocation},
		{"two nodes in one mode", map[string]string{"a": hyperthreadsAllocation, "b": hyperthreadsAllocation}, ", " + hyperthreadsAllocation},
		{"two modes", map[string]string{"a": hyperthreadsAllocation, "b": balloons.CPUAllocationPhysicalCores}, ""},
		{"no candidates", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sharedMode(tc.modes); got != tc.want {
				t.Errorf("sharedMode(%v) = %q, want %q", tc.modes, got, tc.want)
			}
		})
	}
}

// TestBoundAdmissionReadsItsNodeOnce proves every decision about a bound pod's node
// comes from one read, so a mode override landing mid-admission cannot split them.
func TestBoundAdmissionReadsItsNodeOnce(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})

	var nodeGets atomic.Int32
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(allocationNode("n1", hyperthreadsAllocation)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isNode := obj.(*corev1.Node); isNode {
					nodeGets.Add(1)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	// A topology cache, hence a reservation store, so both the gate and the reservation
	// read the node's labels.
	m, _ := newHandleMutatorFromClient(t, "enforce", c, topology.NewCache())
	m.PolicyIndex.SetClusterConfig(match.ClusterConfig{CPUAllocation: balloons.CPUAllocationPhysicalCores})

	resp := admitPrescheduled(t, m, "vllm-a", "4", "1", "", "n1", false)
	if !resp.Allowed {
		t.Fatalf("4 cores must be admitted, got %q", respMessage(resp))
	}
	assertHashRecordsMode(t, resp, hyperthreadsAllocation, "of a bound pod")
	if got := nodeGets.Load(); got != 1 {
		t.Errorf("the admission read the pod's node %d times, want 1: one read is what keeps the "+
			"unit, the density rule and the recorded mode describing one state of the machine", got)
	}
}

// TestFleetModeReadsOnlyTheNodesThePodCanLandOn proves a two-mode fleet still answers
// for a pod whose nodeSelector or required feature labels leave it one mode.
func TestFleetModeReadsOnlyTheNodesThePodCanLandOn(t *testing.T) {
	physical := allocationNode("n1", balloons.CPUAllocationPhysicalCores)
	physical.Labels["feature.example/amx"] = "true"
	m, _ := newHandleMutator(t, "warn", physical, allocationNode("n2", hyperthreadsAllocation))
	for _, tc := range []struct {
		name     string
		selector map[string]string
		features []string
		want     string
	}{
		{name: "no selection", want: ""},
		{name: "nodeSelector on the mode label", selector: map[string]string{controller.LabelCPUAllocation: hyperthreadsAllocation}, want: hyperthreadsAllocation},
		{name: "required feature label", features: []string{"feature.example/amx"}, want: balloons.CPUAllocationPhysicalCores},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Spec: corev1.PodSpec{NodeSelector: tc.selector}}
			if got := m.fleetCPUAllocation(context.Background(), pod, tc.features, balloons.CPUAllocationPhysicalCores); got != tc.want {
				t.Errorf("mode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStaticCapacityReadsOnlyTheNodesThePodCanLandOn proves the no-live-data check
// judges an unplaced pod only against the nodes its nodeSelector and required
// feature labels allow: 12 cores fit the hyperthreads node (16), not physical-cores (8).
func TestStaticCapacityReadsOnlyTheNodesThePodCanLandOn(t *testing.T) {
	ht := allocationNode("n-ht", hyperthreadsAllocation)
	ht.Labels["feature.example/amx"] = "true"
	m := newMutator(allocationNode("n-pc", balloons.CPUAllocationPhysicalCores), ht)
	for _, tc := range []struct {
		name     string
		selector map[string]string
		features []string
		want     bool
	}{
		{name: "no selection", want: true},
		{name: "nodeSelector on physical-cores", selector: map[string]string{controller.LabelCPUAllocation: balloons.CPUAllocationPhysicalCores}, want: false},
		{name: "required feature on the hyperthreads node", features: []string{"feature.example/amx"}, want: true},
		{name: "required feature no node has", features: []string{"feature.example/none"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := unscheduledPod("p", "12")
			pod.Spec.NodeSelector = tc.selector
			fits, detail, err := m.capacityFits(context.Background(), pod, &pod.Spec.Containers[0], false, 1, "", balloons.CPUAllocationPhysicalCores, nil, tc.features)
			if err != nil {
				t.Fatalf("capacityFits: %v", err)
			}
			if fits != tc.want {
				t.Errorf("fits = %v, want %v (%s)", fits, tc.want, detail)
			}
		})
	}
}
