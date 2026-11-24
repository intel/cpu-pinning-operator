// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// These cases edit the shared default policy's match, so they run as subtests of
// TestGatedMutatingSuite.

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// csCtr is a probe container: a name and a CPU count used as request and limit.
type csCtr struct {
	name string
	cpu  string
}

// runContainerSelectionGlobSingleHit proves a glob matching exactly one container pins
// that container container-scoped and leaves the sidecar alone.
func runContainerSelectionGlobSingleHit(t *testing.T, c client.Client, _ string) {
	ns := useTargetNamespace(t, c)

	csApplyPolicyMatch(t, c, []string{"vllm-*"}, "")

	key := csContainerKey("vllm-server")
	created := csAdmitUntil(t, c, ns, "e2e-cs-single",
		[]csCtr{{name: "vllm-server", cpu: "2"}, {name: "sidecar", cpu: "250m"}},
		func(p *corev1.Pod) bool { return p.Annotations[key] != "" })

	got := created.Annotations[key]
	if got == "" {
		t.Fatalf("glob %q matched one container but no container-scoped annotation %q was stamped (index not updated or bug)", "vllm-*", key)
	}
	if !strings.HasPrefix(got, "balloon-span") {
		t.Errorf("annotation %s = %q, want a balloon-span<N> value", key, got)
	}
	if created.Annotations[pinnedByAnnotation] != "cpu-pinning-operator" {
		t.Errorf("pinned-by = %q, want cpu-pinning-operator", created.Annotations[pinnedByAnnotation])
	}
	if v := created.Annotations[csContainerKey("sidecar")]; v != "" {
		t.Errorf("non-matching sidecar got a balloon annotation %q (glob should not select it)", v)
	}
	if v := created.Annotations[csPodKey()]; v != "" {
		t.Errorf("single glob hit produced a POD-scoped annotation %q, want container-scoped only", v)
	}
}

