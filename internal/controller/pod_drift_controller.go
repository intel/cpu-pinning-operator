// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
)

const (
	pinnedByAnnotation   = cpopv1alpha1.LabelPrefix + "pinned-by"
	pinnedHashAnnotation = cpopv1alpha1.LabelPrefix + "pinned-hash"
	pinnedByValue        = "cpu-pinning-operator"

	driftPolicyRestart    = "restart"
	driftPolicyEventsOnly = "eventsOnly"

	// DefaultDriftEvictionInterval and DefaultDriftEvictionBurst rate-limit
	// drift-driven evictions (token bucket): one config change can drift every pinned
	// pod at once.
	DefaultDriftEvictionInterval = 10 * time.Second
	DefaultDriftEvictionBurst    = 2

	// driftEvictionBackoff delays retrying an eviction a PodDisruptionBudget blocks.
	driftEvictionBackoff = 30 * time.Second

	// pinVerifyGracePeriod is how long a container must run before its NRT placement
	// is checked; it must exceed NRT publish latency (a few seconds). It re-arms on
	// every container restart, exactly when a container can return unpinned.
	pinVerifyGracePeriod = 30 * time.Second

	// pinVerifyRequeue re-checks a container still inside its grace window.
	pinVerifyRequeue = 15 * time.Second
)

// PodDriftReconciler detects drift on operator-pinned pods: the desired balloon
// no longer matches the hash stamped at admission, or NRT shows the container
// unpinned. It emits a Drifted event and, under driftPolicy "restart", evicts the
// pod (PDB-aware). Plugin refusals take the blocked-pinning path.
type PodDriftReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Log         logr.Logger
	PolicyIndex *match.Index
	Recorder    record.EventRecorder

	// EvictionLimiter paces evictions within one active manager. Built lazily from
	// EvictionInterval / EvictionBurst unless injected.
	EvictionLimiter *rate.Limiter
	limiterOnce     sync.Once

	// Non-positive values fall back to the Default constants.
	EvictionInterval time.Duration
	EvictionBurst    int

	// Per blocked pod: when the refusal was first seen and last reported.
	blockedReports map[types.NamespacedName]blockedReport
	blockedMu      sync.Mutex

	// now is overridable for tests.
	now func() time.Time
}

// blockedReport is one pod's blocked-pinning bookkeeping. UID tells a recreated pod
// apart; container and cause tell a different blocker apart.
type blockedReport struct {
	uid       types.UID
	container string
	cause     string
	firstSeen time.Time
	at        time.Time
}

