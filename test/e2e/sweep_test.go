// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// The run label and the sweeper. Every object the suite creates goes through
// e2eCreate, which stamps runLabel; e2eSweep restores the shared objects the run
// snapshotted and deletes everything carrying runLabel, from any run. Renaming a
// test here means updating the -run filter in the pre-commit hook and in the
// Makefile's test-e2e-guards.

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// sweepOut receives the sweep's progress lines. The cluster-free guard test discards
// them so its fake objects never appear in a real run's log.
var sweepOut io.Writer = os.Stdout

const (
	// runLabel marks every object the suite creates; its value is runID.
	runLabel = "cpu-pinning-e2e/run"
	// sweepFile holds e2eCreate, the one place allowed to call Create.
	sweepFile = "sweep_test.go"
	// operatorLabelPrefix marks node labels the operator derives; the sweep leaves them to it.
	operatorLabelPrefix = "cpu-pinning-operator.intel.com/"
	// sweepNamespaceWait bounds the wait for labelled namespaces to finish terminating.
	sweepNamespaceWait = 3 * time.Minute
	// sweepTimeout bounds one whole sweep.
	sweepTimeout = 5 * time.Minute
)

// runID is generated once per process.
var runID = utilrand.String(8)

// e2eLabel stamps obj with runLabel and returns it.
func e2eLabel[T client.Object](obj T) T {
	l := obj.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[runLabel] = runID
	obj.SetLabels(l)
	return obj
}

// e2eCreate labels obj with the run label and creates it. Every Create in the
// package goes through here; TestEveryCreateIsLabelled enforces it.
func e2eCreate(ctx context.Context, c client.Client, obj client.Object, opts ...client.CreateOption) error {
	return c.Create(ctx, e2eLabel(obj), opts...)
}

