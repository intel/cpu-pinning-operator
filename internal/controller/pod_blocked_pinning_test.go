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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/intel/cpu-pinning-operator/internal/match"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
)

// Real kubelet error chain: the balloons policy prefixes "balloons: ", the resource
// policy wraps with "failed to allocate resources: ", the runtime adds its prefix, and
// the kubelet copies it into state.waiting.message (reason CreateContainerError).
const (
	runtimePrefix = "failed to get NRI adjustment for container: failed to allocate resources: "
	allocPrefix   = `balloons: balloon allocation for container inference/vllm-x/vllm failed: balloons: `

	msgNoBalloonDef  = runtimePrefix + allocPrefix + `no balloon for annotation "balloon-span2-colocate"`
	msgNoInstance    = runtimePrefix + allocPrefix + `no suitable balloon instance available`
	msgNotEnoughCPUs = runtimePrefix + allocPrefix + `not enough CPUs to create new balloon for container inference/vllm-x/vllm requesting 2000 mCPU. free CPUs: 0`
	msgMaxBalloons   = runtimePrefix + allocPrefix + `cannot create new "balloon-span2" balloon, MaxBalloons limit (4) reached`
	msgResizeFailed  = runtimePrefix + allocPrefix + `resizing balloon balloon-span2[0] failed: balloons: resize/inflate: allocating 2 CPUs for balloon-span2[0] failed: not enough free CPUs`
	msgOtherBalloons = runtimePrefix + `balloons: fill method balanced failed: cannot find pod for container inference/vllm-x/vllm`
)

// blockedPod builds a pinned, bound pod Pending because CreateContainer was refused.
// The settling window is timed from the reconciler's first observation, not initializedAt.
func blockedPod(reason, message string, initializedAt time.Time) *corev1.Pod {
	pod := pinnedPod("deadbeef")
	pod.UID = types.UID("pod-uid-1")
	pod.Status = corev1.PodStatus{
		Phase:     corev1.PodPending,
		StartTime: &metav1.Time{Time: initializedAt},
		Conditions: []corev1.PodCondition{{
			Type:               corev1.PodInitialized,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(initializedAt),
		}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "vllm",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
		}},
	}
	return pod
}

// blockedReconciler builds a reconciler with a fixed clock and the given driftPolicy;
// evictCount, when non-nil, counts eviction calls.
func blockedReconciler(t *testing.T, driftPolicy string, now time.Time, evictCount *int, objs ...runtime.Object) (*PodDriftReconciler, *record.FakeRecorder) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(driftScheme()).
		WithIndex(&corev1.Pod{}, PodPinnedByField, indexPodByPinnedBy).
		WithRuntimeObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceCreate: func(_ context.Context, _ client.Client, subResourceName string, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if subResourceName == "eviction" && evictCount != nil {
					(*evictCount)++
				}
				return nil
			},
		}).Build()
	idx := match.NewIndex()
	idx.Update([]match.PolicyRule{{
		Namespaces:      []string{"inference"},
		ByContainerName: []string{"vllm"},
		DriftPolicy:     driftPolicy,
		Intent:          vllmTPIntent(t),
	}})
	rec := record.NewFakeRecorder(16)
	clock := now
	return &PodDriftReconciler{
		Client:      c,
		Log:         ctrl.Log.WithName("test"),
		PolicyIndex: idx,
		Recorder:    rec,
		now:         func() time.Time { return clock },
	}, rec
}

// TestBlockedPinning_ReportsCause: a refused pinned pod gets a PinningBlocked event and
// counter increment under the cause the plugin's error implies.
func TestBlockedPinning_ReportsCause(t *testing.T) {
	// Whole seconds: the fake client truncates timestamps to RFC3339.
	now := time.Now().Truncate(time.Second)
	cases := []struct {
		name      string
		message   string
		wantCause string
	}{
		{"balloon type not defined on the node", msgNoBalloonDef, metrics.PinBlockedNoBalloon},
		{"no free balloon instance", msgNoInstance, metrics.PinBlockedNoCapacity},
		{"not enough CPUs for a new balloon", msgNotEnoughCPUs, metrics.PinBlockedNoCapacity},
		{"MaxBalloons limit reached", msgMaxBalloons, metrics.PinBlockedNoCapacity},
		{"existing balloon cannot grow", msgResizeFailed, metrics.PinBlockedNoCapacity},
		{"balloons error this operator does not classify", msgOtherBalloons, metrics.PinBlockedOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := blockedPod("CreateContainerError", tc.message, now.Add(-2*time.Minute))
			r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode())
			settle(t, r, now, driftReq())
			before := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "inference", tc.wantCause))

			res, err := r.Reconcile(context.Background(), driftReq())
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			after := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "inference", tc.wantCause))
			if after-before != 1 {
				t.Errorf("cpu_pinning_pods_pinning_blocked_total{cause=%q} delta = %v, want 1 (a stuck pinned pod must be metered)",
					tc.wantCause, after-before)
			}
			events := drainEvents(rec)
			if len(events) != 1 {
				t.Fatalf("events = %v, want exactly one PinningBlocked event", events)
			}
			for _, want := range []string{"PinningBlocked", tc.wantCause, `container "vllm" cannot start`, "CreateContainerError"} {
				if !strings.Contains(events[0], want) {
					t.Errorf("event %q does not mention %q", events[0], want)
				}
			}
			// Requeued so the metric keeps moving while the pod stays blocked.
			if res.RequeueAfter != pinBlockedReportInterval {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, pinBlockedReportInterval)
			}
		})
	}
}

