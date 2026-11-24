// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ccMetric is labeled {workload_namespace,reason,mode}; workload_namespace is the pod's.
	ccMetric = "cpu_pinning_capacity_check_failures_total"

	ccReasonDenied  = "CapacityCheckFailed"  // enforce-mode event reason
	ccReasonWarning = "CapacityCheckWarning" // warn-mode event reason

	// ccDenialSubstr is the stable prefix of the enforce-mode denial, surfaced as the Create error.
	ccDenialSubstr = "cpu-pinning capacity check failed"

	// ccReconcileTimeout bounds the wait for a new mode to reach the webhook's in-memory index (nothing rendered to poll).
	ccReconcileTimeout = 90 * time.Second
)

// ccMode returns the policy's capacityCheck.mode, defaulting to "warn". Read errors are fatal: the value restores shared state.
func ccMode(t *testing.T, c client.Client, name string) string {
	t.Helper()
	bpp := newUnstructured(bppGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, bpp); err != nil {
		t.Fatalf("get BalloonPinningPolicy %q to read capacityCheck.mode: %v", name, err)
	}
	m, _, _ := unstructured.NestedString(bpp.Object, "spec", "capacityCheck", "mode")
	if m == "" {
		return "warn"
	}
	return m
}

// ccSetMode patches capacityCheck.mode and restores the original on cleanup. Both wait for status.observedGeneration,
// which the operator sets only after the spec reached the webhook match index.
func ccSetMode(t *testing.T, c client.Client, name, mode string) {
	t.Helper()
	orig := ccMode(t, c, name)
	key := client.ObjectKey{Name: name}
	t.Cleanup(func() {
		restore := fmt.Sprintf(`{"spec":{"capacityCheck":{"mode":%q}}}`, orig)
		cur := newUnstructured(bppGVK)
		if err := c.Get(context.Background(), key, cur); err != nil {
			t.Errorf("cleanup: get BalloonPinningPolicy %q to restore mode: %v", name, err)
			return
		}
		if err := c.Patch(context.Background(), cur, client.RawPatch(types.MergePatchType, []byte(restore))); err != nil {
			t.Errorf("cleanup: FAILED to restore capacityCheck.mode to %q: %v", orig, err)
			return
		}
		// Cleanup barrier: the next gated test must start from the restored mode.
		if !waitForPolicySpecSettled(c, key, ccReconcileTimeout) {
			t.Logf("cleanup: policy %q observedGeneration did not catch up after restoring capacityCheck.mode=%q; the next test may briefly see stale mode", name, orig)
		}
	})
	patch := fmt.Sprintf(`{"spec":{"capacityCheck":{"mode":%q}}}`, mode)
	cur := newUnstructured(bppGVK)
	if err := c.Get(context.Background(), key, cur); err != nil {
		t.Fatalf("get BalloonPinningPolicy %q: %v", name, err)
	}
	if err := c.Patch(context.Background(), cur, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatalf("patch capacityCheck.mode to %q: %v", mode, err)
	}
	// The per-test retry loops absorb any residual lag.
	if !waitForPolicySpecSettled(c, key, ccReconcileTimeout) {
		t.Logf("policy %q observedGeneration did not catch up after setting capacityCheck.mode=%q; relying on the per-test reconcile retry loop", name, mode)
	}
}

// ccOverCapacityCores returns one more core than the largest per-NUMA hardware capacity across the fleet:
// the pods are unbound and pass if ANY node fits. It over-shoots the true boundary (nodeFits also subtracts
// the reserved share), which is fine since it only has to exceed capacity.
func ccOverCapacityCores(t *testing.T, c client.Client, node string) int {
	t.Helper()
	var nodes corev1.NodeList
	if err := c.List(context.Background(), &nodes); err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	hyperthreads := clusterCPUAllocation(t, c) == "hyperthreads"
	maxPerNUMA := 0
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Labels[labelBalloonsEnabled] != "true" {
			continue
		}
		phys, _ := strconv.Atoi(n.Labels[labelPhysicalCPUs])
		numa, _ := strconv.Atoi(n.Labels[labelNumaNodes])
		if phys == 0 || numa == 0 {
			skipNoConfig(t, "node %s missing physical-cpus/numa-nodes labels; cannot size an over-capacity request", n.Name)
		}
		perNUMA := phys / numa
		if hyperthreads {
			threads, _ := strconv.Atoi(n.Labels[labelThreadsPerCore])
			perNUMA *= max(threads, 1)
		}
		maxPerNUMA = max(maxPerNUMA, perNUMA)
	}
	if maxPerNUMA < 1 {
		skipNoCapability(t, "node %s computed per-NUMA capacity %d; cannot build an over-capacity request", node, maxPerNUMA)
	}
	return maxPerNUMA + 1
}