// e2eDelete deletes obj for a cleanup: NotFound counts as done, other errors are
// retried three times over about ten seconds and then reported with t.Errorf.
func e2eDelete(t testing.TB, c client.Client, obj client.Object, opts ...client.DeleteOption) {
	t.Helper()
	var err error
	for try := 0; try < 3; try++ {
		if try > 0 {
			time.Sleep(time.Duration(try) * 3 * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = c.Delete(ctx, obj, opts...)
		cancel()
		if err == nil || apierrors.IsNotFound(err) {
			return
		}
	}
	t.Errorf("cleanup: delete %T %s: %v (left behind; `make test-e2e-sweep` removes it)", obj, objKey(obj), err)
}

func objKey(obj client.Object) string {
	if obj.GetNamespace() == "" {
		return obj.GetName()
	}
	return obj.GetNamespace() + "/" + obj.GetName()
}

// -- snapshot of the shared objects -----------------------------------------

// e2eSnapshot is the spec of every shared object a test may change; never status.
type e2eSnapshot struct {
	// Cluster is clusterIdentity of the cluster the snapshot was taken on.
	Cluster string `json:"cluster"`
	// Policies maps each BalloonPinningPolicy without runLabel to its spec.
	Policies map[string]map[string]interface{} `json:"policies"`
	// BCC is the cluster BalloonsClusterConfig's spec, nil when absent.
	BCC map[string]interface{} `json:"bcc,omitempty"`
	// DaemonSet is the balloons plugin's pod-template placement, nil when absent.
	DaemonSet *dsSnapshot `json:"daemonSet,omitempty"`
	// Nodes holds each node's cordon and labels, operator-derived labels left out.
	Nodes map[string]nodeSnapshot `json:"nodes"`
	// Operator is the operator Deployment's replica count, nil when absent.
	Operator *operatorSnapshot `json:"operator,omitempty"`
	// Webhook is the mpod.kb.io failurePolicy, nil when absent.
	Webhook *webhookSnapshot `json:"webhook,omitempty"`
	// NetworkPolicy is the operator NetworkPolicy's ingress, nil when absent.
	NetworkPolicy *netpolSnapshot `json:"networkPolicy,omitempty"`
}

type operatorSnapshot struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Replicas  int32  `json:"replicas"`
}

type webhookSnapshot struct {
	Name          string                                    `json:"name"`
	FailurePolicy admissionregistrationv1.FailurePolicyType `json:"failurePolicy"`
}

type netpolSnapshot struct {
	Namespace string                                  `json:"namespace"`
	Name      string                                  `json:"name"`
	Ingress   []networkingv1.NetworkPolicyIngressRule `json:"ingress"`
}

type dsSnapshot struct {
	Namespace    string            `json:"namespace"`
	Name         string            `json:"name"`
	Affinity     *corev1.Affinity  `json:"affinity,omitempty"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

type nodeSnapshot struct {
	Unschedulable bool              `json:"unschedulable"`
	Labels        map[string]string `json:"labels"`
}

// snapshotPath is where the snapshot is kept for a later `make test-e2e-sweep`: a
// -timeout panic or a SIGKILL skips the in-process restore.
func snapshotPath() string {
	return env("E2E_SWEEP_SNAPSHOT", filepath.Join(os.TempDir(), "cpu-pinning-e2e-snapshot.json"))
}

// clusterIdentity is the API server host and the kube-system namespace UID: a
// snapshot is only ever restored onto the cluster it came from.
func clusterIdentity(ctx context.Context, c client.Client, host string) (string, error) {
	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "kube-system"}, &ns); err != nil {
		return "", fmt.Errorf("get namespace kube-system: %w", err)
	}
	return host + " " + string(ns.UID), nil
}

func takeSnapshot(ctx context.Context, c client.Client, cluster string, dsKey client.ObjectKey) (*e2eSnapshot, error) {
	s := &e2eSnapshot{Cluster: cluster, Policies: map[string]map[string]interface{}{}, Nodes: map[string]nodeSnapshot{}}
	pols := &unstructured.UnstructuredList{}
	pols.SetGroupVersionKind(bppGVK.GroupVersion().WithKind(bppGVK.Kind + "List"))
	if err := c.List(ctx, pols); err != nil && !meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("list BalloonPinningPolicies: %w", err)
	}
	for _, p := range pols.Items {
		if _, ours := p.GetLabels()[runLabel]; !ours {
			spec, _, _ := unstructured.NestedMap(p.Object, "spec")
			s.Policies[p.GetName()] = spec
		}
	}
	if bcc, found, err := getBCC(ctx, c); err != nil && !meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("get BalloonsClusterConfig: %w", err)
	} else if found {
		s.BCC, _, _ = unstructured.NestedMap(bcc.Object, "spec")
	}
	var ds appsv1.DaemonSet
	key := dsKey
	switch err := c.Get(ctx, key, &ds); {
	case err == nil:
		s.DaemonSet = &dsSnapshot{Namespace: key.Namespace, Name: key.Name,
			Affinity: ds.Spec.Template.Spec.Affinity, NodeSelector: ds.Spec.Template.Spec.NodeSelector}
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("get DaemonSet %s: %w", key, err)
	}
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	for _, n := range nodes.Items {
		s.Nodes[n.Name] = nodeSnapshot{Unschedulable: n.Spec.Unschedulable, Labels: userLabels(n.Labels)}
	}
	var dep appsv1.Deployment
	depKey := client.ObjectKey{Name: env("E2E_OPERATOR_DEPLOYMENT", operatorDeployment), Namespace: operatorNamespace()}
	switch err := c.Get(ctx, depKey, &dep); {
	case err == nil:
		s.Operator = &operatorSnapshot{Namespace: depKey.Namespace, Name: depKey.Name, Replicas: 1}
		if dep.Spec.Replicas != nil {
			s.Operator.Replicas = *dep.Spec.Replicas
		}
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("get Deployment %s: %w", depKey, err)
	}
	var wh admissionregistrationv1.MutatingWebhookConfiguration
	switch err := c.Get(ctx, client.ObjectKey{Name: webhookConfigName()}, &wh); {
	case err == nil:
		for _, w := range wh.Webhooks {
			if w.Name == "mpod.kb.io" && w.FailurePolicy != nil {
				s.Webhook = &webhookSnapshot{Name: wh.Name, FailurePolicy: *w.FailurePolicy}
			}
		}
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("get MutatingWebhookConfiguration %s: %w", webhookConfigName(), err)
	}
	var np networkingv1.NetworkPolicy
	npKey := client.ObjectKey{Name: env("E2E_NETWORK_POLICY", operatorDeployment), Namespace: operatorNamespace()}
	switch err := c.Get(ctx, npKey, &np); {
	case err == nil:
		s.NetworkPolicy = &netpolSnapshot{Namespace: npKey.Namespace, Name: npKey.Name, Ingress: np.Spec.Ingress}
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("get NetworkPolicy %s: %w", npKey, err)
	}
	return s, nil
}

// userLabels drops the operator-derived labels: the operator re-renders them, and
// the helm-uninstall test strips them on purpose.
func userLabels(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if !strings.HasPrefix(k, operatorLabelPrefix) {
			out[k] = v
		}
	}
	return out
}

// sameJSON compares two decoded values by encoding: a snapshot read back from disk
// holds float64 where the live object holds int64.
func sameJSON(a, b interface{}) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// sweepCluster restores the operator from s, deletes every labelled object, then
// writes the other shared objects' snapshotted specs back where they changed; a nil
// s only deletes. The operator comes first because its webhooks admit the policy
// restores; the labelled pods go before those restores because a guarded policy
// rejects a match change that drops a pod it still holds.
func sweepCluster(ctx context.Context, c client.Client, s *e2eSnapshot) (restoreErr, deleteErr error) {
	if s == nil {
		return nil, deleteLabelled(ctx, c)
	}
	opErr := restoreOperator(ctx, c, s)
	deleteErr = deleteLabelled(ctx, c)
	return errors.Join(opErr, restoreShared(ctx, c, s)), deleteErr
}

// restoreShared writes every shared object's snapshotted spec but the operator's back
// where it changed.
func restoreShared(ctx context.Context, c client.Client, s *e2eSnapshot) error {
	var errs []error
	for name, spec := range s.Policies {
		if err := restoreSpec(ctx, c, bppGVK, name, spec); err != nil {
			errs = append(errs, err)
		}
	}
	if s.BCC != nil {
		if err := restoreSpec(ctx, c, bccGVK, bccName, s.BCC); err != nil {
			errs = append(errs, err)
		}
	}
	if d := s.DaemonSet; d != nil {
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var ds appsv1.DaemonSet
			if err := c.Get(ctx, client.ObjectKey{Name: d.Name, Namespace: d.Namespace}, &ds); err != nil {
				return client.IgnoreNotFound(err)
			}
			pod := &ds.Spec.Template.Spec
			if sameJSON(pod.Affinity, d.Affinity) && sameJSON(pod.NodeSelector, d.NodeSelector) {
				return nil
			}
			fmt.Fprintf(sweepOut, "e2e sweep: restoring DaemonSet %s/%s pod-template affinity and nodeSelector\n", d.Namespace, d.Name)
			pod.Affinity, pod.NodeSelector = d.Affinity, d.NodeSelector
			return c.Update(ctx, &ds)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("restore DaemonSet %s/%s: %w", d.Namespace, d.Name, err))
		}
	}
	for name, want := range s.Nodes {
		if err := restoreNode(ctx, c, name, want); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// restoreOperator puts back the webhook failurePolicy, then the operator replicas
// (waiting for them, bounded by ctx), then the NetworkPolicy ingress.
func restoreOperator(ctx context.Context, c client.Client, s *e2eSnapshot) error {
	var errs []error
	if w := s.Webhook; w != nil {
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var cfg admissionregistrationv1.MutatingWebhookConfiguration
			if err := c.Get(ctx, client.ObjectKey{Name: w.Name}, &cfg); err != nil {
				return client.IgnoreNotFound(err)
			}
			for _, h := range cfg.Webhooks {
				if h.Name == "mpod.kb.io" && (h.FailurePolicy == nil || *h.FailurePolicy != w.FailurePolicy) {
					fmt.Fprintf(sweepOut, "e2e sweep: restoring webhook %s failurePolicy %s\n", w.Name, w.FailurePolicy)
					return setWebhookFailurePolicy(ctx, c, w.Name, w.FailurePolicy)
				}
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("restore webhook %s failurePolicy: %w", w.Name, err))
		}
	}
	if o := s.Operator; o != nil {
		key := client.ObjectKey{Name: o.Name, Namespace: o.Namespace}
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var dep appsv1.Deployment
			if err := c.Get(ctx, key, &dep); err != nil {
				return err
			}
			if dep.Spec.Replicas != nil && *dep.Spec.Replicas == o.Replicas {
				return nil
			}
			fmt.Fprintf(sweepOut, "e2e sweep: restoring Deployment %s to %d replicas\n", key, o.Replicas)
			return scaleOperator(ctx, c, key, o.Replicas)
		})
		wait := 2 * time.Minute
		if dl, ok := ctx.Deadline(); ok {
			wait = min(wait, time.Until(dl))
		}
		switch {
		case apierrors.IsNotFound(err):
			// The helm-uninstall test removes the Deployment on purpose: nothing to wait for.
		case err != nil:
			errs = append(errs, fmt.Errorf("restore Deployment %s replicas: %w", key, err))
		case !waitOperatorReplicas(ctx, c, key, o.Replicas, wait):
			errs = append(errs, fmt.Errorf("operator Deployment %s: %d replicas not available within %s", key, o.Replicas, wait))
		}
	}
	if p := s.NetworkPolicy; p != nil {
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var np networkingv1.NetworkPolicy
			if err := c.Get(ctx, client.ObjectKey{Name: p.Name, Namespace: p.Namespace}, &np); err != nil {
				return client.IgnoreNotFound(err)
			}
			if sameJSON(np.Spec.Ingress, p.Ingress) {
				return nil
			}
			fmt.Fprintf(sweepOut, "e2e sweep: restoring NetworkPolicy %s/%s ingress\n", p.Namespace, p.Name)
			np.Spec.Ingress = p.Ingress
			return c.Update(ctx, &np)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("restore NetworkPolicy %s/%s ingress: %w", p.Namespace, p.Name, err))
		}
	}
	return errors.Join(errs...)
}

// restoreSpec puts spec back on a cluster-scoped custom object when it changed; a
// deleted object is left to the Helm values restore.
func restoreSpec(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, name string, spec map[string]interface{}) error {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cur := newUnstructured(gvk)
		if err := c.Get(ctx, client.ObjectKey{Name: name}, cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		live, _, _ := unstructured.NestedMap(cur.Object, "spec")
		if sameJSON(live, spec) {
			return nil
		}
		fmt.Fprintf(sweepOut, "e2e sweep: restoring %s %q spec\n", gvk.Kind, name)
		cur.Object["spec"] = spec
		return c.Update(ctx, cur)
	})
	if err != nil {
		return fmt.Errorf("restore %s %q spec: %w", gvk.Kind, name, err)
	}
	return nil
}

func restoreNode(ctx context.Context, c client.Client, name string, want nodeSnapshot) error {
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &n); err != nil {
		return client.IgnoreNotFound(err)
	}
	labels := map[string]interface{}{}
	have := userLabels(n.Labels)
	for k, v := range want.Labels {
		if hv, ok := have[k]; !ok || hv != v {
			labels[k] = v
		}
	}
	for k := range have {
		if _, ok := want.Labels[k]; !ok {
			labels[k] = nil // merge-patch null removes it
		}
	}
	if len(labels) == 0 && n.Spec.Unschedulable == want.Unschedulable {
		return nil
	}
	fmt.Fprintf(sweepOut, "e2e sweep: restoring node %s (unschedulable=%v, labels %v)\n", name, want.Unschedulable, labels)
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"labels": labels},
		"spec":     map[string]interface{}{"unschedulable": want.Unschedulable},
	})
	if err == nil {
		err = c.Patch(ctx, &n, client.RawPatch(types.MergePatchType, patch))
	}
	if err != nil {
		return fmt.Errorf("restore node %s: %w", name, err)
	}
	return nil
}

// -- delete what the run labelled --------------------------------------------

// deleteLabelled deletes every object carrying runLabel, from any run, namespaces
// last, then waits (bounded) for the namespaces to go. Objects without the label are
// never touched.
func deleteLabelled(ctx context.Context, c client.Client) error {
	var errs []error
	// Owners before what they recreate; policies after the pods a guard would protect.
	lists := []client.ObjectList{
		&appsv1.DeploymentList{}, &policyv1.PodDisruptionBudgetList{}, &corev1.PodList{},
		&networkingv1.NetworkPolicyList{}, &corev1.ConfigMapList{}, &corev1.SecretList{},
		bppList(), &corev1.NamespaceList{},
	}
	for _, list := range lists {
		if err := c.List(ctx, list, client.HasLabels{runLabel}); err != nil {
			if !meta.IsNoMatchError(err) {
				errs = append(errs, fmt.Errorf("list %T: %w", list, err))
			}
			continue
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, item := range items {
			obj := item.(client.Object)
			if obj.GetDeletionTimestamp() != nil {
				continue
			}
			var opts []client.DeleteOption
			if _, isPod := obj.(*corev1.Pod); isPod {
				opts = append(opts, client.GracePeriodSeconds(0))
			}
			// The policy guard denies delete while status.boundPods lists pods, which can
			// lag their deletion; unsetting the guard is always admitted.
			if u, ok := obj.(*unstructured.Unstructured); ok {
				if guard, _, _ := unstructured.NestedBool(u.Object, "spec", "guardActivePods"); guard {
					patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"guardActivePods":false}}`))
					if err := c.Patch(ctx, u, patch); client.IgnoreNotFound(err) != nil {
						errs = append(errs, fmt.Errorf("release guardActivePods on %s: %w", objKey(u), err))
						continue
					}
				}
			}
			if err := c.Delete(ctx, obj, opts...); client.IgnoreNotFound(err) != nil {
				errs = append(errs, fmt.Errorf("delete %T %s: %w", obj, objKey(obj), err))
			} else {
				fmt.Fprintf(sweepOut, "e2e sweep: deleted %T %s\n", obj, objKey(obj))
			}
		}
	}
	deadline := time.Now().Add(sweepNamespaceWait)
	for {
		var nss corev1.NamespaceList
		err := c.List(ctx, &nss, client.HasLabels{runLabel})
		if err == nil && len(nss.Items) == 0 {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			left := make([]string, 0, len(nss.Items))
			for _, ns := range nss.Items {
				left = append(left, ns.Name)
			}
			errs = append(errs, fmt.Errorf("labelled namespaces still present after %s: %v (list error: %v)", sweepNamespaceWait, left, err))
			break
		}
		time.Sleep(3 * time.Second)
	}
	return errors.Join(errs...)
}

