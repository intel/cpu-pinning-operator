// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nriv1alpha1 "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/balloons"
	"github.com/intel/cpu-pinning-operator/internal/classification"
	"github.com/intel/cpu-pinning-operator/internal/dashboard"
	"github.com/intel/cpu-pinning-operator/internal/match"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

const (
	// LabelBalloonsEnabled marks a node with a provisioned BalloonsPolicy.
	LabelBalloonsEnabled = cpopv1alpha1.LabelPrefix + "balloons-enabled"
	// LabelMaxSpan is the largest span ({1,2,4,8}) the node's BalloonsPolicy defines,
	// derived from the generated defs, not the NUMA count.
	LabelMaxSpan        = cpopv1alpha1.LabelPrefix + "max-span"
	LabelNumaNodes      = cpopv1alpha1.LabelPrefix + "numa-nodes"
	LabelPhysicalCPUs   = cpopv1alpha1.LabelPrefix + "physical-cpus"
	LabelThreadsPerCore = cpopv1alpha1.LabelPrefix + "threads-per-core"
	// LabelMemoryPerNUMABytes is the smallest per-NUMA MemTotal in bytes. Absent
	// when the probe reported none, in which case the memory capacity check skips.
	LabelMemoryPerNUMABytes = cpopv1alpha1.LabelPrefix + "memory-per-numa-bytes"
	// LabelPackageTopology is per-package NUMA counts, "_"-joined (e.g. "2_2"),
	// so the webhook only steers a variant to nodes that define it.
	LabelPackageTopology = cpopv1alpha1.LabelPrefix + "package-topology"
	LabelDisabled        = cpopv1alpha1.LabelPrefix + "disabled"
	// LabelReservedCPUs is the resolved reserved-pool size after overrides and clamps.
	LabelReservedCPUs = cpopv1alpha1.LabelPrefix + "reserved-cpus"
	// LabelCPUAllocation is the resolved mode (physical-cores | hyperthreads).
	LabelCPUAllocation = cpopv1alpha1.LabelPrefix + "cpu-allocation"
	// LabelAMX marks AMX-capable nodes from the operator's own probe (amx_tile).
	LabelAMX = cpopv1alpha1.LabelPrefix + "amx"
	// LabelNodeClass is the hardware class (mixed-cpu-amx-gpu | cpu-amx | gpu-only | cpu-only).
	LabelNodeClass = cpopv1alpha1.LabelPrefix + "node-class"
	// LabelCPUManagerConflict is "true" when the kubelet runs the CPU Manager static
	// policy, "false" for none, and absent when unread or unrecognised. Absent means
	// unknown, never compatible.
	LabelCPUManagerConflict = cpopv1alpha1.LabelPrefix + "cpu-manager-conflict"

	// AnnotationTopologyCache carries the topology the webhook's live capacity check
	// reads (opaque JSON). Only the leader writes it; every replica's
	// TopoCachePopulator reads it.
	AnnotationTopologyCache = cpopv1alpha1.LabelPrefix + "topology-cache"

	nodeResyncPeriod = 10 * time.Minute

	// defaultNodeReconcileConcurrency is used when MaxConcurrentReconciles is unset.
	defaultNodeReconcileConcurrency = 5

	// enqueueBurstChunkSize / enqueueBurstChunkDelay pace a fleet-wide re-enqueue.
	enqueueBurstChunkSize  = 10
	enqueueBurstChunkDelay = 1 * time.Second

	// probePendingRequeue is the backoff while a prior probe pod is terminating.
	probePendingRequeue = 5 * time.Second

	// Node event reasons.
	reasonProvisioned           = "Provisioned"
	reasonBalloonsPolicyUpdated = "BalloonsPolicyUpdated"
	reasonDeprovisioned         = "Deprovisioned"
	reasonProvisioningFailed    = "ProvisioningFailed"
	// The override named an unknown class, or an AMX class on a non-AMX node.
	reasonClassificationOverrideRejected = "ClassificationOverrideRejected"
	// NRT has container zones for deleted pods; recover by restarting the plugin pod.
	reasonGhostBalloonAllocation = "GhostBalloonAllocation"
	// Kubelet static CPU Manager and the plugin both allocate cpusets; still provisioned.
	reasonCPUManagerConflict = "CPUManagerConflict"

	// ghostExampleLimit caps the example zone names in the ghost Warning; the count
	// is always complete.
	ghostExampleLimit = 3
)

