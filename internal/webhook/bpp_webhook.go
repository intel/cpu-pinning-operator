// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/match"
)

// failurePolicy is Fail: a safety guard must not be bypassed while it is down. The chart's
// validatingwebhook.yaml mirrors this shape (a drift guard compares them), except that it
// forces Ignore with neither cert-manager nor webhook.caBundle, whose placeholder caBundle
// cannot authenticate calls.
//+kubebuilder:webhook:path=/validate-v1alpha1-balloonpinningpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=cpu-pinning-operator.intel.com,resources=balloonpinningpolicies,verbs=delete;update,versions=v1alpha1,name=vbpp.kb.io,admissionReviewVersions=v1,timeoutSeconds=10

// Generic admission-error messages; raw errors (which can carry RBAC/apiserver detail) are
// logged server-side only.
var (
	errCouldNotDecodePolicy = errors.New("could not decode the BalloonPinningPolicy")
	errPolicyGuardInternal  = errors.New("internal error validating the BalloonPinningPolicy")
)

// PolicyGuard is a validating webhook for guardActivePods policies. It blocks DELETE while
// pods depend on the policy, and UPDATE that would orphan a bound pod or that touches a
// policy whose status.boundPods is capped. Unsetting guardActivePods is always allowed.
// A pinned pod keeps its cpuset when its policy goes, but nothing re-pins or drift-checks it.
type PolicyGuard struct {
	// Client must be uncached (the API reader): a lagging cache can report live pods NotFound.
	Client  client.Reader
	Log     logr.Logger
	decoder admission.Decoder
}

// InjectDecoder receives the admission decoder.
func (g *PolicyGuard) InjectDecoder(d admission.Decoder) error {
	g.decoder = d
	return nil
}

// Handle validates a BalloonPinningPolicy DELETE or UPDATE.
func (g *PolicyGuard) Handle(ctx context.Context, req admission.Request) admission.Response {
	if g.decoder == nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("decoder not initialized"))
	}
	switch req.Operation {
	case admissionv1.Delete:
		return g.handleDelete(req)
	case admissionv1.Update:
		return g.handleUpdate(ctx, req)
	default:
		return admission.Allowed("operation not guarded")
	}
}

// handleDelete rejects deleting a guarded policy that still holds pods.
func (g *PolicyGuard) handleDelete(req admission.Request) admission.Response {
	// On DELETE the object is in OldObject.
	old := &cpopv1alpha1.BalloonPinningPolicy{}
	if err := g.decoder.DecodeRaw(req.OldObject, old); err != nil {

		g.Log.Error(err, "failed to decode BalloonPinningPolicy from delete request")
		return admission.Errored(http.StatusBadRequest, errCouldNotDecodePolicy)
	}
	log := g.Log.WithValues("policy", old.Name, "operation", "delete")

	if !guardEnabled(old.Spec.GuardActivePods) {
		return admission.Allowed("guardActivePods not enabled")
	}
	held := old.Status.BoundPodsTotal
	if held == 0 {
		return admission.Allowed("no dependent pods")
	}

	log.Info("blocking delete of guarded policy with dependent pods", "boundPods", held)
	return admission.Denied(fmt.Sprintf(
		"BalloonPinningPolicy %q has spec.guardActivePods enabled and still holds %d pinned pod(s) (%s). "+
			"Deleting it would silently orphan them. Remove the dependent pods first, or set spec.guardActivePods=false to release the guard, then delete.",
		old.Name, held, joinBoundPods(old.Status.BoundPods, held)))
}

