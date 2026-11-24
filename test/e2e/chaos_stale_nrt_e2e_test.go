// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Chaos e2e: freezes the balloons plugin on one node so its NRT goes stale, then
// checks that pods admitted against the stale NRT are held in Init by the injected
// wait-for-balloons container and never pin. The init gate, not admission, is what
// prevents oversubscription. Gated behind E2E_CHAOS_STALE_NRT; it uses its own
// namespace and policy and restores the DaemonSet affinity on cleanup. Helpers
// local to this file are prefixed "sn".
package e2e

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/topology"
	"github.com/intel/cpu-pinning-operator/internal/webhook"
)

const (
	// snGate opts the stale-NRT guard in.
	snGate = "E2E_CHAOS_STALE_NRT"

	// snHostnameLabel is the node label the DaemonSet exclusion and probe nodeSelector match on.
	snHostnameLabel = "kubernetes.io/hostname"

	// snFreezeSettle is how long the NRT resourceVersion must stay constant to count as
	// frozen; well above the plugin's few-second publish cadence.
	snFreezeSettle = 45 * time.Second

	// snInitHold is how long admitted probe pods must stay in Init; well above
	// scheduling plus kubelet init latency.
	snInitHold = 60 * time.Second

	// snQuietTimeout bounds the wait for pods from earlier tests to leave the node.
	snQuietTimeout = 3 * time.Minute
)

// snDaemonSet returns the balloons DaemonSet name and namespace.
func snDaemonSet() (name, namespace string) {
	return balloonsDaemonSet(), balloonsNS()
}

