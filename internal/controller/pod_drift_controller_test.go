// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/balloons"
	"github.com/intel/cpu-pinning-operator/internal/match"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
)

// vllmTPIntent is the shipped vLLM TP intent source, so drift extracts the span the webhook does.
func vllmTPIntent(t *testing.T) match.IntentRules {
	t.Helper()
	rules, err := match.CompileIntent(cpopv1alpha1.IntentSpec{
		Span: cpopv1alpha1.IntentSourceSpec{
			ArgRegex: `(?:^|\s)(?:--tensor-parallel-size|-tp)(?:=|\s+)(\d+)`,
			Default:  1,
		},
	})
	if err != nil {
		t.Fatalf("compile vllm TP intent: %v", err)
	}
	return rules
}

// driftScheme registers core types plus the unstructured NodeResourceTopology CR.
func driftScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return scheme
}

// nrtZone builds an NRT zone map with a single cpuset attribute.
func nrtZone(name, parent, cpuset string) map[string]interface{} {
	return map[string]interface{}{
		"type":   containerZoneType,
		"name":   name,
		"parent": parent,
		"attributes": []interface{}{
			map[string]interface{}{"name": "cpuset", "value": cpuset},
		},
	}
}

func nrtObj(zones ...map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(nrtpkg.GVK)
	u.SetName("n1")
	zs := make([]interface{}, len(zones))
	for i, z := range zones {
		zs[i] = z
	}
	u.Object["zones"] = zs
	return u
}

func driftReconciler(t *testing.T, objs ...runtime.Object) (*PodDriftReconciler, *record.FakeRecorder) {
	// Same pinned-by field index SetupWithManager registers.
	c := fake.NewClientBuilder().WithScheme(driftScheme()).
		WithIndex(&corev1.Pod{}, PodPinnedByField, indexPodByPinnedBy).
		WithRuntimeObjects(objs...).Build()
	idx := match.NewIndex()
	idx.Update([]match.PolicyRule{{
		Namespaces:      []string{"inference"},
		ByContainerName: []string{"vllm"},
		DriftPolicy:     driftPolicyEventsOnly,
		Intent:          vllmTPIntent(t),
	}})
	// No cluster config: cpuAllocation defaults to physical-cores.
	rec := record.NewFakeRecorder(8)
	return &PodDriftReconciler{
		Client:      c,
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: idx,
		Recorder:    rec,
	}, rec
}

func pinnedPod(hash string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vllm-x",
			Namespace: "inference",
			Annotations: map[string]string{
				pinnedByAnnotation:   pinnedByValue,
				pinnedHashAnnotation: hash,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   "n1",
			Containers: []corev1.Container{{Name: "vllm", Args: []string{"--tensor-parallel-size", "2"}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func enabledNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "n1",
		Labels: map[string]string{LabelBalloonsEnabled: "true", LabelMaxSpan: "2"},
	}}
}

func TestDrift_InSync_NoEvent(t *testing.T) {
	// Hash matches the current desired config → no drift event.
	want := match.PinnedConfigHash("balloon-span2", "physical-cores")
	r, rec := driftReconciler(t, pinnedPod(want), enabledNode())

	req := ctrl.Request{}
	req.Namespace, req.Name = "inference", "vllm-x"
	_, _ = r.Reconcile(context.Background(), req)

	select {
	case e := <-rec.Events:
		t.Errorf("expected no event for in-sync pod, got %q", e)
	default:
	}
}

func TestDrift_StaleHash_EmitsDriftEvent(t *testing.T) {
	// Stale hash: Drifted warning and the drifted counter advances.
	before := testutil.ToFloat64(metrics.PodsDrifted.WithLabelValues("n1", "inference"))
	r, rec := driftReconciler(t, pinnedPod("deadbeef"), enabledNode())
	req := ctrl.Request{}
	req.Namespace, req.Name = "inference", "vllm-x"
	_, _ = r.Reconcile(context.Background(), req)

	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "Drifted") {
			t.Errorf("event = %q, want Drifted", e)
		}
	default:
		t.Fatal("expected a Drifted event, got none")
	}
	if after := testutil.ToFloat64(metrics.PodsDrifted.WithLabelValues("n1", "inference")); after != before+1 {
		t.Errorf("cpu_pinning_pods_drifted_total{workload_namespace=\"inference\"} = %v, want %v (drift detection must be metered)", after, before+1)
	}
}

