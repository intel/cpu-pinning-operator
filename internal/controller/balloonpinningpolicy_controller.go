// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"os"
	"sort"
	"strconv"

	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
)

const (
	// defaultWebhookConfigName is the webhook config name for a release named
	// cpu-pinning-operator; the chart passes the real one via WEBHOOK_CONFIG_NAME. The
	// patch RBAC marker's resourceNames must carry this value.
	defaultWebhookConfigName = "cpu-pinning-operator-mutating-webhook"
	webhookName              = "mpod.kb.io"

	// Two reconcilers write disjoint fields of the same webhook config: this one
	// Webhooks[].NamespaceSelector, BalloonsClusterConfig Webhooks[].FailurePolicy.
	// Both read-modify-write, not SSA: the Webhooks list is atomic under SSA, so a
	// minimal apply would drop the sibling field, rules and clientConfig.

	defaultCPUAllocation      = "physical-cores"
	defaultPacking            = "spread"
	defaultHTSiblingPolicy    = "exclude"
	defaultFailurePolicy      = admissionregistrationv1.Ignore
	defaultDriftPolicy        = "eventsOnly"
	defaultCapacityCheckMode  = "warn"
	defaultContainerSelection = "wholePod"

	// PodPinnedByField is a synthetic field index over the pinned-by annotation, so
	// listing pinned pods scales with their count, not every pod. Also used by the
	// webhook's reserve-authority warm-up.
	PodPinnedByField = ".metadata.annotations.pinned-by"

	// maxBoundPods caps status.boundPods (first by namespace, name) so a broad policy
	// cannot push the object past etcd's size limit; boundPodsTotal stays uncapped.
	maxBoundPods = 100
)

// indexPodByPinnedBy lifts a pod's pinned-by annotation into PodPinnedByField.
func indexPodByPinnedBy(obj client.Object) []string {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	if v := pod.Annotations[pinnedByAnnotation]; v != "" {
		return []string{v}
	}
	return nil
}

// webhookConfigName returns WEBHOOK_CONFIG_NAME, or the default.
func webhookConfigName() string {
	if v := os.Getenv("WEBHOOK_CONFIG_NAME"); v != "" {
		return v
	}
	return defaultWebhookConfigName
}

// BalloonPinningPolicyReconciler rebuilds the match Index from all policies and
// reconciles the webhook's namespaceSelector from the union of their namespaces.
type BalloonPinningPolicyReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Log         logr.Logger
	PolicyIndex *match.Index
}

// namespaces is list-only: patterns expand against the live list. events: the
// recorder creates then patches. mutatingwebhookconfigurations get/list/watch stay
// unscoped for the readiness-CA reconciler's cache; patch is pinned by
// resourceNames to defaultWebhookConfigName.
//+kubebuilder:rbac:groups=cpu-pinning-operator.intel.com,resources=balloonpinningpolicies,verbs=get;list;watch
//+kubebuilder:rbac:groups=cpu-pinning-operator.intel.com,resources=balloonpinningpolicies/status,verbs=update
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;list;watch
//+kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=patch,resourceNames=cpu-pinning-operator-mutating-webhook

