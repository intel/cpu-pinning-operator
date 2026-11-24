// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/intel/cpu-pinning-operator/internal/metrics"
)

// Blocked pinning: the balloons plugin refused a pinned pod's container at
// CreateContainer. Neither the scheduler (pod is bound) nor the ReplicaSet (error
// is not terminal) recovers it.

const (
	reasonPinningBlocked = "PinningBlocked"

	// How long a refusal must persist before it is reported, so the plugin can
	// converge on a freshly applied BalloonsPolicy (same window as pinVerifyGracePeriod).
	pinBlockedGracePeriod = pinVerifyGracePeriod

	// One report per pod per interval while the blocker is unchanged.
	pinBlockedReportInterval = 10 * time.Minute

	// Prefix on every balloons policy error; the runtime passes it to the kubelet verbatim.
	balloonsErrorPrefix = "balloons: "
)

// balloonsCauses maps balloons policy error text (containers/nri-plugins
// balloons-policy.go) to a cause. An unhealthy plugin is absent on purpose: the
// runtime then skips NRI and the container starts unpinned, with no message.
var balloonsCauses = []struct{ substr, cause string }{
	{"no balloon for annotation", metrics.PinBlockedNoBalloon},
	{"no applicable balloon type found", metrics.PinBlockedNoBalloon},
	{"unknown balloon definition", metrics.PinBlockedNoBalloon},
	{"unknown ComponentCreation mode", metrics.PinBlockedNoBalloon},
	{"no suitable balloon instance available", metrics.PinBlockedNoCapacity},
	{"no suitable balloons found for container", metrics.PinBlockedNoCapacity},
	{"not enough CPUs to", metrics.PinBlockedNoCapacity},
	{"resize/inflate", metrics.PinBlockedNoCapacity},
	{"MaxBalloons limit", metrics.PinBlockedNoCapacity},
}

// balloonsRefusal is a container the balloons plugin refused.
type balloonsRefusal struct {
	container string
	cause     string
	reason    string // the kubelet's waiting reason, e.g. CreateContainerError
	message   string // the kubelet's message, verbatim (it names the balloon)
	died      string // e.g. `OOMKilled (exit 137)`, empty on a first start
}

// blockedPinningRefusal returns the first container whose waiting message blames
// the balloons plugin. Init containers count: a pod-scoped pin makes the plugin
// refuse the injected wait-for-balloons container first.
func blockedPinningRefusal(pod *corev1.Pod) (balloonsRefusal, bool) {
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for i := range statuses {
			cs := &statuses[i]
			if cs.State.Waiting == nil {
				continue
			}
			cause, ok := classifyBalloonsRefusal(cs.State.Waiting.Message)
			if !ok {
				continue
			}
			return balloonsRefusal{
				container: cs.Name,
				cause:     cause,
				reason:    cs.State.Waiting.Reason,
				message:   cs.State.Waiting.Message,
				died:      previousTermination(cs),
			}, true
		}
	}
	return balloonsRefusal{}, false
}

// previousTermination renders why the container's last run ended, or "" if it never ran.
func previousTermination(cs *corev1.ContainerStatus) string {
	t := cs.LastTerminationState.Terminated
	if t == nil {
		return ""
	}
	if t.Reason == "" {
		return fmt.Sprintf("exit %d", t.ExitCode)
	}
	return fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode)
}

// blockedContainerWaiting reports whether the named container is still Waiting.
// CrashLoopBackOff between retries hides the refusal text, so this, not the
// refusal's visibility, decides when a blocked record may be dropped.
func blockedContainerWaiting(pod *corev1.Pod, name string) bool {
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for i := range statuses {
			if statuses[i].Name == name {
				return statuses[i].State.Waiting != nil
			}
		}
	}
	return false
}

// classifyBalloonsRefusal maps a waiting message to a cause, false when the plugin
// is not what failed; an unknown plugin error is PinBlockedOther.
func classifyBalloonsRefusal(message string) (string, bool) {
	if !strings.Contains(message, balloonsErrorPrefix) {
		return "", false
	}
	for _, c := range balloonsCauses {
		if strings.Contains(message, c.substr) {
			return c.cause, true
		}
	}
	return metrics.PinBlockedOther, true
}

