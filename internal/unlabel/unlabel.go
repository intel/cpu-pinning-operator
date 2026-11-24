// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package unlabel strips operator-managed labels from every node in the chart's
// post-delete hook, since GC cannot remove label keys from Nodes. It runs after
// the manager is gone so the NodeReconciler cannot re-label a node.
package unlabel

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/intel/cpu-pinning-operator/internal/controller"
)

// StripManagedLabels removes controller.ManagedNodeLabels and the topology-cache
// annotation from every node, idempotently, and returns how many nodes it patched.
func StripManagedLabels(ctx context.Context, c client.Client, out io.Writer) (int, error) {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return 0, fmt.Errorf("listing nodes: %w", err)
	}

	patched := 0
	for i := range nodes.Items {
		node := &nodes.Items[i]
		present := false
		for _, l := range controller.ManagedNodeLabels {
			if _, ok := node.Labels[l]; ok {
				present = true
				break
			}
		}
		_, hasTopo := node.Annotations[controller.AnnotationTopologyCache]
		if !present && !hasTopo {
			continue
		}
		orig := node.DeepCopy()
		for _, l := range controller.ManagedNodeLabels {
			delete(node.Labels, l)
		}
		delete(node.Annotations, controller.AnnotationTopologyCache)
		if err := c.Patch(ctx, node, client.MergeFrom(orig)); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return patched, fmt.Errorf("stripping labels from node %q: %w", node.Name, err)
		}

		_, _ = fmt.Fprintf(out, "stripped operator-managed labels from node %q\n", node.Name)
		patched++
	}
	return patched, nil
}