// NodeReconciler probes each node's CPU topology, generates its balloon types,
// applies the per-node BalloonsPolicy CR and labels the node.
type NodeReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Log         logr.Logger
	PolicyIndex *match.Index
	LogsGetter  topology.LogsGetter
	// Recorder emits Events on the Node.
	Recorder record.EventRecorder
	// KubeletConfigReader reads the kubelet configuration for the CPU Manager check.
	// It must be uncached (mgr.GetAPIReader): a cached Get would watch every
	// ConfigMap. Nil disables the check.
	KubeletConfigReader client.Reader
	// TopoCache receives each probed node's topology, for the NRT metrics reconciler
	// and the webhook.
	TopoCache *topology.Cache
	// TopoNotify, if set, signals the NRT metrics reconciler after a cache update.
	TopoNotify chan<- event.GenericEvent
	// Dashboards, if set, writes a per-node Grafana dashboard ConfigMap.
	Dashboards *dashboard.Writer
	// BalloonsNamespace is where per-node BalloonsPolicy CRs go; it must match where
	// the NRI plugin reads them. Empty means kube-system.
	BalloonsNamespace string
	// MaxConcurrentReconciles caps parallel node reconciles; non-positive means
	// defaultNodeReconcileConcurrency.
	MaxConcurrentReconciles int
}

// reconcileConcurrency returns MaxConcurrentReconciles or the default.
func (r *NodeReconciler) reconcileConcurrency() int {
	if r.MaxConcurrentReconciles > 0 {
		return r.MaxConcurrentReconciles
	}
	return defaultNodeReconcileConcurrency
}

// balloonsNamespace returns BalloonsNamespace or the default.
func (r *NodeReconciler) balloonsNamespace() string {
	if r.BalloonsNamespace != "" {
		return r.BalloonsNamespace
	}
	return balloons.DefaultBalloonsNamespace
}

func (r *NodeReconciler) balloonsPolicyName(nodeName string) string {
	return "node." + nodeName
}

// cachedTopology returns the cached topology if tagged with the current boot id;
// false forces a fresh probe.
func (r *NodeReconciler) cachedTopology(nodeName, bootID string) (*topology.NodeTopology, bool) {
	if r.TopoCache == nil {
		return nil, false
	}
	return r.TopoCache.GetTopology(nodeName, bootID)
}

// SSA of BalloonsPolicy needs create and patch, never update. The CPU Manager
// check reads kubeadm's kubelet-config through the existing configmaps grant.
// Probe pods run in the manager's namespace, so pod create/delete is a namespaced Role.
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",namespace=system,resources=pods,verbs=create;delete
//+kubebuilder:rbac:groups="",resources=pods/log,verbs=get
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;delete
//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get
//+kubebuilder:rbac:groups=config.nri,resources=balloonspolicies,verbs=get;list;watch;create;patch;delete

// notifyTopology signals the NRT metrics reconciler. Dropping on a full buffer is
// safe: a pending reconcile reads the cache when it runs.
func (r *NodeReconciler) notifyTopology(node *corev1.Node) {
	if r.TopoNotify == nil {
		return
	}
	select {
	case r.TopoNotify <- event.GenericEvent{Object: node}:
	default:
	}
}