// handleUpdate rejects an update that narrows a guarded policy away from a held pod.
func (g *PolicyGuard) handleUpdate(ctx context.Context, req admission.Request) admission.Response {
	old := &cpopv1alpha1.BalloonPinningPolicy{}
	if err := g.decoder.DecodeRaw(req.OldObject, old); err != nil {
		g.Log.Error(err, "failed to decode old BalloonPinningPolicy from update request")
		return admission.Errored(http.StatusBadRequest, errCouldNotDecodePolicy)
	}
	updated := &cpopv1alpha1.BalloonPinningPolicy{}
	if err := g.decoder.Decode(req, updated); err != nil {
		g.Log.Error(err, "failed to decode updated BalloonPinningPolicy from update request")
		return admission.Errored(http.StatusBadRequest, errCouldNotDecodePolicy)
	}
	log := g.Log.WithValues("policy", old.Name, "operation", "update")

	// Guard is keyed on the old spec.
	if !guardEnabled(old.Spec.GuardActivePods) {
		return admission.Allowed("guardActivePods not enabled")
	}

	if !guardEnabled(updated.Spec.GuardActivePods) {
		return admission.Allowed("guardActivePods being disabled")
	}
	if old.Status.BoundPodsTotal == 0 {
		return admission.Allowed("no dependent pods")
	}

	// A bound pod the new spec no longer covers means narrowing.
	rule, err := g.compileRule(ctx, updated)
	if err != nil {
		log.Error(err, "failed to compile match rule for updated policy")
		return admission.Errored(http.StatusInternalServerError, errPolicyGuardInternal)
	}
	idx := match.NewIndex()
	idx.Update([]match.PolicyRule{rule})

	var orphaned []cpopv1alpha1.BoundPod
	for _, bp := range old.Status.BoundPods {
		pod := &corev1.Pod{}
		if err := g.Client.Get(ctx, client.ObjectKey{Namespace: bp.Namespace, Name: bp.Name}, pod); err != nil {
			if apierrors.IsNotFound(err) {
				// Gone pods hold no balloon to orphan.
				continue
			}

			log.Error(err, "failed to get dependent pod", "pod", bp.Namespace+"/"+bp.Name)
			return admission.Errored(http.StatusInternalServerError, errPolicyGuardInternal)
		}
		// status.boundPods lags; skip terminating/finished pods.
		if pod.DeletionTimestamp != nil ||
			pod.Status.Phase == corev1.PodSucceeded ||
			pod.Status.Phase == corev1.PodFailed {
			continue
		}
		// Unmatched or ambiguous matches are orphaned.
		res := idx.Match(pod)
		if res.Rule == nil || res.Ambiguous {
			orphaned = append(orphaned, bp)
		}
	}
	if len(orphaned) > 0 {
		log.Info("blocking narrowing update of guarded policy", "orphanedPods", len(orphaned))
		return admission.Denied(fmt.Sprintf(
			"BalloonPinningPolicy %q has spec.guardActivePods enabled and this update would stop matching %d pinned pod(s) it currently holds (%s). "+
				"Narrowing the match/namespaces would silently orphan them. Remove those pods first, or set spec.guardActivePods=false to release the guard, then apply the change.",
			old.Name, len(orphaned), joinBoundPods(orphaned, len(orphaned))))
	}

	// boundPods is capped for etcd: elided pods cannot be verified, so fail closed.
	held := old.Status.BoundPodsTotal
	if held > len(old.Status.BoundPods) {
		log.Info("blocking update of guarded policy with a capped bound-pod list",
			"boundPodsTotal", held, "retained", len(old.Status.BoundPods))
		retained := len(old.Status.BoundPods)
		return admission.Denied(fmt.Sprintf(
			"BalloonPinningPolicy %q has spec.guardActivePods enabled and holds %d pinned pod(s), more than the %d retained in status.boundPods. "+
				"The guard cannot verify this update keeps matching the elided pod(s), so it is rejected to avoid silently orphaning them. "+
				"Set spec.guardActivePods=false to release the guard (then apply the change), or split the policy so no single policy holds more than %d pods.",
			old.Name, held, retained, retained))
	}

	return admission.Allowed("update does not narrow away from any dependent pod")
}

// compileRule builds the policy's match rule, resolving namespaceSelector as the reconciler does.
func (g *PolicyGuard) compileRule(ctx context.Context, policy *cpopv1alpha1.BalloonPinningPolicy) (match.PolicyRule, error) {
	namespaces := nonEmptyNamespaces(policy.Spec.Namespaces)
	if policy.Spec.NamespaceSelector != nil {
		matched, err := g.selectorNamespaces(ctx, policy.Spec.NamespaceSelector)
		if err != nil {
			return match.PolicyRule{}, err
		}
		namespaces = append(namespaces, matched...)
	}
	containerSelection := policy.Spec.Match.ContainerSelection
	if containerSelection == "" {
		containerSelection = defaultContainerSelection
	}
	return match.PolicyRule{
		Name:               policy.Name,
		Namespaces:         namespaces,
		ByContainerName:    policy.Spec.Match.ByContainerName,
		ByPodLabels:        policy.Spec.Match.ByPodLabels,
		ContainerSelection: containerSelection,
	}, nil
}

// selectorNamespaces resolves a namespaceSelector; invalid selectors match nothing, as in the reconciler.
func (g *PolicyGuard) selectorNamespaces(ctx context.Context, ls *metav1.LabelSelector) ([]string, error) {
	sel, err := metav1.LabelSelectorAsSelector(ls)
	if err != nil {

		g.Log.Error(err, "invalid namespaceSelector — treating it as matching no namespace", "namespaceSelector", ls)
		return nil, nil
	}
	var nsList corev1.NamespaceList
	if err := g.Client.List(ctx, &nsList, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(nsList.Items))
	for i := range nsList.Items {
		out = append(out, nsList.Items[i].Name)
	}
	return out, nil
}

// defaultContainerSelection mirrors the reconciler's default.
const defaultContainerSelection = "wholePod"

// guardEnabled reports whether the opt-in guard is active.
func guardEnabled(p *bool) bool {
	return p != nil && *p
}

// nonEmptyNamespaces drops empty entries, mirroring the reconciler.
func nonEmptyNamespaces(namespaces []string) []string {
	var out []string
	for _, ns := range namespaces {
		if ns != "" {
			out = append(out, ns)
		}
	}
	return out
}

// maxListedBoundPods caps pod names in a denial message; the rest become "+N more".
const maxListedBoundPods = 10

// joinBoundPods renders pods for a denial message, truncated at maxListedBoundPods; total is
// the count reported, which can exceed len(pods) for a capped list.
func joinBoundPods(pods []cpopv1alpha1.BoundPod, total int) string {
	shown := pods
	if len(shown) > maxListedBoundPods {
		shown = shown[:maxListedBoundPods]
	}
	out := ""
	for i := range shown {
		if i > 0 {
			out += ", "
		}
		out += shown[i].Namespace + "/" + shown[i].Name
	}
	if extra := total - len(shown); extra > 0 {
		out += fmt.Sprintf(", +%d more", extra)
	}
	return out
}
