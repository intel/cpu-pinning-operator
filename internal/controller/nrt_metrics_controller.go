// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strconv"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/intel/cpu-pinning-operator/internal/balloons"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

const (
	// NRT zone types the NRI balloons plugin publishes.
	containerZoneType = "allocation for container"
	balloonZoneType   = "balloon"
	// reservedBalloonName is the built-in reserved-pool balloon.
	reservedBalloonName = "reserved"
	// freeBalloonName marks CPUs no balloon claimed.
	freeBalloonName = "free"
	// cpuAllocationHyperthreads counts capacity in logical CPUs; the other mode is
	// balloons.CPUAllocationPhysicalCores.
	cpuAllocationHyperthreads = "hyperthreads"
)

// nrtGVK identifies the NodeResourceTopology CR, read as unstructured.
var nrtGVK = nrtpkg.GVK

// NRTMetricsReconciler rebuilds the per-node CPU assignment, pool membership,
// logical and pinnable CPU gauges from the placement NRI recorded in the NRT CR.
type NRTMetricsReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
	// TopoCache is filled by the Node reconciler; this one never probes, so it
	// cannot race it on the fixed probe-pod name.
	TopoCache *topology.Cache
	// TopoEvents fires after a node's topology is cached, to re-label the series.
	TopoEvents <-chan event.GenericEvent
}

//+kubebuilder:rbac:groups=topology.node.k8s.io,resources=noderesourcetopologies,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

