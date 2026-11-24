// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Mirror of the sizing constants in internal/balloons/reserved.go, not imported, so a regression there is caught rather than shared.
const (
	msReservedMinCPUs         = 4  // reservedMinCPUs
	msReservedMaxPercent      = 25 // reservedMaxPercent
	msReservedMinWorkloadCPUs = 16 // reservedMinWorkloadCPUs
	// msHugeMin dwarfs any node's demand, so it always resolves to the clamp ceiling.
	msHugeMin = 100000
)

// msResolveReserved mirrors ResolveReservedCPUs for the measured path: ceil(usage*(1+headroom%)), raised to min,
// clamped, then ÷threadsPerCore in physical-cores mode.
func msResolveReserved(headroom, min, physical, threads, usage int, physMode bool) int {
	if usage < 0 {
		usage = 0
	}
	sized := (usage*(100+headroom) + 99) / 100
	if sized < min {
		sized = min
	}
	reserved := sized
	if reserved < msReservedMinCPUs {
		reserved = msReservedMinCPUs
	}
	if maxByPercent := physical * msReservedMaxPercent / 100; reserved > maxByPercent {
		reserved = maxByPercent
	}
	if maxByWorkload := physical - msReservedMinWorkloadCPUs; reserved > maxByWorkload {
		reserved = maxByWorkload
	}
	reserved -= reserved % 2
	if reserved < 0 {
		reserved = 0
	}
	// Measured is a LOGICAL-CPU baseline, so physical-cores mode converts it.
	if physMode && threads > 1 && reserved > 0 {
		reserved /= threads
		reserved -= reserved % 2
	}
	return reserved
}

