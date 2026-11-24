// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
)

// PolicyIndexPopulator fills the match Index the pod webhook reads, on every
// replica. The webhook serves on all replicas but the policy reconcilers are
// leader-gated; without this a follower's index stays empty and, with
// failurePolicy Ignore, admits matched pods unmutated. It only writes the
// mutex-guarded Index, never an API object.
type PolicyIndexPopulator struct {
	client.Client
	Log         logr.Logger
	PolicyIndex *match.Index
}

// NeedLeaderElection returns false so the populator runs on every replica.
func (p *PolicyIndexPopulator) NeedLeaderElection() bool { return false }

// Reconcile repopulates the Index from all policies and the singleton.
func (p *PolicyIndexPopulator) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if err := p.populate(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// populate compiles the policy rules and the singleton's config into the Index;
// an absent singleton resets the config to defaults.
func (p *PolicyIndexPopulator) populate(ctx context.Context) error {
	var list cpopv1alpha1.BalloonPinningPolicyList
	if err := p.List(ctx, &list); err != nil {
		return err
	}
	rules, _, err := compileRules(ctx, p.Client, list.Items, p.Log)
	if err != nil {
		return err
	}
	p.PolicyIndex.Update(rules)

	cfg := &cpopv1alpha1.BalloonsClusterConfig{}
	err = p.Get(ctx, client.ObjectKey{Name: cpopv1alpha1.ClusterConfigName}, cfg)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}

		p.PolicyIndex.SetClusterConfig(match.ClusterConfig{})
		return nil
	}
	p.PolicyIndex.SetClusterConfig(compileClusterConfig(cfg, p.Log))
	return nil
}

// SetupWithManager registers a non-leader-gated controller watching policies, the
// singleton and Namespaces (namespaceSelector inputs), all funnelled to one key.
func (p *PolicyIndexPopulator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("policyindexpopulator").
		Watches(&cpopv1alpha1.BalloonPinningPolicy{},
			handler.EnqueueRequestsFromMapFunc(p.enqueue)).
		Watches(&cpopv1alpha1.BalloonsClusterConfig{},
			handler.EnqueueRequestsFromMapFunc(p.enqueue)).
		Watches(&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(p.enqueue)).
		WithOptions(controller.Options{NeedLeaderElection: ptr.To(p.NeedLeaderElection())}).
		Complete(p)
}

// enqueue maps every event to one key: population always reads the full state.
func (p *PolicyIndexPopulator) enqueue(context.Context, client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "populate"}}}
}