func TestDrift_ClusterConfigCPUAllocation_ChangesHash(t *testing.T) {
	// Switching the cluster config to hyperthreads changes the desired hash, so the
	// pod drifts: cpuAllocation comes from the cluster config, not the rule.
	inSync := match.PinnedConfigHash("balloon-span2", "physical-cores")
	r, rec := driftReconciler(t, pinnedPod(inSync), enabledNode())
	r.PolicyIndex.SetClusterConfig(match.ClusterConfig{CPUAllocation: "hyperthreads"})

	req := ctrl.Request{}
	req.Namespace, req.Name = "inference", "vllm-x"
	_, _ = r.Reconcile(context.Background(), req)

	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "Drifted") {
			t.Errorf("event = %q, want Drifted", e)
		}
	default:
		t.Fatal("expected a Drifted event after cluster-config cpuAllocation change, got none")
	}
}

// restartDriftReconciler builds a reconciler whose rule uses driftPolicy: restart;
// evictCount counts eviction Creates reaching the fake client.
func restartDriftReconciler(t *testing.T, evictErr error, evictCount *int, objs ...runtime.Object) (*PodDriftReconciler, *record.FakeRecorder) {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(driftScheme()).WithRuntimeObjects(objs...)
	builder = builder.WithInterceptorFuncs(interceptor.Funcs{
		SubResourceCreate: func(_ context.Context, _ client.Client, subResourceName string, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
			if subResourceName == "eviction" && evictCount != nil {
				(*evictCount)++
			}
			return evictErr
		},
	})
	c := builder.Build()
	idx := match.NewIndex()
	idx.Update([]match.PolicyRule{{
		Namespaces:      []string{"inference"},
		ByContainerName: []string{"vllm"},
		DriftPolicy:     driftPolicyRestart,
		Intent:          vllmTPIntent(t),
	}})
	// FakeRecorder blocks once its channel fills, and each reconcile may emit two events.
	rec := record.NewFakeRecorder(64)
	return &PodDriftReconciler{
		Client:      c,
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: idx,
		Recorder:    rec,
	}, rec
}

func driftReq() ctrl.Request {
	req := ctrl.Request{}
	req.Namespace, req.Name = "inference", "vllm-x"
	return req
}

func TestDrift_Restart_EvictsAndCountsMetric(t *testing.T) {
	before := testutil.ToFloat64(metrics.DriftEvictions.WithLabelValues("n1", "inference", metrics.DriftEvictionEvicted))
	var evictions int
	r, _ := restartDriftReconciler(t, nil, &evictions, pinnedPod("deadbeef"), enabledNode())
	// Generous limiter so the rate limit does not defer the eviction.
	r.EvictionLimiter = rate.NewLimiter(rate.Inf, 1)

	res, err := r.Reconcile(context.Background(), driftReq())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if evictions != 1 {
		t.Fatalf("expected 1 eviction call, got %d", evictions)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("successful eviction should not requeue, got %v", res.RequeueAfter)
	}
	after := testutil.ToFloat64(metrics.DriftEvictions.WithLabelValues("n1", "inference", metrics.DriftEvictionEvicted))
	if after-before != 1 {
		t.Errorf("evicted counter delta = %v, want 1", after-before)
	}
}

func TestDrift_Restart_RateLimitCapsEvictions(t *testing.T) {
	rlBefore := testutil.ToFloat64(metrics.DriftEvictions.WithLabelValues("n1", "inference", metrics.DriftEvictionRateLimited))
	// Burst 1 with no meaningful refill: one eviction passes, the rest are deferred.
	var evictions int
	r, rec := restartDriftReconciler(t, nil, &evictions, pinnedPod("deadbeef"), enabledNode())
	r.EvictionLimiter = rate.NewLimiter(rate.Every(time.Hour), 1)

	const attempts = 5
	deferred := 0
	for i := 0; i < attempts; i++ {
		res, err := r.Reconcile(context.Background(), driftReq())
		if err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if res.RequeueAfter > 0 {
			deferred++
		}
	}
	if evictions != 1 {
		t.Fatalf("rate limiter should cap evictions at 1 in the burst, got %d", evictions)
	}
	if deferred != attempts-1 {
		t.Errorf("expected %d deferred (requeued) reconciles, got %d", attempts-1, deferred)
	}
	rlAfter := testutil.ToFloat64(metrics.DriftEvictions.WithLabelValues("n1", "inference", metrics.DriftEvictionRateLimited))
	if rlAfter-rlBefore != float64(attempts-1) {
		t.Errorf("rate-limited counter delta = %v, want %d", rlAfter-rlBefore, attempts-1)
	}

	sawDeferred := false
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, "DriftEvictionDeferred") {
				sawDeferred = true
			}
			continue
		default:
		}
		break
	}
	if !sawDeferred {
		t.Error("expected a DriftEvictionDeferred event for rate-limited pod")
	}
}