// TestBlockedPinning_SeparateSeriesPerNode: pods refused on two nodes count 1 each,
// one series per node.
func TestBlockedPinning_SeparateSeriesPerNode(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	nodes := []string{"worker-1", "worker-2"}

	// PinningBlocked is process-wide; isolate these series.
	for _, node := range nodes {
		metrics.PinningBlocked.DeleteLabelValues(node, "inference", metrics.PinBlockedNoCapacity)
	}
	t.Cleanup(func() {
		for _, node := range nodes {
			metrics.PinningBlocked.DeleteLabelValues(node, "inference", metrics.PinBlockedNoCapacity)
		}
	})

	for i, node := range nodes {
		pod := blockedPod("CreateContainerError", msgNoInstance, now.Add(-2*time.Minute))
		pod.Name = "vllm-" + node
		pod.UID = types.UID("pod-uid-" + node)
		pod.Spec.NodeName = node
		req := ctrl.Request{}
		req.Namespace, req.Name = pod.Namespace, pod.Name

		r, _ := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod)
		settle(t, r, now, req)
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("%s: reconcile: %v", node, err)
		}

		// The second pass catches an increment attributed to the wrong node.
		for j, want := range map[int]float64{0: 1, 1: 1} {
			if j > i {
				want = 0
			}
			got := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues(nodes[j], "inference", metrics.PinBlockedNoCapacity))
			if got != want {
				t.Errorf("after blocking a pod on %q, cpu_pinning_pods_pinning_blocked_total{node=%q,cause=%q} = %v, want %v",
					node, nodes[j], metrics.PinBlockedNoCapacity, got, want)
			}
		}
	}
}

// TestBlockedPinning_NoSeriesWithoutANode: an unbound pod adds no series and no Event.
func TestBlockedPinning_NoSeriesWithoutANode(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := blockedPod("CreateContainerError", msgNoInstance, now.Add(-2*time.Minute))
	pod.Spec.NodeName = ""
	before := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("", "inference", metrics.PinBlockedNoCapacity))

	r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod)
	settle(t, r, now, driftReq())
	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("", "inference", metrics.PinBlockedNoCapacity)) - before; got != 0 {
		t.Errorf("an unbound pod added %v to cpu_pinning_pods_pinning_blocked_total{node=\"\"}, want 0", got)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("events = %v, want none: an unbound pod has no node to name in the remedy", events)
	}
}

// TestBlockedPinning_IgnoresOrdinaryPending: Pending for unrelated reasons, including the
// readiness gate every pinned pod passes through, stays silent.
func TestBlockedPinning_IgnoresOrdinaryPending(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	long := now.Add(-2 * time.Minute)

	waitingInInitGate := func() *corev1.Pod {
		pod := blockedPod("PodInitializing", "", long)
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:               corev1.PodInitialized,
			Status:             corev1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(long),
		}}
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name:  "wait-for-balloons",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(long)}},
		}}
		return pod
	}
	initGateTimedOut := func() *corev1.Pod {
		pod := waitingInInitGate()
		pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
		}
		return pod
	}
	evictedAndTerminal := func() *corev1.Pod {
		// An evicted pod keeps its last container status; it must not be reported.
		pod := blockedPod("CreateContainerError", msgNoInstance, long)
		pod.Status.Phase = corev1.PodFailed
		pod.Status.Reason = "Evicted"
		return pod
	}
	unschedulable := func() *corev1.Pod {
		pod := blockedPod("", "", long)
		pod.Status.ContainerStatuses = nil
		pod.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
		}}
		return pod
	}

	cases := []struct {
		name string
		pod  *corev1.Pod
	}{
		{"image pull failure", blockedPod("ImagePullBackOff",
			`Back-off pulling image "registry.example/vllm:latest"`, long)},
		{"container still being created", blockedPod("ContainerCreating", "", long)},
		{"waiting on the operator's own readiness gate", waitingInInitGate()},
		{"readiness gate timed out", initGateTimedOut()},
		{"unschedulable, no container statuses", unschedulable()},
		{"terminal pod keeping its last container status", evictedAndTerminal()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, tc.pod, enabledNode())
			before := blockedSignal(t)

			if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := blockedSignal(t); got != before {
				t.Errorf("blocked-report signal moved by %v; an ordinary Pending pod must not be reported", got-before)
			}
			if events := drainEvents(rec); len(events) != 0 {
				t.Errorf("events = %v, want none for an ordinary Pending pod", events)
			}
		})
	}
}

// blockedSignal fingerprints the counter (known causes plus series count), so an
// assertion of silence also catches reports on unnamed labels.
func blockedSignal(t *testing.T) float64 {
	t.Helper()
	var total float64
	// Read known series first: WithLabelValues creates them and would move the count.
	for _, cause := range []string{metrics.PinBlockedNoBalloon, metrics.PinBlockedNoCapacity, metrics.PinBlockedOther} {
		total += testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "inference", cause))
	}
	return total + float64(testutil.CollectAndCount(metrics.PinningBlocked))
}

// TestBlockedPinning_InitContainerRefusal: a pod-scoped pin refuses the injected init
// container, so the pod never leaves Init.
func TestBlockedPinning_InitContainerRefusal(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := blockedPod("PodInitializing", "", now.Add(-2*time.Minute))
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodInitialized,
		Status:             corev1.ConditionFalse,
		LastTransitionTime: metav1.NewTime(now.Add(-2 * time.Minute)),
	}}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "wait-for-balloons",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CreateContainerError", Message: msgNoBalloonDef,
		}},
	}}
	r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode())
	settle(t, r, now, driftReq())
	before := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "inference", metrics.PinBlockedNoBalloon))

	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if after := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "inference", metrics.PinBlockedNoBalloon)); after-before != 1 {
		t.Errorf("counter delta = %v, want 1 (a refused init container blocks the pod just as a workload container does)", after-before)
	}
	events := drainEvents(rec)
	if len(events) != 1 || !strings.Contains(events[0], "wait-for-balloons") {
		t.Errorf("events = %v, want one PinningBlocked event naming the refused init container", events)
	}
}