// runMeasuredReservedSizing proves measured mode honors the Min floor and grows the reserved pool with non-pinned demand.
// The node controller reads pods only on reconcile (it does not watch them), so demand is created before the patch.
func runMeasuredReservedSizing(t *testing.T, c client.Client, node string) {
	ctx := context.Background()

	// Density mode ignores the reserved sizing spec.
	if clusterHTSiblingPolicy(t, c) == "reserved" {
		skipNoConfig(t, "cluster htSiblingPolicy=reserved (density mode); the reserved sizing spec is ignored there")
	}
	physMode := clusterCPUAllocation(t, c) == "physical-cores"
	physical, threads := nodeTopology(t, c, node)
	if physical == 0 {
		skipNoConfig(t, "node %s has no usable physical-cpus label; cannot compute expected reserved size", node)
	}

	// Restore clears the block first: a merge patch would otherwise leave stale sibling keys.
	orig, origSet := msCaptureReserved(t, c)
	t.Cleanup(func() {
		bg := context.Background()
		msPatchReserved(t, c, bg, []byte(`{"spec":{"balloon":{"reserved":null}}}`))
		if origSet && orig != nil {
			body, err := json.Marshal(map[string]any{"spec": map[string]any{"balloon": map[string]any{"reserved": orig}}})
			if err != nil {
				t.Errorf("cleanup: marshal original reserved spec: %v", err)
				return
			}
			msPatchReserved(t, c, bg, body)
		}
	})

	// Expectations are in rendered CPUs: a reserved cpuset rounds up across NUMA nodes (pfApplied).
	numa, spread := pfNUMANodes(t, c, node), msReservedSpread(t, c, node)
	expect := func(headroom, min, usage int) int {
		return pfApplied(msResolveReserved(headroom, min, physical, threads, usage, physMode), numa, spread)
	}
	ceiling := expect(0, msHugeMin, 0)
	if ceiling == 0 {
		skipNoCapability(t, "node %s reserved clamp ceiling resolves to 0 (physical=%d, threads=%d); nothing to size", node, physical, threads)
	}
	floor := expect(0, msReservedMinCPUs, 0)
	t.Logf("node %s: physical=%d threads=%d numa=%d spread=%v physicalCores=%v reservedCeiling=%d minFloor=%d",
		node, physical, threads, numa, spread, physMode, ceiling, floor)

	t.Run("MinFloor", func(t *testing.T) {
		// A huge Min must resolve to exactly the clamp ceiling.
		msSetMeasured(t, c, ctx, 0, msHugeMin)
		rBig := msWaitReservedStable(t, c, ctx, node)
		if rBig != ceiling {
			t.Errorf("measured{headroom:0,min:%d} reserved=%d, want the clamp ceiling %d", msHugeMin, rBig, ceiling)
		}

		// A small Min never drops below the floor.
		msSetMeasured(t, c, ctx, 0, msReservedMinCPUs)
		rLow := msWaitReservedStable(t, c, ctx, node)
		if rLow < floor {
			t.Errorf("measured{headroom:0,min:%d} reserved=%d, want >= min floor %d", msReservedMinCPUs, rLow, floor)
		}
		if rLow > ceiling {
			t.Errorf("measured reserved=%d exceeds the clamp ceiling %d", rLow, ceiling)
		}
		// Raising Min never shrinks the pool, and grows it whenever the ceiling leaves room.
		if rBig < rLow {
			t.Errorf("raising measured Min shrank the reserved pool (min=%d→%d, min=%d→%d)",
				msReservedMinCPUs, rLow, msHugeMin, rBig)
		}
		if ceiling > floor && rBig <= rLow {
			t.Errorf("measured reserved did not respond to Min: min=%d gave %d and min=%d gave %d (want strictly larger; ceiling %d > floor %d)",
				msReservedMinCPUs, rLow, msHugeMin, rBig, ceiling, floor)
		}

		// Exact check against the operator's own observed usage (needs metrics).
		if usage, ok := msObservedUsage(t, c, node); ok {
			want := expect(0, msReservedMinCPUs, usage)
			if rLow != want {
				t.Errorf("measured{headroom:0,min:%d} reserved=%d, want %d for observed usage=%d",
					msReservedMinCPUs, rLow, want, usage)
			}
			msAssertMetricMatchesRendered(t, c, node, rLow)
		}
	})

	t.Run("TracksDemand", func(t *testing.T) {
		// Baseline with a small Min and no added demand.
		msSetMeasured(t, c, ctx, 0, msReservedMinCPUs)
		rNoFill := msWaitReservedStable(t, c, ctx, node)
		if rNoFill == ceiling {
			skipNoCapability(t, "node %s already saturates the reserved ceiling (%d) before adding demand; cannot show growth", node, ceiling)
		}

		// "default" is not a webhook target, so these pods count toward reserved usage.
		demandCores := msFillerCores()
		fillers := msCreateFillers(t, c, node, demandCores)
		if msAnyTerminal(t, c, fillers) {
			skipNoCapability(t, "filler demand of %d cores did not fit on node %s (a filler went terminal); cannot exercise usage-driven sizing", demandCores, node)
		}

		// Re-patch to force a reconcile that observes the fillers.
		msSetMeasured(t, c, ctx, 0, msReservedMinCPUs)
		rWithFill := msWaitReservedStable(t, c, ctx, node)

		// Usage is at least the added demand, and sizing is monotonic up to the ceiling.
		lb := expect(0, msReservedMinCPUs, demandCores)
		if lb > ceiling {
			lb = ceiling
		}
		if rWithFill < lb {
			t.Errorf("with %d cores of added demand reserved=%d, want >= %d (formula lower bound, ceiling %d)",
				demandCores, rWithFill, lb, ceiling)
		}
		if rWithFill < rNoFill {
			t.Errorf("adding demand shrank the reserved pool (%d → %d)", rNoFill, rWithFill)
		}
		// Demand that beats the no-demand size must strictly grow the pool.
		if lb > rNoFill && rWithFill <= rNoFill {
			t.Errorf("reserved pool did not grow with demand: no-demand=%d, +%d cores=%d (want strictly larger; formula lower bound %d)",
				rNoFill, demandCores, rWithFill, lb)
		}

		// Exact check: observed usage includes the fillers and the size matches the formula.
		if usage, ok := msObservedUsage(t, c, node); ok {
			if usage < demandCores {
				t.Errorf("operator observed usage=%d < added demand=%d; filler demand was not counted", usage, demandCores)
			}
			want := expect(0, msReservedMinCPUs, usage)
			if rWithFill != want {
				t.Errorf("with demand reserved=%d, want %d for observed usage=%d", rWithFill, want, usage)
			}
			msAssertMetricMatchesRendered(t, c, node, rWithFill)
		}
	})
}