func (r *BalloonPinningPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var list cpopv1alpha1.BalloonPinningPolicyList
	if err := r.List(ctx, &list); err != nil {
		return ctrl.Result{}, err
	}

	rules, namespaces, err := compileRules(ctx, r.Client, list.Items, r.Log)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Duplicates PolicyIndexPopulator's work; Index.Update is mutex-guarded.
	r.PolicyIndex.Update(rules)

	// The webhook's metadata.name In[...] selector cannot express globs, so expand them.
	selectorNS, err := r.expandNamespaces(ctx, namespaces)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileWebhookNamespaceSelector(ctx, dedupStrings(selectorNS)); err != nil {
		r.Log.Error(err, "failed to reconcile MutatingWebhookConfiguration namespaceSelector")
		return ctrl.Result{}, err
	}

	if err := r.reconcileStatus(ctx, list.Items); err != nil {
		r.Log.Error(err, "failed to reconcile BalloonPinningPolicy status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// compileRules derives the match rules and the union of their namespaces. It is
// pure, shared with PolicyIndexPopulator; a policy with a bad intent is skipped.
func compileRules(ctx context.Context, c client.Client, policies []cpopv1alpha1.BalloonPinningPolicy, log logr.Logger) (rules []match.PolicyRule, namespaces []string, err error) {
	rules = make([]match.PolicyRule, 0, len(policies))

	for _, policy := range policies {
		// Raw patterns stay on the rule for the request-time isolation check.
		policyNS := nonEmpty(policy.Spec.Namespaces)
		// Selector matches resolve to names so the webhook gate and the isolation filter
		// see the same set.
		if policy.Spec.NamespaceSelector != nil {
			matched, err := selectorNamespaces(ctx, c, log, policy.Name, policy.Spec.NamespaceSelector)
			if err != nil {
				return nil, nil, err
			}
			policyNS = dedupStrings(append(policyNS, matched...))
		}
		// Unset EnforcePinning means true.
		enforce := policy.Spec.Scheduling.EnforcePinning == nil || *policy.Spec.Scheduling.EnforcePinning
		capacityCheck := policy.Spec.CapacityCheck.Mode
		if capacityCheck == "" {
			capacityCheck = defaultCapacityCheckMode
		}
		drift := policy.Spec.DriftPolicy
		if drift == "" {
			drift = defaultDriftPolicy
		}
		containerSelection := policy.Spec.Match.ContainerSelection
		if containerSelection == "" {
			containerSelection = defaultContainerSelection
		}
		intent, err := match.CompileIntent(policy.Spec.Intent)
		if err != nil {
			// Bad argRegex: drop this rule, keep the others live.
			log.Error(err, "invalid intent spec — dropping rule for this reconcile",
				"policy", policy.Name)
			continue
		}
		rules = append(rules, match.PolicyRule{
			Name:               policy.Name,
			Namespaces:         policyNS,
			ByContainerName:    policy.Spec.Match.ByContainerName,
			ByPodLabels:        policy.Spec.Match.ByPodLabels,
			ContainerSelection: containerSelection,
			EnforcePinning:     enforce,
			NodeFeatures:       policy.Spec.Scheduling.NodeFeatures,
			Intent:             intent,
			CapacityCheckMode:  capacityCheck,
			DriftPolicy:        drift,
		})
		namespaces = append(namespaces, policyNS...)
	}

	return rules, namespaces, nil
}

// reconcileStatus sets each policy's Ready condition and boundPods.
func (r *BalloonPinningPolicyReconciler) reconcileStatus(ctx context.Context, policies []cpopv1alpha1.BalloonPinningPolicy) error {
	nodeCount, err := r.provisionedNodeCount(ctx)
	if err != nil {
		return err
	}
	ready := poolReadyCondition(nodeCount)

	boundPods, boundPodsTotal, err := r.boundPodsByPolicy(ctx)
	if err != nil {
		return err
	}

	for i := range policies {
		name := policies[i].Name
		// Re-fetch: the listed copy may be stale and conflict on Update.
		fresh := &cpopv1alpha1.BalloonPinningPolicy{}
		if err := r.Get(ctx, client.ObjectKey{Name: name}, fresh); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		fresh.Status.ObservedGeneration = fresh.Generation
		meta.SetStatusCondition(&fresh.Status.Conditions, ready)
		fresh.Status.BoundPods = boundPods[name]
		fresh.Status.BoundPodsTotal = boundPodsTotal[name]
		if err := r.Status().Update(ctx, fresh); err != nil {
			return err
		}
	}
	return nil
}

// boundPodsByPolicy groups pinned pods by owning policy, returning capped slices
// and uncapped counts. It uses the same Index match and BalloonNameForPod as the
// webhook and drift reconciler.
func (r *BalloonPinningPolicyReconciler) boundPodsByPolicy(ctx context.Context) (map[string][]cpopv1alpha1.BoundPod, map[string]int, error) {
	// Narrowed by the pinned-by field index.
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{PodPinnedByField: pinnedByValue}); err != nil {
		return nil, nil, err
	}

	// Memoize node max-span lookups.
	maxSpanByNode := map[string]int{}
	nodeMaxSpan := func(nodeName string) (int, error) {
		if v, ok := maxSpanByNode[nodeName]; ok {
			return v, nil
		}
		node := &corev1.Node{}
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
			if errors.IsNotFound(err) {
				maxSpanByNode[nodeName] = 0
				return 0, nil
			}
			return 0, err
		}
		span := labelInt(node.Labels, LabelMaxSpan)
		maxSpanByNode[nodeName] = span
		return span, nil
	}

	out := map[string][]cpopv1alpha1.BoundPod{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[pinnedByAnnotation] != pinnedByValue {
			continue
		}
		// Terminating or finished pods are releasing their cpuset; Pending ones still hold it.
		if pod.DeletionTimestamp != nil ||
			pod.Status.Phase == corev1.PodSucceeded ||
			pod.Status.Phase == corev1.PodFailed {
			continue
		}
		// Unscheduled: no balloon, and no max span to name one.
		if pod.Spec.NodeName == "" {
			continue
		}
		res := r.PolicyIndex.Match(pod)
		if res.Rule == nil || res.Rule.Name == "" || res.Ambiguous {
			// Unmatched, unattributable or ambiguous (skipped at admission).
			continue
		}
		rule, container := res.Rule, res.Container
		span := 8
		if pod.Spec.NodeName != "" {
			ms, err := nodeMaxSpan(pod.Spec.NodeName)
			if err != nil {
				return nil, nil, err
			}
			// No max-span label: keep the largest span, as the drift reconciler does.
			if ms > 0 {
				span = ms
			}
		}
		out[rule.Name] = append(out[rule.Name], cpopv1alpha1.BoundPod{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Balloon:   match.BalloonNameForPod(pod, container, rule.Intent, span),
		})
	}

	// Sort before capping so the kept set is stable across reconciles.
	totals := map[string]int{}
	for name := range out {
		pods := out[name]
		sort.Slice(pods, func(a, b int) bool {
			if pods[a].Namespace != pods[b].Namespace {
				return pods[a].Namespace < pods[b].Namespace
			}
			return pods[a].Name < pods[b].Name
		})
		totals[name] = len(pods)
		if len(pods) > maxBoundPods {
			out[name] = pods[:maxBoundPods]
		}
	}
	return out, totals, nil
}

// provisionedNodeCount counts the nodes in the pinning pool.
func (r *BalloonPinningPolicyReconciler) provisionedNodeCount(ctx context.Context) (int, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, provisionedNodesOnly()); err != nil {
		return 0, err
	}
	return len(nodes.Items), nil
}

