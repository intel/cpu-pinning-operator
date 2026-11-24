// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBalloonsTestAPIsOffByDefault proves the balloons subchart leaves ENABLE_TEST_APIS
// off by default and plugin.test.enableAPIs still turns it on.
func TestBalloonsTestAPIsOffByDefault(t *testing.T) {
	render := func(t *testing.T, extra ...string) string {
		t.Helper()
		args := append([]string{"template", "t", filepath.Join("..", "..", "deploy", "cpu-pinning-operator"), "-f", policyTargetValues,
			"--show-only", "charts/nri-resource-policy-balloons/templates/daemonset.yaml"}, extra...)
		out, err := exec.Command(helmBin(t), args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		return string(out)
	}
	if out := render(t); strings.Contains(out, "ENABLE_TEST_APIS") {
		t.Errorf("default install renders ENABLE_TEST_APIS on the balloons DaemonSet:\n%s", out)
	}
	if out := render(t, "--set", "nri-resource-policy-balloons.plugin.test.enableAPIs=true"); !strings.Contains(out, "ENABLE_TEST_APIS") {
		t.Errorf("plugin.test.enableAPIs=true does not render ENABLE_TEST_APIS — the subchart value moved:\n%s", out)
	}
}