// msCaptureReserved returns spec.balloon.reserved and whether it was set; skips when the singleton is absent.
func msCaptureReserved(t *testing.T, c client.Client) (map[string]any, bool) {
	t.Helper()
	bcc, found, err := getBCC(context.Background(), c)
	if err != nil {
		t.Fatalf("get BalloonsClusterConfig/cluster: %v", err)
	}
	if !found {
		skipNoConfig(t, "BalloonsClusterConfig/cluster not present; cannot exercise/restore measured sizing")
	}
	reserved, present, err := unstructured.NestedMap(bcc.Object, "spec", "balloon", "reserved")
	if err != nil {
		t.Fatalf("read spec.balloon.reserved: %v", err)
	}
	if !present {
		return nil, false
	}
	return reserved, true
}

// msMeasuredSpec builds a spec.balloon.reserved value selecting measured mode.
func msMeasuredSpec(headroom, min int) map[string]any {
	return map[string]any{"measured": map[string]any{"headroomPercent": headroom, "min": min}}
}

// msSetMeasured switches to measured mode, clearing the block first so the merge does not violate the
// "at most one of cpus/percent/measured" CEL rule.
func msSetMeasured(t *testing.T, c client.Client, ctx context.Context, headroom, min int) {
	t.Helper()
	msPatchReserved(t, c, ctx, []byte(`{"spec":{"balloon":{"reserved":null}}}`))
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"balloon": map[string]any{"reserved": msMeasuredSpec(headroom, min)}}})
	if err != nil {
		t.Fatalf("marshal measured spec: %v", err)
	}
	msPatchReserved(t, c, ctx, body)
}

// msPatchReserved merge-patches the singleton, reporting errors without Fatal so t.Cleanup can call it.
func msPatchReserved(t *testing.T, c client.Client, ctx context.Context, body []byte) {
	t.Helper()
	patchBCC(ctx, c, body, func(verb string, err error) {
		t.Errorf("%s for spec.balloon.reserved patch (%s): %v", verb, string(body), err)
	})
}

// msReservedCount returns the reserved CPU count from reservedResources.cpu (bare count or cpuset).
func msReservedCount(t *testing.T, c client.Client, node string) (int, bool) {
	t.Helper()
	bp := newUnstructured(bpGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "node." + node, Namespace: balloonsNS()}, bp); err != nil {
		t.Fatalf("get BalloonsPolicy node.%s: %v", node, err)
	}
	v, found, _ := unstructured.NestedString(bp.Object, "spec", "reservedResources", "cpu")
	if !found || v == "" {
		return 0, false
	}
	if rest, ok := strings.CutPrefix(v, "cpuset:"); ok {
		return len(parseCPUSet(rest)), true
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, false
	}
	return n, true
}

// msReservedSpread reports whether the node's reserved pool is a cpuset (NUMA-spread).
func msReservedSpread(t *testing.T, c client.Client, node string) bool {
	t.Helper()
	bp := newUnstructured(bpGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "node." + node, Namespace: balloonsNS()}, bp); err != nil {
		t.Fatalf("get BalloonsPolicy node.%s: %v", node, err)
	}
	v, _, _ := unstructured.NestedString(bp.Object, "spec", "reservedResources", "cpu")
	return strings.HasPrefix(v, "cpuset:")
}

