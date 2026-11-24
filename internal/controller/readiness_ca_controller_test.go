// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/readiness"
)

const testCAConfigMap = "op-readiness-ca"

func caNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name, Labels: map[string]string{"kubernetes.io/metadata.name": name},
	}}
}

// readinessCAReconciler seeds a webhook targeting ns-a and ns-b and a ca.crt file.
func readinessCAReconciler(t *testing.T, ca string, objs ...runtime.Object) (*ReadinessCAReconciler, string) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = cpopv1alpha1.AddToScheme(scheme)
	_ = admissionregistrationv1.AddToScheme(scheme)
	wh := webhookConfig()
	wh.Webhooks[0].NamespaceSelector = namespaceNameSelector([]string{"ns-a", "ns-b"})
	objs = append(objs, wh, caNamespace("ns-a"), caNamespace("ns-b"), caNamespace("ns-c"))
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	caFile := filepath.Join(t.TempDir(), readiness.CAKey)
	if ca != "" {
		if err := os.WriteFile(caFile, []byte(ca), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &ReadinessCAReconciler{
		Client: c, Reader: c, CAFile: caFile, ConfigMapName: testCAConfigMap, Log: ctrl.Log.WithName("test"),
	}, caFile
}

func clusterConfig() *cpopv1alpha1.BalloonsClusterConfig {
	return &cpopv1alpha1.BalloonsClusterConfig{ObjectMeta: metav1.ObjectMeta{Name: cpopv1alpha1.ClusterConfigName, UID: "bcc-uid"}}
}

func caConfigMap(t *testing.T, c client.Client, ns string) (*corev1.ConfigMap, bool) {
	t.Helper()
	cm := &corev1.ConfigMap{}
	err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: testCAConfigMap}, cm)
	if errors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return cm, true
}

// TestReadinessCASyncsTargetedNamespaces: every targeted namespace gets the CA,
// owned by the singleton for GC; a same-named ConfigMap a tenant planted is
// overwritten; a copy in a namespace no longer targeted is pruned.
func TestReadinessCASyncsTargetedNamespaces(t *testing.T) {
	planted := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testCAConfigMap, Namespace: "ns-b"},
		Data:       map[string]string{readiness.CAKey: "attacker CA", "extra": "x"},
	}
	stale := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: testCAConfigMap, Namespace: "ns-c", Labels: map[string]string{ReadinessCALabel: "true"},
	}}
	r, _ := readinessCAReconciler(t, "CA-1", clusterConfig(), planted, stale)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, ns := range []string{"ns-a", "ns-b"} {
		cm, ok := caConfigMap(t, r.Client, ns)
		if !ok {
			t.Fatalf("no CA ConfigMap in targeted namespace %s", ns)
		}
		if len(cm.Data) != 1 || cm.Data[readiness.CAKey] != "CA-1" {
			t.Errorf("%s data = %v, want only ca.crt=CA-1", ns, cm.Data)
		}
		if cm.Labels[ReadinessCALabel] != "true" || len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != "bcc-uid" {
			t.Errorf("%s labels=%v owners=%v, want the readiness label and the singleton owner", ns, cm.Labels, cm.OwnerReferences)
		}
	}
	if _, ok := caConfigMap(t, r.Client, "ns-c"); ok {
		t.Error("the copy in untargeted ns-c was not pruned")
	}
}

// TestReadinessCAFollowsRotation: a changed ca.crt reaches every copy on the next reconcile.
func TestReadinessCAFollowsRotation(t *testing.T) {
	r, caFile := readinessCAReconciler(t, "CA-1", clusterConfig())
	res, err := r.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter <= 0 {
		t.Error("no periodic requeue: a rotated ca.crt would never be picked up")
	}
	if err := os.WriteFile(caFile, []byte("CA-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	if cm, _ := caConfigMap(t, r.Client, "ns-a"); cm == nil || cm.Data[readiness.CAKey] != "CA-2" {
		t.Errorf("ns-a after rotation = %v, want CA-2", cm)
	}
}

// TestReadinessCAWritesNothingWithoutOwnerOrCA: no singleton means GC could not
// reap a copy, and no ca.crt means nothing to copy — neither writes.
func TestReadinessCAWritesNothingWithoutOwnerOrCA(t *testing.T) {
	for name, r := range map[string]*ReadinessCAReconciler{
		"no singleton": func() *ReadinessCAReconciler { r, _ := readinessCAReconciler(t, "CA-1"); return r }(),
		"no ca.crt":    func() *ReadinessCAReconciler { r, _ := readinessCAReconciler(t, "", clusterConfig()); return r }(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if _, ok := caConfigMap(t, r.Client, "ns-a"); ok {
				t.Error("a CA ConfigMap was written")
			}
		})
	}
}
