// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/readiness"
)

// injectNodeAffinity AND-s enforcePinning and nodeFeatures terms (require: required,
// prefer: preferred weight 100, ignore: skip) into existing affinity. Keys are sorted
// so the pod spec, and so the drift hash, is deterministic.
func injectNodeAffinity(pod *corev1.Pod, enforcePinning bool, nodeFeatures map[string]cpopv1alpha1.NodeFeatureMode) {
	var required []corev1.NodeSelectorRequirement
	if enforcePinning {
		required = append(required, corev1.NodeSelectorRequirement{
			Key:      controller.LabelBalloonsEnabled,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{"true"},
		})
	}
	var preferred []corev1.PreferredSchedulingTerm
	keys := make([]string, 0, len(nodeFeatures))
	for k := range nodeFeatures {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch nodeFeatures[key] {
		case cpopv1alpha1.NodeFeatureRequire:
			required = append(required, corev1.NodeSelectorRequirement{
				Key:      key,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"true"},
			})
		case cpopv1alpha1.NodeFeaturePrefer:
			preferred = append(preferred, corev1.PreferredSchedulingTerm{
				Weight: 100,
				Preference: corev1.NodeSelectorTerm{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      key,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"true"},
					}},
				},
			})
		case cpopv1alpha1.NodeFeatureIgnore:
			// Explicit no-op so a policy can disable a chart default.
		}
	}

	if len(required) == 0 && len(preferred) == 0 {
		return
	}

	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	na := pod.Spec.Affinity.NodeAffinity

	if len(required) > 0 {
		if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
			na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
		}
		terms := na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		if len(terms) == 0 {

			terms = []corev1.NodeSelectorTerm{{MatchExpressions: required}}
		} else {

			for i := range terms {
				terms[i].MatchExpressions = append(terms[i].MatchExpressions, required...)
			}
		}
		na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = terms
	}

	if len(preferred) > 0 {
		na.PreferredDuringSchedulingIgnoredDuringExecution = append(
			na.PreferredDuringSchedulingIgnoredDuringExecution, preferred...)
	}
}

const hostnameLabel = "kubernetes.io/hostname"

// injectHeadroomAffinity restricts a pod to fitting nodes and prefers best. Used where no
// reservation was recorded (dry-run).
func injectHeadroomAffinity(pod *corev1.Pod, fitting []string, best string) {
	if len(fitting) == 0 {
		return
	}
	requireHostnameIn(pod, fitting)

	// Weight 50 < nodeFeatures prefer's 100, so explicit preferences win.
	na := pod.Spec.Affinity.NodeAffinity
	na.PreferredDuringSchedulingIgnoredDuringExecution = append(
		na.PreferredDuringSchedulingIgnoredDuringExecution,
		corev1.PreferredSchedulingTerm{
			Weight: 50,
			Preference: corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      hostnameLabel,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{best},
				}},
			},
		},
	)
}

// injectReservedNodeAffinity requires the node the reservation was recorded against; a soft
// term could lose to other preferences and charge one node while running on another.
func injectReservedNodeAffinity(pod *corev1.Pod, node string) {
	if node == "" {
		return
	}
	requireHostnameIn(pod, []string{node})
}

// requireHostnameIn AND-s a required kubernetes.io/hostname term into every NodeSelectorTerm.
func requireHostnameIn(pod *corev1.Pod, nodes []string) {
	req := corev1.NodeSelectorRequirement{
		Key:      hostnameLabel,
		Operator: corev1.NodeSelectorOpIn,
		Values:   nodes,
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	na := pod.Spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}
	andRequirementIntoTerms(na.RequiredDuringSchedulingIgnoredDuringExecution, req)
}

// injectMaxSpanAffinity requires max-span >= span (as Gt span-1), AND-ed into every term.
func injectMaxSpanAffinity(pod *corev1.Pod, span int) {
	req := corev1.NodeSelectorRequirement{
		Key:      controller.LabelMaxSpan,
		Operator: corev1.NodeSelectorOpGt,
		Values:   []string{strconv.Itoa(span - 1)},
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	na := pod.Spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}
	andRequirementIntoTerms(na.RequiredDuringSchedulingIgnoredDuringExecution, req)
}

// andRequirementIntoTerms AND-s req into every NodeSelectorTerm, or seeds one term.
func andRequirementIntoTerms(sel *corev1.NodeSelector, req corev1.NodeSelectorRequirement) {
	if len(sel.NodeSelectorTerms) == 0 {
		sel.NodeSelectorTerms = []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{req}}}
		return
	}
	for i := range sel.NodeSelectorTerms {
		sel.NodeSelectorTerms[i].MatchExpressions = append(sel.NodeSelectorTerms[i].MatchExpressions, req)
	}
}