func (r *NRTMetricsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	node := req.Name
	log := r.Log.WithValues("node", node)

	nrt := &unstructured.Unstructured{}
	nrt.SetGroupVersionKind(nrtGVK)
	err := r.Get(ctx, client.ObjectKey{Name: node}, nrt)
	if errors.IsNotFound(err) {
		// NRT gone: drop the node's series so stale CPUs don't linger.
		r.dropNodeSeries(node)
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Malformed zones mean occupancy is unknown, as the webhook also concludes; don't
	// emit an all-free series.
	zones, zonesOK := nrtpkg.Zones(nrt)
	if !zonesOK {
		r.dropNodeSeries(node)
		return ctrl.Result{}, nil
	}

	// Unprobed nodes still emit, without numa/core; a later event re-labels them.
	cpuInfo, ok := r.TopoCache.Get(node)
	if !ok {
		log.V(1).Info("node topology not yet cached; emitting assignment without numa/core labels")
	}

	// Pool resolved by the Node reconciler. Unknown until provisioned; free then
	// falls back to the hardware set, which never understates the pool.
	pinnable, _ := r.TopoCache.GetPinnable(node)

	// This node's resolved mode, never the cluster default.
	cpuAllocation := r.resolvedCPUAllocation(ctx, log, node)

	// Rebuild from scratch, so departed containers don't linger.
	metrics.CPUAssignment.DeletePartialMatch(map[string]string{"node": node})
	metrics.PoolMembership.DeletePartialMatch(map[string]string{"node": node})
	r.emitLogicalCPUs(node, cpuInfo)
	r.emitPinnableCPUs(node, cpuInfo, pinnable)

	// Claimed CPUs; the rest of the pinnable pool is emitted as free.
	assigned := map[int]bool{}

	// Accumulated, not written through: two zones can name one container in one
	// balloon (a ghost allocation), and the later zone must not just win.
	members := map[membershipKey]int{}

	for _, z := range zones {
		zone, ok := z.(map[string]interface{})
		if !ok {
			continue
		}
		zoneType := nrtpkg.ZoneType(zone)
		name := nrtpkg.ZoneName(zone)
		cpus := nrtpkg.ParseCPUSet(nrtpkg.ZoneCpuset(zone))
		if len(cpus) == 0 {
			continue
		}

		switch zoneType {
		case containerZoneType:
			// name = "<namespace>/<pod>/<container>"; balloon = parent zone name.
			ns, pod, ctr := nrtpkg.SplitContainerZoneName(name)
			balloon := nrtpkg.StripInstance(nrtpkg.ZoneParent(zone))
			// Skip names missing any part: such a row names no container and cannot join
			// cAdvisor usage. cpu_assignment is unaffected, so it is still emitted below.
			if ns != "" && pod != "" && ctr != "" {
				k := membershipKey{balloon: balloon, namespace: ns, pod: pod, container: ctr}
				if len(cpus) > members[k] {
					members[k] = len(cpus)
				}
			}
			// Reserved containers share one cpuset; the reserved balloon zone emits it once.
			if balloon == reservedBalloonName {
				continue
			}
			r.emitAssignment(node, cpus, cpuInfo, balloon, pod, ns, assigned)
		case balloonZoneType:
			// Workload balloons are covered by their container subzones.
			if nrtpkg.StripInstance(name) == reservedBalloonName {
				r.emitAssignment(node, cpus, cpuInfo, reservedBalloonName, "", "", assigned)
			}
		}
	}

	r.emitPoolMembership(node, members)

	r.emitFree(node, cpuInfo, assigned, pinnable)

	// Same occupied set as the free pass, so the two cannot disagree.
	r.emitFragmentation(node, cpuInfo, assigned, cpuAllocation)

	return ctrl.Result{}, nil
}

// membershipKey is one container's membership of one balloon. Duplicate zones in
// the same balloon collapse; in different balloons they stay two series, since
// the CR does not say which is live and both pools are booked. This is the only
// metric that shows a reserved-involving duplicate: ghost-allocation counting
// skips reserved zones.
type membershipKey struct{ balloon, namespace, pod, container string }

// emitPoolMembership writes one series per container per balloon with the largest
// cpuset seen, so a duplicated zone never understates contention.
func (r *NRTMetricsReconciler) emitPoolMembership(node string, members map[membershipKey]int) {
	for k, cpus := range members {
		metrics.PoolMembership.WithLabelValues(
			node, k.balloon, k.namespace, k.pod, k.container,
		).Set(float64(cpus))
	}
}

// dropNodeSeries removes every per-node series, so a node with a missing or
// malformed NRT reads as unknown, matching the webhook.
func (r *NRTMetricsReconciler) dropNodeSeries(node string) {
	metrics.CPUAssignment.DeletePartialMatch(map[string]string{"node": node})
	metrics.PoolMembership.DeletePartialMatch(map[string]string{"node": node})
	metrics.NodeLogicalCPUs.DeletePartialMatch(map[string]string{"node": node})
	metrics.NodePinnableCPUs.DeletePartialMatch(map[string]string{"node": node})
	metrics.CPUFragmentation.DeletePartialMatch(map[string]string{"node": node})
}

// ResolvedCPUAllocation returns the node's cpuAllocation mode from the label the
// Node reconciler stamps, or "" when unresolved or unrecognised (FreePerNUMA would
// treat any unknown value as physical-cores). Reading the applied label keeps
// metrics, admission and drift detection agreeing on a node's mode.
func ResolvedCPUAllocation(labels map[string]string) string {
	switch mode := labels[LabelCPUAllocation]; mode {
	case balloons.CPUAllocationPhysicalCores, cpuAllocationHyperthreads:
		return mode
	default:
		return ""
	}
}

// resolvedCPUAllocation reads one node's mode; the Node watch re-enqueues on change.
func (r *NRTMetricsReconciler) resolvedCPUAllocation(ctx context.Context, log logr.Logger, node string) string {
	n := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: node}, n); err != nil {
		if !errors.IsNotFound(err) {
			log.V(1).Info("reading the node's allocation mode failed; it reports no fragmentation this pass", "err", err.Error())
		}
		return ""
	}
	return ResolvedCPUAllocation(n.Labels)
}

// emitFragmentation counts free CPUs the largest balloon span cannot absorb due to
// NUMA imbalance, in this node's cpuAllocation unit; an unresolved mode emits
// nothing. Density-mode nodes report 0 free here, unlike the webhook. maxSpan is
// derived from the NUMA count, not the node's max-span label.
func (r *NRTMetricsReconciler) emitFragmentation(node string, cpuInfo map[int]topology.CPUInfo, assigned map[int]bool, cpuAllocation string) {
	// Drop first so missing topology leaves no stale value.
	metrics.CPUFragmentation.DeletePartialMatch(map[string]string{"node": node})
	if len(cpuInfo) == 0 {
		return // not probed (or pruned): free room is unknown
	}
	if cpuAllocation == "" {
		return // mode unresolved: the unit this node counts in is unknown
	}
	free := topology.FreePerNUMA(cpuInfo, assigned, cpuAllocation)
	maxSpan := topology.MaxSpanForNUMA(len(free))
	stranded := topology.StrandedFreeCores(free, maxSpan)
	metrics.CPUFragmentation.WithLabelValues(node).Set(float64(stranded))
}

