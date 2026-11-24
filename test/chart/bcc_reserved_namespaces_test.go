// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// bccReserved is the slice of the rendered BalloonsClusterConfig these assertions read.
type bccReserved struct {
	Spec struct {
		Balloon struct {
			ReservedNamespaces []string       `yaml:"reservedNamespaces"`
			Reserved           map[string]any `yaml:"reserved"`
		} `yaml:"balloon"`
	} `yaml:"spec"`
}

// TestBalloonsClusterConfigReservedNamespacesOverlay proves the OpenShift overlay sets
// system reservedNamespaces and reserved.measured, the default leaves namespaces unset
// with reserved.cpus, and each render carries exactly one sizing key (CEL rejects more,
// and -f deep-merges).
func TestBalloonsClusterConfigReservedNamespacesOverlay(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	render := func(t *testing.T, valuesFile string) bccReserved {
		t.Helper()
		args := []string{"template", "t", chartDir, "-f", policyTargetValues}
		if valuesFile != "" {
			args = append(args, "-f", valuesFile)
		}
		args = append(args, "--show-only", "templates/bcc-default.yaml")
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		var bcc bccReserved
		if err := yaml.Unmarshal(out, &bcc); err != nil {
			t.Fatalf("rendered BalloonsClusterConfig failed to parse: %v\n%s", err, out)
		}
		return bcc
	}

	// sizingKeys reports which of the mutually exclusive reserved sizing keys are set.
	sizingKeys := func(reserved map[string]any) []string {
		var keys []string
		for _, k := range []string{"cpus", "percent", "measured"} {
			if _, ok := reserved[k]; ok {
				keys = append(keys, k)
			}
		}
		return keys
	}

	t.Run("global default", func(t *testing.T) {
		bcc := render(t, "")
		if ns := bcc.Spec.Balloon.ReservedNamespaces; ns != nil {
			t.Errorf("reservedNamespaces = %v, want unset so the operator applies its own default", ns)
		}
		if got := sizingKeys(bcc.Spec.Balloon.Reserved); !slices.Equal(got, []string{"cpus"}) {
			t.Errorf("reserved sizing keys = %v, want [cpus]", got)
		}
	})

	t.Run("OpenShift overlay", func(t *testing.T) {
		bcc := render(t, filepath.Join(chartDir, "values-openshift.yaml"))
		want := []string{"kube-system", "openshift-*"}
		if got := bcc.Spec.Balloon.ReservedNamespaces; !slices.Equal(got, want) {
			t.Errorf("reservedNamespaces = %v, want %v", got, want)
		}
		if got := sizingKeys(bcc.Spec.Balloon.Reserved); !slices.Equal(got, []string{"measured"}) {
			t.Errorf("reserved sizing keys = %v, want [measured]", got)
		}
	})
}
