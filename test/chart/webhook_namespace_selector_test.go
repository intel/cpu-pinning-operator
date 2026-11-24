// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metavalidation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

// TestMutatingWebhookNamespaceSelector proves the bootstrap selector is valid and scopes
// admission to exactly targetNamespaces; an empty list renders the closed gate the
// reconciler writes, not an invalid `In` with no values.
func TestMutatingWebhookNamespaceSelector(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	// The API server sets kubernetes.io/metadata.name on every namespace.
	nsLabels := func(name string) labels.Set {
		return labels.Set{"kubernetes.io/metadata.name": name}
	}

	render := func(t *testing.T, setJSON string) *metav1.LabelSelector {
		t.Helper()
		args := []string{"template", "t", chartDir, "-f", policyTargetValues, "--show-only", "templates/mutatingwebhook.yaml"}
		if setJSON != "" {
			args = append(args, "--set-json", setJSON)
		}
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		var cfg admissionregistrationv1.MutatingWebhookConfiguration
		if err := yaml.Unmarshal(out, &cfg); err != nil {
			t.Fatalf("rendered MutatingWebhookConfiguration failed to parse: %v\n%s", err, out)
		}
		if len(cfg.Webhooks) != 1 {
			t.Fatalf("webhooks = %d, want 1\n%s", len(cfg.Webhooks), out)
		}
		sel := cfg.Webhooks[0].NamespaceSelector
		if sel == nil {
			t.Fatalf("namespaceSelector is nil — a nil selector matches every namespace\n%s", out)
		}

		if errs := metavalidation.ValidateLabelSelector(sel, metavalidation.LabelSelectorValidationOptions{},
			field.NewPath("namespaceSelector")); len(errs) > 0 {
			t.Fatalf("rendered namespaceSelector is invalid: %v\n%s", errs, out)
		}
		return sel
	}

	tests := []struct {
		name    string
		setJSON string
		// matches maps a namespace name to whether admission must see it.
		matches map[string]bool
	}{
		{
			name:    "falls back to the policy's namespaces",
			matches: map[string]bool{"inference": true, "default": false, "kube-system": false},
		},
		{
			name:    "fallback drops globs",
			setJSON: `balloonPinningPolicy.namespaces=["inference","team-*"]`,
			matches: map[string]bool{"inference": true, "team-a": false, "team-*": false},
		},
		{
			name:    "no policy, no targets matches nothing",
			setJSON: `balloonPinningPolicy.create=false`,
			matches: map[string]bool{"inference": false, "default": false},
		},
		{
			name:    "targetNamespaces wins over the policy",
			setJSON: `webhook.targetNamespaces=["other"]`,
			matches: map[string]bool{"other": true, "inference": false},
		},
		{
			name:    "two named namespaces",
			setJSON: `webhook.targetNamespaces=["a","b"]`,
			matches: map[string]bool{"a": true, "b": true, "ab": false, "": false},
		},
		{
			name:    "empty list matches nothing",
			setJSON: `webhook.targetNamespaces=[],balloonPinningPolicy.create=false`,
			matches: map[string]bool{"inference": false, "default": false, "kube-system": false},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sel := render(t, tc.setJSON)
			got, err := metav1.LabelSelectorAsSelector(sel)
			if err != nil {
				t.Fatalf("selector does not convert: %v", err)
			}
			for ns, want := range tc.matches {
				if got.Matches(nsLabels(ns)) != want {
					t.Errorf("namespace %q matched = %v, want %v (selector %s)",
						ns, !want, want, got)
				}
			}
		})
	}
}
