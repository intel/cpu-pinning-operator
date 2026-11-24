// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/match"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// The pod mutator is the main trust boundary; these targets fuzz the encoded
// request and the pod fields the decision is derived from.

// fuzzPodNamespace is fixed to bound the namespace-labeled metrics a fuzz run creates.
const fuzzPodNamespace = "inference"

// hugeExponentRe captures the significant digits of a quantity's decimal exponent.
// resource.ParseQuantity never returns for an exponent past int32 range, so such
// inputs would hang the harness; the apiserver rejects them before the webhook.
var hugeExponentRe = regexp.MustCompile(`[eE][-+]?0*([0-9]+)`)

// skipUnparseableQuantity reports whether a quantity's exponent would hang
// resource.ParseQuantity. Leading zeros are ignored.
func skipUnparseableQuantity(qty string) bool {
	m := hugeExponentRe.FindStringSubmatch(qty)
	return m != nil && len(m[1]) > 6
}

// newFuzzMutator builds a PodMutator with one rule over a small fleet; nil recorder
// and nil TopoCache/Reservations keep every admission independent.
func newFuzzMutator(tb testing.TB) *PodMutator {
	tb.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		tb.Fatalf("add client-go scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(
		topoNode("node-a", 64, 2),
		topoNode("node-b", 128, 8),
		labeledNode("node-c", true, ""),
	).Build()

	rules, err := match.CompileIntent(cpopv1alpha1.IntentSpec{
		Span: cpopv1alpha1.IntentSourceSpec{
			Annotation: "cpu-pinning-operator.intel.com/tp",
			ArgRegex:   `--tensor-parallel-size[= ](\d+)`,
			EnvVar:     "TP_SIZE",
			Default:    1,
		},
		CoLocate: "package",
	})
	if err != nil {
		tb.Fatalf("compile intent: %v", err)
	}
	idx := match.NewIndex()
	idx.Update([]match.PolicyRule{{
		Name:              "fuzz",
		Namespaces:        []string{fuzzPodNamespace},
		ByContainerName:   []string{"vllm"},
		ByPodLabels:       map[string]string{"app": "llm"},
		EnforcePinning:    true,
		Intent:            rules,
		CapacityCheckMode: "enforce",
	}})

	m := &PodMutator{Client: c, PolicyIndex: idx}
	if err := m.InjectDecoder(admission.NewDecoder(scheme)); err != nil {
		tb.Fatalf("inject decoder: %v", err)
	}
	return m
}

func fuzzAdmissionRequest(raw []byte) admission.Request {
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}),
		Operation: admissionv1.Create,
		Namespace: fuzzPodNamespace,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

// FuzzPodAdmissionRaw proves undecodable bytes never panic and fail open: Allowed,
// no patch.
func FuzzPodAdmissionRaw(f *testing.F) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: fuzzPodNamespace},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm", Resources: fuzzCPU("4")}}},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		f.Fatalf("marshal seed pod: %v", err)
	}
	f.Add(raw)
	f.Add([]byte(`{"kind":"Pod","apiVersion":"v1"}`))
	f.Add([]byte(`{"spec":{"containers":[{"name":"vllm","resources":{"requests":{"cpu":"x"}}}]}}`))
	f.Add([]byte(`{"spec":{"nodeName":"node-a","containers":[]}}`))
	f.Add([]byte(`{`))
	f.Add([]byte(""))

	m := newFuzzMutator(f)
	// The mutator's own decoder: an independent parser could disagree on "decodable".
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		f.Fatalf("add client-go scheme: %v", err)
	}
	dec := admission.NewDecoder(scheme)

	f.Fuzz(func(t *testing.T, raw []byte) {
		// Skip huge exponents: Quantity.UnmarshalJSON would hang (see hugeExponentRe).
		if skipUnparseableQuantity(string(raw)) {
			return
		}
		req := fuzzAdmissionRequest(raw)
		resp := m.Handle(context.Background(), req)
		assertUsableResponse(t, resp)

		// Fail open: Allowed and no patch, asserted separately so a no-patch denial is caught.
		if decErr := dec.DecodeRaw(req.Object, &corev1.Pod{}); decErr != nil {
			if !resp.Allowed {
				t.Fatalf("denied a request the decoder rejected instead of failing open (%v): %q resp=%+v", decErr, raw, resp)
			}
			if len(resp.Patches) > 0 {
				t.Fatalf("patched a request the decoder rejected (%v): %q patches=%v", decErr, raw, resp.Patches)
			}
		}
	})
}