// emitAssignment sets cpu_assignment=1 per CPU and marks it in assigned.
func (r *NRTMetricsReconciler) emitAssignment(node string, cpus []int, cpuInfo map[int]topology.CPUInfo, balloon, pod, ns string, assigned map[int]bool) {
	for _, cpu := range cpus {
		numa, core := "", ""
		if info, ok := cpuInfo[cpu]; ok {
			numa = strconv.Itoa(info.NUMA)
			core = strconv.Itoa(info.Core)
		}
		metrics.CPUAssignment.WithLabelValues(
			node, strconv.Itoa(cpu), numa, core, balloon, pod, ns,
		).Set(1)
		assigned[cpu] = true
	}
}

// emitFree emits balloon="free" for each cached pinnable CPU no zone claimed. CPUs
// outside the pool get no series; a nil pool (not provisioned) emits all.
func (r *NRTMetricsReconciler) emitFree(node string, cpuInfo map[int]topology.CPUInfo, assigned, pinnable map[int]bool) {
	for cpu, info := range cpuInfo {
		if assigned[cpu] {
			continue
		}
		if pinnable != nil && !pinnable[cpu] {
			continue
		}
		metrics.CPUAssignment.WithLabelValues(
			node, strconv.Itoa(cpu), strconv.Itoa(info.NUMA), strconv.Itoa(info.Core),
			freeBalloonName, "", "",
		).Set(1)
	}
}

// emitLogicalCPUs sets the hardware node_logical_cpus per NUMA node.
func (r *NRTMetricsReconciler) emitLogicalCPUs(node string, cpuInfo map[int]topology.CPUInfo) {
	metrics.NodeLogicalCPUs.DeletePartialMatch(map[string]string{"node": node})
	perNUMA := map[int]int{}
	for _, info := range cpuInfo {
		perNUMA[info.NUMA]++
	}
	for numa, count := range perNUMA {
		metrics.NodeLogicalCPUs.WithLabelValues(node, strconv.Itoa(numa)).Set(float64(count))
	}
}

// emitPinnableCPUs sets node_pinnable_cpus per NUMA node from the resolved pool,
// with an explicit 0 for a fully excluded NUMA node. Nothing while the pool is
// unknown: the absent series makes consumers fall back to node_logical_cpus.
func (r *NRTMetricsReconciler) emitPinnableCPUs(node string, cpuInfo map[int]topology.CPUInfo, pinnable map[int]bool) {
	metrics.NodePinnableCPUs.DeletePartialMatch(map[string]string{"node": node})
	if pinnable == nil {
		return
	}
	perNUMA := map[int]int{}
	for cpu, info := range cpuInfo {
		if _, ok := perNUMA[info.NUMA]; !ok {
			perNUMA[info.NUMA] = 0
		}
		if pinnable[cpu] {
			perNUMA[info.NUMA]++
		}
	}
	for numa, count := range perNUMA {
		metrics.NodePinnableCPUs.WithLabelValues(node, strconv.Itoa(numa)).Set(float64(count))
	}
}

// SetupWithManager watches NRT CRs and Nodes, both keyed by node name.
func (r *NRTMetricsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	nrt := &unstructured.Unstructured{}
	nrt.SetGroupVersionKind(nrtGVK)
	b := ctrl.NewControllerManagedBy(mgr).
		For(nrt).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(enqueueNodeName))
	if r.TopoEvents != nil {
		// Re-reconcile when the Node reconciler caches fresh topology.
		b = b.WatchesRawSource(source.Channel(r.TopoEvents,
			handler.EnqueueRequestsFromMapFunc(enqueueNodeName)))
	}
	return b.Complete(r)
}

// enqueueNodeName maps a Node event to its same-named NRT.
func enqueueNodeName(_ context.Context, obj client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: obj.GetName()}}}
}