// TestStaleNRTInitGateHoldsOverAdmitted proves pods admitted past real capacity
// against a frozen NRT stay held in Init and never run or pin.
func TestStaleNRTInitGateHoldsOverAdmitted(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(snGate) == "" {
		skipNotSelected(at, "set %s=1 to run the stale-NRT init-gate guard "+
			"(freezes the balloons plugin on one node via a DaemonSet nodeAffinity exclusion, "+
			"then asserts over-admitted pods are held in Init and never oversubscribe; needs a real NRI cluster)", snGate)
	}
	c := newClient(t)                 // skips cleanly when no kubeconfig
	node := balloonsEnabledNode(t, c) // skips cleanly when no node is provisioned
	ctx := context.Background()

	perNUMA, numa := snPerNUMACapacity(t, c, node)
	// The reserved balloon owns part of every NUMA node and the frozen NRT reports that,
	// so a full-width request would be denied outright; ask for what a NUMA node can pin.
	perPod := snPinnablePerNUMA(t, c, node, perNUMA, numa)

	// A pod still pinned or terminating on the node when the plugin freezes stays in the
	// frozen NRT and takes room the batch is sized for.
	snWaitNodeQuiet(at, c, node, snQuietTimeout)

	// Skip, not fail, if the freeze does not take: a still-updating NRT is not stale.
	hostname := snNodeHostname(t, c, node)
	snFreezePlugin(t, c, node, hostname)

	if !snPluginGoneFromNode(t, c, node, hostname) {
		skipNoConfig(at, "balloons plugin pod did not leave the node after the DaemonSet exclusion; cannot freeze NRT")
	}
	frozenRV, ok := snNRTResourceVersion(t, c, node)
	if !ok {
		skipNoConfig(at, "node %s has no NodeResourceTopology CR to freeze (NRI agent.nodeResourceTopology may be off)", node)
	}
	if !snNRTFrozen(t, c, node, frozenRV) {
		skipNoConfig(at, "node %s NRT CR kept updating (resourceVersion advanced past %s within %s) — not frozen; skipping the over-admission probe",
			node, frozenRV, snFreezeSettle)
	}
	at.Logf("node %s: balloons plugin frozen, NRT CR pinned at resourceVersion %s (per-NUMA span-1 width %d cores, %d pinnable once the reserved "+
		"pool is netted out, %d NUMA nodes → an empty node fits at most %d such pods)", node, frozenRV, perNUMA, perPod, numa, numa)

	// Admission reads free room from the frozen NRT, so that, not an empty node, is the
	// capacity it can hold the line at.
	realCapacityPods := max(numa, 1)
	if pods, free, ok := snFrozenCapacity(t, c, node, perPod); ok {
		at.Logf("frozen NRT free cores per NUMA %v → room for %d such pods (an empty node has room for %d)", free, pods, realCapacityPods)
		realCapacityPods = pods
	} else {
		at.Logf("no live free room readable from the frozen NRT or the topology-cache annotation; admission falls back to the node labels, so expecting %d",
			realCapacityPods)
	}

	// -- own throwaway namespace + enforce-mode policy pinning span-1 whole cores --
	ns := "cpu-pinning-e2e-stale-nrt"
	const containerName = "vllm"
	policyName := "e2e-stale-nrt"

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	_ = c.Delete(ctx, nsObj)
	if err := createEventuallyGone(c, nsObj); err != nil {
		at.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Cleanup(func() {
		e2eDelete(t, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	// Enforce mode would deny over-capacity pods on a fresh NRT; against the frozen one
	// it reads stale free room, which is the setup this guard needs.
	policy := newUnstructured(bppGVK)
	policy.SetName(policyName)
	policy.Object["spec"] = map[string]interface{}{
		"match":         map[string]interface{}{"byContainerName": []interface{}{containerName}},
		"namespaces":    []interface{}{ns},
		"intent":        map[string]interface{}{"span": map[string]interface{}{"default": int64(1)}},
		"capacityCheck": map[string]interface{}{"mode": "enforce"},
	}
	_ = c.Delete(ctx, policy)
	if err := createEventuallyGone(c, policy); err != nil {
		at.Fatalf("create BalloonPinningPolicy %s: %v", policyName, err)
	}
	t.Cleanup(func() {
		p := newUnstructured(bppGVK)
		p.SetName(policyName)
		e2eDelete(t, c, p)
	})

	if !waitForWebhookNamespace(t, c, ns, containerName) {
		skipNoConfig(at, "webhook never began pinning pods in %q (policy reconcile / selector patch did not take effect)", ns)
	}
	// Reservations an earlier test left for this node hold room the batch needs, which
	// under-admits it into the inconclusive outcome below.
	cs, opNS := fabManager(at)
	raxDrainLedger(at, cs, opNS, node)

	// Unscheduled pods take the reservation-aware headroom path; the hostname nodeSelector
	// keeps them on the frozen node. Admitted means stamped and not denied.
	balloonKey := balloonAnnotationBase + "/container." + containerName

	// Several over both the label estimate and the frozen NRT's room, in case a pod is
	// steered off the frozen node; a node mode override can make the NRT room the larger.
	total := max(numa, realCapacityPods, 1) + 3

	// The denial counters separate a reservation-ledger refusal from any other one.
	var admittedPods []string
	capacityDenied, otherDenied := 0, 0
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("e2e-stale-%02d", i)
		pod := vllmPod(name, ns, strconv.Itoa(perPod), strconv.Itoa(perPod), 1 /*TP → span 1*/)
		pod.Spec.NodeSelector = map[string]string{snHostnameLabel: hostname}
		_ = c.Delete(ctx, pod)
		err := e2eCreate(ctx, c, pod)
		t.Cleanup(func() {
			e2eDelete(t, c, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
				client.GracePeriodSeconds(0))
		})
		if err != nil {

			if strings.Contains(err.Error(), ccDenialSubstr) {
				capacityDenied++
			} else {
				otherDenied++
			}
			at.Logf("pod %s DENIED at admission against the frozen NRT: %v", name, err)
			continue
		}
		// The object Create returns carries the webhook's mutations.
		if pod.Annotations[balloonKey] != "" && pod.Annotations[pinnedByAnnotation] == "cpu-pinning-operator" {
			if !hasInitContainer(pod, waitForBalloonsInit) {
				// The init container is only injected when BALLOONS_READY_ENDPOINT is set. This
				// Errorf wins over a later skip.
				at.Errorf("pod %s was admitted+pinned (%s) but carries no %q init container — the readiness gate that must hold "+
					"over-admitted pods is absent; the operator's BALLOONS_READY_ENDPOINT is likely unset",
					name, pod.Annotations[balloonKey], waitForBalloonsInit)
				continue
			}
			admittedPods = append(admittedPods, name)
			at.Logf("pod %s ADMITTED+pinned (%s) against frozen NRT, %s init container injected", name, pod.Annotations[balloonKey], waitForBalloonsInit)
		} else {

			at.Logf("pod %s admitted but not pinned to the frozen node (annotation=%q)", name, pod.Annotations[balloonKey])
		}
	}

	at.Logf("stale-NRT probe: %d/%d pods ADMITTED+pinned against the frozen NRT (frozen-NRT capacity %d full-NUMA span-1 pods, %d cores each)",
		len(admittedPods), total, realCapacityPods, perPod)

	switch snVerdict(len(admittedPods), realCapacityPods, capacityDenied, otherDenied) {
	case snHeldTheLine:
		// The reservation store already prevented the over-admission; that is a
		// capability skip that survives E2E_STRICT.
		skipNoCapability(at, "admission itself held the line: %d pod(s) admitted (the frozen NRT's capacity) and %d refused with the "+
			"enforce-mode capacity denial against a FROZEN NRT. The in-process reservation store subtracts pending admissions from the "+
			"stale free room, so no over-admitted pod can be produced for the init gate to hold",
			len(admittedPods), capacityDenied)
	case snInconclusive:
		skipNoConfig(at, "inconclusive: only %d pods were admitted+pinned to the frozen node but the frozen NRT has room for %d (%d refused on capacity, "+
			"%d refused for another reason) — the batch was not over-admitted, so there is no over-capacity pod to prove the init gate holds. "+
			"Check whether admission denied or pods were steered off the frozen node.",
			len(admittedPods), realCapacityPods, capacityDenied, otherDenied)
	}
	at.Logf("%d pod(s) admitted past the frozen NRT's capacity of %d — asserting the init gate holds them", len(admittedPods), realCapacityPods)

	// If the NRT moved, the plugin recovered and the hold is no longer attributable to it.
	if rv, ok := snNRTResourceVersion(t, c, node); !ok || rv != frozenRV {
		skipNoConfig(at, "node %s NRT CR changed during the probe (was %s, now %s, present=%v) — freeze did not hold; cannot attribute the init hold to the frozen plugin",
			node, frozenRV, rv, ok)
	}

	// A pod reaching Running while the plugin is frozen is real oversubscription.
	deadline := time.Now().Add(snInitHold)
	var everRan []string
	for time.Now().Before(deadline) {
		for _, name := range admittedPods {
			p := &corev1.Pod{}
			if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, p); err != nil {
				continue // transient read error — re-checked next tick
			}
			if p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodSucceeded {
				everRan = append(everRan, name)
			}
		}
		if len(everRan) > 0 {
			break // fail fast — a held pod broke out and ran
		}
		time.Sleep(5 * time.Second)
	}

	if len(everRan) > 0 {
		at.Errorf("OVERSUBSCRIPTION: over-admitted pod(s) %v reached Running while the balloons plugin was frozen — the wait-for-balloons "+
			"init gate did NOT hold them. Admission over-admitted against the stale NRT (expected) AND the init gate failed to prevent the "+
			"resulting oversubscription; this is exactly the harm the init container is supposed to prevent.", everRan)
	}

	held := 0
	for _, name := range admittedPods {
		p := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, p); err != nil {
			at.Logf("pod %s: read error while confirming the init hold: %v", name, err)
			continue
		}
		phase := p.Status.Phase
		initReady := snInitContainerReady(p, waitForBalloonsInit)
		if phase != corev1.PodPending || initReady {
			at.Errorf("pod %s: expected it held in Init (Pending, %s not ready) while the plugin is frozen, but phase=%s initReady=%v — "+
				"the init gate is not holding the over-admitted pod", name, waitForBalloonsInit, phase, initReady)
		}
		if cpuset := containerCPUSet(t, c, node, ns, name, "vllm"); cpuset != "" {
			at.Errorf("pod %s: NRT shows an allocation-for-container cpuset %q on the frozen node — it actually pinned despite being held in Init "+
				"(the frozen plugin should not have published any new allocation)", name, cpuset)
		}
		if phase == corev1.PodPending && !initReady {
			held++
		}
	}

	if held == len(admittedPods) && len(everRan) == 0 {
		at.Logf("init-gate guard PASSED: all %d over-admitted pods are held in Init (Pending, %s not ready) and none pinned/ran while the balloons "+
			"plugin was frozen — the readiness init container, not admission, prevents oversubscription against a stale NRT", held, waitForBalloonsInit)
	}

	// Recovery after cleanup is not asserted: it would race the DaemonSet restore, and
	// snFreezePlugin's cleanup already fails if the plugin does not return.
}

// snOutcome is what a probe batch against the frozen NRT showed.
type snOutcome int

const (
	snInconclusive snOutcome = iota // fewer admitted than the room, or a non-capacity refusal
	snHeldTheLine                   // admission stopped exactly at the room
	snOverAdmitted                  // admitted past the room: the case the init gate must hold
)

// snVerdict judges a batch: admitted pods against the frozen NRT's room in probe pods,
// and the refusals split into capacity denials and anything else.
func snVerdict(admitted, capacity, capacityDenied, otherDenied int) snOutcome {
	switch {
	case admitted > capacity:
		return snOverAdmitted
	case admitted == capacity && capacityDenied > 0 && otherDenied == 0:
		return snHeldTheLine
	default:
		return snInconclusive
	}
}

// TestStaleNRTVerdict is cluster-free.
func TestStaleNRTVerdict(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		admitted, capacity, capDenied, other int
		want                                 snOutcome
	}{
		{"over-admitted", 3, 2, 2, 0, snOverAdmitted},
		{"held at an empty node's room", 2, 2, 3, 0, snHeldTheLine},
		// Another pod held one NUMA node at freeze time: one fit, and admission was right.
		{"held at a part-occupied node's room", 1, 1, 4, 0, snHeldTheLine},
		{"held with no room at all", 0, 0, 5, 0, snHeldTheLine},
		{"under the room", 1, 2, 4, 0, snInconclusive},
		{"a non-capacity refusal", 2, 2, 2, 1, snInconclusive},
		{"nothing refused", 2, 2, 0, 0, snInconclusive},
	} {
		if got := snVerdict(tc.admitted, tc.capacity, tc.capDenied, tc.other); got != tc.want {
			t.Errorf("%s: snVerdict(%d, %d, %d, %d) = %d, want %d", tc.name, tc.admitted, tc.capacity, tc.capDenied, tc.other, got, tc.want)
		}
	}
}