// FuzzPodAdmission admits a canonically-encoded pod with fuzzed fields, so every
// patch op is the mutator's own and can be held to the fields it may touch.
func FuzzPodAdmission(f *testing.F) {
	f.Add(fuzzPodNamespace, "vllm", "app", "llm", "4", "16Gi", "2", "--tensor-parallel-size 2", "")
	f.Add(fuzzPodNamespace, "vllm", "app", "llm", "1500m", "0", "0", "", "node-a")
	f.Add(fuzzPodNamespace, "other", "app", "llm", "8", "1Ei", "9", "--tensor-parallel-size=8", "node-b")
	f.Add(fuzzPodNamespace, "vllm", "", "", "9223372036854775807", "9223372036854775807", "4", "", "missing-node")
	f.Add("other-ns", "vllm", "app", "llm", "2", "1Gi", "1", "", "")
	f.Add(fuzzPodNamespace, "vllm", "app", "llm", "1e18", "1e18", "1", "", "")
	f.Add(fuzzPodNamespace, "vllm", "app", "llm", "1m", "1", "1", "", "")
	f.Add(fuzzPodNamespace, "vllm", "app", "llm", "4", "9227000000000000000", "1", "", "")
	f.Add(fuzzPodNamespace, "vllm", "app", "llm", "-1", "-1", "1", "", "")

	m := newFuzzMutator(f)
	f.Fuzz(func(t *testing.T, ns, containerName, labelKey, labelValue, cpu, mem, tp, args, nodeName string) {
		// Skip quantities the apiserver would reject; huge exponents must be filtered
		// before the parse (see hugeExponentRe).
		if skipUnparseableQuantity(cpu) || skipUnparseableQuantity(mem) {
			return
		}
		cpuQ, err := resource.ParseQuantity(cpu)
		if err != nil {
			return
		}
		memQ, err := resource.ParseQuantity(mem)
		if err != nil {
			return
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "fuzz-pod",
				Namespace:   ns,
				Labels:      map[string]string{labelKey: labelValue},
				Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": tp},
			},
			Spec: corev1.PodSpec{
				NodeName: nodeName,
				Containers: []corev1.Container{
					{
						Name: containerName,
						Args: []string{args},
						Env:  []corev1.EnvVar{{Name: "TP_SIZE", Value: tp}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: cpuQ, corev1.ResourceMemory: memQ},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: cpuQ, corev1.ResourceMemory: memQ},
						},
					},
					{Name: "sidecar", Resources: fuzzCPU("1")},
				},
			},
		}
		raw, err := json.Marshal(pod)
		if err != nil {
			return
		}

		// A pod that passes the whole-core gate must derive a positive core demand: the
		// x1000 scaling must not wrap negative and read as fitting.
		container := &pod.Spec.Containers[0]
		if hasWholeCoreGuaranteedCPU(container) {
			if cores := podCPURequestCores(pod, container, false); cores <= 0 {
				t.Fatalf("whole-core cpu request %q derived a core demand of %d", cpu, cores)
			}
		}
		if hasWholeCorePodCPU(pod) {
			if cores := podCPURequestCores(pod, nil, true); cores <= 0 {
				t.Fatalf("whole-core pod cpu request %q derived a core demand of %d", cpu, cores)
			}
		}
		if memQ.Sign() > 0 {
			// Past int64 the demand saturates. Assert the value, not the sign.
			wantSaturated := memQ.Cmp(maxInt64Quantity) >= 0
			for _, tc := range []struct {
				name  string
				bytes int64
			}{
				{"container-scoped", podMemoryRequestBytes(pod, container, false)},
				{"pod-scoped", podMemoryRequestBytes(pod, nil, true)},
			} {
				if tc.bytes <= 0 {
					t.Fatalf("%s memory request %q derived a byte demand of %d", tc.name, mem, tc.bytes)
				}
				if wantSaturated && tc.bytes != math.MaxInt64 {
					t.Fatalf("%s memory request %q is past int64 but derived %d, want the saturated %d",
						tc.name, mem, tc.bytes, int64(math.MaxInt64))
				}
			}
		}

		resp := m.Handle(context.Background(), fuzzAdmissionRequest(raw))
		assertUsableResponse(t, resp)

		for _, p := range resp.Patches {
			if !mutablePath(p.Path) {
				t.Fatalf("patch %s %q touches a field the mutator must not change (patches: %v)",
					p.Operation, p.Path, resp.Patches)
			}
		}
	})
}