// msWaitReservedStable returns the reserved count once two spaced reads agree.
func msWaitReservedStable(t *testing.T, c client.Client, ctx context.Context, node string) int {
	t.Helper()
	// Let the triggered reconcile begin before sampling.
	time.Sleep(5 * time.Second)
	deadline := time.Now().Add(120 * time.Second)
	prev, prevOK := msReservedCount(t, c, node)
	for time.Now().Before(deadline) {
		time.Sleep(4 * time.Second)
		cur, ok := msReservedCount(t, c, node)
		if ok && prevOK && cur == prev {
			return cur
		}
		prev, prevOK = cur, ok
	}
	if !prevOK {
		t.Fatalf("node %s BalloonsPolicy never reported a reservedResources.cpu count", node)
	}
	t.Logf("reserved count for node %s did not fully stabilize; using last read %d", node, prev)
	return prev
}

// msObservedUsage derives the operator's observed usage as round(utilization_ratio × reserved_cpus); ok=false without E2E_METRICS.
func msObservedUsage(t *testing.T, c client.Client, node string) (int, bool) {
	t.Helper()
	if os.Getenv("E2E_METRICS") == "" {
		return 0, false
	}
	body := scrapeOperatorMetrics(t)
	reserved, ok1 := metricValueForNode(body, "cpu_pinning_node_reserved_cpus", node)
	ratio, ok2 := metricValueForNode(body, "cpu_pinning_reserved_pool_utilization_ratio", node)
	if !ok1 || !ok2 {
		return 0, false
	}
	return int(ratio*reserved + 0.5), true
}

// msAssertMetricMatchesRendered checks cpu_pinning_node_reserved_cpus equals the rendered count.
func msAssertMetricMatchesRendered(t *testing.T, c client.Client, node string, rendered int) {
	t.Helper()
	if os.Getenv("E2E_METRICS") == "" {
		return
	}
	body := scrapeOperatorMetrics(t)
	got, ok := metricValueForNode(body, "cpu_pinning_node_reserved_cpus", node)
	if !ok {
		t.Errorf("metrics missing cpu_pinning_node_reserved_cpus for node %q", node)
		return
	}
	if int(got) != rendered {
		t.Errorf("cpu_pinning_node_reserved_cpus=%v, want rendered reserved count %d", got, rendered)
	}
}

// msFillerCores is the TracksDemand filler size; override with E2E_MEASURED_FILLER_CORES on a busy node.
func msFillerCores() int {
	if v := os.Getenv("E2E_MEASURED_FILLER_CORES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 6
}

// msCreateFillers creates single-core non-pinned pods on the node and returns their keys.
// Names carry a random token: createAndCleanup deletes by name first, so a predictable name could delete a bystander.
func msCreateFillers(t *testing.T, c client.Client, node string, cores int) []types.NamespacedName {
	t.Helper()
	token := msRunToken(t)
	keys := make([]types.NamespacedName, 0, cores)
	for i := 0; i < cores; i++ {
		name := fmt.Sprintf("ms-filler-%s-%02d", token, i)
		createAndCleanup(t, c, msFillerPod(name, "default", node, "1"))
		keys = append(keys, types.NamespacedName{Name: name, Namespace: "default"})
	}
	// Let the scheduler place them before the reconcile reads.
	time.Sleep(10 * time.Second)
	return keys
}

// msRunToken returns a short random hex token for filler names.
func msRunToken(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate filler name token: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// msFillerPod builds a non-pinned CPU-requesting pod bound to the node.
func msFillerPod(name, ns, node, req string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"app": "ms-filler"},
		},
		Spec: corev1.PodSpec{
			NodeName:    node,
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:    "filler",
				Image:   "registry.k8s.io/pause:3.9",
				Command: []string{"/pause"},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(req)},
				},
			}},
		},
	}
}

// msAnyTerminal reports whether a filler went terminal (e.g. OutOfcpu), which means its demand is not counted.
func msAnyTerminal(t *testing.T, c client.Client, keys []types.NamespacedName) bool {
	t.Helper()
	for _, k := range keys {
		var p corev1.Pod
		if err := c.Get(context.Background(), k, &p); err != nil {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			return true
		}
	}
	return false
}