// TestStaleNRTFrozenCapacity is cluster-free: room is read from the frozen NRT's
// occupancy, and an unreadable topology annotation is the label fallback.
func TestStaleNRTFrozenCapacity(t *testing.T) {
	// Two NUMA nodes of four single-thread cores: CPUs 0-3 on NUMA 0, 4-7 on NUMA 1.
	cpus := map[int]topology.CPUInfo{}
	for cpu := 0; cpu < 8; cpu++ {
		cpus[cpu] = topology.CPUInfo{CPU: cpu, NUMA: cpu / 4, Core: cpu}
	}
	cache, err := topology.EncodeNodeCache(cpus, nil)
	if err != nil {
		t.Fatal(err)
	}
	zone := func(typ, name, parent, cpuset string) interface{} {
		return map[string]interface{}{"type": typ, "name": name, "parent": parent,
			"attributes": []interface{}{map[string]interface{}{"name": "cpuset", "value": cpuset}}}
	}
	// The reserved balloon holds CPU 0 and a pinned pod holds all of NUMA 1.
	nrt := newUnstructured(nrtGVK)
	nrt.SetName("n1")
	nrt.Object["zones"] = []interface{}{
		zone("balloon", "reserved", "", "0"),
		zone("balloon", "full-numa[0]", "", "4-7"),
		zone("allocation for container", "ns/held/vllm", "full-numa[0]", "4-7"),
	}
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	s.AddKnownTypeWithName(nrtGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(bccGVK, &unstructured.Unstructured{})

	for _, tc := range []struct {
		name, annotation string
		wantPods         int
		wantFree         map[int]int
		wantOK           bool
	}{
		// NUMA 0 has three free cores, NUMA 1 none: one 3-core pod, not the two an empty node fits.
		{"one NUMA occupied", cache, 1, map[int]int{0: 3, 1: 0}, true},
		{"unreadable topology", "not json", 0, nil, false},
	} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1",
			Annotations: map[string]string{controller.AnnotationTopologyCache: tc.annotation}}}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(node, nrt.DeepCopy()).Build()
		pods, free, ok := snFrozenCapacity(t, c, "n1", 3)
		if pods != tc.wantPods || ok != tc.wantOK || !reflect.DeepEqual(free, tc.wantFree) {
			t.Errorf("%s: snFrozenCapacity = (%d, %v, %v), want (%d, %v, %v)",
				tc.name, pods, free, ok, tc.wantPods, tc.wantFree, tc.wantOK)
		}
	}
}