// TestBlockedPinning_Windows pins the window constants, which docs and the alert quote.
func TestBlockedPinning_Windows(t *testing.T) {
	if pinBlockedGracePeriod != 30*time.Second {
		t.Errorf("pinBlockedGracePeriod = %v, want 30s (docs and the plugin-convergence rationale assume it)", pinBlockedGracePeriod)
	}
	if pinBlockedReportInterval != 10*time.Minute {
		t.Errorf("pinBlockedReportInterval = %v, want 10m (docs and the CPUPinningPinningBlocked alert window assume it)", pinBlockedReportInterval)
	}
}

// settle runs the observing reconcile, then advances the clock past the settling window
// so the next reconcile may report.
func settle(t *testing.T, r *PodDriftReconciler, now time.Time, req ctrl.Request) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("settling reconcile: %v", err)
	}
	// TestBlockedPinning_GraceWindow owns the requeue assertion.
	r.now = func() time.Time { return now.Add(pinBlockedGracePeriod) }
}

// TestBlockedPinning_GraceWindow: a fresh refusal is left to converge and the reconciler
// returns exactly when the window closes; literal durations keep the boundary honest.
func TestBlockedPinning_GraceWindow(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	cases := []struct {
		name        string
		observedFor time.Duration
		wantReport  bool
		wantRequeue time.Duration
	}{
		{"one second short of the window", 29 * time.Second, false, time.Second},
		{"exactly at the window", 30 * time.Second, true, pinBlockedReportInterval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := blockedPod("CreateContainerError", msgNoInstance, now.Add(-time.Hour))
			r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode())

			// First reconcile only observes.
			if res, err := r.Reconcile(context.Background(), driftReq()); err != nil {
				t.Fatalf("reconcile: %v", err)
			} else if res.RequeueAfter != pinBlockedGracePeriod {
				t.Fatalf("RequeueAfter = %v, want the full %v window", res.RequeueAfter, pinBlockedGracePeriod)
			}
			if events := drainEvents(rec); len(events) != 0 {
				t.Fatalf("events = %v, want none on first observation", events)
			}

			r.now = func() time.Time { return now.Add(tc.observedFor) }
			before := blockedSignal(t)
			res, err := r.Reconcile(context.Background(), driftReq())
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			reported := blockedSignal(t) != before
			if reported != tc.wantReport {
				t.Errorf("reported = %v, want %v after %v observed", reported, tc.wantReport, tc.observedFor)
			}
			if events := drainEvents(rec); (len(events) > 0) != tc.wantReport {
				t.Errorf("events = %v, want reported = %v", events, tc.wantReport)
			}
			if res.RequeueAfter != tc.wantRequeue {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tc.wantRequeue)
			}
		})
	}
}

// TestBlockedPinning_WindowIgnoresRestartDowntime: restart backoff must not shorten the
// window, or a rebooting node reports every pinned pod at once.
func TestBlockedPinning_WindowIgnoresRestartDowntime(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, downtime := range []time.Duration{40 * time.Second, 5 * time.Minute, 4 * time.Hour} {
		t.Run(downtime.String(), func(t *testing.T) {
			pod := refusedAfterRestartPod(now.Add(-72*time.Hour), now.Add(-downtime))
			r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode(), nrtObj())
			before := blockedSignal(t)

			res, err := r.Reconcile(context.Background(), driftReq())
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := blockedSignal(t); got != before {
				t.Errorf("signal moved by %v after %v downtime, want 0 — the window starts at first observation", got-before, downtime)
			}
			if events := drainEvents(rec); len(events) != 0 {
				t.Errorf("events = %v, want none: the refusal has been observed for 0s", events)
			}
			if res.RequeueAfter != pinBlockedGracePeriod {
				t.Errorf("RequeueAfter = %v, want the full %v window", res.RequeueAfter, pinBlockedGracePeriod)
			}
		})
	}
}

func TestBlockedPinning_ThrottlesRepeatReports(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := blockedPod("CreateContainerError", msgNoInstance, now.Add(-2*time.Minute))
	r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode())
	settle(t, r, now, driftReq())
	before := blockedSignal(t)
	// settle left the clock where the first report lands.
	first := now.Add(pinBlockedGracePeriod)

	// Ten one-minute reconciles: one report, and each suppressed reconcile requeues within
	// the report cadence.
	for i := 0; i < 10; i++ {
		r.now = func() time.Time { return first.Add(time.Duration(i) * time.Minute) }
		res, err := r.Reconcile(context.Background(), driftReq())
		if err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if res.RequeueAfter <= 0 || res.RequeueAfter > pinBlockedReportInterval {
			t.Errorf("reconcile %d: RequeueAfter = %v, want (0, %v] — a throttled report asks for the time left, never zero",
				i, res.RequeueAfter, pinBlockedReportInterval)
		}
	}
	if got := blockedSignal(t) - before; got != 1 {
		t.Errorf("signal delta over 10 reconciles inside the interval = %v, want 1", got)
	}
	if events := drainEvents(rec); len(events) != 1 {
		t.Errorf("events = %v, want exactly one across 10 reconciles", events)
	}

	r.now = func() time.Time { return first.Add(pinBlockedReportInterval - time.Second) }
	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile just inside the interval: %v", err)
	}
	if got := blockedSignal(t) - before; got != 1 {
		t.Errorf("signal delta one second short of the interval = %v, want 1", got)
	}

	// At the interval the condition is re-reported.
	r.now = func() time.Time { return first.Add(pinBlockedReportInterval) }
	res, err := r.Reconcile(context.Background(), driftReq())
	if err != nil {
		t.Fatalf("reconcile at the interval: %v", err)
	}
	if got := blockedSignal(t) - before; got != 2 {
		t.Errorf("signal delta at the report interval = %v, want 2", got)
	}
	if events := drainEvents(rec); len(events) != 1 {
		t.Errorf("events at the report interval = %v, want one", events)
	}
	if res.RequeueAfter != pinBlockedReportInterval {
		t.Errorf("RequeueAfter after a report = %v, want the full %v", res.RequeueAfter, pinBlockedReportInterval)
	}
}

