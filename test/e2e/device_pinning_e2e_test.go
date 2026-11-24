// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// No real device is needed: the webhook stamps at CREATE, so the probe pod only has to
// be admitted. Device-NUMA CPU locality is resolved by the plugin at CreateContainer
// and needs a multi-NUMA accelerator node to observe; it is not checked here.

package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// deviceEnvGate opts this case in. deviceResourceName is the resource the probe pod
// requests (e.g. gpu.intel.com/i915).
const deviceEnvGate = "E2E_DEVICE_PINNING"

func deviceResourceName() string { return env("E2E_DEVICE_RESOURCE", "nvidia.com/gpu") }

// TestDevicePodDeviceLocality proves a device pod is stamped balloon-device ignoring its
// tensor-parallel hint, while an identical CPU-only pod keeps its span balloon.
func TestDevicePodDeviceLocality(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(deviceEnvGate) == "" {
		skipNotSelected(at, "set %s=1 to exercise device-locality pinning against the live webhook", deviceEnvGate)
	}
	c := newClient(at)
	balloonsEnabledNode(at, c) // skips when the operator provisions no node
	ns := useTargetNamespace(at, c)

	csApplyPolicyMatch(t, c, []string{"device-e2e-*"}, "largestContainer")

	deviceKey := csContainerKey("device-e2e-device")
	devicePod := deviceAdmitUntilStamped(t, c, ns, "e2e-device", "device-e2e-device", true, deviceKey)
	if got := devicePod.Annotations[deviceKey]; got != "balloon-device" {
		at.Errorf("device pod stamped %q, want balloon-device (tensor-parallel hint must be ignored for device pods)", got)
	}

	// Control: same hint, no device, so the hint is shown to be honoured.
	cpuKey := csContainerKey("device-e2e-cpu")
	cpuPod := deviceAdmitUntilStamped(t, c, ns, "e2e-cpu", "device-e2e-cpu", false, cpuKey)
	got := cpuPod.Annotations[cpuKey]
	if !strings.HasPrefix(got, "balloon-span") {
		at.Errorf("non-device control stamped %q, want a balloon-span<N> value (intent-driven)", got)
	}
	if got == "balloon-span1" {
		at.Logf("non-device control landed on balloon-span1 (node max-span 1 or policy did not derive a wider span); device-vs-intent span divergence not observable on this cluster, but the balloon-device vs balloon-span routing distinction holds")
	}
}

// deviceAdmitUntilStamped creates probe pods until one carries the annotation at key or
// the deadline passes, absorbing webhook index-refresh lag.
func deviceAdmitUntilStamped(t *testing.T, c client.Client, ns, base, ctrName string, device bool, key string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var last *corev1.Pod
	for i := 0; ; i++ {
		last = createAndCleanup(t, c, deviceProbePod(fmt.Sprintf("%s-%d", base, i), ns, ctrName, device))
		if last.Annotations[key] != "" || time.Now().After(deadline) {
			return last
		}
		time.Sleep(3 * time.Second)
	}
}

// deviceProbePod builds a whole-core probe pod with a --tensor-parallel-size 4 hint,
// plus one device resource when device is true.
func deviceProbePod(name, ns, ctrName string, device bool) *corev1.Pod {
	q := resource.MustParse("2")
	reqs := corev1.ResourceList{corev1.ResourceCPU: q}
	lims := corev1.ResourceList{corev1.ResourceCPU: q}
	if device {
		g := resource.MustParse("1")
		reqs[corev1.ResourceName(deviceResourceName())] = g
		lims[corev1.ResourceName(deviceResourceName())] = g
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"app": "device-e2e"},
		},
		Spec: corev1.PodSpec{
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:      ctrName,
				Image:     "registry.k8s.io/pause:3.9",
				Command:   []string{"/pause"},
				Args:      []string{"--tensor-parallel-size", "4"},
				Resources: corev1.ResourceRequirements{Requests: reqs, Limits: lims},
			}},
		},
	}
}
