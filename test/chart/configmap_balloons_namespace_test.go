// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestBalloonsNamespaceResolution proves BALLOONS_NAMESPACE resolves to the release
// namespace when the subchart is enabled, kube-system when disabled, and an explicit
// initContainer.balloons.namespace always wins.
// A wrong namespace makes the readiness check wait forever, leaving pinned pods stuck in
// Init.
func TestBalloonsNamespaceResolution(t *testing.T) {
	helm := helmBin(t)
	const chartDir = "../../deploy/cpu-pinning-operator"
	const releaseNS = "cpu-pinning-operator-system"

	render := func(t *testing.T, extra ...string) string {
		t.Helper()
		args := []string{
			"template", "t", chartDir, "-f", policyTargetValues,
			"-n", releaseNS,
			"--show-only", "templates/configmap.yaml",
		}
		args = append(args, extra...)
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		return string(out)
	}

	// balloonsNamespace extracts the unquoted BALLOONS_NAMESPACE value from the rendered
	// configmap.
	balloonsNamespace := func(t *testing.T, out string) string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "BALLOONS_NAMESPACE:") {
				v := strings.TrimSpace(strings.TrimPrefix(line, "BALLOONS_NAMESPACE:"))
				return strings.Trim(v, `"`)
			}
		}
		t.Fatalf("BALLOONS_NAMESPACE not found in render:\n%s", out)
		return ""
	}

	t.Run("subchart enabled defaults to release namespace", func(t *testing.T) {
		// Default: the subchart's DaemonSet lands in the release namespace.
		got := balloonsNamespace(t, render(t))
		if got != releaseNS {
			t.Errorf("BALLOONS_NAMESPACE = %q, want release namespace %q (subchart self-installed there)", got, releaseNS)
		}
	})

	t.Run("subchart disabled falls back to kube-system", func(t *testing.T) {
		// External balloons install: the documented kube-system fallback.
		got := balloonsNamespace(t, render(t, "--set", "nri-resource-policy-balloons.enabled=false"))
		if got != "kube-system" {
			t.Errorf("BALLOONS_NAMESPACE = %q, want kube-system (balloons subchart disabled)", got)
		}
	})

	t.Run("explicit override wins over auto-resolution", func(t *testing.T) {
		// An explicit namespace wins even with the subchart enabled.
		const external = "my-balloons-ns"
		got := balloonsNamespace(t, render(t, "--set", "initContainer.balloons.namespace="+external))
		if got != external {
			t.Errorf("BALLOONS_NAMESPACE = %q, want explicit override %q", got, external)
		}
	})
}