// TestBlockedPinning_RepeatsWhileStillBlocked: each report schedules the next, driven
// only by requeues, so the pinning_blocked alert stays firing while the pod is stuck.
func TestBlockedPinning_RepeatsWhileStillBlocked(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := blockedPod("CreateContainerError", msgNoInstance, now.Add(-2*time.Minute))
	r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode())
	settle(t, r, now, driftReq())
	before := blockedSignal(t)

	const blockedFor = 4 * time.Hour
	at := now.Add(pinBlockedGracePeriod)
	deadline := at.Add(blockedFor)
	events := 0
	for at.Before(deadline) {
		clock := at
		r.now = func() time.Time { return clock }
		res, err := r.Reconcile(context.Background(), driftReq())
		if err != nil {
			t.Fatalf("reconcile %v into the blockage: %v", at.Sub(now), err)
		}
		if res.RequeueAfter <= 0 {
			t.Fatalf("the reconcile %v into the blockage asks for no requeue, so nothing reports "+
				"the pod again: increase(cpu_pinning_pods_pinning_blocked_total[15m]) goes flat and the "+
				"alert resolves while the pod is still stuck", at.Sub(now))
		}
		// The recorder buffer is smaller than the report count, so drain as we go.
		events += len(drainEvents(rec))
		at = at.Add(res.RequeueAfter)
	}

	want := int(blockedFor / pinBlockedReportInterval)
	if got := blockedSignal(t) - before; got != float64(want) {
		t.Errorf("signal delta over %v blocked = %v, want %d — one report per %v",
			blockedFor, got, want, pinBlockedReportInterval)
	}
	if events != want {
		t.Errorf("events over %v blocked = %d, want %d — one Event per report",
			blockedFor, events, want)
	}
}

// TestBlockedPinning_ChangedBlockerReportsAgain: the throttle covers one blocker; a new
// container or cause is a new report.
func TestBlockedPinning_ChangedBlockerReportsAgain(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	initRefused := func(message string) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
				Name: "wait-for-balloons",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CreateContainerError", Message: message,
				}},
			}}
		}
	}
	initStarted := func(pod *corev1.Pod) {
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name:  "wait-for-balloons",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}
	}

	cases := []struct {
		name                  string
		first, second         string            // the workload container's waiting message
		firstInit, secondInit func(*corev1.Pod) // optional init container state
		wantCause, wantOn     string
	}{
		{
			// Pod-scoped pin: init refused on policy, then the workload hits capacity.
			name:  "container and cause both change",
			first: msgNotEnoughCPUs, firstInit: initRefused(msgNoBalloonDef),
			second: msgNotEnoughCPUs, secondInit: initStarted,
			wantCause: metrics.PinBlockedNoCapacity, wantOn: "vllm",
		},
		{
			// Same container: policy fixed, then no room.
			name:  "same container, new cause",
			first: msgNoBalloonDef, second: msgNoInstance,
			wantCause: metrics.PinBlockedNoCapacity, wantOn: "vllm",
		},
		{
			// Same cause, but the event must name the container now stuck.
			name:  "same cause, new container",
			first: msgNoInstance, firstInit: initRefused(msgNoInstance),
			second: msgNoInstance, secondInit: initStarted,
			wantCause: metrics.PinBlockedNoCapacity, wantOn: "vllm",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := blockedPod("CreateContainerError", tc.first, now.Add(-2*time.Minute))
			if tc.firstInit != nil {
				tc.firstInit(pod)
			}
			r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode())
			settle(t, r, now, driftReq())
			if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
				t.Fatalf("first reconcile: %v", err)
			}
			if events := drainEvents(rec); len(events) != 1 {
				t.Fatalf("events = %v, want one for the first blocker", events)
			}

			// Two minutes later, inside the interval, the blocker differs.
			changed := blockedPod("CreateContainerError", tc.second, now.Add(-2*time.Minute))
			if tc.secondInit != nil {
				tc.secondInit(changed)
			}
			if err := r.Status().Update(context.Background(), changed); err != nil {
				t.Fatalf("update pod status: %v", err)
			}
			before := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "inference", tc.wantCause))
			r.now = func() time.Time { return now.Add(2 * time.Minute) }
			if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
				t.Fatalf("reconcile after the blocker changed: %v", err)
			}
			if after := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "inference", tc.wantCause)); after-before != 1 {
				t.Errorf("counter delta on cause %q = %v, want 1 (a new blocker inside the interval must still be reported)",
					tc.wantCause, after-before)
			}
			events := drainEvents(rec)
			want := `container "` + tc.wantOn + `" cannot start`
			if len(events) != 1 || !strings.Contains(events[0], tc.wantCause) || !strings.Contains(events[0], want) {
				t.Errorf("events = %v, want one %s event whose text contains %q", events, tc.wantCause, want)
			}
		})
	}
}

// TestBlockedPinning_ReturningBlockerReportsAgain: only the last blocker is remembered,
// so a returning one is reported again; the retry loop the throttle targets never changes blocker.
func TestBlockedPinning_ReturningBlockerReportsAgain(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	first := blockedPod("CreateContainerError", msgNoBalloonDef, now.Add(-2*time.Minute))
	r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, first, enabledNode())
	settle(t, r, now, driftReq())
	before := blockedSignal(t)
	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	for i, message := range []string{msgNoInstance, msgNoBalloonDef} {
		pod := blockedPod("CreateContainerError", message, now.Add(-2*time.Minute))
		if err := r.Status().Update(context.Background(), pod); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
		r.now = func() time.Time { return now.Add(time.Duration(i+1) * time.Minute) }
		if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if got := blockedSignal(t) - before; got != 3 {
		t.Errorf("signal delta = %v, want 3 (each change of blocker reports, including a return)", got)
	}
	if events := drainEvents(rec); len(events) != 3 {
		t.Errorf("events = %v, want three", events)
	}
}