// TestDrift_EvictionLimiterTuning: the limiter honors EvictionInterval / EvictionBurst,
// and non-positive values fall back to the 10s/2 default.
func TestDrift_EvictionLimiterTuning(t *testing.T) {
	t.Run("defaults preserved when unset", func(t *testing.T) {
		r := &PodDriftReconciler{}
		lim := r.limiter()
		if got, want := lim.Limit(), rate.Every(DefaultDriftEvictionInterval); got != want {
			t.Errorf("default limit = %v, want %v (DefaultDriftEvictionInterval)", got, want)
		}
		if got := lim.Burst(); got != DefaultDriftEvictionBurst {
			t.Errorf("default burst = %d, want %d (DefaultDriftEvictionBurst)", got, DefaultDriftEvictionBurst)
		}
	})

	t.Run("configured values honored", func(t *testing.T) {
		r := &PodDriftReconciler{EvictionInterval: 2 * time.Second, EvictionBurst: 5}
		lim := r.limiter()
		if got, want := lim.Limit(), rate.Every(2*time.Second); got != want {
			t.Errorf("limit = %v, want %v", got, want)
		}
		if got := lim.Burst(); got != 5 {
			t.Errorf("burst = %d, want 5", got)
		}
	})

	t.Run("non-positive fields fall back to defaults", func(t *testing.T) {
		r := &PodDriftReconciler{EvictionInterval: -1, EvictionBurst: 0}
		lim := r.limiter()
		if got, want := lim.Limit(), rate.Every(DefaultDriftEvictionInterval); got != want {
			t.Errorf("limit = %v, want %v (fallback)", got, want)
		}
		if got := lim.Burst(); got != DefaultDriftEvictionBurst {
			t.Errorf("burst = %d, want %d (fallback)", got, DefaultDriftEvictionBurst)
		}
	})
}

func TestDrift_Restart_PDBBlocked_BacksOff(t *testing.T) {
	pdbBefore := testutil.ToFloat64(metrics.DriftEvictions.WithLabelValues("n1", "inference", metrics.DriftEvictionPDBBlocked))
	// A PDB violation returns TooManyRequests; the reconciler must requeue, never force-delete.
	tooMany := apierrors.NewTooManyRequests("cannot evict pod as it would violate the pod's disruption budget", 0)

	tooMany.ErrStatus.Reason = metav1.StatusReasonTooManyRequests
	tooMany.ErrStatus.Details = &metav1.StatusDetails{Group: schema.GroupResource{Group: "policy", Resource: "poddisruptionbudgets"}.Group}

	var evictions int
	r, rec := restartDriftReconciler(t, tooMany, &evictions, pinnedPod("deadbeef"), enabledNode())
	r.EvictionLimiter = rate.NewLimiter(rate.Inf, 1)

	res, err := r.Reconcile(context.Background(), driftReq())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if evictions != 1 {
		t.Fatalf("expected 1 eviction attempt, got %d", evictions)
	}
	if res.RequeueAfter != driftEvictionBackoff {
		t.Errorf("PDB-blocked eviction should requeue after %v, got %v", driftEvictionBackoff, res.RequeueAfter)
	}
	pdbAfter := testutil.ToFloat64(metrics.DriftEvictions.WithLabelValues("n1", "inference", metrics.DriftEvictionPDBBlocked))
	if pdbAfter-pdbBefore != 1 {
		t.Errorf("pdb-blocked counter delta = %v, want 1", pdbAfter-pdbBefore)
	}

	pod := &corev1.Pod{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "inference", Name: "vllm-x"}, pod); err != nil {
		t.Errorf("PDB-blocked pod should still exist, got get error: %v", err)
	}
	sawDeferred := false
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, "DriftEvictionDeferred") {
				sawDeferred = true
			}
			continue
		default:
		}
		break
	}
	if !sawDeferred {
		t.Error("expected a DriftEvictionDeferred event for PDB-blocked pod")
	}
}

