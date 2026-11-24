// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestPrometheusRuleRendering proves the PrometheusRule renders nothing without the
// monitoring API, always carries the recording rules, and drops alerts when
// metrics.prometheusRule.alerts=false.
func TestPrometheusRuleRendering(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	render := func(t *testing.T, valuesBody string) string {
		t.Helper()
		vf := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(vf, []byte(valuesBody), 0o600); err != nil {
			t.Fatalf("write values: %v", err)
		}
		args := []string{"template", "t", chartDir, "-f", policyTargetValues, "--show-only", "templates/prometheusrule.yaml", "-f", vf}
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			s := string(out)
			requireSubcharts(t, s)
			// A gated-empty template still renders its Source header, so any error here
			// is real.
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		return string(out)
	}

	// parseRule returns the first PrometheusRule doc's group names, or nil.
	parseRule := func(t *testing.T, out string) []string {
		t.Helper()
		for _, doc := range strings.Split(out, "\n---") {
			var pr struct {
				Kind string `yaml:"kind"`
				Spec struct {
					Groups []struct {
						Name string `yaml:"name"`
					} `yaml:"groups"`
				} `yaml:"spec"`
			}
			if err := yaml.Unmarshal([]byte(doc), &pr); err != nil {
				// A PrometheusRule doc that will not parse is a malformed template.
				if strings.Contains(doc, "kind: PrometheusRule") {
					t.Fatalf("PrometheusRule doc failed to parse: %v\n%s", err, doc)
				}
				continue
			}
			if pr.Kind != "PrometheusRule" {
				continue
			}
			names := make([]string, 0, len(pr.Spec.Groups))
			for _, g := range pr.Spec.Groups {
				names = append(names, g.Name)
			}
			return names
		}
		return nil
	}

	t.Run("not rendered without the monitoring API", func(t *testing.T) {
		// No --api-versions means no monitoring.coreos.com kind.
		if groups := parseRule(t, render(t, "{}\n")); groups != nil {
			t.Errorf("PrometheusRule rendered where the apiserver serves no PrometheusRule kind: groups=%v", groups)
		}
	})

	t.Run("not rendered when metrics disabled", func(t *testing.T) {
		// Gated on metrics.enabled too: no endpoint, no input series.
		groups := parseRule(t, render(t, "metrics:\n  enabled: false\n  prometheusRule:\n    enabled: true\n"))
		if groups != nil {
			t.Errorf("PrometheusRule rendered with metrics.enabled=false: groups=%v", groups)
		}
	})

	t.Run("recording + alert groups when enabled", func(t *testing.T) {
		groups := parseRule(t, render(t, "metrics:\n  prometheusRule:\n    enabled: true\n"))
		if len(groups) != 2 {
			t.Fatalf("groups = %v, want recording + alerts", groups)
		}
		joined := strings.Join(groups, ",")
		if !strings.Contains(joined, "recording") || !strings.Contains(joined, "alerts") {
			t.Errorf("groups = %v, want a recording and an alerts group", groups)
		}
	})

	t.Run("alerts=false ships only recording rules", func(t *testing.T) {
		groups := parseRule(t, render(t, "metrics:\n  prometheusRule:\n    enabled: true\n    alerts: false\n"))
		if len(groups) != 1 || !strings.Contains(groups[0], "recording") {
			t.Errorf("groups = %v, want only the recording group when alerts=false", groups)
		}
	})

	// Alert text is held to the panel description length limit.
	t.Run("alert annotations stay inside the length limit", func(t *testing.T) {
		for name, a := range renderAlerts(t) {
			for _, field := range []string{"summary", "description"} {
				text := a.Annotations[field]
				if strings.TrimSpace(text) == "" {
					t.Errorf("alert %s has no %s", name, field)
					continue
				}
				assertDescriptionFits(t, fmt.Sprintf("the %s of alert %s", field, name), text)
			}
		}
	})

	// Free CPUs are counted against the pinnable pool: excluded CPUs can be taken by
	// nobody.
	t.Run("free CPUs are counted against the pinnable pool", func(t *testing.T) {
		out := render(t, "metrics:\n  prometheusRule:\n    enabled: true\n")
		expr := recordedExpr(t, out, "cpu_pinning:cpu_free:per_numa")
		if !strings.Contains(expr, "cpu_pinning_node_pinnable_cpus") {
			t.Errorf("cpu_free:per_numa does not read the pinnable pool: %s", expr)
		}
		// The hardware total is the fallback for a node whose pool is not resolved yet.
		if !strings.Contains(expr, "or max by (node, numa) (cpu_pinning_node_logical_cpus)") {
			t.Errorf("cpu_free:per_numa lost its hardware-total fallback: %s", expr)
		}
		// Used and pinnable can cross after a mode switch; room does not go negative.
		if !strings.Contains(expr, "clamp_min(") {
			t.Errorf("cpu_free:per_numa is not clamped at 0, so it can report a negative "+
				"amount of room once used exceeds the pinnable total: %s", expr)
		}
	})
}

// TestEveryAlertHasADocumentedRemedy proves every alert has a remedy row in
// docs/troubleshooting.md and links to it in its own text.
func TestEveryAlertHasADocumentedRemedy(t *testing.T) {
	const docPath = "docs/troubleshooting.md"
	body, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(docPath)))
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	lines := strings.Split(string(body), "\n")

	for name, a := range renderAlerts(t) {
		anchor := strings.ToLower(name)
		// The row must carry the anchor and name the alert.
		row := ""
		for _, line := range lines {
			if strings.Contains(line, `<a id="`+anchor+`">`) {
				row = line
				break
			}
		}
		switch {
		case row == "":
			t.Errorf("alert %s has no remedy in %s: no row anchored %q", name, docPath, anchor)
		case !strings.Contains(row, name):
			t.Errorf("the %q row in %s does not name alert %s: %s", anchor, docPath, name, row)
		}

		// The link must be in the text Alertmanager shows.
		link := docPath + "#" + anchor
		if !strings.Contains(a.Annotations["summary"]+a.Annotations["description"], link) {
			t.Errorf("alert %s does not point at its remedy: neither its summary nor its description contains %q", name, link)
		}
	}
}

// recordedExpr returns the expr of one recording rule in the rendered chart output.
func recordedExpr(t *testing.T, out, record string) string {
	t.Helper()
	for _, doc := range strings.Split(out, "\n---") {
		var pr struct {
			Kind string `yaml:"kind"`
			Spec struct {
				Groups []struct {
					Rules []struct {
						Record string `yaml:"record"`
						Expr   string `yaml:"expr"`
					} `yaml:"rules"`
				} `yaml:"groups"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &pr); err != nil || pr.Kind != "PrometheusRule" {
			continue
		}
		for _, g := range pr.Spec.Groups {
			for _, r := range g.Rules {
				if r.Record == record {
					return r.Expr
				}
			}
		}
	}
	t.Fatalf("recording rule %q not found in the rendered chart", record)
	return ""
}
