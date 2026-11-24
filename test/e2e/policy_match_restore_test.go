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
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// globbedPolicy is the default policy as a container-selection case leaves it: match
// re-pointed at a glob, status already observing the current generation.
func globbedPolicy() *unstructured.Unstructured {
	p := newUnstructured(bppGVK)
	p.SetName("default")
	p.Object["spec"] = map[string]interface{}{
		"match": map[string]interface{}{"byContainerName": []interface{}{"vllm-*"}},
	}
	p.Object["status"] = map[string]interface{}{"observedGeneration": int64(1 << 30)}
	return p
}

// TestPolicyMatchRestoreRetriesRejectedWrites proves the restore retries a Conflict or
// guard-denied UPDATE until the baseline spec.match is back.
func TestPolicyMatchRestoreRetriesRejectedWrites(t *testing.T) {
	defer func(p time.Duration) { e2eSettlePoll = p }(e2eSettlePoll)
	e2eSettlePoll = time.Millisecond

	baselines := []struct {
		name    string
		want    map[string]interface{}
		present bool
	}{
		{"match", map[string]interface{}{"byContainerName": []interface{}{"vllm"}}, true},
		// No baseline spec.match: the restore removes the field.
		{"absent", nil, false},
	}
	for _, base := range baselines {
		for _, rejection := range []error{
			apierrors.NewConflict(bppResource, "default", errors.New("the object has been modified")),
			apierrors.NewForbidden(bppResource, "default", errors.New("guardActivePods: would stop matching 1 pinned pod")),
		} {
			t.Run(base.name+"/"+string(apierrors.ReasonForError(rejection)), func(t *testing.T) {
				updates := 0
				c := fake.NewClientBuilder().
					WithScheme(bppScheme(t)).
					WithObjects(globbedPolicy()).
					WithInterceptorFuncs(interceptor.Funcs{
						Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
							updates++
							if updates == 1 {
								return rejection
							}
							return cl.Update(ctx, obj, opts...)
						},
					}).
					Build()

				key := client.ObjectKey{Name: "default"}
				// Short: a restore that stops retrying fails here in seconds, not after a minute.
				if err := restorePolicyMatch(c, key, base.want, base.present, 2*time.Second); err != nil {
					t.Fatalf("restorePolicyMatch: %v", err)
				}
				if updates != 2 {
					t.Errorf("updates = %d, want 2 (one rejected, one retried)", updates)
				}
				got := newUnstructured(bppGVK)
				if err := c.Get(context.Background(), key, got); err != nil {
					t.Fatal(err)
				}
				m, present, _ := unstructured.NestedMap(got.Object, "spec", "match")
				if present != base.present || (base.present && !reflect.DeepEqual(m, base.want)) {
					t.Errorf("spec.match = %v (present=%v), want %v (present=%v)", m, present, base.want, base.present)
				}
			})
		}
	}
}

// TestPolicyMatchRestoreReportsWhyItFailed proves a restore that never lands returns an
// error naming the live match and the write failure.
func TestPolicyMatchRestoreReportsWhyItFailed(t *testing.T) {
	defer func(p time.Duration) { e2eSettlePoll = p }(e2eSettlePoll)
	e2eSettlePoll = time.Millisecond

	c := fake.NewClientBuilder().
		WithScheme(bppScheme(t)).
		WithObjects(globbedPolicy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				return apierrors.NewForbidden(bppResource, "default", errors.New("denied by guard"))
			},
		}).
		Build()

	err := restorePolicyMatch(c, client.ObjectKey{Name: "default"},
		map[string]interface{}{"byContainerName": []interface{}{"vllm"}}, true, 20*time.Millisecond)
	if err == nil {
		t.Fatal("restorePolicyMatch returned nil for a write that is always denied")
	}
	for _, s := range []string{"vllm-*", "denied by guard"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not name %q", err, s)
		}
	}
}
