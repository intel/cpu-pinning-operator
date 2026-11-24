// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/match"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
)

// WarmReservations rebuilds the reservation set from stamped pods whose pins the node's NRT
// CR does not yet reflect; reflected pods are skipped to avoid double counting. Each
// reservation expires one TTL after pod creation. Failed reads skip that pod or node and it
// never errors, so warm-up cannot wedge the lease.
func (m *PodMutator) WarmReservations(ctx context.Context) {
	if m.Reservations == nil {
		return
	}
	log := m.Log.WithName("warm-reserve")

	var pods corev1.PodList
	if err := m.Client.List(ctx, &pods, client.MatchingFields{controller.PodPinnedByField: PinnedByValue}); err != nil {
		log.Error(err, "listing pinned pods failed; serving reserves from an empty store")
		return
	}
	byNode := map[string][]*corev1.Pod{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		// Unscheduled pods have no node; terminating ones have released their cpuset.
		if pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		byNode[pod.Spec.NodeName] = append(byNode[pod.Spec.NodeName], pod)
	}

	clusterAllocation := m.PolicyIndex.CPUAllocation()
	if clusterAllocation == "" {
		clusterAllocation = defaultCPUAllocation
	}

	seeded, skipped := 0, 0
	for node, nodePods := range byNode {
		n, ok := m.warmNode(ctx, node, nodePods, clusterAllocation, log)
		if !ok {
			skipped++
		}
		seeded += n
		// Publish every examined node, so "derived nothing" differs from "never looked".
		m.Reservations.republishLive(node)
	}
	log.Info("warmed the reserve authority store from pinned pods not yet reflected in NodeResourceTopology",
		"reservations", seeded, "nodesWarmed", len(byNode)-skipped, "nodesSkipped", skipped)
}

// warmNode seeds one node's unreflected pods and returns the count. ok is false when the
// node is skipped: a missing or malformed CR or no per-NUMA free room. clusterAllocation
// applies only to a node with no mode of its own.
func (m *PodMutator) warmNode(ctx context.Context, node string, pods []*corev1.Pod, clusterAllocation string, log logr.Logger) (int, bool) {
	nrt := &unstructured.Unstructured{}
	nrt.SetGroupVersionKind(nrtpkg.GVK)
	if err := m.Client.Get(ctx, client.ObjectKey{Name: node}, nrt); err != nil {
		log.V(1).Info("warm-up skipping node: NodeResourceTopology unreadable", "node", node, "error", err.Error())
		return 0, false
	}
	// ok is false only for malformed "zones". A CR with no zones seeds every pinned pod: the
	// plugin-just-restarted case.
	zones, ok := nrtpkg.ContainerZoneNames(nrt)
	if !ok {
		log.V(1).Info("warm-up skipping node: malformed NodeResourceTopology, occupancy unknown", "node", node)
		return 0, false
	}
	reflected := make(map[string]bool, len(zones))
	for _, z := range zones {
		reflected[z] = true
	}

	// Free room from the same CR version as the zone set, falling back to static capacity
	// labels like the pre-scheduled reserve. A Node read failure only blocks that fallback.
	obj := &corev1.Node{}
	nodeErr := m.nodeReader().Get(ctx, client.ObjectKey{Name: node}, obj)
	if nodeErr != nil {
		// Without labels the sibling rule stands (density nodes read 0 free). Logged, not
		// skipped: skipping would hide these pods' demand and over-admit.
		log.V(1).Info("warm-up reading a node's free room without the density rule: node read failed",
			"node", node, "error", nodeErr.Error())
	}
	// One label read feeds both the unit and the density rule.
	cpuAllocation := m.nodeCPUAllocation(obj.Labels, clusterAllocation)
	free, ok := m.liveFreeFromNRT(node, nrt, cpuAllocation, m.densityNode(obj.Labels))
	if !ok {
		if nodeErr != nil {
			log.V(1).Info("warm-up skipping node: node read failed", "node", node, "error", nodeErr.Error())
			return 0, false
		}
		if free, ok = nodeStaticFree(obj.Labels, cpuAllocation, nil); !ok {
			log.V(1).Info("warm-up skipping node: no per-NUMA free room to place reservations on", "node", node)
			return 0, false
		}
	}

	seeded := 0
	for _, pod := range pods {
		balloon, container, podScoped, ok := pinnedBalloon(pod)
		if !ok {
			continue // no balloon annotation to read a span off
		}
		if podInNRT(pod, container, podScoped, reflected) {
			continue // already subtracted from raw free room
		}
		span, ok := match.SpanFromBalloonName(balloon)
		if !ok {
			log.V(1).Info("warm-up skipping pod: unrecognized balloon name",
				"pod", pod.Namespace+"/"+pod.Name, "balloon", balloon)
			continue
		}
		cores := podCPURequestCores(pod, container, podScoped)
		if cores == 0 {
			continue
		}
		memBytes := podMemoryRequestBytes(pod, container, podScoped)
		// Pick NUMAs net of existing reservations, or every seeded pod lands on the same NUMAs.
		reserved, _ := m.nodeReservations(node)
		numas := topSpanNUMAs(subtractReservations(free, reserved), span)
		if len(numas) == 0 {
			continue
		}
		expires := pod.CreationTimestamp.Add(m.Reservations.ttl)
		if !m.Reservations.seed(node, numas, perNUMACPUShare(cores, span), perNUMAMemShare(memBytes, span), expires) {
			continue // already past its TTL: the deposed authority had dropped it too
		}
		seeded++
		log.V(1).Info("warm-up seeded a reservation", "pod", pod.Namespace+"/"+pod.Name,
			"node", node, "numas", numas, "perNUMAShare", perNUMACPUShare(cores, span), "span", span)
	}
	return seeded, true
}

// podInNRT reports whether the CR has a zone for a container the stamp put in a balloon
// (all containers if pod-scoped, the stamped one otherwise). One zone counts as the whole
// pod; partial publication under-accounts, never double counts, except for a container in
// the reserved pool, which is seeded on top for a TTL.
func podInNRT(pod *corev1.Pod, container *corev1.Container, podScoped bool, reflected map[string]bool) bool {
	prefix := pod.Namespace + "/" + pod.Name + "/"
	if !podScoped {
		return container != nil && reflected[prefix+container.Name]
	}
	for i := range pod.Spec.Containers {
		if reflected[prefix+pod.Spec.Containers[i].Name] {
			return true
		}
	}
	for i := range pod.Spec.InitContainers {
		if reflected[prefix+pod.Spec.InitContainers[i].Name] {
			return true
		}
	}
	return false
}

// pinnedBalloon returns the stamped balloon and scope: pod-scoped (container nil) or one
// container. Reads the stamp, not the current policy. ok is false with no balloon annotation.
func pinnedBalloon(pod *corev1.Pod) (balloon string, container *corev1.Container, podScoped, ok bool) {
	if v := pod.Annotations[balloonAnnotationBase+"/pod"]; v != "" {
		return v, nil, true, true
	}
	for i := range pod.Spec.Containers {
		if v := pod.Annotations[balloonAnnotationBase+"/container."+pod.Spec.Containers[i].Name]; v != "" {
			return v, &pod.Spec.Containers[i], false, true
		}
	}
	return "", nil, false, false
}