func bppList() *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(bppGVK.GroupVersion().WithKind(bppGVK.Kind + "List"))
	return l
}

// -- lifecycle ---------------------------------------------------------------

var sweepState struct {
	mu    sync.Mutex
	armed bool
	c     client.Client
	snap  *e2eSnapshot
	// err is why armSweep could not delete leftovers or load, take or save the snapshot; newClient fails on it.
	err error
}

// sweepInitErr is armSweep's leftover or snapshot error, nil once it is ready.
func sweepInitErr() error {
	sweepState.mu.Lock()
	defer sweepState.mu.Unlock()
	return sweepState.err
}

func sweepArmed() bool {
	sweepState.mu.Lock()
	defer sweepState.mu.Unlock()
	return sweepState.armed
}

// sweepOnly selects `make test-e2e-sweep`: restore the snapshot an earlier run left
// on disk instead of taking a new one.
func sweepOnly() bool { return os.Getenv("E2E_SWEEP_ONLY") != "" }

// armSweep runs once, on the first newClient, so the cluster-free guards never
// reach a cluster: it deletes leftovers of earlier runs (no restore, there is no
// snapshot yet) and snapshots the shared objects, in memory and on disk. No client,
// or an on-disk snapshot from another cluster, leaves the sweep disarmed; leftovers it cannot delete, or a snapshot it cannot
// load, take or save, are kept in sweepState.err, and newClient fails every cluster
// test on them.
var armSweep = sync.OnceFunc(func() {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return
	}
	cfg.Timeout = 30 * time.Second
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		fmt.Fprintf(sweepOut, "e2e sweep: disabled, no client: %v\n", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sweepTimeout)
	defer cancel()
	var snap *e2eSnapshot
	armed := true
	cluster, initErr := clusterIdentity(ctx, c, cfg.Host)
	switch {
	case initErr != nil:
		// No identity, so no snapshot can be checked against this cluster or taken.
	case sweepOnly():
		switch snap, err = loadSnapshot(); {
		case os.IsNotExist(err):
			fmt.Fprintf(sweepOut, "e2e sweep: no snapshot at %s, nothing to restore\n", snapshotPath())
		case err != nil:
			initErr = fmt.Errorf("load snapshot %s (left in place): %w", snapshotPath(), err)
		case snap.Cluster != cluster:
			initErr = fmt.Errorf("snapshot %s (left in place) is from cluster %q, not this one (%q); switch to that kubeconfig to restore it", snapshotPath(), snap.Cluster, cluster)
			// Another cluster's snapshot: restore nothing and delete nothing here.
			snap, armed = nil, false
		}
	default:
		if _, err := os.Stat(snapshotPath()); err == nil {
			fmt.Fprintf(sweepOut, "e2e sweep: replacing the unrestored snapshot %s an earlier run left; run `make test-e2e-sweep` after a crash to restore it\n", snapshotPath())
		}
		// Leftovers would skew this run's admission and capacity checks, and a
		// snapshot taken among them would hold them as the baseline.
		if err := deleteLabelled(ctx, c); err != nil {
			initErr = fmt.Errorf("delete leftovers of earlier runs: %w", err)
		} else if snap, err = takeSnapshot(ctx, c, cluster, client.ObjectKey{Name: balloonsDaemonSet(), Namespace: balloonsNS()}); err != nil {
			initErr = fmt.Errorf("take snapshot: %w", err)
		} else if err := saveSnapshot(snap); err != nil {
			initErr = fmt.Errorf("save snapshot %s: %w", snapshotPath(), err)
		}
	}
	sweepState.mu.Lock()
	sweepState.armed, sweepState.c, sweepState.snap, sweepState.err = armed, c, snap, initErr
	sweepState.mu.Unlock()
	fmt.Fprintf(sweepOut, "e2e: run label %s=%s\n", runLabel, runID)
})

