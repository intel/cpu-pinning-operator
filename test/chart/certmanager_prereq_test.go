// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// certManagerCRDNames are the CRD names the cert-manager chart registers; this chart must
// never emit them.
var certManagerCRDNames = []string{
	"issuers.cert-manager.io",
	"clusterissuers.cert-manager.io",
	"certificates.cert-manager.io",
	"certificaterequests.cert-manager.io",
	"orders.acme.cert-manager.io",
	"challenges.acme.cert-manager.io",
}

// renderChart runs `helm template` with optional extra args.
func renderChart(t *testing.T, extra ...string) string {
	t.Helper()
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")
	args := append([]string{"template", "t", chartDir, "-f", policyTargetValues, "-n", "cpu-pinning-operator-system"}, extra...)
	out, err := exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		requireSubcharts(t, string(out))
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// hasName reports whether the rendered YAML declares a resource with the given
// metadata.name (quoted or bare).
func hasName(out, name string) bool {
	return strings.Contains(out, "name: \""+name+"\"") || strings.Contains(out, "name: "+name+"\n")
}

// TestCertManagerIsAPrerequisiteNotBundled proves the default render ships no
// cert-manager subchart resources or CRDs, yet still renders the operator's own
// Issuer/Certificate.
func TestCertManagerIsAPrerequisiteNotBundled(t *testing.T) {
	t.Run("default omits cert-manager subchart + CRDs", func(t *testing.T) {
		out := renderChart(t, "--include-crds")

		// Every subchart resource carries this label; the operator's own do not.
		if strings.Contains(out, "app.kubernetes.io/name: cert-manager") {
			t.Error("default render includes cert-manager subchart resources — cert-manager must be a prerequisite (cert-manager.enabled=false by default), not bundled")
		}

		if strings.Contains(out, "charts/cert-manager/templates/crd") {
			t.Error("default render templates cert-manager CRDs from the subchart — the chart must ship no cert-manager content by default")
		}
		for _, name := range certManagerCRDNames {
			if hasName(out, name) {
				t.Errorf("cert-manager CRD %q is emitted by the chart — cert-manager's CRDs are the prerequisite's content, installed out-of-band, never shipped here", name)
			}
		}
	})

	t.Run("default renders the operator's own Issuer/Certificate", func(t *testing.T) {
		out := renderChart(t)
		// cert-manager is a prerequisite, so the serving cert renders without the
		// subchart.
		if !hasName(out, "t-cpu-pinning-operator-serving-cert") {
			t.Error("operator Certificate (serving-cert) not rendered — the chart still owns its webhook serving cert against the cert-manager prerequisite")
		}
		if !hasName(out, "t-cpu-pinning-operator-selfsigned-issuer") {
			t.Error("operator Issuer (selfsigned-issuer) not rendered — the chart still owns its webhook serving cert against the cert-manager prerequisite")
		}
		if !strings.Contains(out, "apiVersion: cert-manager.io/v1") {
			t.Error("operator serving-cert resources are not cert-manager.io/v1 — they must bind to the prerequisite's CRDs")
		}
	})

	t.Run("opt-in bundles the subchart", func(t *testing.T) {
		out := renderChart(t, "--set", "cert-manager.enabled=true")
		if !strings.Contains(out, "app.kubernetes.io/name: cert-manager") {
			t.Error("--set cert-manager.enabled=true did not render the bundled cert-manager subchart — the opt-in escape hatch must still work")
		}
	})
}