// labelInt parses an integer node label, 0 when absent or invalid.
func labelInt(labels map[string]string, key string) int {
	if v, ok := labels[key]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// dedupStrings returns the input with duplicates removed, order preserved.
func dedupStrings(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// nonEmpty drops empty entries, preserving order.
func nonEmpty(namespaces []string) []string {
	var out []string
	for _, ns := range namespaces {
		if ns != "" {
			out = append(out, ns)
		}
	}
	return out
}

// expandNamespaces resolves patterns to concrete names: exact names pass through,
// globs match the live list. A new matching namespace is picked up on the next
// Namespace-triggered reconcile.
func (r *BalloonPinningPolicyReconciler) expandNamespaces(ctx context.Context, patterns []string) ([]string, error) {
	hasGlob := false
	for _, p := range patterns {
		if match.IsGlob(p) {
			hasGlob = true
			break
		}
	}

	var nsList corev1.NamespaceList
	if hasGlob {
		if err := r.List(ctx, &nsList); err != nil {
			return nil, err
		}
	}

	var out []string
	for _, p := range patterns {
		if !match.IsGlob(p) {
			out = append(out, p)
			continue
		}
		for i := range nsList.Items {
			if name := nsList.Items[i].Name; match.NamespaceMatches(p, name) {
				out = append(out, name)
			}
		}
	}
	return out, nil
}

// selectorNamespaces resolves a namespaceSelector to matching namespace names. An
// empty selector matches all; an invalid one matches none (logged, not fatal).
func selectorNamespaces(ctx context.Context, c client.Client, log logr.Logger, policyName string, ls *metav1.LabelSelector) ([]string, error) {
	sel, err := metav1.LabelSelectorAsSelector(ls)
	if err != nil {
		log.Error(err, "invalid namespaceSelector — matching no namespaces for this reconcile",
			"policy", policyName)
		return nil, nil
	}
	var nsList corev1.NamespaceList
	if err := c.List(ctx, &nsList, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(nsList.Items))
	for i := range nsList.Items {
		out = append(out, nsList.Items[i].Name)
	}
	return out, nil
}

// namespaceNameSelector matches exactly the given names via kubernetes.io/metadata.name.
// In[] cannot be empty and an empty selector matches everything, so no names
// becomes DoesNotExist on that label, which every namespace carries.
func namespaceNameSelector(namespaces []string) *metav1.LabelSelector {
	const nameLabel = "kubernetes.io/metadata.name"
	if len(namespaces) == 0 {
		return &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      nameLabel,
					Operator: metav1.LabelSelectorOpDoesNotExist,
				},
			},
		}
	}
	return &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      nameLabel,
				Operator: metav1.LabelSelectorOpIn,
				Values:   namespaces,
			},
		},
	}
}

