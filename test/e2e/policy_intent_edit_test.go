// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Renaming a test here means updating the -run filter in the pre-commit hook and in
// the Makefile's test-e2e-guards.

package e2e

import (
	"context"
	"errors"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestPolicyIntentEditRetriesConflicts proves the intent restore re-reads after a
// Conflict, keeps a field another writer changed meanwhile, restores both a present
// and an absent original intent, and does not retry any other rejection.
func TestPolicyIntentEditRetriesConflicts(t *testing.T) {
	key := client.ObjectKey{Name: "default"}
	orig := map[string]interface{}{"span": map[string]interface{}{"annotation": "example.com/tp", "default": int64(2)}}
	edited := map[string]interface{}{"span": map[string]interface{}{"argRegex": vllmSpanArgRegex, "default": int64(1)}}

	for _, tc := range []struct {
		name      string
		had       bool
		rejection error
		retried   bool
	}{
		{"conflict, original intent present", true, apierrors.NewConflict(bppResource, "default", errors.New("the object has been modified")), true},
		{"conflict, original intent absent", false, apierrors.NewConflict(bppResource, "default", errors.New("the object has been modified")), true},
		{"forbidden is not retried", true, apierrors.NewForbidden(bppResource, "default", errors.New("guardActivePods")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUnstructured(bppGVK)
			p.SetName("default")
			p.Object["spec"] = map[string]interface{}{"namespaces": []interface{}{"inference"}, "intent": edited}
			updates := 0
			c := fake.NewClientBuilder().
				WithScheme(bppScheme(t)).
				WithObjects(p).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						updates++
						if updates > 1 {
							return cl.Update(ctx, obj, opts...)
						}
						// Another writer lands first: the stored object moves on.
						cur := newUnstructured(bppGVK)
						if err := cl.Get(ctx, key, cur); err != nil {
							return err
						}
						_ = unstructured.SetNestedSlice(cur.Object, []interface{}{"inference", "staging"}, "spec", "namespaces")
						if err := cl.Update(ctx, cur); err != nil {
							return err
						}
						return tc.rejection
					},
				}).
				Build()

			err := editPolicyOnConflict(context.Background(), c, key, restoreIntent(orig, tc.had))
			got := newUnstructured(bppGVK)
			if gerr := c.Get(context.Background(), key, got); gerr != nil {
				t.Fatal(gerr)
			}
			intent, present, _ := unstructured.NestedMap(got.Object, "spec", "intent")
			ns, _, _ := unstructured.NestedStringSlice(got.Object, "spec", "namespaces")

			if !tc.retried {
				if !apierrors.IsForbidden(err) || updates != 1 {
					t.Fatalf("err = %v after %d updates, want the Forbidden back after 1", err, updates)
				}
				if !reflect.DeepEqual(intent, edited) {
					t.Errorf("spec.intent = %v, want it untouched %v", intent, edited)
				}
				return
			}
			if err != nil || updates != 2 {
				t.Fatalf("err = %v after %d updates, want nil after 2 (one Conflict, one retry)", err, updates)
			}
			if present != tc.had || (tc.had && !reflect.DeepEqual(intent, orig)) {
				t.Errorf("spec.intent = %v (present=%v), want %v (present=%v)", intent, present, orig, tc.had)
			}
			if !reflect.DeepEqual(ns, []string{"inference", "staging"}) {
				t.Errorf("spec.namespaces = %v, want the other writer's [inference staging] kept", ns)
			}
		})
	}
}