// ccPod builds a whole-core span-1 vLLM pod whose only failing gate is per-NUMA capacity (no memory request).
func ccPod(name, ns string, cores int) *corev1.Pod {
	return vllmPod(name, ns, strconv.Itoa(cores), strconv.Itoa(cores), 1 /*TP => span 1*/)
}

// ccCreate creates the pod and registers its deletion, returning the Create error instead of failing.
func ccCreate(t *testing.T, c client.Client, pod *corev1.Pod) error {
	t.Helper()
	ctx := context.Background()
	_ = c.Delete(ctx, pod) // clear any leftover from a prior run
	err := e2eCreate(ctx, c, pod)
	t.Cleanup(func() {
		e2eDelete(t, c, pod, client.GracePeriodSeconds(0))
	})
	return err
}

// ccPodHasEvent reports whether an event on the Pod ns/podName matches involvedObject.kind=="Pod"
// + involvedObject.name + type=="Warning" + reason, matching the webhook's
// recorder.
func ccPodHasEvent(t *testing.T, c client.Client, ns, podName, reason string) bool {
	t.Helper()
	var evs corev1.EventList
	if err := c.List(context.Background(), &evs, client.InNamespace(ns)); err != nil {
		t.Fatalf("list events in %s: %v", ns, err)
	}
	for i := range evs.Items {
		e := &evs.Items[i]
		if e.InvolvedObject.Kind == "Pod" &&
			e.InvolvedObject.Name == podName &&
			e.Type == corev1.EventTypeWarning &&
			e.Reason == reason {
			return true
		}
	}
	return false
}

// ccFailureCount sums the failures counter for ns and mode across reasons. The counter is cumulative, so compare deltas.
func ccFailureCount(body, ns, mode string) float64 {
	return counterSum(body, ccMetric, `workload_namespace="`+ns+`"`, `mode="`+mode+`"`)
}

// ccMetricDeltaBody scrapes the after side of a delta, skipping the subtest when it cannot be read.
// Without a baseline every count reads 0 and an unrelated increment would pass.
func ccMetricDeltaBody(t *testing.T, baseline string) string {
	t.Helper()
	after := scrapeMetricsAllReplicasOrSkip(t, "the manager /metrics endpoints did not serve this scrape via the apiserver pod proxy, so the %s "+
		"delta cannot be read. Check metrics.enabled and E2E_METRICS_PORT/E2E_METRICS_SCHEME (see docs/internal/e2e-internals.md)", ccMetric)
	if baseline == "" {
		skipNoConfig(t, "the manager /metrics endpoints serve now but did not when the baseline was taken, so there is no before value to compare "+
			"%s against", ccMetric)
	}
	return after
}