// assertUsableResponse checks that a response admits or carries a reason, and only
// an allow carries a patch.
func assertUsableResponse(t *testing.T, resp admission.Response) {
	t.Helper()
	if !resp.Allowed {
		if resp.Result == nil || (resp.Result.Message == "" && resp.Result.Reason == "") {
			t.Fatalf("rejected the request without a reason: %+v", resp)
		}
		if len(resp.Patches) > 0 {
			t.Fatalf("rejected the request but returned patches: %+v", resp.Patches)
		}
	}
}

// mutablePath reports whether a patch path is inside what the mutator may change:
// its annotations, the readiness init container and the node affinity.
func mutablePath(path string) bool {
	for _, allowed := range []string{
		"/metadata/annotations",
		"/spec/initContainers",
		"/spec/affinity",
	} {
		if path == allowed || strings.HasPrefix(path, allowed+"/") {
			return true
		}
	}
	return false
}

func fuzzCPU(cores string) corev1.ResourceRequirements {
	q := resource.MustParse(cores)
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: q},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: q},
	}
}

// FuzzPolicyGuardAdmission proves the BalloonPinningPolicy guard (failurePolicy=Fail)
// answers malformed DELETE and UPDATE inputs with a usable response and no patches.
func FuzzPolicyGuardAdmission(f *testing.F) {
	guarded := &cpopv1alpha1.BalloonPinningPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: cpopv1alpha1.BalloonPinningPolicySpec{
			Namespaces:      []string{fuzzPodNamespace},
			GuardActivePods: ptrBool(true),
			Match:           cpopv1alpha1.MatchSpec{ByContainerName: []string{"vllm"}},
		},
		Status: cpopv1alpha1.BalloonPinningPolicyStatus{
			BoundPods:      []cpopv1alpha1.BoundPod{{Namespace: fuzzPodNamespace, Name: "vllm-a"}},
			BoundPodsTotal: 1,
		},
	}
	oldRaw, err := json.Marshal(guarded)
	if err != nil {
		f.Fatalf("marshal seed policy: %v", err)
	}
	f.Add(oldRaw, oldRaw, true)
	f.Add(oldRaw, []byte(`{"spec":{"guardActivePods":false}}`), true)
	f.Add(oldRaw, []byte(`{"spec":{"namespaces":["other"],"guardActivePods":true}}`), true)
	f.Add(oldRaw, []byte(`{}`), false)
	f.Add([]byte(`{"status":{"boundPodsTotal":-1}}`), []byte(`{"status":{"boundPodsTotal":9223372036854775807}}`), false)
	f.Add([]byte(`{`), []byte(`{`), true)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		f.Fatalf("add client-go scheme: %v", err)
	}
	if err := cpopv1alpha1.AddToScheme(scheme); err != nil {
		f.Fatalf("add operator scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "vllm-a", Namespace: fuzzPodNamespace, Labels: map[string]string{"app": "llm"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm", Resources: fuzzCPU("4")}}},
		},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fuzzPodNamespace, Labels: map[string]string{"tier": "ai"}}},
	).Build()
	g := &PolicyGuard{Client: c}
	if err := g.InjectDecoder(admission.NewDecoder(scheme)); err != nil {
		f.Fatalf("inject decoder: %v", err)
	}

	f.Fuzz(func(t *testing.T, oldObj, newObj []byte, update bool) {
		op := admissionv1.Delete
		if update {
			op = admissionv1.Update
		}
		resp := g.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: op,
			OldObject: runtime.RawExtension{Raw: oldObj},
			Object:    runtime.RawExtension{Raw: newObj},
		}})
		assertUsableResponse(t, resp)
		if len(resp.Patches) > 0 {
			t.Fatalf("the validating guard returned patches: %v", resp.Patches)
		}
	})
}