func (r *PodDriftReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// limiter returns the eviction limiter, shared across pods so fleet-wide drift is
// paced as a whole.
func (r *PodDriftReconciler) limiter() *rate.Limiter {
	r.limiterOnce.Do(func() {
		if r.EvictionLimiter == nil {
			interval := r.EvictionInterval
			if interval <= 0 {
				interval = DefaultDriftEvictionInterval
			}
			burst := r.EvictionBurst
			if burst <= 0 {
				burst = DefaultDriftEvictionBurst
			}
			r.EvictionLimiter = rate.NewLimiter(rate.Every(interval), burst)
		}
	})
	return r.EvictionLimiter
}

// pods/eviction backs the "restart" drift policy.
//+kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create

func (r *PodDriftReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("pod", req.NamespacedName)

	pod := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, pod); err != nil {
		if errors.IsNotFound(err) {
			r.forgetBlocked(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if pod.Annotations[pinnedByAnnotation] != pinnedByValue {
		return ctrl.Result{}, nil
	}
	if pod.DeletionTimestamp != nil {
		r.forgetBlocked(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	// A refusal shows as Pending (never started) or Running (refused on restart), so
	// phase cannot route it; other phases are left alone.
	refusal, blocked := blockedPinningRefusal(pod)
	blocked = blocked && (pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning)
	if !blocked {
		r.forgetBlockedIfRecovered(req.NamespacedName, pod)
	}
	if blocked && pod.Status.Phase == corev1.PodPending {
		return r.reportBlockedPinning(pod, refusal, log)
	}
	if pod.Status.Phase != corev1.PodRunning {
		return ctrl.Result{}, nil
	}

	// Drift outranks a refusal only where it evicts; every other exit below falls
	// back to reporting the refusal. Unbound pods have neither a node hash nor a refusal.
	if pod.Spec.NodeName == "" {
		return ctrl.Result{}, nil
	}
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// Node gone: no hash to compare, but the refusal still stands.
		if blocked {
			return r.reportBlockedPinning(pod, refusal, log)
		}
		return ctrl.Result{}, nil
	}
	// A node that left the pool is exclusion drift, still flagged.
	nodeMaxSpan := labelInt(node.Labels, LabelMaxSpan)
	if nodeMaxSpan == 0 {
		nodeMaxSpan = 8
	}

	res := r.PolicyIndex.Match(pod)
	if res.Rule == nil || res.Ambiguous {
		// De-match or ambiguity never evicts, so a refusal underneath owns the pod.
		if blocked {
			return r.reportBlockedPinning(pod, refusal, log)
		}
		r.reportDrift(ctx, pod, log, driftPolicyEventsOnly, "pod no longer matches an unambiguous BalloonPinningPolicy")
		return ctrl.Result{}, nil
	}
	rule := res.Rule
	container := res.Container

	// This node's mode; unresolved falls back to the cluster default, as admission does.
	cpuAllocation := ResolvedCPUAllocation(node.Labels)
	if cpuAllocation == "" {
		cpuAllocation = r.PolicyIndex.CPUAllocation()
	}
	if cpuAllocation == "" {
		cpuAllocation = defaultCPUAllocation
	}
	wantBalloon := match.BalloonNameForPod(pod, container, rule.Intent, nodeMaxSpan)
	haveHash := pod.Annotations[pinnedHashAnnotation]
	if !pinnedHashCurrent(haveHash, wantBalloon, cpuAllocation) {
		// eventsOnly cannot converge, so the refusal owns the pod.
		if blocked && rule.DriftPolicy != driftPolicyRestart {
			return r.reportBlockedPinning(pod, refusal, log)
		}
		requeueAfter := r.reportDrift(ctx, pod, log, rule.DriftPolicy,
			"pinning config changed since admission (want balloon %q); restart the pod to re-pin", wantBalloon)
		// A deferred eviction may never happen (PDB), so still report the refusal.
		if blocked && requeueAfter > 0 {
			res, err := r.reportBlockedPinning(pod, refusal, log)
			res.RequeueAfter = earlierRequeue(res.RequeueAfter, requeueAfter)
			return res, err
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	if blocked {
		return r.reportBlockedPinning(pod, refusal, log)
	}

	// A matching hash proves only stamping: a pod recreated while the plugin was
	// down is stamped but unpinned, so cross-check NRT.
	return r.verifyActualPinning(ctx, pod, res, log)
}

// pinnedHashCurrent reports whether the pod's stamped hash matches its config on
// this node. A stamp with an unresolved mode also counts (pods admitted before
// scheduling): comparing it to the node's mode would drift them forever. The
// balloon name is still compared.
func pinnedHashCurrent(have, balloonName, cpuAllocation string) bool {
	return have == match.PinnedConfigHash(balloonName, cpuAllocation) ||
		have == match.PinnedConfigHash(balloonName, "")
}

// verifyActualPinning checks a hash-current pod is confined to a workload balloon
// per NRT. Missing or empty NRT requeues; absent or reserved-bound containers are
// flagged only past pinVerifyGracePeriod.
func (r *PodDriftReconciler) verifyActualPinning(ctx context.Context, pod *corev1.Pod, res match.MatchResult, log logr.Logger) (ctrl.Result, error) {
	nrt := &unstructured.Unstructured{}
	nrt.SetGroupVersionKind(nrtpkg.GVK)
	if err := r.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, nrt); err != nil {
		if errors.IsNotFound(err) {
			// No NRT yet: unobservable, requeue.
			return ctrl.Result{RequeueAfter: pinVerifyRequeue}, nil
		}
		// Transient read error: retry.
		return ctrl.Result{}, err
	}
	if !nrtpkg.HasZones(nrt) {
		// Never published or emptied: same as missing.
		return ctrl.Result{RequeueAfter: pinVerifyRequeue}, nil
	}

	now := r.clock()
	requeueAfter := time.Duration(0)
	for _, name := range containersToVerify(pod, res) {
		zoneName := pod.Namespace + "/" + pod.Name + "/" + name
		balloon, found := nrtpkg.ContainerBalloon(nrt, zoneName)
		if found && balloon != reservedBalloonName {
			continue // confined to a dedicated workload balloon → actually pinned
		}

		// Absent, or on the reserved pool.
		started := containerRunningSince(pod, name)
		if started.IsZero() {
			// Not running yet.
			requeueAfter = earlierRequeue(requeueAfter, pinVerifyRequeue)
			continue
		}
		if elapsed := now.Sub(started); elapsed < pinVerifyGracePeriod {
			// Requeue for when the window closes.
			requeueAfter = earlierRequeue(requeueAfter, pinVerifyGracePeriod-elapsed)
			continue
		}

		log.Info("pinned container not confined to a dedicated balloon per NRT ground truth",
			"container", name, "balloon", balloon, "found", found)
		ra := r.reportDrift(ctx, pod, log, res.Rule.DriftPolicy,
			"pinned container %q is running unpinned: NodeResourceTopology shows it is not confined to a dedicated balloon (plugin likely unavailable when it (re)started); restart the pod to re-pin", name)
		return ctrl.Result{RequeueAfter: ra}, nil
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// containersToVerify returns the matched container, or all containers for a
// pod-scoped match.
func containersToVerify(pod *corev1.Pod, res match.MatchResult) []string {
	if res.Container != nil {
		return []string{res.Container.Name}
	}
	names := make([]string, 0, len(pod.Spec.Containers))
	for i := range pod.Spec.Containers {
		names = append(names, pod.Spec.Containers[i].Name)
	}
	return names
}

// earlierRequeue returns the sooner of two requeues; zero means none requested.
func earlierRequeue(cur, next time.Duration) time.Duration {
	if next <= 0 {
		next = pinVerifyRequeue
	}
	if cur == 0 || next < cur {
		return next
	}
	return cur
}

// containerRunningSince returns when the container entered Running, or zero.
func containerRunningSince(pod *corev1.Pod, name string) time.Time {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name != name {
			continue
		}
		if cs.State.Running != nil {
			return cs.State.Running.StartedAt.Time
		}
		return time.Time{}
	}
	return time.Time{}
}

// reportDrift emits a Drifted event and, under "restart", evicts the pod. It
// returns a non-zero requeue when the eviction was rate-limited or PDB-blocked.
func (r *PodDriftReconciler) reportDrift(ctx context.Context, pod *corev1.Pod, log logr.Logger, driftPolicy string, format string, args ...interface{}) time.Duration {
	metrics.PodsDrifted.WithLabelValues(pod.Spec.NodeName, pod.Namespace).Inc()
	if r.Recorder != nil {
		r.Recorder.Eventf(pod, corev1.EventTypeWarning, "Drifted", format, args...)
	}
	if driftPolicy != driftPolicyRestart {
		return 0
	}

	// Rate-limit evictions; without a token, defer and requeue after the delay.
	res := r.limiter().Reserve()
	// Read Delay once: it is relative to now and could race to 0.
	resDelay := res.Delay()
	if !res.OK() || resDelay > 0 {
		delay := driftEvictionBackoff
		if res.OK() {
			// Not acting now, so return the token.
			res.Cancel()
			if resDelay > 0 {
				delay = resDelay
			}
		}
		log.Info("drift eviction rate-limited; deferring pod", "requeueAfter", delay)
		metrics.DriftEvictions.WithLabelValues(pod.Spec.NodeName, pod.Namespace, metrics.DriftEvictionRateLimited).Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(pod, corev1.EventTypeWarning, "DriftEvictionDeferred",
				"drift eviction rate-limited; will retry in %s", delay)
		}
		return delay
	}

	// Evict via the eviction API so PDBs are respected; TooManyRequests backs off.
	log.Info("drift policy restart: evicting drifted pod")
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
	}
	if err := r.SubResource("eviction").Create(ctx, pod, eviction); err != nil {
		if errors.IsTooManyRequests(err) {
			log.Info("eviction blocked by PodDisruptionBudget; backing off and retrying", "requeueAfter", driftEvictionBackoff)
			metrics.DriftEvictions.WithLabelValues(pod.Spec.NodeName, pod.Namespace, metrics.DriftEvictionPDBBlocked).Inc()
			if r.Recorder != nil {
				r.Recorder.Eventf(pod, corev1.EventTypeWarning, "DriftEvictionDeferred",
					"eviction blocked by PodDisruptionBudget; will retry in %s", driftEvictionBackoff)
			}
			return driftEvictionBackoff
		}
		log.Error(err, "failed to evict drifted pod")
		metrics.DriftEvictions.WithLabelValues(pod.Spec.NodeName, pod.Namespace, metrics.DriftEvictionError).Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(pod, corev1.EventTypeWarning, "DriftEvictionDeferred",
				"could not evict drifted pod now; will retry in %s: %v", driftEvictionBackoff, err)
		}
		return driftEvictionBackoff
	}
	metrics.DriftEvictions.WithLabelValues(pod.Spec.NodeName, pod.Namespace, metrics.DriftEvictionEvicted).Inc()
	return 0
}

// SetupWithManager watches pods, plus policies and node mode changes, which change
// the config under pods without a Pod event.
func (r *PodDriftReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Watches(&cpopv1alpha1.BalloonPinningPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.enqueuePinnedPods)).
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.enqueuePodsOnNode),
			builder.WithPredicates(nodeAllocationChanged)).
		Complete(r)
}