// runCapacityCheckEnforceDeniesOverCapacityPod proves enforce denies an over-capacity pod and increments mode="enforce".
func runCapacityCheckEnforceDeniesOverCapacityPod(t *testing.T, c client.Client, node string) {
	ns := useTargetNamespace(t, c)
	policy := installedPolicyName(t, c)
	cores := ccOverCapacityCores(t, c, node)

	// An empty baseline means nothing served; the metric subtest turns that into a counted skip.
	before := scrapeMetricsAllReplicas(t)
	beforeCount := ccFailureCount(before, ns, "enforce")

	ccSetMode(t, c, policy, "enforce")

	// Retry past reconcile lag with a fresh name per attempt.
	var lastName string
	var createErr error
	deadline := time.Now().Add(ccReconcileTimeout)
	for i := 0; ; i++ {
		lastName = fmt.Sprintf("e2e-cc-enforce-%d", i)
		createErr = ccCreate(t, c, ccPod(lastName, ns, cores))
		if createErr != nil && strings.Contains(createErr.Error(), ccDenialSubstr) {
			break
		}
		if createErr == nil {
			// Admitted: enforce not live yet. Drop it so a stale decision doesn't mask enforce.
			_ = c.Delete(context.Background(), ccPod(lastName, ns, cores), client.GracePeriodSeconds(0))
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}

	if createErr == nil {
		t.Fatalf("over-capacity pod (%d cores, span 1) was ADMITTED under capacityCheck.mode=enforce; want denial", cores)
	}
	if !strings.Contains(createErr.Error(), ccDenialSubstr) {
		t.Fatalf("Create error = %q, want it to contain %q (the enforce-mode capacity denial)", createErr.Error(), ccDenialSubstr)
	}
	t.Logf("enforce denied over-capacity pod: %v", createErr)

	// A subtest, so an unscrapeable endpoint is a counted skip while the denial assertions stand.
	t.Run("metric delta", func(t *testing.T) {
		after := ccMetricDeltaBody(t, before)
		if got := ccFailureCount(after, ns, "enforce"); got <= beforeCount {
			t.Errorf("%s{workload_namespace=%q,mode=\"enforce\"} did not increment (before=%v after=%v)", ccMetric, ns, beforeCount, got)
		}
	})

	// A denied pod is never persisted, so its event may not be queryable: log only.
	if waitForWarningEvent(t, c, ns, lastName, ccReasonDenied, "", 20*time.Second, func(err error) {
		t.Fatalf("list events in %s: %v", ns, err)
	}) {
		t.Logf("observed %s event for denied pod %s", ccReasonDenied, lastName)
	} else {
		t.Logf("no %s event observed for denied pod %s (expected: denied pods are not persisted)", ccReasonDenied, lastName)
	}
}

// runCapacityCheckWarnAdmitsOverCapacityPod proves warn admits and pins the pod, emits CapacityCheckWarning and increments mode="warn".
func runCapacityCheckWarnAdmitsOverCapacityPod(t *testing.T, c client.Client, node string) {
	ns := useTargetNamespace(t, c)
	policy := installedPolicyName(t, c)
	cores := ccOverCapacityCores(t, c, node)

	before := scrapeMetricsAllReplicas(t)
	beforeCount := ccFailureCount(before, ns, "warn")

	ccSetMode(t, c, policy, "warn")

	// A denial here is enforce still in effect; retry with a fresh name.
	balloonKey := balloonAnnotationBase + "/container.vllm"
	var admittedName string
	var pinned, warned bool
	deadline := time.Now().Add(ccReconcileTimeout)
	for i := 0; ; i++ {
		name := fmt.Sprintf("e2e-cc-warn-%d", i)
		err := ccCreate(t, c, ccPod(name, ns, cores))
		if err == nil {
			got := &corev1.Pod{}
			if gErr := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, got); gErr == nil {
				pinned = got.Annotations[balloonKey] != "" && got.Annotations[pinnedByAnnotation] == "cpu-pinning-operator"
			}
			if pinned && waitForWarningEvent(t, c, ns, name, ccReasonWarning, "", 20*time.Second, func(err error) {
				t.Fatalf("list events in %s: %v", ns, err)
			}) {
				admittedName, warned = name, true
				break
			}
		} else if !strings.Contains(err.Error(), ccDenialSubstr) {
			t.Fatalf("unexpected Create error under warn mode: %v", err)
		}
		if time.Now().After(deadline) {
			admittedName = name
			break
		}
		time.Sleep(3 * time.Second)
	}

	if !pinned {
		t.Fatalf("over-capacity pod (%d cores) was not admitted+pinned under capacityCheck.mode=warn (last=%s)", cores, admittedName)
	}
	if !warned {
		t.Errorf("admitted over-capacity pod %s carried no %s event under warn mode", admittedName, ccReasonWarning)
	}

	t.Run("metric delta", func(t *testing.T) {
		after := ccMetricDeltaBody(t, before)
		if got := ccFailureCount(after, ns, "warn"); got <= beforeCount {
			t.Errorf("%s{workload_namespace=%q,mode=\"warn\"} did not increment (before=%v after=%v)", ccMetric, ns, beforeCount, got)
		}
	})
}

// runCapacityCheckOffSkipsCheck proves off admits and pins the pod with no capacity-check event.
// The per-pod event is the signal: the counter is not per-pod.
func runCapacityCheckOffSkipsCheck(t *testing.T, c client.Client, node string) {
	ns := useTargetNamespace(t, c)
	policy := installedPolicyName(t, c)
	cores := ccOverCapacityCores(t, c, node)

	ccSetMode(t, c, policy, "off")

	// A denial here is enforce still in effect; retry with a fresh name.
	balloonKey := balloonAnnotationBase + "/container.vllm"
	var name string
	var pinned bool
	deadline := time.Now().Add(ccReconcileTimeout)
	for i := 0; ; i++ {
		name = fmt.Sprintf("e2e-cc-off-%d", i)
		err := ccCreate(t, c, ccPod(name, ns, cores))
		if err == nil {
			got := &corev1.Pod{}
			if gErr := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, got); gErr == nil {
				pinned = got.Annotations[balloonKey] != "" && got.Annotations[pinnedByAnnotation] == "cpu-pinning-operator"
			}
			if pinned {
				break
			}
		} else if !strings.Contains(err.Error(), ccDenialSubstr) {
			t.Fatalf("unexpected Create error under off mode: %v", err)
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}

	if !pinned {
		t.Fatalf("over-capacity pod (%d cores) was not admitted+pinned under capacityCheck.mode=off (last=%s)", cores, name)
	}

	time.Sleep(10 * time.Second)
	if ccPodHasEvent(t, c, ns, name, ccReasonDenied) {
		t.Errorf("off mode emitted a %s event for %s; the capacity check should be skipped", ccReasonDenied, name)
	}
	if ccPodHasEvent(t, c, ns, name, ccReasonWarning) {
		t.Errorf("off mode emitted a %s event for %s; the capacity check should be skipped", ccReasonWarning, name)
	}
}