// snFrozenCapacity counts the perPod-core span-1 pods the frozen NRT has room for, read
// as the capacity check reads it. ok is false when the check would fall back to labels.
func snFrozenCapacity(t *testing.T, c client.Client, node string, perPod int) (pods int, free map[int]int, ok bool) {
	t.Helper()
	ctx := context.Background()
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		t.Fatalf("get node %s: %v", node, err)
	}
	cpuInfo, _, err := topology.DecodeNodeCache(n.Annotations[controller.AnnotationTopologyCache])
	if err != nil || len(cpuInfo) == 0 {
		return 0, nil, false
	}
	nrt := newUnstructured(nrtGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: node}, nrt); err != nil {
		return 0, nil, false
	}
	// Resolve the mode per node, as admission does: an override can differ from the cluster.
	alloc := controller.ResolvedCPUAllocation(n.Labels)
	if alloc == "" {
		alloc = clusterCPUAllocation(t, c)
	}
	density := controller.ResolvedCPUAllocation(n.Labels) == "physical-cores" && clusterHTSiblingPolicy(t, c) == "reserved"
	if free, ok = webhook.LiveFreeFromNRTForE2E(nrt, cpuInfo, alloc, density); !ok {
		return 0, nil, false
	}
	for _, f := range free {
		pods += f / perPod
	}
	return pods, free, true
}

