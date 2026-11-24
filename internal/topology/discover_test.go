// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
)

// testProbeImage stands in for the chart's digest-pinned PROBE_IMAGE.
const testProbeImage = "registry.example.com/cpu-pinning-operator@sha256:abc123"

func TestProbeTolerations(t *testing.T) {
	tolerateAll := []corev1.Toleration{{Operator: corev1.TolerationOpExists}}

	tests := []struct {
		name   string
		envVal string
		want   []corev1.Toleration
	}{
		{name: "empty/unset falls back to tolerate-all", envVal: "", want: tolerateAll},
		{name: "malformed falls back to tolerate-all", envVal: "not-json", want: tolerateAll},
		{name: "empty array falls back to tolerate-all", envVal: "[]", want: tolerateAll},
		{
			name:   "explicit list is honored",
			envVal: `[{"key":"dedicated","operator":"Equal","value":"gpu","effect":"NoSchedule"}]`,
			want: []corev1.Toleration{{
				Key:      "dedicated",
				Operator: corev1.TolerationOpEqual,
				Value:    "gpu",
				Effect:   corev1.TaintEffectNoSchedule,
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Always pin the env so the process environment cannot leak in.
			t.Setenv("PROBE_TOLERATIONS", tc.envVal)
			got := probeTolerations()
			if len(got) != len(tc.want) {
				t.Fatalf("probeTolerations() len = %d, want %d (%+v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("probeTolerations()[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestBuildProbePod(t *testing.T) {
	const (
		ns       = "cpu-pinning-operator-system"
		podName  = "cpu-topo-probe-node-a"
		nodeName = "node-a"
	)

	t.Run("defaults", func(t *testing.T) {
		// Reset probe env so the process environment cannot leak in.
		t.Setenv("PROBE_TOLERATIONS", "")
		t.Setenv("PROBE_SERVICE_ACCOUNT", "")
		pod := buildProbePod(ns, podName, nodeName, testProbeImage)

		if pod.Labels[managedByLabelKey] != managedByValue {
			t.Errorf("managed-by label = %q, want %q", pod.Labels[managedByLabelKey], managedByValue)
		}
		if pod.Labels[probeLabelKey] != nodeName {
			t.Errorf("probe-for label = %q, want %q", pod.Labels[probeLabelKey], nodeName)
		}
		if managedByLabelKey != cpopv1alpha1.LabelPrefix+"managed-by" {
			t.Errorf("managed-by key %q does not use the operator label prefix", managedByLabelKey)
		}
		if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Operator != corev1.TolerationOpExists {
			t.Errorf("default tolerations = %+v, want tolerate-all", pod.Spec.Tolerations)
		}
		// Unset PROBE_SERVICE_ACCOUNT leaves ServiceAccountName blank.
		if got := pod.Spec.ServiceAccountName; got != "" {
			t.Errorf("ServiceAccountName = %q, want empty (default SA)", got)
		}
	})

	t.Run("least-privilege security context", func(t *testing.T) {
		t.Setenv("PROBE_TOLERATIONS", "")
		t.Setenv("PROBE_SERVICE_ACCOUNT", "")
		pod := buildProbePod(ns, podName, nodeName, testProbeImage)

		sc := pod.Spec.Containers[0].SecurityContext
		if sc == nil {
			t.Fatal("probe container has no SecurityContext")
		}
		// Restricted context matching the chart's OpenShift probe SCC.
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("RunAsNonRoot = %v, want true", sc.RunAsNonRoot)
		}
		if sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
			t.Errorf("RunAsUser = %v, want 1000", sc.RunAsUser)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("AllowPrivilegeEscalation = %v, want false", sc.AllowPrivilegeEscalation)
		}
		if sc.Privileged != nil && *sc.Privileged {
			t.Error("Privileged = true, want unset/false (probe needs no privilege)")
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("Capabilities.Drop = %+v, want [ALL]", sc.Capabilities)
		}
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("SeccompProfile = %+v, want RuntimeDefault", sc.SeccompProfile)
		}
		// Read-only root filesystem.
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("ReadOnlyRootFilesystem = %v, want true", sc.ReadOnlyRootFilesystem)
		}
		// No SA token: the probe never calls the API.
		if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
			t.Errorf("AutomountServiceAccountToken = %v, want false", pod.Spec.AutomountServiceAccountToken)
		}
		// The probe mounts no hostPath and shares no host namespaces.
		if len(pod.Spec.Volumes) != 0 {
			t.Errorf("probe has %d volumes, want none (no hostPath)", len(pod.Spec.Volumes))
		}
		if pod.Spec.HostPID || pod.Spec.HostNetwork || pod.Spec.HostIPC {
			t.Error("probe shares a host namespace, want none")
		}
	})

	t.Run("service account from env", func(t *testing.T) {
		t.Setenv("PROBE_TOLERATIONS", "")
		// OpenShift mode wires a dedicated SCC-bound SA via PROBE_SERVICE_ACCOUNT.
		t.Setenv("PROBE_SERVICE_ACCOUNT", "cpu-pinning-operator-probe")

		pod := buildProbePod(ns, podName, nodeName, testProbeImage)
		if got, want := pod.Spec.ServiceAccountName, "cpu-pinning-operator-probe"; got != want {
			t.Errorf("ServiceAccountName = %q, want %q", got, want)
		}
	})

	// The pod uses the caller's image, never one resolved here (the env decoy catches that).
	t.Run("runs the supplied image with no shell", func(t *testing.T) {
		t.Setenv("PROBE_IMAGE", "registry.example.com/decoy:1")
		t.Setenv("PROBE_TOLERATIONS", "")
		t.Setenv("PROBE_SERVICE_ACCOUNT", "")

		probe := buildProbePod(ns, podName, nodeName, testProbeImage).Spec.Containers[0]

		if probe.Image != testProbeImage {
			t.Errorf("probe image = %q, want the supplied %q", probe.Image, testProbeImage)
		}
		if len(probe.Command) != 1 || probe.Command[0] != "/manager" {
			t.Errorf("probe command = %v, want the operator binary", probe.Command)
		}
		if len(probe.Args) != 1 || probe.Args[0] != ProbeSubcommandName {
			t.Errorf("probe args = %v, want the %q subcommand", probe.Args, ProbeSubcommandName)
		}
	})

	// Requests are required under a LimitRange or ResourceQuota.
	t.Run("declares requests and limits", func(t *testing.T) {
		t.Setenv("PROBE_RESOURCES", "")
		res := buildProbePod(ns, podName, nodeName, testProbeImage).Spec.Containers[0].Resources

		for _, want := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			if q, ok := res.Requests[want]; !ok || q.IsZero() {
				t.Errorf("probe declares no %s request", want)
			}
			if q, ok := res.Limits[want]; !ok || q.IsZero() {
				t.Errorf("probe declares no %s limit", want)
			}
		}
	})

	t.Run("resources from env", func(t *testing.T) {
		t.Setenv("PROBE_RESOURCES", `{"requests":{"cpu":"25m","memory":"64Mi"},"limits":{"cpu":"250m","memory":"256Mi"}}`)
		res := buildProbePod(ns, podName, nodeName, testProbeImage).Spec.Containers[0].Resources

		for name, want := range map[corev1.ResourceName]string{corev1.ResourceCPU: "25m", corev1.ResourceMemory: "64Mi"} {
			if got := res.Requests[name]; got.String() != want {
				t.Errorf("%s request = %s, want %s", name, got.String(), want)
			}
		}
		for name, want := range map[corev1.ResourceName]string{corev1.ResourceCPU: "250m", corev1.ResourceMemory: "256Mi"} {
			if got := res.Limits[name]; got.String() != want {
				t.Errorf("%s limit = %s, want %s", name, got.String(), want)
			}
		}
	})

	// A malformed override falls back to the defaults, not to no requests.
	t.Run("malformed resources fall back to the defaults", func(t *testing.T) {
		t.Setenv("PROBE_RESOURCES", "not-json")
		res := buildProbePod(ns, podName, nodeName, testProbeImage).Spec.Containers[0].Resources

		if len(res.Requests) == 0 || len(res.Limits) == 0 {
			t.Errorf("resources = %+v, want the built-in request/limit pair", res)
		}
	})

	// The probe needs the manager's pull secrets.
	t.Run("image pull secrets from env", func(t *testing.T) {
		t.Setenv("PROBE_IMAGE_PULL_SECRETS", `[{"name":"registry-creds"},{"name":"mirror-creds"}]`)
		pod := buildProbePod(ns, podName, nodeName, testProbeImage)

		want := []corev1.LocalObjectReference{{Name: "registry-creds"}, {Name: "mirror-creds"}}
		if len(pod.Spec.ImagePullSecrets) != len(want) {
			t.Fatalf("ImagePullSecrets = %+v, want %+v", pod.Spec.ImagePullSecrets, want)
		}
		for i := range want {
			if pod.Spec.ImagePullSecrets[i] != want[i] {
				t.Errorf("ImagePullSecrets[%d] = %+v, want %+v", i, pod.Spec.ImagePullSecrets[i], want[i])
			}
		}
	})

	t.Run("no image pull secrets configured", func(t *testing.T) {
		for _, envVal := range []string{"", "not-json"} {
			t.Setenv("PROBE_IMAGE_PULL_SECRETS", envVal)
			if got := buildProbePod(ns, podName, nodeName, testProbeImage).Spec.ImagePullSecrets; len(got) != 0 {
				t.Errorf("ImagePullSecrets = %+v for PROBE_IMAGE_PULL_SECRETS %q, want none", got, envVal)
			}
		}
	})

	// A request above its limit is refused; the API server would reject the pod.
	t.Run("a request above its limit falls back to the defaults", func(t *testing.T) {
		t.Setenv("PROBE_RESOURCES", `{"requests":{"memory":"512Mi"},"limits":{"memory":"128Mi"}}`)
		res := buildProbePod(ns, podName, nodeName, testProbeImage).Spec.Containers[0].Resources

		request := res.Requests[corev1.ResourceMemory]
		limit := res.Limits[corev1.ResourceMemory]
		if request.Cmp(limit) > 0 {
			t.Errorf("memory request %s exceeds its limit %s, which the API server refuses", request.String(), limit.String())
		}
		if request.String() == "512Mi" {
			t.Error("the contradictory override was passed through rather than refused")
		}
	})

	t.Run("tolerations from env", func(t *testing.T) {
		t.Setenv("PROBE_TOLERATIONS", `[{"key":"node-role.kubernetes.io/control-plane","operator":"Exists","effect":"NoSchedule"}]`)

		pod := buildProbePod(ns, podName, nodeName, testProbeImage)

		if len(pod.Spec.Tolerations) != 1 ||
			pod.Spec.Tolerations[0].Key != "node-role.kubernetes.io/control-plane" ||
			pod.Spec.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
			t.Errorf("tolerations = %+v, want the single control-plane toleration", pod.Spec.Tolerations)
		}
		// The managed-by label is independent of toleration config.
		if pod.Labels[managedByLabelKey] != managedByValue {
			t.Errorf("managed-by label = %q, want %q", pod.Labels[managedByLabelKey], managedByValue)
		}
	})
}