// inSyncHash is the hash a correctly pinned span-2 vllm pod carries under
// physical-cores on a max-span-2 node.
func inSyncHash() string {
	return match.PinnedConfigHash("balloon-span2", "physical-cores")
}

// pinnedPodRunningSince returns an in-sync pinned pod whose vllm container has been Running since t.
func pinnedPodRunningSince(started time.Time) *corev1.Pod {
	pod := pinnedPod(inSyncHash())
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "vllm",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}},
	}}
	return pod
}

// TestDrift_ActualUnpinned_HashMatches_Detected: a pod whose hash matches but whose
// container has no dedicated balloon in NRT, past the grace window, is flagged.
func TestDrift_ActualUnpinned_HashMatches_Detected(t *testing.T) {
	before := testutil.ToFloat64(metrics.PodsDrifted.WithLabelValues("n1", "inference"))
	// NRT has data for the node but no subzone for our container: unpinned.
	nrt := nrtObj(nrtZone("inference/other/other", "balloon-span2[0]", "0,2"))
	now := time.Now()
	pod := pinnedPodRunningSince(now.Add(-5 * time.Minute)) // well past the grace window
	r, rec := driftReconciler(t, pod, enabledNode(), nrt)
	r.now = func() time.Time { return now }

	_, err := r.Reconcile(context.Background(), driftReq())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "Drifted") {
			t.Errorf("event = %q, want Drifted", e)
		}
	default:
		t.Fatal("expected a Drifted event for an unpinned-but-hash-matching pod, got none")
	}
	if after := testutil.ToFloat64(metrics.PodsDrifted.WithLabelValues("n1", "inference")); after != before+1 {
		t.Errorf("pods_drifted_total delta = %v, want 1 (actual-cpuset drift must be metered)", after-before)
	}
}

// TestDrift_ActuallyPinned_HashMatches_NoEvent: a pod confined to its NRT balloon is not flagged.
func TestDrift_ActuallyPinned_HashMatches_NoEvent(t *testing.T) {
	nrt := nrtObj(nrtZone("inference/vllm-x/vllm", "balloon-span2[0]", "0,2"))
	now := time.Now()
	pod := pinnedPodRunningSince(now.Add(-5 * time.Minute))
	r, rec := driftReconciler(t, pod, enabledNode(), nrt)
	r.now = func() time.Time { return now }

	_, err := r.Reconcile(context.Background(), driftReq())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	select {
	case e := <-rec.Events:
		t.Errorf("expected no event for a correctly-pinned pod, got %q", e)
	default:
	}
}

// TestDrift_ActualUnpinned_WithinGraceWindow_NoFalsePositive: a just-started
// unpinned-looking container is not flagged but requeued for the remaining grace time.
func TestDrift_ActualUnpinned_WithinGraceWindow_NoFalsePositive(t *testing.T) {
	nrt := nrtObj(nrtZone("inference/other/other", "balloon-span2[0]", "0,2"))
	now := time.Now()
	elapsed := 5 * time.Second
	pod := pinnedPodRunningSince(now.Add(-elapsed)) // just started; still settling
	r, rec := driftReconciler(t, pod, enabledNode(), nrt)
	r.now = func() time.Time { return now }

	res, err := r.Reconcile(context.Background(), driftReq())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	select {
	case e := <-rec.Events:
		t.Errorf("expected no event within the grace window, got %q", e)
	default:
	}
	// Requeue after window - elapsed (about 25s); StartedAt is truncated to seconds, so allow slop.
	remaining := pinVerifyGracePeriod - elapsed
	if res.RequeueAfter <= pinVerifyRequeue || res.RequeueAfter > remaining {
		t.Errorf("requeue = %v, want remaining grace time ~%v (and > fixed %v)",
			res.RequeueAfter, remaining, pinVerifyRequeue)
	}
}