// TestBlockedPinning_RecreatedPodReportsAgain: a replacement pod with the same name is not throttled.
func TestBlockedPinning_RecreatedPodReportsAgain(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := blockedPod("CreateContainerError", msgNoInstance, now.Add(-2*time.Minute))
	r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode())
	settle(t, r, now, driftReq())
	before := blockedSignal(t)

	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	replacement := blockedPod("CreateContainerError", msgNoInstance, now.Add(-2*time.Minute))
	replacement.UID = types.UID("pod-uid-2")
	if err := r.Update(context.Background(), replacement); err != nil {
		t.Fatalf("replace pod: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile replacement: %v", err)
	}
	// New UID, new episode: it settles again before reporting.
	r.now = func() time.Time { return now.Add(2 * pinBlockedGracePeriod) }
	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile replacement after its window: %v", err)
	}
	if got := blockedSignal(t) - before; got != 2 {
		t.Errorf("counter delta = %v, want 2 (a recreated pod is a fresh episode)", got)
	}
	if events := drainEvents(rec); len(events) != 2 {
		t.Errorf("events = %v, want one per pod", events)
	}
}

// TestBlockedPinning_ForgetsDeletedPod: a deleted pod's throttle record is dropped.
func TestBlockedPinning_ForgetsDeletedPod(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := blockedPod("CreateContainerError", msgNoInstance, now.Add(-2*time.Minute))
	r, _ := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode())

	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := r.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}
	r.blockedMu.Lock()
	remaining := len(r.blockedReports)
	r.blockedMu.Unlock()
	if remaining != 0 {
		t.Errorf("blockedReports holds %d entries after the pod was deleted, want 0", remaining)
	}
}

// TestBlockedPinning_NeverEvicts: the blocked path never evicts, under either drift
// policy: the replacement is stamped identically, the throttle cannot brake it, and
// PDBs are skipped for Pending pods.
func TestBlockedPinning_NeverEvicts(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, policy := range []string{driftPolicyEventsOnly, driftPolicyRestart} {
		t.Run(policy, func(t *testing.T) {
			var evictions int
			pod := blockedPod("CreateContainerError", msgNoInstance, now.Add(-2*time.Minute))
			r, rec := blockedReconciler(t, policy, now, &evictions, pod, enabledNode())
			settle(t, r, now, driftReq())
			r.EvictionLimiter = rate.NewLimiter(rate.Inf, 1)

			res, err := r.Reconcile(context.Background(), driftReq())
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if evictions != 0 {
				t.Errorf("eviction calls = %d, want 0 (a blocked pod is reported, not deleted)", evictions)
			}
			if res.RequeueAfter != pinBlockedReportInterval {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, pinBlockedReportInterval)
			}
			events := drainEvents(rec)
			if len(events) != 1 || !strings.Contains(events[0], "PinningBlocked") {
				t.Fatalf("events = %v, want one PinningBlocked event", events)
			}
			// The event names the manual step, since the operator takes none.
			if !strings.Contains(events[0], "delete the pod") {
				t.Errorf("event %q does not tell the operator to delete the pod", events[0])
			}

			if err := r.Get(context.Background(), driftReq().NamespacedName, &corev1.Pod{}); err != nil {
				t.Errorf("blocked pod should still exist: %v", err)
			}
		})
	}
}

// TestBlockedPinning_IgnoresTerminatingPod: a terminating pod is not reported and its record is dropped.
func TestBlockedPinning_IgnoresTerminatingPod(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := blockedPod("CreateContainerError", msgNoInstance, now.Add(-2*time.Minute))
	pod.Finalizers = []string{"cpu-pinning-operator.intel.com/test-hold"}
	r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode())

	// The finalizer keeps the deleted object with a DeletionTimestamp.
	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	drainEvents(rec)
	if err := r.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	before := blockedSignal(t)
	r.now = func() time.Time { return now.Add(pinBlockedReportInterval + time.Minute) }
	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile terminating pod: %v", err)
	}
	if got := blockedSignal(t); got != before {
		t.Errorf("signal moved by %v for a terminating pod, want 0", got-before)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("events = %v, want none for a terminating pod", events)
	}
	r.blockedMu.Lock()
	remaining := len(r.blockedReports)
	r.blockedMu.Unlock()
	if remaining != 0 {
		t.Errorf("blockedReports holds %d entries for a terminating pod, want 0", remaining)
	}
}

// TestBlockedPinning_ForgetsRecoveredPod: a recovered pod that blocks again is reported again.
func TestBlockedPinning_ForgetsRecoveredPod(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	recovered := []struct {
		name  string
		apply func(*corev1.Pod)
	}{
		{"container started", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodRunning
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now)},
			}
		}},
		{"container terminated, as a completed init container does", func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"},
			}
		}},
	}
	for _, tc := range recovered {
		t.Run(tc.name, func(t *testing.T) {
			pod := blockedPod("CreateContainerError", msgNoInstance, now.Add(-2*time.Minute))
			r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode())
			settle(t, r, now, driftReq())
			if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
				t.Fatalf("first reconcile: %v", err)
			}
			drainEvents(rec)

			healthy := blockedPod("CreateContainerError", msgNoInstance, now.Add(-2*time.Minute))
			tc.apply(healthy)
			// Pod status is a subresource.
			if err := r.Status().Update(context.Background(), healthy); err != nil {
				t.Fatalf("update pod status: %v", err)
			}
			if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
				t.Fatalf("reconcile recovered pod: %v", err)
			}
			r.blockedMu.Lock()
			remaining := len(r.blockedReports)
			r.blockedMu.Unlock()
			if remaining != 0 {
				t.Fatalf("blockedReports holds %d entries after the pod recovered, want 0", remaining)
			}

			blockedAgain := blockedPod("CreateContainerError", msgNoInstance, now.Add(-2*time.Minute))
			if err := r.Status().Update(context.Background(), blockedAgain); err != nil {
				t.Fatalf("re-block pod: %v", err)
			}
			before := blockedSignal(t)
			if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
				t.Fatalf("reconcile re-blocked pod: %v", err)
			}
			// Forgetting cleared the window too, so the new episode settles first.
			r.now = func() time.Time { return now.Add(2 * pinBlockedGracePeriod) }
			if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
				t.Fatalf("reconcile re-blocked pod after its window: %v", err)
			}
			if got := blockedSignal(t) - before; got != 1 {
				t.Errorf("signal delta after re-blocking = %v, want 1", got)
			}
		})
	}
}

