// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"fmt"
	"strings"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LeaderDiscovery resolves the leader-election lease holder to a pod IP for forwarding
// reserve requests. holderIdentity is "<hostname>_<uuid>"; the pod name is the part
// before the first "_". Needs leases get and pods get RBAC; without them a commit cannot
// reach the leader (warn/off reserve locally, enforce refuses).
type LeaderDiscovery struct {
	// Reader can be cached: a stale read forwards to a departed leader, which fails and
	// is retried like any other transport error.
	Reader      client.Reader
	Namespace   string // lease + pod namespace (the operator's own namespace)
	LeaseName   string // the leader-election lease name (leaderElectionID)
	SelfPodName string // this replica's pod name (os.Hostname); leader short-circuit
}

// Leader returns the leader's pod IP, or isSelf=true for this replica. Callers treat any
// error as "leader unreachable": after retries, warn/off reserve locally and enforce refuses.
func (d *LeaderDiscovery) Leader(ctx context.Context) (ip string, isSelf bool, err error) {
	lease := &coordinationv1.Lease{}
	if err := d.Reader.Get(ctx, client.ObjectKey{Namespace: d.Namespace, Name: d.LeaseName}, lease); err != nil {
		return "", false, fmt.Errorf("reading leader-election lease %s/%s: %w", d.Namespace, d.LeaseName, err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return "", false, fmt.Errorf("leader-election lease %s/%s has no holder yet", d.Namespace, d.LeaseName)
	}
	podName := holderPodName(*lease.Spec.HolderIdentity)
	if podName == "" {
		return "", false, fmt.Errorf("cannot parse pod name from holder identity %q", *lease.Spec.HolderIdentity)
	}
	if podName == d.SelfPodName {
		return "", true, nil
	}
	pod := &corev1.Pod{}
	if err := d.Reader.Get(ctx, client.ObjectKey{Namespace: d.Namespace, Name: podName}, pod); err != nil {
		return "", false, fmt.Errorf("reading leader pod %s/%s: %w", d.Namespace, podName, err)
	}
	if pod.Status.PodIP == "" {
		return "", false, fmt.Errorf("leader pod %s/%s has no IP yet", d.Namespace, podName)
	}
	return pod.Status.PodIP, false, nil
}

// holderPodName returns identity up to the first "_", or all of it when there is none.
func holderPodName(identity string) string {
	if identity == "" {
		return ""
	}
	if i := strings.Index(identity, "_"); i >= 0 {
		return identity[:i]
	}
	return identity
}