// snInitContainerReady reports whether the named init container is Ready.
func snInitContainerReady(p *corev1.Pod, name string) bool {
	for i := range p.Status.InitContainerStatuses {
		s := &p.Status.InitContainerStatuses[i]
		if s.Name == name {
			return s.Ready
		}
	}
	return false
}

// snPerNUMACapacity returns the per-NUMA whole-core hardware width and NUMA count from
// the node labels, in nodeFits units. It does not subtract the reserved pool.
func snPerNUMACapacity(t testing.TB, c client.Client, node string) (perNUMA, numa int) {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, &n); err != nil {
		t.Fatalf("get node %s: %v", node, err)
	}
	phys, _ := strconv.Atoi(n.Labels[labelPhysicalCPUs])
	numa, _ = strconv.Atoi(n.Labels[labelNumaNodes])
	if phys == 0 || numa == 0 {
		skipNoConfig(t, "node %s missing physical-cpus/numa-nodes labels; cannot size a per-NUMA request", node)
	}
	perNUMA = phys / numa
	if clusterCPUAllocation(t, c) == "hyperthreads" {
		threads, _ := strconv.Atoi(n.Labels[labelThreadsPerCore])
		if threads < 1 {
			threads = 1
		}
		perNUMA *= threads
	}
	if perNUMA < 1 {
		skipNoCapability(t, "node %s computed per-NUMA capacity %d; cannot build a full-NUMA request", node, perNUMA)
	}
	return perNUMA, numa
}