// TestBlockedPinning_InitRefusalWinsOverWorkload: the init container's cause wins, since it runs first.
func TestBlockedPinning_InitRefusalWinsOverWorkload(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := blockedPod("CreateContainerError", msgNotEnoughCPUs, now.Add(-2*time.Minute))
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "wait-for-balloons",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CreateContainerError", Message: msgNoBalloonDef,
		}},
	}}
	r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode())
	settle(t, r, now, driftReq())
	before := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "inference", metrics.PinBlockedNoBalloon))

	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if after := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "inference", metrics.PinBlockedNoBalloon)); after-before != 1 {
		t.Errorf("counter delta on cause %q = %v, want 1", metrics.PinBlockedNoBalloon, after-before)
	}
	events := drainEvents(rec)
	if len(events) != 1 || !strings.Contains(events[0], `container "wait-for-balloons" cannot start`) {
		t.Errorf("events = %v, want one event naming the init container", events)
	}
}

// TestBlockedPinning_ReportsPodWithoutAMatchingPolicy: reported even with no matching
// policy, labelled with the pod's namespace.
func TestBlockedPinning_ReportsPodWithoutAMatchingPolicy(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := blockedPod("CreateContainerError", msgNoInstance, now.Add(-2*time.Minute))
	pod.Namespace = "retired"
	req := ctrl.Request{}
	req.Namespace, req.Name = pod.Namespace, pod.Name

	var evictions int
	r, rec := blockedReconciler(t, driftPolicyRestart, now, &evictions, pod, enabledNode())
	settle(t, r, now, req)
	r.EvictionLimiter = rate.NewLimiter(rate.Inf, 1)
	before := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "retired", metrics.PinBlockedNoCapacity))

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if after := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "retired", metrics.PinBlockedNoCapacity)); after-before != 1 {
		t.Errorf("counter delta for namespace %q = %v, want 1 (the metric must carry the POD's namespace)", pod.Namespace, after-before)
	}
	if events := drainEvents(rec); len(events) != 1 {
		t.Errorf("events = %v, want one PinningBlocked event for a de-matched pinned pod", events)
	}
	if evictions != 0 {
		t.Errorf("eviction calls = %d, want 0", evictions)
	}
	if res.RequeueAfter != pinBlockedReportInterval {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, pinBlockedReportInterval)
	}
}

// TestBlockedPinning_RunningPodTakesDriftPath: a running container with no refusal is
// judged by the drift path, never the blocked path.
func TestBlockedPinning_RunningPodTakesDriftPath(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := pinnedPodRunningSince(now.Add(-2 * time.Minute))
	pod.Annotations[pinnedHashAnnotation] = "stale-hash"
	r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode())
	before := blockedSignal(t)

	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := blockedSignal(t); got != before {
		t.Errorf("blocked counter moved by %v for a Running pod, want 0", got-before)
	}
	events := drainEvents(rec)
	if len(events) != 1 || !strings.Contains(events[0], "Drifted") {
		t.Errorf("events = %v, want the drift path's Drifted event for a Running pod", events)
	}
}

// TestClassifyBalloonsRefusal_RequiresPluginAttribution: without the plugin's error
// prefix nothing is classified.
func TestClassifyBalloonsRefusal_RequiresPluginAttribution(t *testing.T) {
	for _, message := range []string{
		"",
		"Back-off pulling image \"registry.example/vllm:latest\"",
		`failed to create containerd task: no balloon for annotation "balloon-span2"`,
	} {
		if cause, ok := classifyBalloonsRefusal(message); ok {
			t.Errorf("classifyBalloonsRefusal(%q) = %q, true; want not classified", message, cause)
		}
	}
	if cause, ok := classifyBalloonsRefusal(msgNoBalloonDef); !ok || cause != metrics.PinBlockedNoBalloon {
		t.Errorf("classifyBalloonsRefusal(no-balloon message) = %q, %v; want %q, true", cause, ok, metrics.PinBlockedNoBalloon)
	}
}

// refusedAfterRestartPod: its container ran, died, and was refused on recreate. The
// kubelet reports such a pod as Running, so the phase cannot route it.
func refusedAfterRestartPod(startedAt, diedAt time.Time) *corev1.Pod {
	pod := pinnedPod(inSyncHash())
	pod.UID = types.UID("pod-uid-restart")
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	pod.Status.Phase = corev1.PodRunning
	pod.Status.StartTime = &metav1.Time{Time: startedAt}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "vllm",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CreateContainerError", Message: msgNotEnoughCPUs,
		}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 137, FinishedAt: metav1.NewTime(diedAt),
		}},
	}}
	return pod
}

// TestBlockedPinning_RefusedAfterRestartReported: a refusal after restart is reported
// whatever the NRT state (stale zone, missing zone, absent CR).
func TestBlockedPinning_RefusedAfterRestartReported(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name string
		nrt  []runtime.Object
	}{
		{"dead container's zone still published", []runtime.Object{nrtObj(nrtZone("inference/vllm-x/vllm", "balloon-span2[0]", "0-1"))}},
		{"zone already removed", []runtime.Object{nrtObj()}},
		{"no NodeResourceTopology CR at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := refusedAfterRestartPod(now.Add(-2*time.Hour), now.Add(-30*time.Minute))
			objs := append([]runtime.Object{pod, enabledNode()}, tc.nrt...)
			r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, objs...)
			settle(t, r, now, driftReq())
			before := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "inference", metrics.PinBlockedNoCapacity))

			res, err := r.Reconcile(context.Background(), driftReq())
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "inference", metrics.PinBlockedNoCapacity)) - before; got != 1 {
				t.Errorf("cpu_pinning_pods_pinning_blocked_total delta = %v, want 1", got)
			}
			events := drainEvents(rec)
			if len(events) != 1 || !strings.Contains(events[0], reasonPinningBlocked) {
				t.Fatalf("events = %v, want one PinningBlocked event", events)
			}
			if res.RequeueAfter != pinBlockedReportInterval {
				t.Errorf("RequeueAfter = %v, want %v so the metric keeps moving", res.RequeueAfter, pinBlockedReportInterval)
			}
		})
	}
}