// runContainerSelectionAmbiguousWholePod proves a multi-container glob under wholePod
// leaves the pod unpinned, emits NotPinned, and counts reason="ambiguous".
func runContainerSelectionAmbiguousWholePod(t *testing.T, c client.Client, _ string) {
	ns := useTargetNamespace(t, c)

	csApplyPolicyMatch(t, c, []string{"vllm-*"}, "wholePod")

	baseline := scrapeMetricsAllReplicas(t)
	before := csSkippedAmbiguous(baseline, ns)

	pod := csPod("e2e-cs-ambiguous", ns,
		[]csCtr{{name: "vllm-a", cpu: "2"}, {name: "vllm-b", cpu: "2"}})
	created := createAndCleanup(t, c, pod)

	for k, v := range created.Annotations {
		if strings.HasPrefix(k, balloonAnnotationBase) {
			t.Errorf("ambiguous multi-hit pod got balloon annotation %s=%q (should be skipped)", k, v)
		}
	}
	if created.Annotations[pinnedByAnnotation] != "" {
		t.Errorf("ambiguous multi-hit pod was marked pinned-by=%q", created.Annotations[pinnedByAnnotation])
	}

	// The event tells an ambiguous skip from a non-target pod, which emits none.
	if !waitForWarningEvent(t, c, ns, created.Name, "NotPinned", "byContainerName matched", 60*time.Second, nil) {
		t.Errorf("no NotPinned Warning event mentioning the ambiguous byContainerName match for pod %s/%s", ns, created.Name)
	}

	t.Run("metric delta", func(t *testing.T) {
		deadline := time.Now().Add(60 * time.Second)
		var after float64
		for {
			after = csSkippedAmbiguous(scrapeMetricsAllReplicasOrSkip(t, "the manager /metrics endpoints did not serve this scrape via the "+
				"apiserver pod proxy, so the cpu_pinning_pods_skipped_total delta cannot be read. Check metrics.enabled and "+
				"E2E_METRICS_PORT/E2E_METRICS_SCHEME (see docs/internal/e2e-internals.md)"), ns)
			if after > before || time.Now().After(deadline) {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if baseline == "" {
			skipNoConfig(t, "the manager /metrics endpoints serve now but did not when the baseline was taken, so there is no before value to "+
				"compare cpu_pinning_pods_skipped_total against")
		}
		if after <= before {
			t.Errorf("cpu_pinning_pods_skipped_total{workload_namespace=%q,reason=\"ambiguous\"} did not increment (before=%v after=%v)", ns, before, after)
		}
	})
}

// runContainerSelectionLargestContainer proves largestContainer pins only the
// highest-CPU glob match.
func runContainerSelectionLargestContainer(t *testing.T, c client.Client, _ string) {
	ns := useTargetNamespace(t, c)

	csApplyPolicyMatch(t, c, []string{"vllm-*"}, "largestContainer")

	bigKey := csContainerKey("vllm-big")
	smallKey := csContainerKey("vllm-small")
	created := csAdmitUntil(t, c, ns, "e2e-cs-largest",
		[]csCtr{{name: "vllm-small", cpu: "2"}, {name: "vllm-big", cpu: "4"}},
		func(p *corev1.Pod) bool { return p.Annotations[bigKey] != "" })

	got := created.Annotations[bigKey]
	if got == "" {
		t.Fatalf("largestContainer did not stamp the highest-CPU container: annotation %q missing", bigKey)
	}
	if !strings.HasPrefix(got, "balloon-span") {
		t.Errorf("annotation %s = %q, want a balloon-span<N> value", bigKey, got)
	}
	if created.Annotations[pinnedByAnnotation] != "cpu-pinning-operator" {
		t.Errorf("pinned-by = %q, want cpu-pinning-operator", created.Annotations[pinnedByAnnotation])
	}
	if v := created.Annotations[smallKey]; v != "" {
		t.Errorf("smaller candidate vllm-small got a balloon annotation %q; largestContainer must pin only the biggest", v)
	}
	if v := created.Annotations[csPodKey()]; v != "" {
		t.Errorf("largestContainer produced a POD-scoped annotation %q, want container-scoped only", v)
	}
}

// csContainerKey / csPodKey render the NRI balloons annotation keys.
func csContainerKey(container string) string {
	return fmt.Sprintf("%s/container.%s", balloonAnnotationBase, container)
}
func csPodKey() string { return fmt.Sprintf("%s/pod", balloonAnnotationBase) }

// csApplyPolicyMatch re-points the default policy's spec.match and waits for
// observedGeneration, which the operator sets only after the webhook index has the
// new rules. The restore cleanup is registered before the probe pods' (LIFO), so the
// pods are gone when it narrows the match; a failed restore is recorded for
// useTargetNamespace.
func csApplyPolicyMatch(t *testing.T, c client.Client, byContainerName []string, containerSelection string) {
	t.Helper()
	ctx := context.Background()
	key := client.ObjectKey{Name: installedPolicyName(t, c)}

	bpp := newUnstructured(bppGVK)
	if err := c.Get(ctx, key, bpp); err != nil {
		t.Fatalf("get BalloonPinningPolicy %q: %v", key.Name, err)
	}

	// NestedMap returns a deep copy, safe to stash for restore.
	origMatch, hadMatch, _ := unstructured.NestedMap(bpp.Object, "spec", "match")
	t.Cleanup(func() {
		if err := restorePolicyMatch(c, key, origMatch, hadMatch, e2eSettleTimeout); err != nil {
			policyMatchResidue = fmt.Sprintf("%s left byContainerName %v live: %v", t.Name(), byContainerName, err)
			t.Errorf("cleanup: %v; later tests relying on the baseline match would admit their pods unpinned", err)
		}
	})

	// Retry on Conflict: the reconciler rewrites status on every pinned-pod event.
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := c.Get(ctx, key, bpp); err != nil {
			return err
		}
		if err := unstructured.SetNestedStringSlice(bpp.Object, byContainerName, "spec", "match", "byContainerName"); err != nil {
			return err
		}
		if containerSelection != "" {
			if err := unstructured.SetNestedField(bpp.Object, containerSelection, "spec", "match", "containerSelection"); err != nil {
				return err
			}
		} else {
			unstructured.RemoveNestedField(bpp.Object, "spec", "match", "containerSelection")
		}
		return c.Update(ctx, bpp)
	})
	if err != nil {
		t.Fatalf("update BalloonPinningPolicy %q match (a guardActivePods policy holding bound pods can reject a narrowing edit): %v", key.Name, err)
	}

	if !waitForPolicySpecSettled(c, key, 90*time.Second) {
		t.Fatalf("policy %q observedGeneration never caught up to metadata.generation; reconciler did not pick up the match edit", key.Name)
	}
}

// csPod builds a probe pod with whole-core containers; it only needs to be admitted.
func csPod(name, ns string, containers []csCtr) *corev1.Pod {
	specs := make([]corev1.Container, 0, len(containers))
	for _, cc := range containers {
		q := resource.MustParse(cc.cpu)
		specs = append(specs, corev1.Container{
			Name:    cc.name,
			Image:   "registry.k8s.io/pause:3.9",
			Command: []string{"/pause"},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q},
			},
		})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"app": "cs-e2e"},
		},
		Spec: corev1.PodSpec{
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers:  specs,
		},
	}
}

// csAdmitUntil creates probe pods until one satisfies check or the deadline passes,
// absorbing the lag before every webhook replica refreshes its index.
func csAdmitUntil(t *testing.T, c client.Client, ns, base string, containers []csCtr, check func(*corev1.Pod) bool) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var last *corev1.Pod
	for i := 0; ; i++ {
		last = createAndCleanup(t, c, csPod(fmt.Sprintf("%s-%d", base, i), ns, containers))
		if check(last) || time.Now().After(deadline) {
			return last
		}
		time.Sleep(3 * time.Second)
	}
}

// csSkippedAmbiguous reads pods_skipped_total{reason="ambiguous"} for ns from a scraped
// body; the caller owns the scrape so it owns the skip when nothing served.
func csSkippedAmbiguous(body, ns string) float64 {
	return counterSum(body, "cpu_pinning_pods_skipped_total",
		`workload_namespace="`+ns+`"`, `reason="ambiguous"`)
}
