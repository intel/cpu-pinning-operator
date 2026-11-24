// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package crdinstall installs the generated CRDs on a real envtest apiserver, which
// runs server-only checks such as the CEL rule-cost budget that unit tests never see.
package crdinstall

import (
	"context"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/test/ctrlplane"
)

// crdDir returns the chart's CRD directory, resolved from this file.
func crdDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller for CRD path")
	}

	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(root, "deploy", "cpu-pinning-operator", "crds")
}

// operatorScheme is core kinds plus the operator's own API group.
func operatorScheme() *k8sruntime.Scheme {
	sch := k8sruntime.NewScheme()
	utilruntime.Must(scheme.AddToScheme(sch))
	utilruntime.Must(v1alpha1.AddToScheme(sch))
	return sch
}

// startEnv boots an envtest apiserver with the generated CRDs installed.
func startEnv(t *testing.T) (client.Client, func()) {
	t.Helper()
	return ctrlplane.Start(t, &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir(t)},
		ErrorIfCRDPathMissing: true,
		Scheme:                operatorScheme(),
	})
}

// TestGeneratedCRDsInstall proves the CRDs install without busting the CEL cost budget.
func TestGeneratedCRDsInstall(t *testing.T) {
	_, stop := startEnv(t)
	stop()
}

// clusterConfig builds a singleton with reserved set on spec.balloon.
func clusterConfig(reserved *v1alpha1.ReservedSizingSpec) *v1alpha1.BalloonsClusterConfig {
	return &v1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: v1alpha1.BalloonsClusterConfigSpec{
			Balloon: v1alpha1.BalloonSpec{Reserved: reserved},
		},
	}
}

// overrideConfig builds a singleton with reserved set on a nodeConfigOverrides[].balloon.
func overrideConfig(reserved *v1alpha1.ReservedSizingSpec) *v1alpha1.BalloonsClusterConfig {
	return &v1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: v1alpha1.BalloonsClusterConfigSpec{
			NodeConfigOverrides: []v1alpha1.NodeConfigOverride{{
				NodeSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/arch": "amd64"},
				},
				Balloon: v1alpha1.BalloonSpec{Reserved: reserved},
			}},
		},
	}
}

func intp(i int) *int { return &i }

// TestReservedMutualExclusion proves CEL allows at most one reserved mode, at both locations.
func TestReservedMutualExclusion(t *testing.T) {
	c, stop := startEnv(t)
	defer stop()

	twoOfThree := &v1alpha1.ReservedSizingSpec{Cpus: intp(4), Percent: intp(10)}
	allThree := &v1alpha1.ReservedSizingSpec{
		Cpus:     intp(4),
		Percent:  intp(10),
		Measured: &v1alpha1.MeasuredSizingSpec{},
	}
	oneOnly := &v1alpha1.ReservedSizingSpec{Cpus: intp(4)}
	measuredOnly := &v1alpha1.ReservedSizingSpec{Measured: &v1alpha1.MeasuredSizingSpec{}}

	cases := []struct {
		name     string
		obj      *v1alpha1.BalloonsClusterConfig
		wantDeny bool
	}{
		{"clusterDefault/twoOfThree", clusterConfig(twoOfThree), true},
		{"clusterDefault/allThree", clusterConfig(allThree), true},
		{"clusterDefault/cpusOnly", clusterConfig(oneOnly), false},
		{"clusterDefault/measuredOnly", clusterConfig(measuredOnly), false},
		{"clusterDefault/none", clusterConfig(&v1alpha1.ReservedSizingSpec{}), false},
		{"override/twoOfThree", overrideConfig(twoOfThree), true},
		{"override/allThree", overrideConfig(allThree), true},
		{"override/cpusOnly", overrideConfig(oneOnly), false},
		{"override/measuredOnly", overrideConfig(measuredOnly), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			err := c.Create(ctx, tc.obj)

			if err == nil {
				defer func() { _ = c.Delete(context.Background(), tc.obj) }()
			}

			if tc.wantDeny {
				if err == nil {
					t.Fatalf("expected apiserver to REJECT (>1 of reserved.cpus/percent/measured), but it was admitted")
				}
				if !apierrors.IsInvalid(err) {
					t.Fatalf("expected an Invalid (CEL) rejection, got: %v", err)
				}
				if !strings.Contains(err.Error(), "at most one of reserved") {
					t.Fatalf("rejection did not carry the mutual-exclusion message: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected admission (<=1 reserved mode set), got rejection: %v", err)
			}
		})
	}
}

// TestNodeConfigOverridesMaxItems proves the apiserver enforces the maxItems cap that
// bounds the CEL cost multiplier.
func TestNodeConfigOverridesMaxItems(t *testing.T) {
	c, stop := startEnv(t)
	defer stop()

	const maxItems = 32
	overrides := make([]v1alpha1.NodeConfigOverride, 0, maxItems+1)
	for i := 0; i <= maxItems; i++ { // maxItems+1 entries -> over the limit
		overrides = append(overrides, v1alpha1.NodeConfigOverride{
			NodeSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"node-class": "c" + strconv.Itoa(i)},
			},
			Balloon: v1alpha1.BalloonSpec{Reserved: &v1alpha1.ReservedSizingSpec{Cpus: intp(4)}},
		})
	}
	obj := &v1alpha1.BalloonsClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       v1alpha1.BalloonsClusterConfigSpec{NodeConfigOverrides: overrides},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := c.Create(ctx, obj)
	if err == nil {
		_ = c.Delete(context.Background(), obj)
		t.Fatalf("expected apiserver to reject %d overrides (> maxItems=%d)", maxItems+1, maxItems)
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid (maxItems) rejection, got: %v", err)
	}
}