// nodeAllocationChanged passes Node creates and resolved-mode changes only;
// kubelet heartbeats would otherwise re-enqueue every pinned pod on the node.
var nodeAllocationChanged = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return true },
	UpdateFunc: func(e event.UpdateEvent) bool {
		return ResolvedCPUAllocation(e.ObjectOld.GetLabels()) != ResolvedCPUAllocation(e.ObjectNew.GetLabels())
	},
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// enqueuePodsOnNode enqueues every operator-pinned pod bound to the node.
func (r *PodDriftReconciler) enqueuePodsOnNode(ctx context.Context, obj client.Object) []reconcile.Request {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{PodPinnedByField: pinnedByValue}); err != nil {
		r.Log.Error(err, "failed to list pods for a node-triggered drift check", "node", obj.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != obj.GetName() {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: p.Namespace, Name: p.Name,
		}})
	}
	return reqs
}

// enqueuePinnedPods enqueues every operator-pinned pod.
func (r *PodDriftReconciler) enqueuePinnedPods(ctx context.Context, _ client.Object) []reconcile.Request {
	// Via the pinned-by field index, so cost scales with pinned pods.
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{PodPinnedByField: pinnedByValue}); err != nil {
		r.Log.Error(err, "failed to list pods for policy-triggered drift check")
		return nil
	}
	var reqs []reconcile.Request
	for i := range pods.Items {
		p := &pods.Items[i]
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: p.Namespace, Name: p.Name,
		}})
	}
	return reqs
}