// TestBlockedPinning_TerminalPodLeftAlone: a refusal on a terminal pod is history.
func TestBlockedPinning_TerminalPodLeftAlone(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed} {
		t.Run(string(phase), func(t *testing.T) {
			pod := refusedAfterRestartPod(now.Add(-2*time.Hour), now.Add(-30*time.Minute))
			pod.Status.Phase = phase
			r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode())
			before := blockedSignal(t)

			if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := blockedSignal(t); got != before {
				t.Errorf("blocked signal moved by %v for a %s pod, want 0", got-before, phase)
			}
			if events := drainEvents(rec); len(events) != 0 {
				t.Errorf("events = %v, want none for a %s pod", events, phase)
			}
		})
	}
}

// TestBlockedPinning_StaleHashRefusalStillDrifts: drifted and refused under restart,
// drift wins, because its replacement re-admits with the current config.
func TestBlockedPinning_StaleHashRefusalStillDrifts(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := refusedAfterRestartPod(now.Add(-2*time.Hour), now.Add(-30*time.Minute))
	pod.Annotations[pinnedHashAnnotation] = "stamped-under-the-old-policy"

	var evictions int
	r, rec := blockedReconciler(t, driftPolicyRestart, now, &evictions, pod, enabledNode())
	r.EvictionLimiter = rate.NewLimiter(rate.Inf, 1)
	beforeBlocked := blockedSignal(t)
	beforeDrift := testutil.ToFloat64(metrics.PodsDrifted.WithLabelValues("n1", "inference"))

	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := testutil.ToFloat64(metrics.PodsDrifted.WithLabelValues("n1", "inference")) - beforeDrift; got != 1 {
		t.Errorf("cpu_pinning_pods_drifted_total delta = %v, want 1", got)
	}
	if got := blockedSignal(t); got != beforeBlocked {
		t.Errorf("blocked signal moved by %v, want 0 — drift owns a pod whose config moved", got-beforeBlocked)
	}
	if evictions != 1 {
		t.Errorf("eviction calls = %d, want 1 so the replacement re-admits under the current config", evictions)
	}
	events := drainEvents(rec)
	if len(events) != 1 || !strings.Contains(events[0], "Drifted") {
		t.Errorf("events = %v, want the Drifted event", events)
	}
}

// TestBlockedPinning_InSyncRefusalReportsNotDrifts: with the config in sync the refusal
// owns the pod and drift stays quiet.
func TestBlockedPinning_InSyncRefusalReportsNotDrifts(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := refusedAfterRestartPod(now.Add(-2*time.Hour), now.Add(-30*time.Minute))

	var evictions int
	r, rec := blockedReconciler(t, driftPolicyRestart, now, &evictions, pod, enabledNode(), nrtObj())
	settle(t, r, now, driftReq())
	beforeDrift := testutil.ToFloat64(metrics.PodsDrifted.WithLabelValues("n1", "inference"))
	before := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "inference", metrics.PinBlockedNoCapacity))

	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := testutil.ToFloat64(metrics.PinningBlocked.WithLabelValues("n1", "inference", metrics.PinBlockedNoCapacity)) - before; got != 1 {
		t.Errorf("blocked delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.PodsDrifted.WithLabelValues("n1", "inference")) - beforeDrift; got != 0 {
		t.Errorf("drifted delta = %v, want 0 — the config never moved", got)
	}
	if evictions != 0 {
		t.Errorf("eviction calls = %d, want 0 (the blocked path never evicts)", evictions)
	}
	if events := drainEvents(rec); len(events) != 1 || !strings.Contains(events[0], reasonPinningBlocked) {
		t.Errorf("events = %v, want one PinningBlocked event", events)
	}
}

// TestBlockedPinning_ReportedWhereDriftCannotConverge: where drift cannot evict the
// refusal is reported and drift is silent, so one remedy is not warned twice.
func TestBlockedPinning_ReportedWhereDriftCannotConverge(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	cases := []struct {
		name      string
		policy    string
		wantDrift float64
		mutate    func(*corev1.Pod)
		objs      []runtime.Object
	}{
		{
			name: "de-matched, which never evicts", policy: driftPolicyRestart, wantDrift: 0,
			mutate: func(p *corev1.Pod) { p.Spec.Containers[0].Name = "not-vllm" },
		},
		{
			name: "stale hash under eventsOnly, which never evicts", policy: driftPolicyEventsOnly, wantDrift: 0,
			mutate: func(p *corev1.Pod) { p.Annotations[pinnedHashAnnotation] = "stale" },
		},
		{
			name: "node object gone, so there is no hash to compare", policy: driftPolicyRestart, wantDrift: 0,
			objs: []runtime.Object{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := refusedAfterRestartPod(now.Add(-2*time.Hour), now.Add(-30*time.Minute))
			if tc.mutate != nil {
				tc.mutate(pod)
			}
			objs := append([]runtime.Object{pod}, tc.objs...)
			if tc.objs == nil {
				objs = append(objs, enabledNode())
			}
			var evictions int
			r, rec := blockedReconciler(t, tc.policy, now, &evictions, objs...)
			r.EvictionLimiter = rate.NewLimiter(rate.Inf, 1)
			settle(t, r, now, driftReq())
			before := blockedSignal(t)
			beforeDrift := testutil.ToFloat64(metrics.PodsDrifted.WithLabelValues("n1", "inference"))

			res, err := r.Reconcile(context.Background(), driftReq())
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := blockedSignal(t) - before; got != 1 {
				t.Errorf("blocked signal delta = %v, want 1 — nothing else will move this pod", got)
			}
			if got := testutil.ToFloat64(metrics.PodsDrifted.WithLabelValues("n1", "inference")) - beforeDrift; got != tc.wantDrift {
				t.Errorf("drifted delta = %v, want %v — an unthrottled Drifted event must not repeat alongside the refusal", got, tc.wantDrift)
			}
			if evictions != 0 {
				t.Errorf("eviction calls = %d, want 0 on a path that cannot converge", evictions)
			}
			if !hasBlockedEvent(drainEvents(rec)) {
				t.Error("expected a PinningBlocked event")
			}
			if res.RequeueAfter != pinBlockedReportInterval {
				t.Errorf("RequeueAfter = %v, want %v so the metric keeps moving", res.RequeueAfter, pinBlockedReportInterval)
			}
		})
	}
}