// webhookEntry returns a pointer aliasing the named entry in cfg.Webhooks.
func webhookEntry(cfg *admissionregistrationv1.MutatingWebhookConfiguration, name string) (*admissionregistrationv1.MutatingWebhook, bool) {
	for i := range cfg.Webhooks {
		if cfg.Webhooks[i].Name == name {
			return &cfg.Webhooks[i], true
		}
	}
	return nil, false
}

// reconcileWebhookNamespaceSelector writes only the webhook's namespaceSelector,
// read-modify-write with an optimistic lock (see the SSA note on the constants).
func (r *BalloonPinningPolicyReconciler) reconcileWebhookNamespaceSelector(ctx context.Context, namespaces []string) error {
	cfg := &admissionregistrationv1.MutatingWebhookConfiguration{}
	err := r.Get(ctx, client.ObjectKey{Name: webhookConfigName()}, cfg)
	if errors.IsNotFound(err) {
		// Installed by static manifests; nothing to reconcile yet.
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

	selector := namespaceNameSelector(namespaces)
	if apiequality.Semantic.DeepEqual(wh.NamespaceSelector, selector) {
		// Already in sync; skip the write.
		return nil
	}

	// Patch, not Update, so RBAC needs only get+patch.
	patch := client.MergeFromWithOptions(cfg.DeepCopy(), client.MergeFromWithOptimisticLock{})
	wh.NamespaceSelector = selector

	return r.Patch(ctx, cfg, patch)
}

// SetupWithManager registers the reconciler and the pinned-by field index. Node,
// Namespace and pinned-Pod events each trigger a full reconcile.
func (r *BalloonPinningPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Pinned-by index for boundPodsByPolicy.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &corev1.Pod{}, PodPinnedByField, indexPodByPinnedBy); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&cpopv1alpha1.BalloonPinningPolicy{}).
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueClusterReconcile)).
		Watches(&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueClusterReconcile)).
		// Only pinned pods enqueue; others never appear in boundPods.
		Watches(&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.enqueuePinnedPodReconcile)).
		Complete(r)
}

// enqueuePinnedPodReconcile enqueues a cluster reconcile for operator-pinned pods only.
func (r *BalloonPinningPolicyReconciler) enqueuePinnedPodReconcile(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Annotations[pinnedByAnnotation] != pinnedByValue {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "cluster-change"}}}
}

// enqueueClusterReconcile enqueues the one cluster-wide reconcile key.
func (r *BalloonPinningPolicyReconciler) enqueueClusterReconcile(ctx context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "cluster-change"}}}
}
