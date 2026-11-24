// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/classification"
	"github.com/intel/cpu-pinning-operator/internal/match"
)

// BalloonsClusterConfigReconciler reconciles the BalloonsClusterConfig singleton:
// it publishes the node-global config into the match Index, drives the webhook
// failurePolicy, and reports the pinning pool in status. CEL allows only the name
// "cluster"; any other name is "no config".
type BalloonsClusterConfigReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Log         logr.Logger
	PolicyIndex *match.Index
}

// The balloonsclusterconfigs get also covers the Node reconciler's ownerReference
// read. leases list/watch back the webhook's cached leader lookup; the election
// lock itself only gets, creates and updates.
//+kubebuilder:rbac:groups=cpu-pinning-operator.intel.com,resources=balloonsclusterconfigs,verbs=get;list;watch
//+kubebuilder:rbac:groups=cpu-pinning-operator.intel.com,resources=balloonsclusterconfigs/status,verbs=update
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update

func (r *BalloonsClusterConfigReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	cfg := &cpopv1alpha1.BalloonsClusterConfig{}
	err := r.Get(ctx, client.ObjectKey{Name: cpopv1alpha1.ClusterConfigName}, cfg)
	if errors.IsNotFound(err) {
		// No singleton: built-in defaults and the default failurePolicy.
		r.PolicyIndex.SetClusterConfig(match.ClusterConfig{})
		if err := r.reconcileFailurePolicy(ctx, defaultFailurePolicy); err != nil {
			r.Log.Error(err, "failed to reset webhook failurePolicy")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// No finalizer: per-node BalloonsPolicy CRs are GC'd via their ownerReference to
	// this singleton, and the chart's post-delete hook strips node labels.

	// Duplicates PolicyIndexPopulator's work; SetClusterConfig is mutex-guarded.
	r.PolicyIndex.SetClusterConfig(compileClusterConfig(cfg, r.Log))

	failurePolicy := defaultFailurePolicy
	if cfg.Spec.Webhook.FailurePolicy == string(admissionregistrationv1.Fail) {
		failurePolicy = admissionregistrationv1.Fail
	}
	if err := r.reconcileFailurePolicy(ctx, failurePolicy); err != nil {
		r.Log.Error(err, "failed to reconcile webhook failurePolicy")
		return ctrl.Result{}, err
	}

	if err := r.reconcileStatus(ctx, cfg); err != nil {
		r.Log.Error(err, "failed to reconcile BalloonsClusterConfig status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// compileClusterConfig derives the node-global config from the singleton. It is
// pure, shared with PolicyIndexPopulator; an invalid excludeNodeSelector is
// logged and dropped.
func compileClusterConfig(cfg *cpopv1alpha1.BalloonsClusterConfig, log logr.Logger) match.ClusterConfig {
	alloc := cfg.Spec.Balloon.CPUAllocation
	if alloc == "" {
		alloc = defaultCPUAllocation
	}

	packing := cfg.Spec.Balloon.Packing
	if packing == "" {
		packing = defaultPacking
	}

	htSiblingPolicy := cfg.Spec.Balloon.HTSiblingPolicy
	if htSiblingPolicy == "" {
		htSiblingPolicy = defaultHTSiblingPolicy
	}

	var excludeSelector labels.Selector
	if cfg.Spec.ExcludeNodeSelector != nil {
		sel, err := metav1.LabelSelectorAsSelector(cfg.Spec.ExcludeNodeSelector)
		if err != nil {
			log.Error(err, "invalid excludeNodeSelector; ignoring")
		} else {
			excludeSelector = sel
		}
	}

	return match.ClusterConfig{
		CPUAllocation: alloc,
		Packing:       packing,
		// Verbatim: nil means the operator default, an empty slice means none.
		ReservedNamespaces: cfg.Spec.Balloon.ReservedNamespaces,
		HTSiblingPolicy:    htSiblingPolicy,
		Reserved:           cfg.Spec.Balloon.Reserved,
		NodeOverrides:      compileNodeOverrides(cfg.Spec.NodeConfigOverrides, log),
		ExcludeNode:        excludeSelector,
		Classification:     compileClassification(cfg.Spec.Classification, log),
		ClassProfiles:      compileClassProfiles(cfg.Spec.ClassProfiles),
	}
}

// compileClassification parses the selectors once and fills defaults for an unset
// override label and zero threshold. An invalid selector stays nil and never matches.
func compileClassification(spec cpopv1alpha1.ClassificationSpec, log logr.Logger) classification.Spec {
	out := classification.Spec{
		OverrideLabel:        spec.OverrideLabel,
		CPUAMXMinLogicalCPUs: spec.CPUAMXMinLogicalCPUs,
	}
	if out.OverrideLabel == "" {
		out.OverrideLabel = classification.DefaultOverrideLabel
	}
	if out.CPUAMXMinLogicalCPUs <= 0 {
		out.CPUAMXMinLogicalCPUs = classification.DefaultCPUAMXMinLogicalCPUs
	}
	if spec.AMXNodeSelector != nil {
		if sel, err := metav1.LabelSelectorAsSelector(spec.AMXNodeSelector); err != nil {
			log.Error(err, "invalid classification amxNodeSelector; ignoring")
		} else {
			out.AMXSelector = sel
		}
	}
	if spec.GPUOnlyNodeSelector != nil {
		if sel, err := metav1.LabelSelectorAsSelector(spec.GPUOnlyNodeSelector); err != nil {
			log.Error(err, "invalid classification gpuOnlyNodeSelector; ignoring")
		} else {
			out.GPUOnlySelector = sel
		}
	}
	return out
}

// compileClassProfiles keeps only the per-node fields NodeConfigOverride honors;
// cluster-global knobs come from spec.balloon only.
func compileClassProfiles(profiles map[string]cpopv1alpha1.BalloonSpec) map[string]match.ClassProfile {
	if len(profiles) == 0 {
		return nil
	}
	out := make(map[string]match.ClassProfile, len(profiles))
	for class, spec := range profiles {
		out[class] = match.ClassProfile{
			CPUAllocation: spec.CPUAllocation,
			Reserved:      spec.Reserved,
		}
	}
	return out
}

// compileNodeOverrides parses each override's selector once, keeping order (first
// match wins downstream). Overrides with a nil, invalid or match-everything
// selector, or that set neither cpuAllocation nor reserved, are dropped: a
// match-everything one would shadow the default and every later override.
func compileNodeOverrides(overrides []cpopv1alpha1.NodeConfigOverride, log logr.Logger) []match.NodeConfigOverride {
	if len(overrides) == 0 {
		return nil
	}
	out := make([]match.NodeConfigOverride, 0, len(overrides))
	for i := range overrides {
		ov := &overrides[i]
		if ov.NodeSelector == nil {
			log.Info("nodeConfigOverride has no nodeSelector; ignoring", "index", i)
			continue
		}
		// packing is cluster-global, so it does not count as an override.
		if ov.Balloon.CPUAllocation == "" && ov.Balloon.Reserved == nil {
			log.Info("nodeConfigOverride overrides nothing (no cpuAllocation or reserved); ignoring", "index", i)
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(ov.NodeSelector)
		if err != nil {
			log.Error(err, "invalid nodeConfigOverride nodeSelector; ignoring", "index", i)
			continue
		}

		if sel.Empty() {
			log.Info("nodeConfigOverride nodeSelector is empty (matches every node); ignoring", "index", i)
			continue
		}
		out = append(out, match.NodeConfigOverride{
			Selector:      sel,
			CPUAllocation: ov.Balloon.CPUAllocation,
			Reserved:      ov.Balloon.Reserved,
		})
	}
	return out
}

// reconcileStatus writes the pinning-pool inventory to the singleton's status.
func (r *BalloonsClusterConfigReconciler) reconcileStatus(ctx context.Context, cfg *cpopv1alpha1.BalloonsClusterConfig) error {
	perNode, cpuManagerConflicts, cpuManagerUnread, err := r.provisionedNodes(ctx)
	if err != nil {
		return err
	}

	ready := poolReadyCondition(len(perNode))

	// Re-Get right before the Update to avoid a resourceVersion conflict.
	fresh := &cpopv1alpha1.BalloonsClusterConfig{}
	if err := r.Get(ctx, client.ObjectKey{Name: cfg.Name}, fresh); err != nil {
		return client.IgnoreNotFound(err)
	}
	fresh.Status.ObservedGeneration = fresh.Generation
	fresh.Status.ProvisionedNodes = len(perNode)
	fresh.Status.PerNode = perNode
	meta.SetStatusCondition(&fresh.Status.Conditions, ready)
	meta.SetStatusCondition(&fresh.Status.Conditions, excludeSelectorCondition(fresh))
	meta.SetStatusCondition(&fresh.Status.Conditions, cpuManagerCondition(cpuManagerConflicts, cpuManagerUnread))
	return r.Status().Update(ctx, fresh)
}

// conflictExampleLimit caps the node names the CPUManagerCompatible message lists;
// the count is always complete.
const conflictExampleLimit = 3

// cpuManagerCondition reports provisioned nodes whose kubelet runs the CPU Manager
// static policy. They stay in the pool; this is where an admin sees placement there
// is unpredictable. Any unread node makes it Unknown rather than a guessed True; a
// conflict outranks unread nodes.
func cpuManagerCondition(conflicting []string, unread int) metav1.Condition {
	c := metav1.Condition{
		Type:    "CPUManagerCompatible",
		Status:  metav1.ConditionTrue,
		Reason:  "NoConflictDetected",
		Message: "every provisioned node's kubelet CPU Manager policy was read and none runs the static policy",
	}
	if len(conflicting) == 0 {
		if unread > 0 {
			c.Status = metav1.ConditionUnknown
			c.Reason = "PolicyUnread"
			c.Message = fmt.Sprintf("the kubelet CPU Manager policy could not be read for %d provisioned node(s), so a conflict with the "+
				"NRI balloons plugin cannot be ruled out there; those nodes are still provisioned and pinned. Either the "+
				"kube-system/kubelet-config ConfigMap kubeadm provisions is absent or unreadable, or it names a policy this operator does "+
				"not recognise — a KubeletConfiguration that simply omits cpuManagerPolicy reads as the default, none. Check "+
				"cpuManagerPolicy on the nodes themselves.", unread)
		}
		return c
	}
	examples := conflicting
	if len(examples) > conflictExampleLimit {
		examples = examples[:conflictExampleLimit]
	}
	c.Status = metav1.ConditionFalse
	c.Reason = "CPUManagerStatic"
	c.Message = fmt.Sprintf("%d provisioned node(s) run the kubelet CPU Manager static policy alongside the NRI balloons plugin, "+
		"e.g. %s; the kubelet and the plugin both allocate cpusets there, so placement on those nodes is unpredictable. "+
		"They are still provisioned: set their kubelet to --cpu-manager-policy=none, or exclude them from the pinning pool.",
		len(conflicting), strings.Join(examples, ", "))
	return c
}

// excludeSelectorCondition surfaces an invalid spec.excludeNodeSelector, which is
// dropped and so fails open. Echoing the parse error is safe: it is the admin's
// own selector.
func excludeSelectorCondition(cfg *cpopv1alpha1.BalloonsClusterConfig) metav1.Condition {
	c := metav1.Condition{
		Type:    "ExcludeNodeSelectorValid",
		Status:  metav1.ConditionTrue,
		Reason:  "Valid",
		Message: "spec.excludeNodeSelector is valid",
	}
	if cfg.Spec.ExcludeNodeSelector != nil {
		if _, err := metav1.LabelSelectorAsSelector(cfg.Spec.ExcludeNodeSelector); err != nil {
			c.Status = metav1.ConditionFalse
			c.Reason = "InvalidSelector"
			c.Message = fmt.Sprintf("spec.excludeNodeSelector is invalid and was ignored; "+
				"the matching nodes are NOT excluded from the pinning pool: %v", err)
		}
	}
	return c
}

// provisionedNodes returns status for every provisioned node sorted by name, plus
// the CPU Manager conflicting names and unread count, from one list so status and
// condition agree.
func (r *BalloonsClusterConfigReconciler) provisionedNodes(ctx context.Context) (perNode []cpopv1alpha1.NodeStatus, cpuManagerConflicts []string, cpuManagerUnread int, err error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, provisionedNodesOnly()); err != nil {
		return nil, nil, 0, err
	}
	perNode = make([]cpopv1alpha1.NodeStatus, 0, len(nodes.Items))
	for i := range nodes.Items {
		n := &nodes.Items[i]
		// Absent label means unread, counted rather than folded into compatible.
		switch n.Labels[LabelCPUManagerConflict] {
		case "true":
			cpuManagerConflicts = append(cpuManagerConflicts, n.Name)
		case "false":
		default:
			cpuManagerUnread++
		}
		perNode = append(perNode, cpopv1alpha1.NodeStatus{
			Name:         n.Name,
			MaxSpan:      labelInt(n.Labels, LabelMaxSpan),
			NumaNodes:    labelInt(n.Labels, LabelNumaNodes),
			PhysicalCPUs: labelInt(n.Labels, LabelPhysicalCPUs),
			// Effective per-node values (post-override, post-clamp) from the Node's labels.
			ReservedCPUs:  labelInt(n.Labels, LabelReservedCPUs),
			CPUAllocation: n.Labels[LabelCPUAllocation],
			Features:      nodeFeatures(n.Labels),
		})
	}
	sort.Slice(perNode, func(i, j int) bool { return perNode[i].Name < perNode[j].Name })
	// Sorted so the truncated example list does not churn the condition message.
	sort.Strings(cpuManagerConflicts)
	return perNode, cpuManagerConflicts, cpuManagerUnread, nil
}

// nodeFeatureLabels lists the operator-managed feature labels copied to status;
// a new feature only needs its constant here.
var nodeFeatureLabels = []string{LabelAMX}

// nodeFeatures projects the feature labels present on a node.
func nodeFeatures(nodeLabels map[string]string) map[string]string {
	out := map[string]string{}
	for _, key := range nodeFeatureLabels {
		if v, ok := nodeLabels[key]; ok && v != "" {
			out[key] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// poolReadyCondition builds the Ready condition: True when the pool has a node.
func poolReadyCondition(nodeCount int) metav1.Condition {
	ready := metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "NodesProvisioned",
		Message: fmt.Sprintf("%d node(s) provisioned for CPU pinning", nodeCount),
	}
	if nodeCount == 0 {
		ready.Status = metav1.ConditionFalse
		ready.Reason = "NoNodesProvisioned"
		ready.Message = "no balloons-enabled nodes in the pinning pool yet"
	}
	return ready
}

// reconcileFailurePolicy writes only failurePolicy, read-modify-write with an
// optimistic lock. The BalloonPinningPolicy reconciler owns the sibling
// namespaceSelector; neither may use Server-Side Apply (see the note there).
func (r *BalloonsClusterConfigReconciler) reconcileFailurePolicy(ctx context.Context, failurePolicy admissionregistrationv1.FailurePolicyType) error {
	cfg := &admissionregistrationv1.MutatingWebhookConfiguration{}
	err := r.Get(ctx, client.ObjectKey{Name: webhookConfigName()}, cfg)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// Only edit an existing entry; never synthesize one the static manifests own.
	wh, ok := webhookEntry(cfg, webhookName)
	if !ok {
		return nil
	}

	if wh.FailurePolicy != nil && *wh.FailurePolicy == failurePolicy {
		// Already in sync; skip the write.
		return nil
	}

	// Patch, not Update, so RBAC needs only get+patch.
	patch := client.MergeFromWithOptions(cfg.DeepCopy(), client.MergeFromWithOptimisticLock{})
	fp := failurePolicy
	wh.FailurePolicy = &fp

	return r.Patch(ctx, cfg, patch)
}

// SetupWithManager registers the reconciler; the Node watch refreshes pool status.
func (r *BalloonsClusterConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cpopv1alpha1.BalloonsClusterConfig{}).
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueSingleton)).
		Complete(r)
}

// enqueueSingleton maps every event to the singleton.
func (r *BalloonsClusterConfigReconciler) enqueueSingleton(context.Context, client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: cpopv1alpha1.ClusterConfigName}}}
}
