// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestPodDisruptionBudgetRendering proves the manager PDB renders when enabled, not when
// disabled, selects the manager's pods, and honours minAvailable.
func TestPodDisruptionBudgetRendering(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	// render runs `helm template` for the poddisruptionbudget template only ("" = chart
	// defaults).
	render := func(t *testing.T, valuesFile string) string {
		t.Helper()
		args := []string{
			"template", "t", chartDir, "-f", policyTargetValues,
			"--show-only", "templates/poddisruptionbudget.yaml",
		}
		if valuesFile != "" {
			args = append(args, "-f", valuesFile)
		}
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		return string(out)
	}

	// writeValues writes a values overlay in the test's temp dir.
	writeValues := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write values: %v", err)
		}
		return p
	}

	// hasPDB ignores the source header helm emits even for an empty render.
	hasPDB := func(out string) bool {
		return strings.Contains(out, "kind: PodDisruptionBudget")
	}

	t.Run("present with minAvailable 1 by default", func(t *testing.T) {
		// Chart default is podDisruptionBudget.enabled=true.
		out := render(t, "")
		if !hasPDB(out) {
			t.Fatalf("PodDisruptionBudget not rendered with chart defaults:\n%s", out)
		}
		pdb := parsePDB(t, out)
		if ma := childValue(pdb, "spec", "minAvailable"); ma == nil || ma.Value != "1" {
			t.Errorf("expected spec.minAvailable=1 by default, got %v", ma)
		}
	})

	t.Run("selects the manager via selectorLabels helper", func(t *testing.T) {
		out := render(t, "")
		pdb := parsePDB(t, out)
		// A PDB whose selector misses the manager's control-plane label protects nothing.
		sel := childValue(pdb, "spec", "selector", "matchLabels", "control-plane")
		if sel == nil || sel.Value != "controller-manager" {
			t.Errorf("expected spec.selector.matchLabels.control-plane=controller-manager, got %v:\n%s", sel, out)
		}
	})

	t.Run("absent when disabled", func(t *testing.T) {
		vf := writeValues(t, "podDisruptionBudget:\n  enabled: false\n")
		out := render(t, vf)
		if hasPDB(out) {
			t.Errorf("PodDisruptionBudget rendered while podDisruptionBudget.enabled=false:\n%s", out)
		}
	})

	t.Run("minAvailable overridable", func(t *testing.T) {
		vf := writeValues(t, "podDisruptionBudget:\n  enabled: true\n  minAvailable: 2\n")
		out := render(t, vf)
		pdb := parsePDB(t, out)
		if ma := childValue(pdb, "spec", "minAvailable"); ma == nil || ma.Value != "2" {
			t.Errorf("expected spec.minAvailable=2 when overridden, got %v", ma)
		}
	})

	t.Run("absent at replicaCount 1 even when enabled", func(t *testing.T) {
		// On one replica, minAvailable:1 would block node drains, so the template gates
		// off.
		vf := writeValues(t, "replicaCount: 1\npodDisruptionBudget:\n  enabled: true\n  minAvailable: 1\n")
		out := render(t, vf)
		if hasPDB(out) {
			t.Errorf("PodDisruptionBudget rendered at replicaCount=1 (would block drains):\n%s", out)
		}
	})

	t.Run("present at replicaCount 2 when enabled", func(t *testing.T) {
		vf := writeValues(t, "replicaCount: 2\npodDisruptionBudget:\n  enabled: true\n  minAvailable: 1\n")
		out := render(t, vf)
		if !hasPDB(out) {
			t.Errorf("PodDisruptionBudget must render at replicaCount=2 when enabled:\n%s", out)
		}
	})
}

// parsePDB extracts the single PodDisruptionBudget document from a multi-doc render.
func parsePDB(t *testing.T, rendered string) *yaml.Node {
	t.Helper()
	for _, doc := range strings.Split(rendered, "\n---") {
		if !strings.Contains(doc, "kind: PodDisruptionBudget") {
			continue
		}
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
			t.Fatalf("parse rendered PodDisruptionBudget: %v", err)
		}
		if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
			return n.Content[0]
		}
		return &n
	}
	t.Fatalf("no PodDisruptionBudget document in render:\n%s", rendered)
	return nil
}