// snPinnablePerNUMA conservatively sizes the probe as the per-NUMA width minus the
// whole node's reserved count: under-sizing is safe, an oversized request could fit
// nowhere. The result must exceed half the NUMA width so only one probe fits per NUMA.
func snPinnablePerNUMA(t testing.TB, c client.Client, node string, perNUMA, numa int) int {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, &n); err != nil {
		t.Fatalf("get node %s: %v", node, err)
	}
	reserved, _ := strconv.Atoi(n.Labels[labelReservedCPUs])
	pinnable := perNUMA - reserved
	if pinnable < 1 {
		skipNoCapability(t, "node %s reserves %d CPU(s) out of a %d-core per-NUMA width across %d NUMA node(s), leaving nothing pinnable "+
			"for a full-NUMA span-1 request", node, reserved, perNUMA, numa)
	}
	// "One pod per NUMA node" only holds while a request takes more than half a NUMA node.
	if 2*pinnable <= perNUMA {
		skipNoCapability(t, "node %s reserves %d of its %d-core per-NUMA width, so a %d-core request no longer fills more than half a NUMA "+
			"node and two of them could share one — the one-pod-per-NUMA capacity this probe reasons from does not hold here",
			node, reserved, perNUMA, pinnable)
	}
	return pinnable
}

// snWaitNodeQuiet waits up to timeout until no pod outside kube-system and the operator
// and plugin namespaces holds pinned CPUs on the node or is still terminating there. It
// logs what is left rather than skipping: the expected capacity is read from the frozen
// NRT, which counts those pods.
func snWaitNodeQuiet(t testing.TB, c client.Client, node string, timeout time.Duration) {
	t.Helper()
	skipNS := map[string]bool{"kube-system": true, operatorNamespace(): true, balloonsNS(): true}
	deadline := time.Now().Add(timeout)
	for {
		var pods corev1.PodList
		err := c.List(context.Background(), &pods, client.MatchingFields{"spec.nodeName": node})
		var holders []string
		if err == nil {
			if holders = snCPUHolders(pods.Items, skipNS); len(holders) == 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Logf("node %s did not quiesce within %s (pods that may hold pinned CPUs: %v; last list error: %v); the frozen NRT will count them",
				node, timeout, holders, err)
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// snCPUHolders names the pods outside skipNS that are terminating, carry the pinning
// annotation, or are Guaranteed with a whole-core CPU request.
func snCPUHolders(pods []corev1.Pod, skipNS map[string]bool) []string {
	var out []string
	for i := range pods {
		p := &pods[i]
		if skipNS[p.Namespace] || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		why := ""
		switch {
		case p.DeletionTimestamp != nil:
			why = "terminating"
		case p.Annotations[pinnedByAnnotation] != "":
			why = "pinned"
		case p.Status.QOSClass == corev1.PodQOSGuaranteed && snWholeCoreContainer(p):
			why = "guaranteed whole-core"
		default:
			continue
		}
		out = append(out, p.Namespace+"/"+p.Name+" ("+why+")")
	}
	return out
}

// snWholeCoreContainer reports whether any container asks for whole Guaranteed cores.
func snWholeCoreContainer(p *corev1.Pod) bool {
	for i := range p.Spec.Containers {
		if webhook.HasWholeCoreGuaranteedCPUForE2E(&p.Spec.Containers[i]) {
			return true
		}
	}
	return false
}

// snNodeHostname returns the node's hostname label, or its name when absent.
func snNodeHostname(t testing.TB, c client.Client, node string) string {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, &n); err != nil {
		t.Fatalf("get node %s: %v", node, err)
	}
	if h := n.Labels[snHostnameLabel]; h != "" {
		return h
	}
	return node
}

// snFreezePlugin removes the balloons pod from one node with a hostname NotIn
// affinity term, and on cleanup restores the original affinity and waits for it to return.
func snFreezePlugin(t *testing.T, c client.Client, node, hostname string) {
	t.Helper()
	ctx := context.Background()
	dsName, dsNS := snDaemonSet()
	key := client.ObjectKey{Name: dsName, Namespace: dsNS}

	var ds appsv1.DaemonSet
	if err := c.Get(ctx, key, &ds); err != nil {
		if apierrors.IsNotFound(err) {
			skipNoConfig(t, "balloons DaemonSet %s/%s not found (set E2E_BALLOONS_DAEMONSET/E2E_BALLOONS_NAMESPACE): %v", dsNS, dsName, err)
		}
		t.Fatalf("get balloons DaemonSet %s/%s: %v", dsNS, dsName, err)
	}

	origAffinity := ds.Spec.Template.Spec.Affinity.DeepCopy()
	t.Cleanup(func() {
		cur := &appsv1.DaemonSet{}
		if err := c.Get(context.Background(), key, cur); err != nil {
			t.Errorf("cleanup: get balloons DaemonSet to restore affinity: %v", err)
			return
		}
		cur.Spec.Template.Spec.Affinity = origAffinity
		if err := c.Update(context.Background(), cur); err != nil {
			t.Errorf("cleanup: FAILED to restore balloons DaemonSet affinity (plugin may stay off node %s): %v", node, err)
			return
		}

		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			if snPluginReadyOnNode(c, dsNS, hostname, node) {
				t.Logf("cleanup: balloons plugin restored on node %s", node)
				return
			}
			time.Sleep(5 * time.Second)
		}
		// A plugin that does not return leaves the node broken for later tests.
		t.Errorf("cleanup: balloons plugin did not return to node %s within the deadline; the node may be left without its plugin — check the DaemonSet", node)
	})

	ds.Spec.Template.Spec.Affinity = snExcludeNodeAffinity(origAffinity, hostname)
	if err := c.Update(ctx, &ds); err != nil {
		t.Fatalf("patch balloons DaemonSet affinity to exclude node %s: %v", node, err)
	}
	t.Logf("froze balloons plugin: excluded hostname %q from DaemonSet %s/%s pod template", hostname, dsNS, dsName)
}

// snExcludeNodeAffinity returns a copy of orig with a hostname NotIn requirement
// AND-ed onto every required term.
func snExcludeNodeAffinity(orig *corev1.Affinity, hostname string) *corev1.Affinity {
	a := orig.DeepCopy()
	if a == nil {
		a = &corev1.Affinity{}
	}
	if a.NodeAffinity == nil {
		a.NodeAffinity = &corev1.NodeAffinity{}
	}
	if a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}
	req := a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	exclude := corev1.NodeSelectorRequirement{
		Key:      snHostnameLabel,
		Operator: corev1.NodeSelectorOpNotIn,
		Values:   []string{hostname},
	}
	if len(req.NodeSelectorTerms) == 0 {
		req.NodeSelectorTerms = []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{exclude}}}
	} else {
		for i := range req.NodeSelectorTerms {
			req.NodeSelectorTerms[i].MatchExpressions = append(req.NodeSelectorTerms[i].MatchExpressions, exclude)
		}
	}
	return a
}

