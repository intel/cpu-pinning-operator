// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestManagerAffinity proves the default affinity is a preferred (not required)
// anti-affinity on the manager's selector labels, and a custom .Values.affinity replaces
// it wholesale.
func TestManagerAffinity(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	render := func(t *testing.T, valuesBody string) string {
		t.Helper()
		args := []string{"template", "t", chartDir, "-f", policyTargetValues, "--show-only", "templates/deployment.yaml"}
		if valuesBody != "" {
			vf := filepath.Join(t.TempDir(), "values.yaml")
			if err := os.WriteFile(vf, []byte(valuesBody), 0o600); err != nil {
				t.Fatalf("write values: %v", err)
			}
			args = append(args, "-f", vf)
		}
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		return string(out)
	}

	// affinityNode returns the pod spec's affinity node from a rendered Deployment.
	affinityNode := func(t *testing.T, rendered string) *yaml.Node {
		t.Helper()
		dep := parseDeployment(t, rendered)
		aff := childValue(dep, "spec", "template", "spec", "affinity")
		if aff == nil {
			t.Fatalf("Deployment pod spec has no affinity block:\n%s", rendered)
		}
		return aff
	}

	t.Run("default ships soft podAntiAffinity spreading across hostnames", func(t *testing.T) {
		aff := affinityNode(t, render(t, ""))

		// Preferred, so a single-node cluster schedules every replica.
		preferred := childValue(aff, "podAntiAffinity", "preferredDuringSchedulingIgnoredDuringExecution")
		if preferred == nil || preferred.Kind != yaml.SequenceNode || len(preferred.Content) == 0 {
			t.Fatalf("expected a preferred podAntiAffinity term; got %v", preferred)
		}
		if req := childValue(aff, "podAntiAffinity", "requiredDuringSchedulingIgnoredDuringExecution"); req != nil {
			t.Errorf("podAntiAffinity must be soft, not required (single-node must still schedule); got required: %v", req)
		}

		term := childValue(preferred.Content[0], "podAffinityTerm")
		if tk := childValue(term, "topologyKey"); tk == nil || tk.Value != "kubernetes.io/hostname" {
			t.Errorf("expected topologyKey kubernetes.io/hostname, got %v", tk)
		}

		sel := childValue(term, "labelSelector", "matchLabels", "control-plane")
		if sel == nil || sel.Value != "controller-manager" {
			t.Errorf("expected anti-affinity labelSelector control-plane=controller-manager, got %v", sel)
		}
	})

	t.Run("default prefers control-plane nodes (soft)", func(t *testing.T) {
		aff := affinityNode(t, render(t, ""))
		preferred := childValue(aff, "nodeAffinity", "preferredDuringSchedulingIgnoredDuringExecution")
		if preferred == nil || preferred.Kind != yaml.SequenceNode || len(preferred.Content) == 0 {
			t.Fatalf("expected a preferred nodeAffinity term; got %v", preferred)
		}
		if req := childValue(aff, "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution"); req != nil {
			t.Errorf("control-plane preference must be soft, not required; got required: %v", req)
		}
		exprs := childValue(preferred.Content[0], "preference", "matchExpressions")
		if exprs == nil || exprs.Kind != yaml.SequenceNode || len(exprs.Content) == 0 {
			t.Fatalf("expected a control-plane matchExpressions term; got %v", exprs)
		}
		key := childValue(exprs.Content[0], "key")
		if key == nil || key.Value != "node-role.kubernetes.io/control-plane" {
			t.Errorf("expected control-plane node label key, got %v", key)
		}
	})

	t.Run("operator-supplied affinity replaces the default wholesale", func(t *testing.T) {
		// A custom affinity renders verbatim, with no trace of the default.
		vf := "affinity:\n" +
			"  podAntiAffinity:\n" +
			"    requiredDuringSchedulingIgnoredDuringExecution:\n" +
			"      - topologyKey: topology.kubernetes.io/zone\n" +
			"        labelSelector:\n" +
			"          matchLabels:\n" +
			"            app: custom\n"
		aff := affinityNode(t, render(t, vf))
		if req := childValue(aff, "podAntiAffinity", "requiredDuringSchedulingIgnoredDuringExecution"); req == nil {
			t.Errorf("expected the operator-supplied required anti-affinity to render")
		}
		if pref := childValue(aff, "podAntiAffinity", "preferredDuringSchedulingIgnoredDuringExecution"); pref != nil {
			t.Errorf("shipped soft default must not leak alongside a custom affinity; got %v", pref)
		}
		if na := childValue(aff, "nodeAffinity"); na != nil {
			t.Errorf("shipped control-plane nodeAffinity must not leak alongside a custom affinity; got %v", na)
		}
	})
}