// TestDrift_NoNRTData_NoFalsePositive: an absent NRT or one with no zones is unknown,
// never unpinned; the pod is requeued after a bounded interval to re-check.
func TestDrift_NoNRTData_NoFalsePositive(t *testing.T) {
	now := time.Now()
	oldPod := func() *corev1.Pod { return pinnedPodRunningSince(now.Add(-10 * time.Minute)) }

	t.Run("NRT CR absent", func(t *testing.T) {
		r, rec := driftReconciler(t, oldPod(), enabledNode()) // no NRT object seeded
		r.now = func() time.Time { return now }
		res, err := r.Reconcile(context.Background(), driftReq())
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		select {
		case e := <-rec.Events:
			t.Errorf("absent NRT must not produce drift, got %q", e)
		default:
		}
		if res.RequeueAfter != pinVerifyRequeue {
			t.Errorf("absent NRT requeue = %v, want bounded %v", res.RequeueAfter, pinVerifyRequeue)
		}
	})

	t.Run("NRT CR present but no zones", func(t *testing.T) {
		r, rec := driftReconciler(t, oldPod(), enabledNode(), nrtObj())
		r.now = func() time.Time { return now }
		res, err := r.Reconcile(context.Background(), driftReq())
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		select {
		case e := <-rec.Events:
			t.Errorf("empty NRT (plugin not yet published) must not produce drift, got %q", e)
		default:
		}
		if res.RequeueAfter != pinVerifyRequeue {
			t.Errorf("no-zones NRT requeue = %v, want bounded %v", res.RequeueAfter, pinVerifyRequeue)
		}
	})
}

// TestDrift_ActualUnpinned_Restart_Evicts: under driftPolicy restart an actually
// unpinned pod is evicted so it re-pins.
func TestDrift_ActualUnpinned_Restart_Evicts(t *testing.T) {
	nrt := nrtObj(nrtZone("inference/other/other", "balloon-span2[0]", "0,2"))
	now := time.Now()
	pod := pinnedPodRunningSince(now.Add(-5 * time.Minute))
	pod.Annotations[pinnedHashAnnotation] = inSyncHash() // ensure hash matches under the restart policy
	var evictions int
	r, _ := restartDriftReconciler(t, nil, &evictions, pod, enabledNode(), nrt)
	r.EvictionLimiter = rate.NewLimiter(rate.Inf, 1)
	r.now = func() time.Time { return now }

	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if evictions != 1 {
		t.Fatalf("expected the actually-unpinned pod to be evicted once, got %d", evictions)
	}
}

func TestDrift_UnmanagedPod_Ignored(t *testing.T) {
	pod := pinnedPod("whatever")
	delete(pod.Annotations, pinnedByAnnotation) // not operator-pinned
	r, rec := driftReconciler(t, pod, enabledNode())
	req := ctrl.Request{}
	req.Namespace, req.Name = "inference", "vllm-x"
	_, _ = r.Reconcile(context.Background(), req)

	select {
	case e := <-rec.Events:
		t.Errorf("unmanaged pod should be ignored, got event %q", e)
	default:
	}
}

// namedPod builds a pod in ns with an optional pinned-by annotation value.
func namedPod(namespace, name, pinnedBy string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       corev1.PodSpec{NodeName: "n1"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if pinnedBy != "" {
		p.Annotations = map[string]string{pinnedByAnnotation: pinnedBy}
	}
	return p
}

func TestDrift_EnqueuePinnedPods_OnlyOperatorPinned(t *testing.T) {
	// Only the two operator-pinned pods may be enqueued, not the unpinned or foreign-pinned ones.
	pinnedA := namedPod("inference", "vllm-a", pinnedByValue)
	pinnedB := namedPod("serving", "vllm-b", pinnedByValue)
	unpinned := namedPod("default", "web", "")
	foreign := namedPod("inference", "other", "some-other-operator")

	r, _ := driftReconciler(t, pinnedA, pinnedB, unpinned, foreign)

	reqs := r.enqueuePinnedPods(context.Background(), &cpopv1alpha1.BalloonPinningPolicy{})

	got := map[types.NamespacedName]bool{}
	for _, req := range reqs {
		got[req.NamespacedName] = true
	}
	want := []types.NamespacedName{
		{Namespace: "inference", Name: "vllm-a"},
		{Namespace: "serving", Name: "vllm-b"},
	}
	if len(reqs) != len(want) {
		t.Fatalf("enqueued %d requests, want %d: %v", len(reqs), len(want), reqs)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("expected pinned pod %v to be enqueued, got %v", w, reqs)
		}
	}
	// The field selector must exclude them from the List itself.
	for _, bad := range []types.NamespacedName{
		{Namespace: "default", Name: "web"},
		{Namespace: "inference", Name: "other"},
	} {
		if got[bad] {
			t.Errorf("pod %v should not be enqueued (not operator-pinned)", bad)
		}
	}
}