func saveSnapshot(s *e2eSnapshot) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(snapshotPath(), b, 0o600)
}

func loadSnapshot() (*e2eSnapshot, error) {
	b, err := os.ReadFile(snapshotPath())
	if err != nil {
		return nil, err
	}
	s := &e2eSnapshot{}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	return s, nil
}

// e2eSweep restores the snapshotted shared objects, then deletes everything
// labelled runLabel, within budget. Idempotent; a no-op until armSweep ran. The
// on-disk snapshot is removed once restored, unless keepSnapshot: a sweep on a
// signal or the -timeout deadline runs while tests may still change the cluster, so
// `make test-e2e-sweep` restores again once the process is gone.
func e2eSweep(budget time.Duration, keepSnapshot bool) error {
	sweepState.mu.Lock()
	defer sweepState.mu.Unlock()
	if !sweepState.armed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	s := sweepState.snap
	restoreErr, deleteErr := sweepCluster(ctx, sweepState.c, s)
	errs := []error{restoreErr, deleteErr}
	switch {
	case s == nil || restoreErr != nil:
	case keepSnapshot:
		fmt.Fprintf(sweepOut, "e2e sweep: snapshot kept at %s; run `make test-e2e-sweep` once this process has exited\n", snapshotPath())
	default:
		if err := os.Remove(snapshotPath()); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// TestE2ESweepOnly is `make test-e2e-sweep`: restore what a crashed run left
// changed and delete every labelled object, running no test.
func TestE2ESweepOnly(t *testing.T) {
	if !sweepOnly() {
		skipNotSelected(t, "set E2E_SWEEP_ONLY=1 (make test-e2e-sweep) to restore the shared objects and delete every %s object", runLabel)
	}
	// Recovery was asked for: no kubeconfig, or a sweep that never armed, is a failure, not a skip.
	if _, err := ctrl.GetConfig(); err != nil {
		t.Fatalf("no kubeconfig, nothing restored or deleted: %v", err)
	}
	newClient(t)
	if !sweepArmed() {
		t.Fatal("sweep not armed (no client), nothing restored or deleted")
	}
	if err := e2eSweep(sweepTimeout, false); err != nil {
		t.Errorf("sweep: %v", err)
	}
}

// -- guard ---------------------------------------------------------------------

// createCallRE matches a Create call on any client or typed clientset.
var createCallRE = regexp.MustCompile(`\.Create\(`)

// TestEveryCreateIsLabelled fails when a Create call bypasses e2eCreate, so every
// object the suite makes carries the run label the sweeper deletes by.
func TestEveryCreateIsLabelled(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob the e2e package: %v", err)
	}
	var sawHelperFile bool
	var bare []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !createCallRE.MatchString(line) || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			// The one allowed call: e2eCreate's own body.
			if f == sweepFile && strings.Contains(line, "e2eLabel(obj)") {
				sawHelperFile = true
				continue
			}
			bare = append(bare, fmt.Sprintf("%s:%d", f, i+1))
		}
	}
	requireSourceScan(t, files, sawHelperFile, sweepFile)
	if len(bare) > 0 {
		t.Fatalf("%d Create call(s) bypass e2eCreate, so the sweeper cannot find what they make; call e2eCreate (or createEventuallyGone):\n\t%s",
			len(bare), strings.Join(bare, "\n\t"))
	}
}
