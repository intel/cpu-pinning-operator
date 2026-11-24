// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hasKind(rendered, kind string) bool {
	return strings.Contains(rendered, "kind: "+kind+"\n")
}

// TestMonitoringObjectsFollowTheCapability proves the ServiceMonitor and PrometheusRule
// render by default only where the apiserver serves their CRDs (exercised with
// --api-versions).
func TestMonitoringObjectsFollowTheCapability(t *testing.T) {
	// Kind-qualified: a cluster can serve one kind without the other.
	const (
		ruleAPI    = "--api-versions=monitoring.coreos.com/v1/PrometheusRule"
		monitorAPI = "--api-versions=monitoring.coreos.com/v1/ServiceMonitor"
	)

	values := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write values: %v", err)
		}
		return "-f=" + p
	}
	const disable = "metrics:\n  serviceMonitor:\n    enabled: false\n  prometheusRule:\n    enabled: false\n"
	const enable = "metrics:\n  serviceMonitor:\n    enabled: true\n  prometheusRule:\n    enabled: true\n"

	t.Run("absent CRDs render neither object", func(t *testing.T) {
		out := renderChart(t)
		if hasKind(out, "ServiceMonitor") || hasKind(out, "PrometheusRule") {
			t.Error("a cluster that does not serve monitoring.coreos.com/v1 got a monitoring object it cannot accept")
		}
	})

	// Unset must render byte-identically to an explicit disable.
	t.Run("absent CRDs render what an explicit disable renders", func(t *testing.T) {
		if unset, disabled := renderChart(t), renderChart(t, values(t, disable)); unset != disabled {
			t.Error("the unset default and an explicit disable render differently without the monitoring CRDs")
		}
	})

	t.Run("present CRDs render both objects", func(t *testing.T) {
		out := renderChart(t, ruleAPI, monitorAPI)
		if !hasKind(out, "ServiceMonitor") {
			t.Error("ServiceMonitor not rendered on a cluster serving monitoring.coreos.com/v1")
		}
		if !hasKind(out, "PrometheusRule") {
			t.Error("PrometheusRule not rendered on a cluster serving monitoring.coreos.com/v1")
		}
	})

	t.Run("an explicit value wins over the capability", func(t *testing.T) {
		off := renderChart(t, ruleAPI, monitorAPI, values(t, disable))
		if hasKind(off, "ServiceMonitor") || hasKind(off, "PrometheusRule") {
			t.Error("an explicit enabled=false was overridden by the capability check")
		}
		on := renderChart(t, values(t, enable))
		if !hasKind(on, "ServiceMonitor") || !hasKind(on, "PrometheusRule") {
			t.Error("an explicit enabled=true was overridden by the capability check")
		}
		// A string "false" (Terraform, --set-string) must not read as truthy; asserted
		// with no serving cert because the ServiceMonitor would fail the install.
		str := renderChart(t, ruleAPI, monitorAPI,
			values(t, "certManager:\n  enabled: false\nballoonsReady:\n  secure: false\n"),
			"--set-string=metrics.serviceMonitor.enabled=false",
			"--set-string=metrics.prometheusRule.enabled=false")
		if hasKind(str, "ServiceMonitor") || hasKind(str, "PrometheusRule") {
			t.Error("a string-typed enabled=\"false\" was read as truthy")
		}
	})

	// Rendering a kind the cluster lacks fails the apply with "no matches for kind".
	t.Run("one kind present does not render the other", func(t *testing.T) {
		out := renderChart(t, ruleAPI)
		if !hasKind(out, "PrometheusRule") {
			t.Error("PrometheusRule not rendered on a cluster serving its kind")
		}
		if hasKind(out, "ServiceMonitor") {
			t.Error("ServiceMonitor rendered on a cluster that does not serve the ServiceMonitor kind")
		}
	})

	// Both read the metrics endpoint, so metrics off wins.
	t.Run("metrics disabled outranks the capability", func(t *testing.T) {
		out := renderChart(t, ruleAPI, monitorAPI, values(t, "metrics:\n  enabled: false\n"))
		if hasKind(out, "ServiceMonitor") || hasKind(out, "PrometheusRule") {
			t.Error("a monitoring object was rendered with metrics.enabled=false")
		}
	})

	// CRDs alone asked for nothing, so with no serving cert the render must succeed: no
	// ServiceMonitor, PrometheusRule still rendered.
	t.Run("a default cannot fail an install with no serving cert", func(t *testing.T) {
		out := renderChart(t, ruleAPI, monitorAPI, values(t, "certManager:\n  enabled: false\nballoonsReady:\n  secure: false\n"))
		if hasKind(out, "ServiceMonitor") {
			t.Error("a secure ServiceMonitor was defaulted on with no serving cert to verify the scrape against")
		}
		if !hasKind(out, "PrometheusRule") {
			t.Error("the PrometheusRule needs no serving cert and must still render")
		}
	})
}
