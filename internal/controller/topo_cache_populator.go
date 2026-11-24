// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// TopoCachePopulator fills the topology Cache the pod webhook's live capacity check
// reads, on every replica. Only the leader probes nodes and stamps the
// topology-cache annotation; without this a follower's cache stays empty and its
// admission falls back to the coarser static check. It only writes the
// mutex-guarded Cache, never an API object.
type TopoCachePopulator struct {
	client.Client
	Log       logr.Logger
	TopoCache *topology.Cache
}

// NeedLeaderElection returns false so the populator runs on every replica.
func (p *TopoCachePopulator) NeedLeaderElection() bool { return false }

// Reconcile decodes the Node's topology-cache annotation into the Cache, dropping
// the entry when the Node or annotation is gone or malformed so a follower never
// reads stale topology.
func (p *TopoCachePopulator) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if p.TopoCache == nil {
		return ctrl.Result{}, nil
	}
	node := &corev1.Node{}
	if err := p.Get(ctx, req.NamespacedName, node); err != nil {
		if errors.IsNotFound(err) {
			// Node deleted: forget it.
			p.TopoCache.Delete(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	payload, ok := node.Annotations[AnnotationTopologyCache]
	if !ok || payload == "" {
		// Not probed yet, or excluded from the pool.
		p.TopoCache.Delete(node.Name)
		return ctrl.Result{}, nil
	}
	cpus, mem, err := topology.DecodeNodeCache(payload)
	if err != nil {
		// Drop it rather than keep a possibly stale entry; the leader re-stamps it.
		p.Log.Error(err, "failed to decode topology-cache annotation; follower will fall back to the static check", "node", node.Name)
		p.TopoCache.Delete(node.Name)
		return ctrl.Result{}, nil
	}
	p.TopoCache.Set(node.Name, cpus)
	p.TopoCache.SetMemory(node.Name, mem)
	return ctrl.Result{}, nil
}

// SetupWithManager registers a non-leader-gated Node controller.
func (p *TopoCachePopulator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("topocachepopulator").
		For(&corev1.Node{}).
		WithOptions(controller.Options{NeedLeaderElection: ptr.To(p.NeedLeaderElection())}).
		Complete(p)
}
