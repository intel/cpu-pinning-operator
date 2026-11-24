// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/readiness"
)

// ReadinessCALabel marks the CA ConfigMaps this reconciler owns, so it lists and
// prunes only those.
const ReadinessCALabel = "cpu-pinning-operator.intel.com/readiness-ca"

// DefaultReadinessCAResync is how often ca.crt is re-read and copies repaired; the
// mounted Secret changes with no API event, so this carries CA rotation.
const DefaultReadinessCAResync = time.Minute

// ReadinessCAReconciler copies the serving cert's ca.crt into a ConfigMap in every
// namespace the pod webhook targets, for the wait-for-balloons init container. Each
// copy is owned by the BalloonsClusterConfig singleton, so uninstall GCs them.
type ReadinessCAReconciler struct {
	// Client reads Namespaces, the webhook configuration and the singleton from
	// the cache, and writes ConfigMaps.
	Client client.Client
	// Reader lists ConfigMaps uncached; a cached list would watch every ConfigMap.
	Reader        client.Reader
	CAFile        string
	ConfigMapName string
	Resync        time.Duration
	Log           logr.Logger
}

//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;delete

func (r *ReadinessCAReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	requeue := ctrl.Result{RequeueAfter: r.Resync}
	if r.Resync <= 0 {
		requeue.RequeueAfter = DefaultReadinessCAResync
	}
	ca, err := os.ReadFile(r.CAFile) // #nosec G304 -- the operator's own cert mount
	if err != nil || len(ca) == 0 {
		// Existing copies stay as they are; init containers keep waiting on them.
		r.Log.Error(err, "cannot read the serving cert's ca.crt; readiness CA ConfigMaps not synced", "path", r.CAFile)
		return requeue, nil
	}
	// Without the owner, GC could never reap a copy, so none is written yet.
	owner, err := clusterConfigOwnerRef(ctx, r.Client)
	if err != nil || owner == nil {
		return requeue, err
	}
	want, err := r.targetNamespaces(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	var have corev1.ConfigMapList
	if err := r.Reader.List(ctx, &have, client.MatchingLabels{ReadinessCALabel: "true"}); err != nil {
		return ctrl.Result{}, err
	}
	existing := map[string]*corev1.ConfigMap{}
	for i := range have.Items {
		if cm := &have.Items[i]; cm.Name == r.ConfigMapName {
			existing[cm.Namespace] = cm
		}
	}

	var errs []error
	for ns := range want {
		if err := r.ensure(ctx, ns, existing[ns], ca, owner); err != nil {
			errs = append(errs, fmt.Errorf("namespace %s: %w", ns, err))
		}
	}
	for ns, cm := range existing {
		if !want[ns] {
			if err := r.Client.Delete(ctx, cm); client.IgnoreNotFound(err) != nil {
				errs = append(errs, fmt.Errorf("pruning %s/%s: %w", ns, cm.Name, err))
			}
		}
	}
	if len(errs) > 0 {
		return ctrl.Result{}, stderrors.Join(errs...)
	}
	return requeue, nil
}

// targetNamespaces resolves the pod webhook's namespaceSelector, the same gate
// that decides which pods get the init container at all.
func (r *ReadinessCAReconciler) targetNamespaces(ctx context.Context) (map[string]bool, error) {
	cfg := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: webhookConfigName()}, cfg); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	var wh *admissionregistrationv1.MutatingWebhook
	for i := range cfg.Webhooks {
		if cfg.Webhooks[i].Name == webhookName {
			wh = &cfg.Webhooks[i]
		}
	}
	if wh == nil {
		return nil, nil
	}
	sel := labels.Everything() // a nil namespaceSelector matches every namespace
	if wh.NamespaceSelector != nil {
		var err error
		if sel, err = metav1.LabelSelectorAsSelector(wh.NamespaceSelector); err != nil {
			return nil, err
		}
	}
	var nsList corev1.NamespaceList
	if err := r.Client.List(ctx, &nsList, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(nsList.Items))
	for i := range nsList.Items {
		if nsList.Items[i].DeletionTimestamp == nil {
			out[nsList.Items[i].Name] = true
		}
	}
	return out, nil
}

// ensure writes ca into ns, overwriting a same-named ConfigMap the reconciler did
// not create: the name is the operator's, and the init container trusts it.
func (r *ReadinessCAReconciler) ensure(ctx context.Context, ns string, cm *corev1.ConfigMap, ca []byte, owner *metav1.OwnerReference) error {
	if cm == nil {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: r.ConfigMapName, Namespace: ns}}
		r.apply(cm, ca, owner)
		err := r.Client.Create(ctx, cm)
		if !errors.IsAlreadyExists(err) {
			return err
		}
		cm = &corev1.ConfigMap{}
		if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: r.ConfigMapName}, cm); err != nil {
			return err
		}
	} else if bytes.Equal([]byte(cm.Data[readiness.CAKey]), ca) && len(cm.Data) == 1 && hasOwner(cm, owner) {
		return nil
	}
	r.apply(cm, ca, owner)
	return r.Client.Update(ctx, cm)
}

func (r *ReadinessCAReconciler) apply(cm *corev1.ConfigMap, ca []byte, owner *metav1.OwnerReference) {
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	cm.Labels[ReadinessCALabel] = "true"
	if !hasOwner(cm, owner) {
		cm.OwnerReferences = append(cm.OwnerReferences, *owner)
	}
	cm.Data = map[string]string{readiness.CAKey: string(ca)}
	cm.BinaryData = nil
}

func hasOwner(obj metav1.Object, owner *metav1.OwnerReference) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == owner.UID {
			return true
		}
	}
	return false
}

// SetupWithManager reconciles on webhook-selector, Namespace and singleton
// changes; the periodic requeue covers ca.crt rotation and tampering.
func (r *ReadinessCAReconciler) SetupWithManager(mgr ctrl.Manager) error {
	enqueue := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "readiness-ca"}}}
	})
	ownWebhook := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetName() == webhookConfigName() })
	return ctrl.NewControllerManagedBy(mgr).
		Named("readiness-ca").
		Watches(&admissionregistrationv1.MutatingWebhookConfiguration{}, enqueue, builder.WithPredicates(ownWebhook)).
		Watches(&corev1.Namespace{}, enqueue).
		Watches(&cpopv1alpha1.BalloonsClusterConfig{}, enqueue).
		Complete(r)
}