// Reconcile probes, generates, applies the BalloonsPolicy and labels the node;
// deleted or excluded nodes are pruned.
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("node", req.Name)

	node := &corev1.Node{}
	err := r.Get(ctx, client.ObjectKey{Name: req.Name}, node)
	if errors.IsNotFound(err) {
		// No Node left to record an Event on.
		_, err := r.pruneNode(ctx, req.Name)
		return ctrl.Result{}, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Excluded (disabled label or excludeNodeSelector) nodes are pruned.
	excluded := node.Labels[LabelDisabled] == "true"
	if !excluded && r.PolicyIndex != nil {
		excluded = r.PolicyIndex.NodeExcluded(node.Labels)
	}
	if excluded {
		log.V(1).Info("node excluded from pinning pool, skipping provisioning")
		removed, err := r.pruneNode(ctx, node.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if removed {
			r.emitEvent(node, corev1.EventTypeNormal, reasonDeprovisioned,
				"node excluded from the pinning pool; removed its BalloonsPolicy")
		}
		return ctrl.Result{}, r.unlabelNode(ctx, node)
	}

	// Topology only changes across a reboot; reusing the boot-id-tagged cache avoids
	// re-probing the fleet on a config flip.
	bootID := node.Status.NodeInfo.BootID
	topo, cached := r.cachedTopology(node.Name, bootID)
	if !cached {
		var err error
		topo, err = topology.DiscoverNode(ctx, r.Client, r.LogsGetter, node.Name)
		if err != nil {
			// Prior probe pod still terminating: back off instead of hot-looping create.
			if topology.IsProbePending(err) {
				log.V(1).Info("probe pod still terminating, backing off before retry", "reason", err.Error())
				return ctrl.Result{RequeueAfter: probePendingRequeue}, nil
			}
			log.Error(err, "topology discovery failed")
			r.emitEvent(node, corev1.EventTypeWarning, reasonProvisioningFailed,
				"topology discovery failed, will retry: %v", err)
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
	}
	// Publish topology for metrics and cache it so config-only reconciles skip the probe.
	if r.TopoCache != nil {
		r.TopoCache.Set(node.Name, topo.CPUs)
		r.TopoCache.SetMemory(node.Name, topo.MemoryPerNUMABytesMap)
		if bootID != "" {
			r.TopoCache.SetTopology(node.Name, bootID, topo)
		}
	}
	r.notifyTopology(node)

	classSpec := classification.Spec{}
	if r.PolicyIndex != nil {
		classSpec = r.PolicyIndex.Classification()
	}
	nodeClass, classReason := classification.ClassifyNode(node, topo, classSpec)
	log.V(1).Info("node classified", "class", nodeClass, "reason", classReason)
	if rejected, _ := classReason["overrideRejected"].(bool); rejected {
		r.emitEvent(node, corev1.EventTypeWarning, reasonClassificationOverrideRejected,
			"ignored node-class override %q: not a known class or requests an AMX class on a node without AMX support; using auto-classified %q",
			classReason["overrideRequested"], nodeClass)
	}

	// Cluster default, then class profile, then first-matching node override.
	cpuAllocation := balloons.CPUAllocationPhysicalCores
	htSiblingPolicy := balloons.HTSiblingPolicyExclude
	var reservedNamespaces []string
	var reservedSpec *cpopv1alpha1.ReservedSizingSpec
	if r.PolicyIndex != nil {
		alloc, reserved := r.PolicyIndex.ResolveNodeConfigForClass(node.Labels, nodeClass)
		if alloc != "" {
			cpuAllocation = alloc
		}
		reservedSpec = reserved
		if htsp := r.PolicyIndex.HTSiblingPolicy(); htsp != "" {
			htSiblingPolicy = htsp
		}
		reservedNamespaces = r.PolicyIndex.ReservedNamespaces()
	}
	measuredUsageCPUs, err := r.reservedUsageCPUs(ctx, node.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	// balloons decides what happens to HT siblings; in density mode the reserved
	// pool is exactly the siblings.
	siblings, densityMode := balloons.ResolveSiblings(topo.CPUs, cpuAllocation, htSiblingPolicy)

	var reservedCPUs int
	if densityMode {
		reservedCPUs = balloons.SiblingCPUCount(topo)
	} else {
		reservedCPUs = balloons.ResolveReservedCPUs(reservedSpec, topo.PhysicalCPUs,
			topo.ThreadsPerCore, measuredUsageCPUs, cpuAllocation == balloons.CPUAllocationPhysicalCores)
	}
	setSiblingOutcomeMetric(node.Name, topo, siblings, densityMode)

	defs := balloons.GenerateBalloonDefs(topo.PkgNode, topo.MaxSpan, cpuAllocation)
	policy := balloons.RenderNodePolicy(node.Name, r.balloonsNamespace(), topo, defs, reservedCPUs, cpuAllocation, htSiblingPolicy, reservedNamespaces)

	// Report the applied pool, not the resolved size: the per-NUMA spread rounds up.
	appliedReservedCPUs := balloons.AppliedReservedCPUs(policy)
	metrics.NodeReservedCPUs.WithLabelValues(node.Name).Set(float64(appliedReservedCPUs))
	if appliedReservedCPUs > 0 {
		metrics.ReservedPoolUtilizationRatio.WithLabelValues(node.Name).
			Set(float64(measuredUsageCPUs) / float64(appliedReservedCPUs))
	} else {
		metrics.ReservedPoolUtilizationRatio.WithLabelValues(node.Name).Set(0)
	}

	// Advertise the span the defs serve; floor 1 matches the webhook's missing-label default.
	advertisedSpan := max(1, balloons.MaxAdvertisableSpan(defs))

	owner, err := r.clusterConfigOwnerRef(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	outcome, err := r.applyBalloonsPolicy(ctx, policy, owner)
	if err != nil {
		metrics.BalloonsPolicyReconcileErrors.WithLabelValues(node.Name).Inc()
		return ctrl.Result{}, err
	}
	switch outcome {
	case policyCreated:
		r.emitEvent(node, corev1.EventTypeNormal, reasonProvisioned,
			"provisioned pinning: applied BalloonsPolicy %q (maxSpan %d, %s)",
			policy.Name, advertisedSpan, cpuAllocation)
	case policyUpdated:
		r.emitEvent(node, corev1.EventTypeNormal, reasonBalloonsPolicyUpdated,
			"updated BalloonsPolicy %q (maxSpan %d, %s)",
			policy.Name, advertisedSpan, cpuAllocation)
	}

	// Publish the pinnable pool after the apply: until then the plugin enforces the
	// previous policy.
	pinnableCPUs := balloons.PinnableCPUs(topo.CPUs, cpuAllocation, htSiblingPolicy)
	if r.TopoCache != nil {
		r.TopoCache.SetPinnable(node.Name, pinnableCPUs)
	}
	// Notify again: the pool reaches the cache only now.
	r.notifyTopology(node)

	// Kubelet CPU Manager policy (cluster-wide reading), stamped with the labels below.
	// Flag only: the node stays provisioned. Refusing would withdraw the pinning
	// while leaving the kubelet's exclusive cpusets in place anyway.
	cpuManagerPolicy, err := topology.CPUManagerPolicy(ctx, r.KubeletConfigReader)
	if err != nil {
		// Unreadable config (e.g. not kubeadm) stays unflagged.
		log.V(1).Info("kubelet CPU Manager policy unknown; not flagging the node", "reason", err.Error())
	}

	if err := r.labelNode(ctx, node, topo, advertisedSpan, reservedCPUs, cpuAllocation, nodeClass, cpuManagerPolicy); err != nil {
		return ctrl.Result{}, err
	}
	r.reportCPUManagerConflict(node, cpuManagerPolicy)

	// Dashboard write is non-fatal; it reads the labels just stamped so it cannot
	// disagree with them.
	if r.Dashboards != nil && node.Labels[LabelBalloonsEnabled] == "true" {
		machine := dashboard.Machine{
			MemoryPerNUMABytes: topo.MemoryPerNUMABytesMap,
			PackageTopology:    node.Labels[LabelPackageTopology],
			MaxSpan:            advertisedSpan,
			NodeClass:          node.Labels[LabelNodeClass],
			AMX:                node.Labels[LabelAMX] == "true",
			CPUAllocation:      cpuAllocation,
			ReservedCPUs:       reservedCPUs,
			PinnableCPUs:       pinnableCPUs,
		}
		if err := r.Dashboards.Write(ctx, node.Name, topo.CPUs, machine); err != nil {
			log.Error(err, "failed to write node dashboard ConfigMap")
		}
	}

	r.detectGhostAllocations(ctx, node)

	return ctrl.Result{RequeueAfter: nodeResyncPeriod}, nil
}

// detectGhostAllocations finds NRT container zones whose pod no longer exists. It
// sets the gauge and emits one Warning; an inconclusive pass deletes the series.
func (r *NodeReconciler) detectGhostAllocations(ctx context.Context, node *corev1.Node) {
	log := r.Log.WithValues("node", node.Name)

	nrt := &unstructured.Unstructured{}
	nrt.SetGroupVersionKind(nrtpkg.GVK)
	if err := r.Get(ctx, client.ObjectKey{Name: node.Name}, nrt); err != nil {
		if !errors.IsNotFound(err) {
			log.V(1).Info("ghost-allocation detection: NodeResourceTopology read failed, skipping this pass", "err", err.Error())
		}
		metrics.GhostAllocations.DeleteLabelValues(node.Name)
		return
	}

	zoneNames, ok := nrtpkg.ContainerZoneNames(nrt)
	if !ok {
		log.V(1).Info("ghost-allocation detection: malformed NodeResourceTopology, skipping this pass")
		metrics.GhostAllocations.DeleteLabelValues(node.Name)
		return
	}
	if len(zoneNames) == 0 {
		metrics.GhostAllocations.WithLabelValues(node.Name).Set(0)
		return
	}

	type podKey struct{ ns, name string }
	exists := map[podKey]bool{}
	var ghostCount int
	var examples []string
	for _, name := range zoneNames {
		ns, pod, _ := nrtpkg.SplitContainerZoneName(name)
		if ns == "" || pod == "" {
			continue // unparseable zone name: cannot attribute to a pod, skip
		}
		key := podKey{ns, pod}
		live, known := exists[key]
		if !known {
			var err error
			live, err = r.podExists(ctx, ns, pod)
			if err != nil {
				log.V(1).Info("ghost-allocation detection: pod lookup failed, skipping this pass", "pod", ns+"/"+pod, "err", err.Error())
				metrics.GhostAllocations.DeleteLabelValues(node.Name)
				return
			}
			exists[key] = live
		}
		if !live {
			ghostCount++
			if len(examples) < ghostExampleLimit {
				examples = append(examples, name)
			}
		}
	}

	metrics.GhostAllocations.WithLabelValues(node.Name).Set(float64(ghostCount))
	if ghostCount == 0 {
		return
	}

	// One aggregated Warning per resync, never one per zone.
	log.Info("detected ghost balloon allocations in NodeResourceTopology",
		"count", ghostCount, "examples", examples)
	r.emitEvent(node, corev1.EventTypeWarning, reasonGhostBalloonAllocation,
		"%d ghost balloon allocation(s) in NodeResourceTopology (container zones for pods that no longer exist), e.g. %s; "+
			"these are leaked by the NRI balloons plugin on a failed CreateContainer and occupy balloon capacity that blocks new pods. "+
			"Recover by restarting the nri-resource-policy-balloons DaemonSet pod on this node.",
		ghostCount, strings.Join(examples, ", "))
}

// reportCPUManagerConflict sets the gauge to 1 under static, 0 under none, and
// deletes it when unknown. It uses the same classification as labelNode. The
// Warning repeats each reconcile while the conflict stands.
func (r *NodeReconciler) reportCPUManagerConflict(node *corev1.Node, policy string) {
	switch topology.ClassifyCPUManagerPolicy(policy) {
	case topology.CPUManagerUnknown:
		metrics.CPUManagerConflict.DeleteLabelValues(node.Name)
	case topology.CPUManagerConflict:
		metrics.CPUManagerConflict.WithLabelValues(node.Name).Set(1)
		r.emitEvent(node, corev1.EventTypeWarning, reasonCPUManagerConflict,
			"kubelet CPU Manager policy is %q, so the kubelet and the NRI balloons plugin both allocate cpusets on this node: "+
				"the kubelet can hand a Guaranteed container a CPU the plugin reserved for a balloon, and the plugin can pin onto a CPU "+
				"the kubelet already assigned exclusively, which makes placement unpredictable. The node is still provisioned; "+
				"either set the kubelet to --cpu-manager-policy=none or take the node out of the pinning pool with %s=true.",
			policy, LabelDisabled)
	default:
		metrics.CPUManagerConflict.WithLabelValues(node.Name).Set(0)
	}
}

// podExists reports whether a pod exists. Errors other than NotFound abandon the
// pass rather than mislabel live pods as ghosts.
func (r *NodeReconciler) podExists(ctx context.Context, namespace, name string) (bool, error) {
	pod := &corev1.Pod{}
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pod)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// setSiblingOutcomeMetric sets the HT sibling outcome gauge: 1 for the state this
// pass rendered, 0 for the others.
func setSiblingOutcomeMetric(nodeName string, topo *topology.NodeTopology, siblings []int, density bool) {
	active := balloons.SiblingOutcome(topo, siblings, density)
	for _, outcome := range balloons.SiblingOutcomes {
		v := 0.0
		if outcome == active {
			v = 1.0
		}
		metrics.NodeHTSiblingPolicy.WithLabelValues(nodeName, outcome).Set(v)
	}
}

// podNodeNameField is a field index over pod.spec.nodeName, so per-node pod lists
// scale with the node, not the cluster.
const podNodeNameField = ".spec.nodeName"

// reservedUsageCPUs sums CPU requests of the node's non-pinned, non-terminal pods,
// rounded up to whole CPUs.
func (r *NodeReconciler) reservedUsageCPUs(ctx context.Context, nodeName string) (int, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{podNodeNameField: nodeName}); err != nil {
		return 0, err
	}
	var milli int64
	for i := range pods.Items {
		pod := &pods.Items[i]
		// Pinned pods use the pinning pool; terminal pods hold no cores.
		if pod.Annotations[pinnedByAnnotation] == pinnedByValue {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		milli += podCPURequestMilli(pod)
	}
	// The reserved pool is sized in whole CPUs.
	return int((milli + 999) / 1000), nil
}

// podCPURequestMilli returns the effective request: max(sum of containers,
// largest init container), using the limit when a container sets no request.
func podCPURequestMilli(pod *corev1.Pod) int64 {
	containerReq := func(c *corev1.Container) int64 {
		if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			return q.MilliValue()
		}
		if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			return q.MilliValue()
		}
		return 0
	}
	var sum int64
	for i := range pod.Spec.Containers {
		sum += containerReq(&pod.Spec.Containers[i])
	}
	var initMax int64
	for i := range pod.Spec.InitContainers {
		if r := containerReq(&pod.Spec.InitContainers[i]); r > initMax {
			initMax = r
		}
	}
	if initMax > sum {
		return initMax
	}
	return sum
}

// applyOutcome is what applyBalloonsPolicy did, to pick the Event to emit.
type applyOutcome int

const (
	policyUnchanged applyOutcome = iota
	policyCreated
	policyUpdated
)

// balloonsPolicyFieldManager is the operator's SSA field manager for BalloonsPolicy CRs.
const balloonsPolicyFieldManager = "cpu-pinning-operator"

func (r *NodeReconciler) applyBalloonsPolicy(ctx context.Context, desired *nriv1alpha1.BalloonsPolicy, owner *metav1.OwnerReference) (applyOutcome, error) {
	// Empty inlined blocks fail CRD validation; prune them.
	applyObj, err := toPrunedUnstructured(desired)
	if err != nil {
		return policyUnchanged, err
	}

	// Read unstructured: a typed read drops CRD fields the vendored struct lacks, which
	// would always differ. The client serves unstructured reads live, not cached; a
	// cached spec could lag our own last write.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(applyObj.GroupVersionKind())
	getErr := r.Get(ctx, client.ObjectKey{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if getErr != nil && !errors.IsNotFound(getErr) {
		return policyUnchanged, getErr
	}
	notFound := errors.IsNotFound(getErr)

	storedSpec := existing.Object["spec"]
	if refs := existing.GetOwnerReferences(); len(refs) > 0 {
		applyObj.SetOwnerReferences(refs)
	}
	setOwnerRef(applyObj, owner)

	// ForceOwnership keeps the operator authoritative on its fields.
	if err := r.Patch(ctx, applyObj, client.Apply,
		client.FieldOwner(balloonsPolicyFieldManager), client.ForceOwnership); err != nil {
		return policyUnchanged, err
	}

	if notFound {
		return policyCreated, nil
	}
	// Patch replaced applyObj with the stored result, so compare stored with stored;
	// comparing the request would flag every CRD default as a change.
	if apiequality.Semantic.DeepEqual(storedSpec, applyObj.Object["spec"]) {
		return policyUnchanged, nil
	}
	return policyUpdated, nil
}

// clusterConfigOwnerRef returns a plain ownerReference to the singleton, or nil if
// absent. Plain, so GC never foreground-blocks the singleton's deletion.
func (r *NodeReconciler) clusterConfigOwnerRef(ctx context.Context) (*metav1.OwnerReference, error) {
	return clusterConfigOwnerRef(ctx, r.Client)
}

func clusterConfigOwnerRef(ctx context.Context, c client.Reader) (*metav1.OwnerReference, error) {
	cfg := &cpopv1alpha1.BalloonsClusterConfig{}
	err := c.Get(ctx, client.ObjectKey{Name: cpopv1alpha1.ClusterConfigName}, cfg)
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	gvk := cpopv1alpha1.GroupVersion.WithKind("BalloonsClusterConfig")
	return &metav1.OwnerReference{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       cfg.Name,
		UID:        cfg.UID,
	}, nil
}

// setOwnerRef idempotently adds owner; nil is a no-op, backfilled on a later resync.
func setOwnerRef(target *unstructured.Unstructured, owner *metav1.OwnerReference) {
	if owner == nil {
		return
	}
	refs := target.GetOwnerReferences()
	for i := range refs {
		if refs[i].UID == owner.UID {
			return // already owned by this singleton
		}
	}
	target.SetOwnerReferences(append(refs, *owner))
}

// emitEvent records an Event if a Recorder is set, and always meters provisioning
// transitions.
func (r *NodeReconciler) emitEvent(node *corev1.Node, eventType, reason, format string, args ...interface{}) {
	if outcome := provisioningOutcome(reason); outcome != "" {
		metrics.NodeProvisioning.WithLabelValues(node.Name, outcome).Inc()
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(node, eventType, reason, format, args...)
	}
}

// provisioningOutcome maps an Event reason to its node_provisioning_total outcome,
// or "".
func provisioningOutcome(reason string) string {
	switch reason {
	case reasonProvisioned:
		return metrics.NodeProvisioned
	case reasonBalloonsPolicyUpdated:
		return metrics.NodeUpdated
	case reasonDeprovisioned:
		return metrics.NodeDeprovisioned
	case reasonProvisioningFailed:
		return metrics.NodeProvisionFailed
	default:
		return ""
	}
}

// prunedSpecKeys are inlined sub-configs that serialize as empty objects the CRD rejects.
var prunedSpecKeys = []string{"control", "log", "instrumentation"}

// toPrunedUnstructured converts the policy to unstructured without those blocks
// or status.
func toPrunedUnstructured(desired *nriv1alpha1.BalloonsPolicy) (*unstructured.Unstructured, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(desired)
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{Object: raw}
	if spec, ok := obj.Object["spec"].(map[string]interface{}); ok {
		for _, k := range prunedSpecKeys {
			delete(spec, k)
		}
	}
	delete(obj.Object, "status")
	return obj, nil
}

// labelNode stamps the operator-managed labels and the topology-cache annotation.
// The webhook steers and caps pods by the max-span label alone, so it carries
// advertisedSpan.
func (r *NodeReconciler) labelNode(ctx context.Context, node *corev1.Node, topo *topology.NodeTopology, advertisedSpan, reservedCPUs int, cpuAllocation, nodeClass, cpuManagerPolicy string) error {
	patch := client.MergeFrom(node.DeepCopy())
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels[LabelBalloonsEnabled] = "true"
	node.Labels[LabelMaxSpan] = strconv.Itoa(advertisedSpan)
	node.Labels[LabelNumaNodes] = strconv.Itoa(topo.NumaNodes)
	node.Labels[LabelPhysicalCPUs] = strconv.Itoa(topo.PhysicalCPUs)
	node.Labels[LabelThreadsPerCore] = strconv.Itoa(topo.ThreadsPerCore)
	// Resolved values, projected into BalloonsClusterConfig status.
	node.Labels[LabelReservedCPUs] = strconv.Itoa(reservedCPUs)
	node.Labels[LabelCPUAllocation] = cpuAllocation
	// Absent when the probe reported no memory; the webhook then skips that check.
	if topo.MemoryPerNUMABytes > 0 {
		node.Labels[LabelMemoryPerNUMABytes] = strconv.FormatInt(topo.MemoryPerNUMABytes, 10)
	} else {
		delete(node.Labels, LabelMemoryPerNUMABytes)
	}
	// Absent package topology makes the webhook treat coLocate variants as unsupported.
	if pkgTopo := balloons.FormatPackageTopology(balloons.PerPackageNUMACounts(topo.PkgNode)); pkgTopo != "" {
		node.Labels[LabelPackageTopology] = pkgTopo
	} else {
		delete(node.Labels, LabelPackageTopology)
	}
	// From the probe's amx_tile flag.
	if topo.AMX {
		node.Labels[LabelAMX] = "true"
	} else {
		delete(node.Labels, LabelAMX)
	}

	if nodeClass != "" {
		node.Labels[LabelNodeClass] = nodeClass
	} else {
		delete(node.Labels, LabelNodeClass)
	}
	// Removed when unknown, so the singleton reports Unknown, not compatible.
	switch topology.ClassifyCPUManagerPolicy(cpuManagerPolicy) {
	case topology.CPUManagerConflict:
		node.Labels[LabelCPUManagerConflict] = "true"
	case topology.CPUManagerCompatible:
		node.Labels[LabelCPUManagerConflict] = "false"
	default:
		delete(node.Labels, LabelCPUManagerConflict)
	}
	// For every replica's webhook. Deterministic encoding makes unchanged topology a
	// no-op patch; an encode failure must not block the labels.
	if payload, err := topology.EncodeNodeCache(topo.CPUs, topo.MemoryPerNUMABytesMap); err != nil {
		r.Log.Error(err, "failed to encode topology cache annotation; followers will fall back to the static check", "node", node.Name)
	} else {
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}
		node.Annotations[AnnotationTopologyCache] = payload
	}
	return r.Patch(ctx, node, patch)
}

// ManagedNodeLabels are the operator-managed node labels. GC cannot remove label
// keys, so the uninstall hook strips exactly this set; keep it complete.
var ManagedNodeLabels = []string{
	LabelBalloonsEnabled,
	LabelMaxSpan,
	LabelNumaNodes,
	LabelPhysicalCPUs,
	LabelThreadsPerCore,
	LabelMemoryPerNUMABytes,
	LabelPackageTopology,
	LabelAMX,
	LabelNodeClass,
	LabelReservedCPUs,
	LabelCPUAllocation,
	LabelCPUManagerConflict,
}

// unlabelNode removes the managed labels and topology-cache annotation from an
// excluded node, so followers load no stale topology.
func (r *NodeReconciler) unlabelNode(ctx context.Context, node *corev1.Node) error {
	present := false
	for _, l := range ManagedNodeLabels {
		if _, ok := node.Labels[l]; ok {
			present = true
			break
		}
	}
	_, hasTopo := node.Annotations[AnnotationTopologyCache]
	if !present && !hasTopo {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	for _, l := range ManagedNodeLabels {
		delete(node.Labels, l)
	}
	delete(node.Annotations, AnnotationTopologyCache)
	return r.Patch(ctx, node, patch)
}

// pruneNode removes a node's BalloonsPolicy and derived state, returning true only
// when the CR was actually deleted.
func (r *NodeReconciler) pruneNode(ctx context.Context, nodeName string) (bool, error) {
	// NRT series go when the NRT CR does.
	metrics.NodeReservedCPUs.DeletePartialMatch(map[string]string{"node": nodeName})
	metrics.NodeHTSiblingPolicy.DeletePartialMatch(map[string]string{"node": nodeName})
	metrics.GhostAllocations.DeletePartialMatch(map[string]string{"node": nodeName})
	metrics.CPUManagerConflict.DeletePartialMatch(map[string]string{"node": nodeName})
	// The webhook republishes this only on change, so drop it here.
	metrics.ReservationsLive.DeletePartialMatch(map[string]string{"node": nodeName})
	if r.TopoCache != nil {
		r.TopoCache.Delete(nodeName)
	}
	// Runs even with dashboards disabled, to clean up.
	if r.Dashboards != nil {
		if err := r.Dashboards.PruneNode(ctx, nodeName); err != nil {
			r.Log.Error(err, "failed to prune node dashboard ConfigMap", "node", nodeName)
		}
	}

	policy := &nriv1alpha1.BalloonsPolicy{}
	err := r.Get(ctx, client.ObjectKey{Name: r.balloonsPolicyName(nodeName), Namespace: r.balloonsNamespace()}, policy)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// A concurrent delete still counts as removed.
	if err := r.Delete(ctx, policy); err != nil && !errors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

// SetupWithManager registers the reconciler; a singleton change re-evaluates every
// node. Parallel reconciles are safe: each touches only its own Node and CR.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &corev1.Pod{}, podNodeNameField,
		func(obj client.Object) []string {
			pod, ok := obj.(*corev1.Pod)
			if !ok || pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		},
	); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Watches(&cpopv1alpha1.BalloonsClusterConfig{}, r.enqueueAllNodesHandler()).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.reconcileConcurrency()}).
		Complete(r)
}

// enqueueAllNodesHandler fans a BalloonsClusterConfig event out to every node.
func (r *NodeReconciler) enqueueAllNodesHandler() handler.EventHandler {
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, _ event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.enqueueAllNodes(ctx, q)
		},
		UpdateFunc: func(ctx context.Context, _ event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.enqueueAllNodes(ctx, q)
		},
		DeleteFunc: func(ctx context.Context, _ event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.enqueueAllNodes(ctx, q)
		},
		GenericFunc: func(ctx context.Context, _ event.GenericEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.enqueueAllNodes(ctx, q)
		},
	}
}

// enqueueAllNodes enqueues every node in paced chunks. Fleet-wide, not just the
// pool: that is how an unlabelled node gets provisioned. It runs only on singleton
// events, never Node events.
func (r *NodeReconciler) enqueueAllNodes(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		r.Log.Error(err, "failed to list nodes for policy-triggered reconcile")
		return
	}
	r.Log.Info("BalloonsClusterConfig change: re-enqueuing all nodes",
		"nodeCount", len(nodes.Items),
		"chunkSize", enqueueBurstChunkSize,
		"chunkDelay", enqueueBurstChunkDelay.String())
	for i := range nodes.Items {
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: nodes.Items[i].Name}}
		delay := time.Duration(i/enqueueBurstChunkSize) * enqueueBurstChunkDelay
		if delay == 0 {
			q.Add(req)
			continue
		}
		q.AddAfter(req, delay)
	}
}