// snPluginReadyOnNode reports whether a balloons pod is Ready on the node; read
// errors count as not ready.
func snPluginReadyOnNode(c client.Client, dsNS, hostname, node string) bool {
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace(dsNS),
		client.MatchingLabels{"app.kubernetes.io/name": "nri-resource-policy-balloons"}); err != nil {
		return false
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != node && p.Spec.NodeName != hostname {
			continue
		}
		if p.DeletionTimestamp != nil {
			continue
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

// snPluginGoneFromNode polls until no ready balloons pod remains on the node.
func snPluginGoneFromNode(t *testing.T, c client.Client, node, hostname string) bool {
	t.Helper()
	_, dsNS := snDaemonSet()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if !snPluginReadyOnNode(c, dsNS, hostname, node) {
			return true
		}
		time.Sleep(5 * time.Second)
	}
	return false
}

// snNRTResourceVersion returns the node's NRT resourceVersion and whether the CR exists.
func snNRTResourceVersion(t *testing.T, c client.Client, node string) (string, bool) {
	t.Helper()
	nrt := newUnstructured(nrtGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, nrt); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false
		}
		t.Fatalf("get NodeResourceTopology %s: %v", node, err)
	}
	return nrt.GetResourceVersion(), true
}

// snNRTFrozen reports whether the NRT resourceVersion stayed at want for snFreezeSettle.
func snNRTFrozen(t *testing.T, c client.Client, node, want string) bool {
	t.Helper()
	deadline := time.Now().Add(snFreezeSettle)
	for time.Now().Before(deadline) {
		rv, ok := snNRTResourceVersion(t, c, node)
		if !ok || rv != want {
			return false
		}
		time.Sleep(5 * time.Second)
	}

	rv, ok := snNRTResourceVersion(t, c, node)
	return ok && rv == want
}
