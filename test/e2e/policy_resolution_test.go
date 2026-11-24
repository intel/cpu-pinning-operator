// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Renaming a test here means updating the -run filter in the pre-commit hook and in
// the Makefile's test-e2e-guards.

package e2e

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// bppResource is the resource name the apiserver reports in a Forbidden for the CRD.
var bppResource = schema.GroupResource{Group: bppGVK.Group, Resource: "balloonpinningpolicies"}

// bppScheme registers the unstructured BalloonPinningPolicy and its list kind for the fake client.
func bppScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(bppGVK, &unstructured.Unstructured{})
	listGVK := bppGVK
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

// forbidListClient serves the given policies but denies a cluster-wide List, modelling
// RBAC scoped to one named resource.
func forbidListClient(t *testing.T, policies ...*unstructured.Unstructured) client.Client {
	t.Helper()
	objs := make([]client.Object, 0, len(policies))
	for _, p := range policies {
		objs = append(objs, p)
	}
	return fake.NewClientBuilder().
		WithScheme(bppScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return apierrors.NewForbidden(bppResource, "", errors.New("list is not permitted at cluster scope"))
			},
		}).
		Build()
}

func bppNamed(name, namespace string) *unstructured.Unstructured {
	p := newUnstructured(bppGVK)
	p.SetName(name)
	p.Object["spec"] = map[string]interface{}{"namespaces": []interface{}{namespace}}
	return p
}

// TestInstalledPoliciesKeepsNamedPolicyWhenListForbidden proves a Forbidden list does
// not discard the policy the Get already returned.
func TestInstalledPoliciesKeepsNamedPolicyWhenListForbidden(t *testing.T) {
	t.Setenv("E2E_POLICY_NAME", "scoped-policy")
	c := forbidListClient(t, bppNamed("scoped-policy", "pinned-ns"))

	got, err := installedPolicies(context.Background(), c)
	if err != nil {
		t.Fatalf("installedPolicies returned %v; a denied list must not discard the named policy", err)
	}
	if len(got) != 1 || got[0].GetName() != "scoped-policy" {
		t.Fatalf("got %d policies %v, want just scoped-policy", len(got), policyNames(got))
	}
}

// TestInstalledPoliciesFailsWhenNothingResolves proves a missing policy plus a denied
// list fails loudly, not with errNoPolicy (which callers skip on).
func TestInstalledPoliciesFailsWhenNothingResolves(t *testing.T) {
	t.Setenv("E2E_POLICY_NAME", "absent-policy")
	c := forbidListClient(t) // no policies at all

	_, err := installedPolicies(context.Background(), c)
	if err == nil {
		t.Fatal("installedPolicies succeeded with no named policy and a denied list; nothing resolved, so it must fail")
	}
	if errors.Is(err, errNoPolicy) {
		t.Fatalf("got %v (errNoPolicy), which callers skip on; a denied list is not a missing capability", err)
	}
}

// TestInstalledPoliciesFindsRenamedPolicyByList proves the cluster-wide list finds a
// policy installed under another name.
func TestInstalledPoliciesFindsRenamedPolicyByList(t *testing.T) {
	t.Setenv("E2E_POLICY_NAME", "absent-policy")
	c := fake.NewClientBuilder().
		WithScheme(bppScheme(t)).
		WithObjects(bppNamed("renamed-policy", "pinned-ns")).
		Build()

	got, err := installedPolicies(context.Background(), c)
	if err != nil {
		t.Fatalf("installedPolicies returned %v; the list should have found the renamed policy", err)
	}
	if len(got) != 1 || got[0].GetName() != "renamed-policy" {
		t.Fatalf("got %d policies %v, want just renamed-policy", len(got), policyNames(got))
	}
}

// TestSelectorTargetsNamespace pins the namespace-targeting premise: an EMPTY selector
// matches every namespace (so presence, not len > 0, decides), and anything
// unresolvable reports not-determined so the caller skips.
func TestSelectorTargetsNamespace(t *testing.T) {
	const probeNS = "probe-ns"
	c := fake.NewClientBuilder().
		WithScheme(nsScheme(t)).
		WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   probeNS,
			Labels: map[string]string{"tier": "tenant"},
		}}).
		Build()

	cases := []struct {
		name string
		// spec is the policy's whole spec, so a case shows exactly what it sets.
		spec           map[string]interface{}
		lookup         string // namespace to resolve; empty means probeNS
		wantTargeted   bool
		wantDetermined bool
	}{
		{
			name:           "absent selector targets nothing, determinably",
			spec:           map[string]interface{}{"namespaces": []interface{}{"some-other-ns"}},
			wantDetermined: true,
		},
		{
			name:           "empty selector targets every namespace",
			spec:           map[string]interface{}{"namespaceSelector": map[string]interface{}{}},
			wantTargeted:   true,
			wantDetermined: true,
		},
		{
			name: "matchLabels matching the namespace targets it",
			spec: map[string]interface{}{"namespaceSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"tier": "tenant"},
			}},
			wantTargeted:   true,
			wantDetermined: true,
		},
		{
			name: "matchLabels not matching the namespace does not target it",
			spec: map[string]interface{}{"namespaceSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"tier": "system"},
			}},
			wantDetermined: true,
		},
		{
			name: "matchExpressions with an unknown operator is not determinable",
			spec: map[string]interface{}{"namespaceSelector": map[string]interface{}{
				"matchExpressions": []interface{}{map[string]interface{}{
					"key": "tier", "operator": "Bogus", "values": []interface{}{"tenant"},
				}},
			}},
		},
		{
			name: "a selector that is not a map is not determinable",
			spec: map[string]interface{}{"namespaceSelector": "everything"},
		},
		{
			name: "a namespace whose labels cannot be read is not determinable",
			spec: map[string]interface{}{"namespaceSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"tier": "tenant"},
			}},
			lookup: "no-such-ns",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := newUnstructured(bppGVK)
			policy.SetName("selector-policy")
			policy.Object["spec"] = tc.spec
			lookup := tc.lookup
			if lookup == "" {
				lookup = probeNS
			}

			targeted, determined := selectorTargetsNamespace(c, policy, lookup)
			if targeted != tc.wantTargeted || determined != tc.wantDetermined {
				t.Errorf("selectorTargetsNamespace(%q) = (targeted=%v, determined=%v), want (%v, %v)",
					lookup, targeted, determined, tc.wantTargeted, tc.wantDetermined)
			}
		})
	}
}

// nsScheme serves Namespaces to the fake client.
func nsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("register corev1: %v", err)
	}
	return s
}

func policyNames(policies []*unstructured.Unstructured) []string {
	names := make([]string, 0, len(policies))
	for _, p := range policies {
		names = append(names, p.GetName())
	}
	return names
}