// FuzzNodeCapacityLabels proves unparseable or absurd operator node labels read as
// "unknown, do not block", never as a bogus capacity.
func FuzzNodeCapacityLabels(f *testing.F) {
	f.Add("64", "2", "2", "16", "8", "137438953472", "1_1", 4, 2, int64(1<<30))
	f.Add("0", "0", "0", "0", "0", "0", "", 0, 0, int64(0))
	f.Add("-1", "-1", "-1", "-1", "-1", "-1", "-1", -1, -1, int64(-1))
	f.Add("9223372036854775807", "1", "1", "9223372036854775807", "8", "9223372036854775807", "8_8", 1, 8, int64(1<<62))
	f.Add("9223372036854775807", "1", "2", "0", "8", "1", "1", 1, 1, int64(1))
	f.Add("9223372036854775807", "9223372036854775807", "1", "1", "8", "1", "1", 1, 1, int64(1))
	f.Add("x", "x", "x", "x", "x", "x", "x", 1, 1, int64(1))

	f.Fuzz(func(t *testing.T, physical, numa, threads, reserved, maxSpan, memFloor, pkgTopology string, cores, span int, memBytes int64) {
		labels := map[string]string{
			controller.LabelBalloonsEnabled:    "true",
			controller.LabelPhysicalCPUs:       physical,
			controller.LabelNumaNodes:          numa,
			controller.LabelThreadsPerCore:     threads,
			controller.LabelReservedCPUs:       reserved,
			controller.LabelMaxSpan:            maxSpan,
			controller.LabelMemoryPerNUMABytes: memFloor,
			controller.LabelPackageTopology:    pkgTopology,
		}

		if got := effectiveMaxSpan(labels); got < 1 {
			t.Fatalf("effectiveMaxSpan = %d, below the conservative floor of 1", got)
		}

		// Unknown topology must report fitting; a readable one may legitimately report 0
		// per-NUMA capacity and reject.
		numaCount := intLabel(labels, controller.LabelNumaNodes)
		topologyKnown := intLabel(labels, controller.LabelPhysicalCPUs) > 0 &&
			numaCount > 0 && numaCount <= maxStaticNUMANodes
		fits, capacity := nodeFits(labels, cores, span, "physical-cores")
		if capacity < 0 {
			t.Fatalf("nodeFits reported a negative per-NUMA capacity %d", capacity)
		}
		if !topologyKnown && (!fits || capacity != 0) {
			t.Fatalf("nodeFits on unknown topology = (%v, %d), want (true, 0)", fits, capacity)
		}
		// The reserved pool is subtracted after the hyperthread scaling, so the wider unit
		// is never narrower.
		if _, htCapacity := nodeFits(labels, cores, span, "hyperthreads"); htCapacity < capacity {
			t.Fatalf("hyperthreads capacity %d is below physical-cores capacity %d", htCapacity, capacity)
		}

		memFits, memCapacity := memoryFits(labels, memBytes, span)
		if memCapacity < 0 {
			t.Fatalf("memoryFits reported a negative capacity %d", memCapacity)
		}
		if memCapacity == 0 && !memFits {
			// Zero memory capacity means "floor unknown" and must not block.
			t.Fatal("memoryFits rejected on unknown capacity (0), which must never block")
		}

		if share := perNUMACPUShare(cores, span); cores > 0 && share < 1 {
			t.Fatalf("perNUMACPUShare(%d, %d) = %d for a positive request", cores, span, share)
		}

		free, ok := nodeStaticFree(labels, "physical-cores", nil)
		if ok {
			if numaCount > maxStaticNUMANodes {
				t.Fatalf("nodeStaticFree built a map for %d NUMA nodes", numaCount)
			}
			for numaID, cpus := range free {
				if cpus < 0 {
					t.Fatalf("nodeStaticFree reported %d free CPUs on NUMA %d", cpus, numaID)
				}
				// Drift guard: with no reservations both static functions must agree per NUMA.
				if cpus != capacity {
					t.Fatalf("nodeStaticFree NUMA %d = %d but nodeFits capacity = %d", numaID, cpus, capacity)
				}
			}
			// The ranking total must not wrap either.
			if total := topology.SumFree(free); total < 0 {
				t.Fatalf("topology.SumFree(%v) = %d, negative", free, total)
			}
		}
		// The memory twin must also refuse an implausible count.
		memFree, memOK := nodeStaticMemFree(labels, nil)
		if memOK {
			if numaCount > maxStaticNUMANodes {
				t.Fatalf("nodeStaticMemFree built a map for %d NUMA nodes", numaCount)
			}
			for numaID, bytes := range memFree {
				if bytes < 0 {
					t.Fatalf("nodeStaticMemFree reported %d free bytes on NUMA %d", bytes, numaID)
				}
			}
		}
	})
}