// modeNode is a balloons-enabled node carrying the cpuAllocation mode resolved for it.
func modeNode(mode string) *corev1.Node {
	n := enabledNode()
	n.Labels[LabelCPUAllocation] = mode
	return n
}

// expectDrift runs one reconcile and reports whether a Drifted event was emitted.
func expectDrift(t *testing.T, r *PodDriftReconciler, rec *record.FakeRecorder) bool {
	t.Helper()
	req := ctrl.Request{}
	req.Namespace, req.Name = "inference", "vllm-x"
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, "Drifted") {
				return true
			}
		default:
			return false
		}
	}
}

// TestDrift_NodeModeIsTheAuthority: the hash is recomputed against the pod's node mode,
// not the cluster default, so a node mode change drifts its pods.
func TestDrift_NodeModeIsTheAuthority(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stamped   string
		nodeMode  string
		wantDrift bool
	}{
		{
			name:      "stamped under the node's override",
			stamped:   cpuAllocationHyperthreads,
			nodeMode:  cpuAllocationHyperthreads,
			wantDrift: false,
		},
		{
			name:      "the node's mode changed after the pod was pinned",
			stamped:   balloons.CPUAllocationPhysicalCores,
			nodeMode:  cpuAllocationHyperthreads,
			wantDrift: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash := match.PinnedConfigHash("balloon-span2", tc.stamped)
			r, rec := driftReconciler(t, pinnedPod(hash), modeNode(tc.nodeMode))
			// Cluster default stays physical-cores, so only the node's mode decides.
			if got := expectDrift(t, r, rec); got != tc.wantDrift {
				t.Errorf("drift reported = %v, want %v for a pod stamped under %q on a node running %q",
					got, tc.wantDrift, tc.stamped, tc.nodeMode)
			}
		})
	}
}

// TestDrift_UnresolvedModeStampIsNotModeDrift: a pod admitted before scheduling has no
// mode in its hash, so only the balloon name is compared.
func TestDrift_UnresolvedModeStampIsNotModeDrift(t *testing.T) {
	t.Run("the mode is not compared", func(t *testing.T) {
		hash := match.PinnedConfigHash("balloon-span2", "")
		r, rec := driftReconciler(t, pinnedPod(hash), modeNode(cpuAllocationHyperthreads))
		if expectDrift(t, r, rec) {
			t.Error("a pod whose hash records no mode was reported as drifted, which restarting it cannot clear")
		}
	})
	t.Run("the balloon name still is", func(t *testing.T) {
		hash := match.PinnedConfigHash("balloon-span4", "")
		r, rec := driftReconciler(t, pinnedPod(hash), modeNode(cpuAllocationHyperthreads))
		if !expectDrift(t, r, rec) {
			t.Error("a pod stamped for another balloon went undetected")
		}
	})
}

// TestDrift_NodeModeChangeEnqueuesItsPinnedPods: a node mode change enqueues only that
// node's pinned pods; other Node events are dropped.
func TestDrift_NodeModeChangeEnqueuesItsPinnedPods(t *testing.T) {
	onNode := namedPod("inference", "vllm-x", pinnedByValue)
	elsewhere := namedPod("inference", "vllm-y", pinnedByValue)
	elsewhere.Spec.NodeName = "n2"
	unpinned := namedPod("default", "web", "")
	r, _ := driftReconciler(t, onNode, elsewhere, unpinned)

	reqs := r.enqueuePodsOnNode(context.Background(), enabledNode())
	want := types.NamespacedName{Namespace: "inference", Name: "vllm-x"}
	if len(reqs) != 1 || reqs[0].NamespacedName != want {
		t.Errorf("enqueued %v, want only %v: a node's event must reach its own pinned pods", reqs, want)
	}

	for _, tc := range []struct {
		name     string
		old, new *corev1.Node
		want     bool
	}{
		{"the mode moved", modeNode(balloons.CPUAllocationPhysicalCores), modeNode(cpuAllocationHyperthreads), true},
		{"the mode appeared", enabledNode(), modeNode(cpuAllocationHyperthreads), true},
		{"a status heartbeat", modeNode(cpuAllocationHyperthreads), modeNode(cpuAllocationHyperthreads), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nodeAllocationChanged.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new})
			if got != tc.want {
				t.Errorf("the Node watch passes the event = %v, want %v", got, tc.want)
			}
		})
	}
}