// TestBlockedPinning_PDBBlockedEvictionStillReports: a PDB that never allows the
// eviction means drift cannot converge, so the refusal is reported.
func TestBlockedPinning_PDBBlockedEvictionStillReports(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := refusedAfterRestartPod(now.Add(-2*time.Hour), now.Add(-30*time.Minute))
	pod.Annotations[pinnedHashAnnotation] = "stale"

	c := fake.NewClientBuilder().WithScheme(driftScheme()).
		WithIndex(&corev1.Pod{}, PodPinnedByField, indexPodByPinnedBy).
		WithRuntimeObjects(pod, enabledNode()).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceCreate: func(_ context.Context, _ client.Client, name string, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if name == "eviction" {
					return apierrors.NewTooManyRequests("PodDisruptionBudget denies the eviction", 0)
				}
				return nil
			},
		}).Build()
	r, rec := blockedReconciler(t, driftPolicyRestart, now, nil)
	r.Client = c
	r.EvictionLimiter = rate.NewLimiter(rate.Inf, 1)
	settle(t, r, now, driftReq())
	before := blockedSignal(t)

	res, err := r.Reconcile(context.Background(), driftReq())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := blockedSignal(t) - before; got != 1 {
		t.Errorf("blocked signal delta = %v, want 1 — a deferred eviction converges on nothing", got)
	}
	if !hasBlockedEvent(drainEvents(rec)) {
		t.Error("expected a PinningBlocked event alongside the deferred eviction")
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > pinBlockedReportInterval {
		t.Errorf("RequeueAfter = %v, want the sooner of the eviction backoff and the report interval", res.RequeueAfter)
	}
}

func hasBlockedEvent(events []string) bool {
	for _, e := range events {
		if strings.Contains(e, reasonPinningBlocked) {
			return true
		}
	}
	return false
}

// TestBlockedPinning_KeepsRecordAcrossBackoff: between retries the kubelet shows
// CrashLoopBackOff or ContainerCreating instead of the refusal; treating that as
// recovery would restart the settling window forever.
func TestBlockedPinning_KeepsRecordAcrossBackoff(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, between := range []corev1.ContainerStateWaiting{
		{Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container=vllm pod=vllm-x"},
		{Reason: "ContainerCreating"},
	} {
		t.Run(between.Reason, func(t *testing.T) {
			pod := refusedAfterRestartPod(now.Add(-2*time.Hour), now.Add(-30*time.Minute))
			r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode(), nrtObj())
			before := blockedSignal(t)

			// Refusal visible, then not, then visible again, within one settling window.
			if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
				t.Fatalf("reconcile on the refusal: %v", err)
			}
			backingOff := refusedAfterRestartPod(now.Add(-2*time.Hour), now.Add(-30*time.Minute))
			backingOff.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: between.DeepCopy()}
			if err := r.Status().Update(context.Background(), backingOff); err != nil {
				t.Fatalf("publish the between-retries status: %v", err)
			}
			r.now = func() time.Time { return now.Add(10 * time.Second) }
			if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
				t.Fatalf("reconcile between retries: %v", err)
			}
			if got := blockedSignal(t) - before; got != 0 {
				t.Fatalf("signal moved by %v with no refusal visible, want 0", got)
			}

			// The next retry republishes the refusal past the window.
			if err := r.Status().Update(context.Background(), refusedAfterRestartPod(now.Add(-2*time.Hour), now.Add(-30*time.Minute))); err != nil {
				t.Fatalf("republish the refusal: %v", err)
			}
			r.now = func() time.Time { return now.Add(pinBlockedGracePeriod) }
			if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
				t.Fatalf("reconcile on the next retry: %v", err)
			}
			if got := blockedSignal(t) - before; got != 1 {
				t.Errorf("signal delta = %v, want 1 — the window must survive the backoff, or the pod is never reported", got)
			}
			if !hasBlockedEvent(drainEvents(rec)) {
				t.Error("expected a PinningBlocked event once the window closed")
			}
		})
	}
}

// TestBlockedPinning_EventNamesWhyItDied: on a refusal after restart the event names the
// previous termination.
func TestBlockedPinning_EventNamesWhyItDied(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pod := refusedAfterRestartPod(now.Add(-2*time.Hour), now.Add(-30*time.Minute))
	pod.Status.ContainerStatuses[0].LastTerminationState.Terminated.Reason = "OOMKilled"
	r, rec := blockedReconciler(t, driftPolicyEventsOnly, now, nil, pod, enabledNode(), nrtObj())
	settle(t, r, now, driftReq())

	if _, err := r.Reconcile(context.Background(), driftReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	events := drainEvents(rec)
	if len(events) != 1 {
		t.Fatalf("events = %v, want one", events)
	}
	for _, want := range []string{"OOMKilled", "exit 137", "had been running"} {
		if !strings.Contains(events[0], want) {
			t.Errorf("event %q does not mention %q", events[0], want)
		}
	}
}
