// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// pluginVerdict is the balloons plugin's status writeback for one node on a
// BalloonsPolicy: the generation it validated and what it concluded.
type pluginVerdict struct {
	generation int64
	found      bool
	status     string
	errors     string
}

func readPluginVerdict(bp *unstructured.Unstructured, node string) pluginVerdict {
	gen, found, _ := unstructured.NestedInt64(bp.Object, "status", "nodes", node, "generation")
	status, _, _ := unstructured.NestedString(bp.Object, "status", "nodes", node, "status")
	errText, _, _ := unstructured.NestedString(bp.Object, "status", "nodes", node, "errors")
	return pluginVerdict{generation: gen, found: found, status: status, errors: errText}
}

// watchBalloonsPolicy hands fn every revision of the BalloonsPolicy at key until fn
// returns true or timeout passes; it returns errors so a t.Cleanup can use it.
// A watch, not a poll: status.nodes.<node> holds only the latest verdict, which the
// next generation can overwrite between polls. fromRV starts just after the caller's
// write (empty: from a fresh read); gap reports an expired history that was re-read.
func watchBalloonsPolicy(key client.ObjectKey, fromRV string, timeout time.Duration, fn func(*unstructured.Unstructured) bool) (decided, gap bool, err error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return false, false, fmt.Errorf("kubeconfig for the BalloonsPolicy watch: %w", err)
	}
	wc, err := client.NewWithWatch(cfg, client.Options{})
	if err != nil {
		return false, false, fmt.Errorf("build a watching client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return watchRevisions(ctx, wc, key, fromRV, fn)
}

// watchRevisions is watchBalloonsPolicy's loop over a given client. The cursor is
// taken before fn sees a copy, so a caller patching its copy cannot skip revisions;
// only an expired history (410) re-reads.
func watchRevisions(ctx context.Context, wc client.WithWatch, key client.ObjectKey, fromRV string,
	fn func(*unstructured.Unstructured) bool) (decided, gap bool, err error) {
	cursor := fromRV
	relist := func() (bool, error) {
		bp := newUnstructured(bpGVK)
		if err := wc.Get(ctx, key, bp); err != nil {
			return false, ignoreDeadline(ctx, err)
		}
		cursor = bp.GetResourceVersion()
		return fn(bp.DeepCopy()), nil
	}
	if cursor == "" {
		if done, err := relist(); err != nil || done {
			return done, false, err
		}
	}
	for ctx.Err() == nil {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(bpGVK.GroupVersion().WithKind(bpGVK.Kind + "List"))
		w, err := wc.Watch(ctx, list, client.InNamespace(key.Namespace),
			client.MatchingFields{"metadata.name": key.Name},
			&client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: cursor}})
		expired := apierrors.IsResourceExpired(err) || apierrors.IsGone(err)
		if err != nil && !expired {
			return false, gap, ignoreDeadline(ctx, err)
		}
		if err == nil {
			for ev := range w.ResultChan() {
				if ev.Type == watch.Error {
					// Only a 410 is a gap to re-read; other streamed errors reach the caller.
					serr := apierrors.FromObject(ev.Object)
					if !apierrors.IsResourceExpired(serr) && !apierrors.IsGone(serr) {
						w.Stop()
						return false, gap, ignoreDeadline(ctx, serr)
					}
					expired = true
					break
				}
				obj, ok := ev.Object.(*unstructured.Unstructured)
				if !ok {
					continue
				}
				cursor = obj.GetResourceVersion()
				if ev.Type != watch.Deleted && fn(obj.DeepCopy()) {
					w.Stop()
					return true, gap, nil
				}
			}
			w.Stop()
		}
		if expired {
			gap = true
			if done, err := relist(); err != nil || done {
				return done, gap, err
			}
		}
	}
	return false, gap, nil
}

// ignoreDeadline drops the error a request returns because the watch's own
// timeout ran out: that is "no verdict", not a failure.
func ignoreDeadline(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// sortedGens lists a set of generations in order, for a failure message.
func sortedGens(gens map[int64]bool) []int64 {
	return slices.Sorted(maps.Keys(gens))
}

// pluginPods lists the balloons plugin pods scheduled on node.
func pluginPods(c client.Client, node string) ([]corev1.Pod, error) {
	ctx := context.Background()
	var ds appsv1.DaemonSet
	if err := c.Get(ctx, client.ObjectKey{Namespace: balloonsNS(), Name: balloonsDaemonSet()}, &ds); err != nil {
		return nil, fmt.Errorf("plugin DaemonSet %s/%s unreadable: %w", balloonsNS(), balloonsDaemonSet(), err)
	}
	sel, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("plugin DaemonSet selector invalid: %w", err)
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(ds.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, fmt.Errorf("list plugin pods: %w", err)
	}
	var on []corev1.Pod
	for _, p := range pods.Items {
		if p.Spec.NodeName == node {
			on = append(on, p)
		}
	}
	return on, nil
}

// pluginReady reports whether the node has a plugin pod and every one is Ready.
func pluginReady(t *testing.T, c client.Client, node string) bool {
	t.Helper()
	pods, err := pluginPods(c, node)
	if err != nil || len(pods) == 0 {
		return false
	}
	for i := range pods {
		if !podReady(&pods[i]) {
			return false
		}
	}
	return true
}

// pluginPodState describes the balloons plugin pod(s) on node, to tell a down plugin
// from a verdict race when no verdict arrived.
func pluginPodState(t *testing.T, c client.Client, node string) string {
	t.Helper()
	pods, err := pluginPods(c, node)
	if err != nil {
		return err.Error()
	}
	var out []string
	for i := range pods {
		p := &pods[i]
		s := fmt.Sprintf("%s phase=%s", p.Name, p.Status.Phase)
		for _, cs := range p.Status.ContainerStatuses {
			s += fmt.Sprintf(" %s(ready=%v restarts=%d", cs.Name, cs.Ready, cs.RestartCount)
			if w := cs.State.Waiting; w != nil {
				s += " waiting=" + w.Reason
			}
			if lt := cs.LastTerminationState.Terminated; lt != nil {
				s += fmt.Sprintf(" lastExit=%d/%s at %s", lt.ExitCode, lt.Reason, lt.FinishedAt.Format(time.RFC3339))
			}
			s += ")"
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return "no plugin pod on node " + node
	}
	return strings.Join(out, "; ")
}
