// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenShiftSCCRendering proves openShift.enabled renders only the least-privilege
// probe SCC bound to a dedicated SA, and nothing on vanilla clusters. The balloons
// plugin's SCC belongs to its subchart (openshift.grant-scc).
func TestOpenShiftSCCRendering(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	// render runs `helm template` for one template ("" = chart defaults) and returns the
	// render error for callers asserting a failure.
	render := func(t *testing.T, showOnly, valuesFile string, extra ...string) (string, error) {
		t.Helper()
		args := []string{"template", "t", chartDir, "-f", policyTargetValues}
		if showOnly != "" {
			args = append(args, "--show-only", showOnly)
		}
		if valuesFile != "" {
			args = append(args, "-f", valuesFile)
		}
		args = append(args, extra...)
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
		}
		return string(out), err
	}

	writeValues := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write values: %v", err)
		}
		return p
	}

	t.Run("no SCC resources by default", func(t *testing.T) {
		// Chart default is openShift.enabled=false: nothing renders.
		out, err := render(t, "templates/openshift-scc.yaml", "")
		if err != nil {
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		for _, unexpected := range []string{
			"kind: SecurityContextConstraints",
			"kind: ServiceAccount",
			"kind: RoleBinding",
			"kind: ClusterRole",
		} {
			if strings.Contains(out, unexpected) {
				t.Errorf("%q rendered while openShift.enabled=false (default):\n%s", unexpected, out)
			}
		}
	})

	t.Run("no PROBE_SERVICE_ACCOUNT in configmap by default", func(t *testing.T) {
		out, err := render(t, "templates/configmap.yaml", "")
		if err != nil {
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		if strings.Contains(out, "PROBE_SERVICE_ACCOUNT") {
			t.Errorf("PROBE_SERVICE_ACCOUNT wired while openShift.enabled=false (default):\n%s", out)
		}
	})

	t.Run("probe SCC is least-privilege when enabled", func(t *testing.T) {
		vf := writeValues(t, "openShift:\n  enabled: true\n")
		out, err := render(t, "templates/openshift-scc.yaml", vf)
		if err != nil {
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "kind: SecurityContextConstraints") {
			t.Fatalf("probe SCC not rendered when openShift.enabled=true:\n%s", out)
		}
		// The probe SCC permits uid 1000 and RuntimeDefault seccomp (what buildProbePod
		// sets) and nothing privileged.
		for _, want := range []string{
			"allowPrivilegedContainer: false",
			"allowHostDirVolumePlugin: false",
			"uidRangeMin: 1000",
			"uidRangeMax: 1000",
			"- runtime/default",
			// The probe writes nothing.
			"readOnlyRootFilesystem: true",
			// The probe declares no volumes and declines its SA token.
			"volumes: []",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("probe SCC missing expected least-privilege setting %q:\n%s", want, out)
			}
		}

		if strings.Contains(out, "allowPrivilegedContainer: true") ||
			strings.Contains(out, "allowHostDirVolumePlugin: true") ||
			strings.Contains(out, "readOnlyRootFilesystem: false") {
			t.Errorf("probe SCC over-grants privilege/hostPath/writable rootfs:\n%s", out)
		}
		for _, vol := range []string{"- hostPath", "- configMap", "- emptyDir", "- secret", "- projected", "- downwardAPI"} {
			if strings.Contains(out, vol) {
				t.Errorf("probe SCC permits volume type %q but the probe pod declares no volumes:\n%s", vol, out)
			}
		}
	})

	t.Run("probe SCC bound to dedicated SA not default", func(t *testing.T) {
		vf := writeValues(t, "openShift:\n  enabled: true\n")
		out, err := render(t, "templates/openshift-scc.yaml", vf)
		if err != nil {
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		// Never the namespace `default` SA. The SCC is cluster-scoped, so its name
		// derives from the release fullname.
		if !strings.Contains(out, "name: t-cpu-pinning-operator-probe") {
			t.Errorf("dedicated probe SA not rendered/bound with fullname-derived name:\n%s", out)
		}
		if strings.Contains(out, "name: default") {
			t.Errorf("probe SCC grant leaked onto the `default` SA:\n%s", out)
		}
	})

	t.Run("probe SA disables token auto-mount by default", func(t *testing.T) {
		// The probe never calls the API, so no token is mounted.
		vf := writeValues(t, "openShift:\n  enabled: true\n")
		out, err := render(t, "templates/openshift-scc.yaml", vf)
		if err != nil {
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "automountServiceAccountToken: false") {
			t.Errorf("probe SA should disable token auto-mount by default:\n%s", out)
		}
	})

	t.Run("SCC and SA names are overridable", func(t *testing.T) {
		// Explicit names override the fullname-derived default across SCC, RBAC and
		// configmap.
		vf := writeValues(t, "openShift:\n  enabled: true\n  probeSCCName: my-scc\n  probeServiceAccountName: my-sa\n")
		out, err := render(t, "templates/openshift-scc.yaml", vf)
		if err != nil {
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "name: my-scc") || !strings.Contains(out, `resourceNames: ["my-scc"]`) {
			t.Errorf("explicit probeSCCName not honored across SCC + RBAC:\n%s", out)
		}
		if !strings.Contains(out, "name: my-sa") {
			t.Errorf("explicit probeServiceAccountName not honored:\n%s", out)
		}
		cm, err := render(t, "templates/configmap.yaml", vf)
		if err != nil {
			t.Fatalf("helm template failed: %v\n%s", err, cm)
		}
		if !strings.Contains(cm, `PROBE_SERVICE_ACCOUNT: "my-sa"`) {
			t.Errorf("configmap PROBE_SERVICE_ACCOUNT did not follow the override:\n%s", cm)
		}
	})

	t.Run("configmap wires PROBE_SERVICE_ACCOUNT when enabled", func(t *testing.T) {
		vf := writeValues(t, "openShift:\n  enabled: true\n")
		out, err := render(t, "templates/configmap.yaml", vf)
		if err != nil {
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, `PROBE_SERVICE_ACCOUNT: "t-cpu-pinning-operator-probe"`) {
			t.Errorf("PROBE_SERVICE_ACCOUNT not wired with fullname-derived name when openShift.enabled=true:\n%s", out)
		}
	})

	t.Run("no operator-owned balloons SCC or privileged binding", func(t *testing.T) {
		// The operator never renders a balloons SCC or a privileged binding, whatever the
		// subchart settings.
		vf := writeValues(t, "openShift:\n  enabled: true\nnri-resource-policy-balloons:\n  enabled: true\n  openshift:\n    grant-scc: true\n")
		out, err := render(t, "templates/openshift-scc.yaml", vf)
		if err != nil {
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		if strings.Contains(out, "system:openshift:scc:privileged") {
			t.Errorf("operator rendered a privileged SCC binding (the balloons plugin's SCC is the subchart's responsibility):\n%s", out)
		}
		if strings.Contains(out, "balloons-privileged-scc") {
			t.Errorf("operator rendered a balloons-privileged-scc binding:\n%s", out)
		}

		if !strings.Contains(out, "kind: SecurityContextConstraints") {
			t.Errorf("probe SCC should still render on OpenShift:\n%s", out)
		}
	})

	t.Run("no render failure or balloons binding when balloons subchart disabled", func(t *testing.T) {
		// The probe SCC renders and no balloons binding appears, whatever the balloons
		// settings.
		vf := writeValues(t, "openShift:\n  enabled: true\nnri-resource-policy-balloons:\n  enabled: false\n")
		out, err := render(t, "templates/openshift-scc.yaml", vf)
		if err != nil {
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "kind: SecurityContextConstraints") {
			t.Errorf("probe SCC should still render with balloons disabled:\n%s", out)
		}
		if strings.Contains(out, "system:openshift:scc:privileged") {
			t.Errorf("balloons privileged binding rendered while balloons subchart disabled:\n%s", out)
		}
	})

	t.Run("no guard failure when enabled without allowPCT or grant-scc", func(t *testing.T) {
		// A bare openShift.enabled=true renders.
		out, err := render(t, "templates/openshift-scc.yaml", "", "--set", "openShift.enabled=true")
		if err != nil {
			t.Fatalf("expected render to succeed with openShift.enabled=true, got error: %v\n%s", err, out)
		}
		if !strings.Contains(out, "kind: SecurityContextConstraints") {
			t.Errorf("probe SCC should render with openShift.enabled=true:\n%s", out)
		}
		if strings.Contains(out, "system:openshift:scc:privileged") {
			t.Errorf("operator rendered a privileged binding with a bare openShift.enabled=true:\n%s", out)
		}
	})

	t.Run("full overlay renders probe SCC and subchart SCC, no operator balloons binding", func(t *testing.T) {
		// values-openshift.yaml must fully render: the probe SCC, the subchart's own SCC
		// and spc_t DaemonSet, and no operator-owned privileged binding.
		overlay := filepath.Join(chartDir, "values-openshift.yaml")
		out, err := render(t, "", overlay)
		if err != nil {
			t.Fatalf("helm template with values-openshift.yaml failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "kind: SecurityContextConstraints") {
			t.Errorf("values-openshift.yaml overlay did not render any SCC:\n%s", out)
		}

		if !strings.Contains(out, "charts/nri-resource-policy-balloons/templates/openshift-scc.yaml") {
			t.Errorf("balloons subchart SCC (grant-scc) not rendered by values-openshift.yaml:\n%s", out)
		}

		if !strings.Contains(out, "type: spc_t") {
			t.Errorf("balloons DaemonSet missing seLinuxOptions.type: spc_t on values-openshift.yaml:\n%s", out)
		}
		if strings.Contains(out, "privileged: true") {
			t.Errorf("balloons DaemonSet is privileged on values-openshift.yaml (should be non-privileged):\n%s", out)
		}
		if strings.Contains(out, "system:openshift:scc:privileged") {
			t.Errorf("operator-owned privileged binding rendered by values-openshift.yaml:\n%s", out)
		}
	})
}