// TestOccupiedMemPerNUMASaturates proves the occupancy sum saturates: a wrapped sum
// would report extra free memory.
func TestOccupiedMemPerNUMASaturates(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})

	huge := resource.MustParse("9223372036854775807")
	pod := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: fuzzPodNamespace},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:      "vllm",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: huge}},
			}}},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(pod("first"), pod("second")).Build()
	m := &PodMutator{Client: c}

	nrt := &unstructured.Unstructured{Object: map[string]interface{}{
		"zones": []interface{}{
			nrtZoneMem(fuzzPodNamespace+"/first/vllm", "balloon-span1[0]", "0-3", "0"),
			nrtZoneMem(fuzzPodNamespace+"/second/vllm", "balloon-span1[1]", "4-7", "0"),
		},
	}}

	occ, ok := m.occupiedMemPerNUMA(context.Background(), nrt, nil)
	if !ok {
		t.Fatal("occupiedMemPerNUMA did not read the zones")
	}
	if occ[0] <= 0 {
		t.Fatalf("occupied memory on NUMA 0 = %d, want a saturated positive total", occ[0])
	}
}

// TestOccupiedMemPerNUMAChargesTheUsableDuplicate proves a duplicate zone copy on
// a non-existent NUMA does not consume the charge for the real copy.
func TestOccupiedMemPerNUMAChargesTheUsableDuplicate(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})

	const gib = 1 << 30
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vllm-a", Namespace: fuzzPodNamespace},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "vllm",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceMemory: *resource.NewQuantity(4*gib, resource.BinarySI),
			}},
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(pod).Build()
	m := &PodMutator{Client: c}

	zone := fuzzPodNamespace + "/vllm-a/vllm"
	nrt := &unstructured.Unstructured{Object: map[string]interface{}{
		"zones": []interface{}{
			// The leaked copy first, on a NUMA the node lacks.
			nrtZoneMem(zone, "balloon-span1[0]", "0-3", "9"),
			nrtZoneMem(zone, "balloon-span1[0]", "0-3", "0"),
		},
	}}

	occ, ok := m.occupiedMemPerNUMA(context.Background(), nrt, map[int]int64{0: 64 * gib})
	if !ok {
		t.Fatal("occupiedMemPerNUMA did not read the zones")
	}
	if occ[0] != 4*gib {
		t.Fatalf("occupied memory on NUMA 0 = %d, want the container's %d", occ[0], 4*gib)
	}
}

// TestOccupiedMemPerNUMAChargesEveryRealDuplicate proves both copies of a duplicate
// zone on real NUMAs are charged, in either order.
func TestOccupiedMemPerNUMAChargesEveryRealDuplicate(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})

	const gib = 1 << 30
	zone := fuzzPodNamespace + "/vllm-a/vllm"
	for _, tc := range []struct{ name, first, second string }{
		{"0 then 1", "0", "1"},
		{"1 then 0", "1", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "vllm-a", Namespace: fuzzPodNamespace},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "vllm",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceMemory: *resource.NewQuantity(4*gib, resource.BinarySI),
					}},
				}}},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(pod).Build()
			m := &PodMutator{Client: c}

			nrt := &unstructured.Unstructured{Object: map[string]interface{}{
				"zones": []interface{}{
					nrtZoneMem(zone, "balloon-span1[0]", "0-3", tc.first),
					nrtZoneMem(zone, "balloon-span1[0]", "0-3", tc.second),
				},
			}}

			occ, ok := m.occupiedMemPerNUMA(context.Background(), nrt,
				map[int]int64{0: 64 * gib, 1: 64 * gib})
			if !ok {
				t.Fatal("occupiedMemPerNUMA did not read the zones")
			}
			for numa := 0; numa <= 1; numa++ {
				if occ[numa] != 4*gib {
					t.Errorf("occupied memory on NUMA %d = %d, want the container's %d",
						numa, occ[numa], 4*gib)
				}
			}
		})
	}
}

// TestSumFreeSaturates proves the ranking total saturates over saturated per-NUMA
// capacities.
func TestSumFreeSaturates(t *testing.T) {
	if total := topology.SumFree(map[int]int{0: math.MaxInt, 1: math.MaxInt}); total != math.MaxInt {
		t.Errorf("SumFree of two saturated NUMAs = %d, want %d", total, math.MaxInt)
	}
	if total := topology.SumFree(map[int]int{0: 3, 1: 4}); total != 7 {
		t.Errorf("SumFree of ordinary values = %d, want 7", total)
	}
}