// reportBlockedPinning events and meters a refused pinned pod. It does not evict:
// a replacement would meet the same refusal until a human fixes capacity or policy.
func (r *PodDriftReconciler) reportBlockedPinning(pod *corev1.Pod, refusal balloonsRefusal, log logr.Logger) (ctrl.Result, error) {
	// Keeps the PinningBlocked node label non-empty; the Event's remedy names the node.
	if pod.Spec.NodeName == "" {
		log.Info("blocked pinned pod is not bound to a node; nothing to report",
			"container", refusal.container, "cause", refusal.cause)
		return ctrl.Result{}, nil
	}
	if wait, ok := r.shouldReportBlocked(pod, refusal); !ok {
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	metrics.PinningBlocked.WithLabelValues(pod.Spec.NodeName, pod.Namespace, refusal.cause).Inc()
	log.Info("pinned pod blocked: the balloons plugin refused its container",
		"container", refusal.container, "cause", refusal.cause, "kubeletReason", refusal.reason)
	if r.Recorder != nil {
		// A previous termination means it died after running, not at admission.
		died := ""
		if refusal.died != "" {
			died = fmt.Sprintf(" It had been running and terminated with %s.", refusal.died)
		}
		r.Recorder.Eventf(pod, corev1.EventTypeWarning, reasonPinningBlocked,
			"container %q cannot start (%s): the NRI balloons plugin refused it — %s: %s.%s "+
				"The pod is bound to node %q, so nothing will move or replace it; free capacity or fix the "+
				"policy/node topology, then delete the pod (see docs/troubleshooting.md).",
			refusal.container, refusal.cause, refusal.reason, refusal.message, died, pod.Spec.NodeName)
	}

	return ctrl.Result{RequeueAfter: pinBlockedReportInterval}, nil
}

// shouldReportBlocked records the refusal and reports whether to report now, or how
// long to wait. The settling window runs from first observation here, not from pod
// status.
func (r *PodDriftReconciler) shouldReportBlocked(pod *corev1.Pod, refusal balloonsRefusal) (time.Duration, bool) {
	key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	now := r.clock()

	r.blockedMu.Lock()
	defer r.blockedMu.Unlock()
	if r.blockedReports == nil {
		r.blockedReports = map[types.NamespacedName]blockedReport{}
	}
	cur, ok := r.blockedReports[key]
	if !ok || cur.uid != pod.UID {
		// New or recreated pod: start its window.
		cur = blockedReport{uid: pod.UID, firstSeen: now}
	}
	if cur.container != refusal.container || cur.cause != refusal.cause {
		// A different blocker clears the throttle but keeps the window.
		cur.container, cur.cause, cur.at = refusal.container, refusal.cause, time.Time{}
	}
	r.blockedReports[key] = cur

	if settling := pinBlockedGracePeriod - now.Sub(cur.firstSeen); settling > 0 {
		return settling, false
	}
	if !cur.at.IsZero() {
		if left := pinBlockedReportInterval - now.Sub(cur.at); left > 0 {
			return left, false
		}
	}
	cur.at = now
	r.blockedReports[key] = cur
	return 0, true
}

// forgetBlockedIfRecovered drops the record once its container stops Waiting.
// Dropping it on any reconcile without a visible refusal would restart the window
// on every kubelet backoff step, so a refused-on-restart pod would never report.
func (r *PodDriftReconciler) forgetBlockedIfRecovered(key types.NamespacedName, pod *corev1.Pod) {
	r.blockedMu.Lock()
	defer r.blockedMu.Unlock()
	rec, ok := r.blockedReports[key]
	if ok && rec.uid == pod.UID && blockedContainerWaiting(pod, rec.container) {
		return
	}
	delete(r.blockedReports, key)
}

// forgetBlocked drops a pod's report record.
func (r *PodDriftReconciler) forgetBlocked(key types.NamespacedName) {
	r.blockedMu.Lock()
	defer r.blockedMu.Unlock()
	delete(r.blockedReports, key)
}
