// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
)

const (
	probeLabelKey = cpopv1alpha1.LabelPrefix + "probe-for"

	managedByLabelKey = cpopv1alpha1.LabelPrefix + "managed-by"
	managedByValue    = "cpu-pinning-operator"

	probeTimeout = 60 * time.Second
)

// Timing knobs. Vars so tests can shrink them without a multi-second sleep.
var (
	probePollInterval = 2 * time.Second
	// probeDeleteTimeout: racing a terminating predecessor leaks NotReady CRI sandboxes.
	probeDeleteTimeout = 30 * time.Second
)

// ErrProbePending is transient: requeue with backoff. Treating it as a hard
// failure hot-loops the create against the terminating pod.
var ErrProbePending = stderrors.New("topology probe pod still terminating")

// IsProbePending reports whether err is ErrProbePending.
func IsProbePending(err error) bool {
	return stderrors.Is(err, ErrProbePending)
}

// probeTolerations: unset/malformed means tolerate every taint so discovery reaches all nodes.
func probeTolerations() []corev1.Toleration {
	if v := os.Getenv("PROBE_TOLERATIONS"); v != "" {
		var tols []corev1.Toleration
		if err := json.Unmarshal([]byte(v), &tols); err == nil && len(tols) > 0 {
			return tols
		}
	}
	return []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
}

// probeImagePullSecrets is a JSON []corev1.LocalObjectReference, the manager's own
// pull secrets; unset or malformed means none.
func probeImagePullSecrets() []corev1.LocalObjectReference {
	v := os.Getenv("PROBE_IMAGE_PULL_SECRETS")
	if v == "" {
		return nil
	}
	var refs []corev1.LocalObjectReference
	if err := json.Unmarshal([]byte(v), &refs); err != nil {
		klog.ErrorS(err, "ignoring malformed PROBE_IMAGE_PULL_SECRETS; the probe pod will pull without a credential")
		return nil
	}
	return refs
}

// probeResources: unset, malformed or contradictory means the defaults below;
// requests are needed under a ResourceQuota or LimitRange.
func probeResources() corev1.ResourceRequirements {
	if v := os.Getenv("PROBE_RESOURCES"); v != "" {
		var res corev1.ResourceRequirements
		if err := json.Unmarshal([]byte(v), &res); err != nil {
			klog.ErrorS(err, "ignoring malformed PROBE_RESOURCES; using the built-in probe requests and limits")
		} else if len(res.Requests) > 0 || len(res.Limits) > 0 {
			if err := validProbeResources(res); err != nil {
				klog.ErrorS(err, "ignoring PROBE_RESOURCES; using the built-in probe requests and limits")
			} else {
				return res
			}
		}
	}
	// Sized like the init container: it reads sysfs once and exits.
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
}

// validProbeResources rejects a request above its limit, which the API server refuses.
func validProbeResources(res corev1.ResourceRequirements) error {
	for name, request := range res.Requests {
		limit, ok := res.Limits[name]
		if ok && request.Cmp(limit) > 0 {
			return fmt.Errorf("%s request %s exceeds its limit %s", name, request.String(), limit.String())
		}
	}
	return nil
}

// probeNamespace is the operator's own namespace (POD_NAMESPACE), else kube-system.
// The probe is unprivileged; the namespace needs only restricted PSS.
func probeNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return "kube-system"
}

// probeServiceAccount: empty admits under the namespace default SA (vanilla).
// OpenShift needs a named SA bound to a custom SCC — built-in SCCs reject the
// probe's fixed uid + seccomp combination.
func probeServiceAccount() string {
	return os.Getenv("PROBE_SERVICE_ACCOUNT")
}

func probePodName(nodeName string) string {
	return fmt.Sprintf("cpu-topo-probe-%s", nodeName)
}