// hasWholeCoreGuaranteedCPU reports requests.cpu set, equal to limits.cpu, and whole cores.
func hasWholeCoreGuaranteedCPU(c *corev1.Container) bool {
	cpuReq, hasReq := c.Resources.Requests[corev1.ResourceCPU]
	cpuLim, hasLim := c.Resources.Limits[corev1.ResourceCPU]
	if !hasReq || !hasLim {
		return false
	}
	// Sign, not IsZero: mutating admission runs before apiserver validation.
	if cpuReq.Sign() <= 0 {
		return false
	}
	if cpuReq.Cmp(cpuLim) != 0 {
		return false
	}
	// cpuMilliCores, not MilliValue, avoids overflow on absurd requests.
	return cpuMilliCores(cpuReq)%1000 == 0
}

// hasWholeCorePodCPU reports whether any container has whole-core guaranteed CPU.
func hasWholeCorePodCPU(pod *corev1.Pod) bool {
	for i := range pod.Spec.Containers {
		if hasWholeCoreGuaranteedCPU(&pod.Spec.Containers[i]) {
			return true
		}
	}
	return false
}

func hasInitContainer(pod *corev1.Pod) bool {
	name := getEnv("INIT_CONTAINER_NAME", defaultInitContainerName)
	for _, c := range pod.Spec.InitContainers {
		if c.Name == name {
			return true
		}
	}
	return false
}

// createInitContainer builds the wait-for-balloons init container: the operator's own
// image polling the per-node readiness endpoint, needing no shell or cluster RBAC. Caller
// guarantees endpoint and image are non-empty.
func createInitContainer(endpoint, image string) corev1.Container {
	name := getEnv("INIT_CONTAINER_NAME", defaultInitContainerName)

	waitTimeout := getEnv("WAIT_TIMEOUT", strconv.Itoa(defaultWaitTimeoutSeconds))
	if _, err := strconv.Atoi(waitTimeout); err == nil {
		waitTimeout += "s"
	}

	// IfNotPresent default suits local images and air-gapped nodes.
	pullPolicy := corev1.PullPolicy(getEnv("INIT_CONTAINER_PULL_POLICY", string(corev1.PullIfNotPresent)))

	args := []string{
		readiness.SubcommandName,
		"--endpoint=" + endpoint,
		"--node=$(NODE_NAME)",
		"--timeout=" + waitTimeout,
	}
	var mounts []corev1.VolumeMount
	if readinessCAConfigMap(endpoint) != "" {
		args = append(args, "--ca-file="+readinessCAMountPath+"/"+readiness.CAKey)
		mounts = []corev1.VolumeMount{{Name: readinessCAVolume, MountPath: readinessCAMountPath, ReadOnly: true}}
	}
	return corev1.Container{
		Name:            name,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		Command:         []string{"/manager"},
		Args:            args,
		Env: []corev1.EnvVar{{
			Name: "NODE_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
			},
		}},
		VolumeMounts: mounts,
		SecurityContext: &corev1.SecurityContext{
			// RunAsNonRoot without RunAsUser, for OpenShift restricted-v2.
			RunAsNonRoot:             ptrBool(true),
			AllowPrivilegeEscalation: ptrBool(false),
			ReadOnlyRootFilesystem:   ptrBool(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resourceQuantity("100m"),
				corev1.ResourceMemory: resourceQuantity("128Mi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resourceQuantity("10m"),
				corev1.ResourceMemory: resourceQuantity("32Mi"),
			},
		},
	}
}

const (
	readinessCAVolume    = "cpu-pinning-readiness-ca"
	readinessCAMountPath = "/etc/cpu-pinning-operator/readiness-ca"
)

// readinessCAConfigMap names the CA ConfigMap for an https endpoint, or "".
func readinessCAConfigMap(endpoint string) string {
	if !strings.HasPrefix(endpoint, "https://") {
		return ""
	}
	return getEnv(readiness.CAConfigMapEnv, "")
}

// injectReadinessCAVolume adds the CA ConfigMap volume. No subPath, so updates propagate;
// optional, so the poller waits until it exists.
func injectReadinessCAVolume(pod *corev1.Pod, endpoint string) {
	name := readinessCAConfigMap(endpoint)
	if name == "" {
		return
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: readinessCAVolume,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
			Optional:             ptrBool(true),
		}},
	})
}

func ptrBool(v bool) *bool { return &v }

func resourceQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}