// DiscoverNode runs an unprivileged probe pod, parses its output, and deletes it.
// A terminating predecessor returns ErrProbePending; an empty PROBE_IMAGE fails.
func DiscoverNode(ctx context.Context, c client.Client, logsGetter LogsGetter, nodeName string) (*NodeTopology, error) {
	// Never guess the image: another build could report a different machine.
	image := os.Getenv("PROBE_IMAGE")
	if image == "" {
		return nil, fmt.Errorf("PROBE_IMAGE is empty: set it to this operator's own image reference "+
			"(a chart install does) — node %s stays unprobed and out of the pinning pool", nodeName)
	}

	ns := probeNamespace()
	podName := probePodName(nodeName)

	if err := deleteProbePodAndWait(ctx, c, ns, podName); err != nil {
		return nil, err
	}

	pod := buildProbePod(ns, podName, nodeName, image)
	if err := c.Create(ctx, pod); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("%w: create raced a terminating probe pod for node %s", ErrProbePending, nodeName)
		}
		return nil, fmt.Errorf("failed to create probe pod: %w", err)
	}
	defer func() {
		// Best-effort; the next DiscoverNode reclaims a leftover pod.
		if err := c.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
			klog.ErrorS(err, "deleting topology probe pod", "namespace", ns, "pod", podName)
		}
	}()

	if err := waitForProbePod(ctx, c, ns, podName); err != nil {
		return nil, err
	}

	output, err := logsGetter.GetLogs(ctx, ns, podName)
	if err != nil {
		return nil, fmt.Errorf("failed to read probe pod logs: %w", err)
	}

	return ParseProbeOutput(output)
}

// LogsGetter reads a topology probe pod's logs.
type LogsGetter interface {
	GetLogs(ctx context.Context, namespace, podName string) (string, error)
}

func deleteProbePodAndWait(ctx context.Context, c client.Client, ns, podName string) error {
	existing := &corev1.Pod{}
	err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: podName}, existing)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to check existing probe pod: %w", err)
	}
	if err := c.Delete(ctx, existing); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete stale probe pod: %w", err)
	}

	deadline := time.Now().Add(probeDeleteTimeout)
	for {
		err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: podName}, existing)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to confirm stale probe pod deletion: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: stale probe pod %s not deleted within %s", ErrProbePending, podName, probeDeleteTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(probePollInterval):
		}
	}
}

// buildProbePod builds the probe pod for one node; image must be non-empty.
func buildProbePod(ns, podName, nodeName, image string) *corev1.Pod {
	runAsNonRoot := true
	runAsUser := int64(1000)
	allowPrivEsc := false
	readOnlyRootFS := true
	automountToken := false
	// A pinned uid: the chart's custom OpenShift SCC grants exactly this one.
	sc := &corev1.SecurityContext{
		RunAsNonRoot:             &runAsNonRoot,
		RunAsUser:                &runAsUser,
		AllowPrivilegeEscalation: &allowPrivEsc,
		ReadOnlyRootFilesystem:   &readOnlyRootFS,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			Labels: map[string]string{
				probeLabelKey:     nodeName,
				managedByLabelKey: managedByValue,
			},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,

			ServiceAccountName: probeServiceAccount(),
			// Same pull credentials as the manager.
			ImagePullSecrets: probeImagePullSecrets(),
			// The probe never calls the API; token off even under the default SA.
			AutomountServiceAccountToken: &automountToken,
			RestartPolicy:                corev1.RestartPolicyNever,
			Tolerations:                  probeTolerations(),
			Containers: []corev1.Container{
				{
					Name:            "probe",
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					// The image has no shell: the probe is a manager subcommand.
					Command:         []string{"/manager"},
					Args:            []string{ProbeSubcommandName},
					SecurityContext: sc,
					Resources:       probeResources(),
				},
			},
		},
	}
}

func waitForProbePod(ctx context.Context, c client.Client, ns, podName string) error {
	deadline := time.Now().Add(probeTimeout)
	pod := &corev1.Pod{}
	for {
		err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: podName}, pod)
		switch {
		case errors.IsNotFound(err):
			// Cached client lags Create; keep polling until the deadline.
		case err != nil:
			return fmt.Errorf("failed to get probe pod status: %w", err)
		case pod.Status.Phase == corev1.PodSucceeded:
			return nil
		case pod.Status.Phase == corev1.PodFailed:
			return fmt.Errorf("probe pod failed: %s", pod.Status.Message)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for probe pod %s to complete", podName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(probePollInterval):
		}
	}
}
