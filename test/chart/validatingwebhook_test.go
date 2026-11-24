// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestValidatingWebhookFailurePolicy proves the policy guard defaults to failurePolicy
// Fail but renders Ignore without cert-manager, where the placeholder caBundle would
// otherwise block every policy DELETE/UPDATE.
func TestValidatingWebhookFailurePolicy(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	render := func(t *testing.T, valuesBody string) string {
		t.Helper()
		vf := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(vf, []byte(valuesBody), 0o600); err != nil {
			t.Fatalf("write values: %v", err)
		}
		args := []string{"template", "t", chartDir, "-f", policyTargetValues, "--show-only", "templates/validatingwebhook.yaml", "-f", vf}
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		return string(out)
	}

	failurePolicy := func(t *testing.T, rendered string) string {
		t.Helper()
		for _, doc := range strings.Split(rendered, "\n---") {
			if !strings.Contains(doc, "kind: ValidatingWebhookConfiguration") {
				continue
			}
			var n yaml.Node
			if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
				t.Fatalf("parse rendered ValidatingWebhookConfiguration: %v", err)
			}
			root := &n
			if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
				root = n.Content[0]
			}
			hooks := childValue(root, "webhooks")
			if hooks == nil || hooks.Kind != yaml.SequenceNode || len(hooks.Content) == 0 {
				t.Fatalf("no webhooks entry in render:\n%s", rendered)
			}
			fp := childValue(hooks.Content[0], "failurePolicy")
			if fp == nil {
				t.Fatalf("webhook has no failurePolicy:\n%s", rendered)
			}
			return fp.Value
		}
		t.Fatalf("no ValidatingWebhookConfiguration document in render:\n%s", rendered)
		return ""
	}

	t.Run("defaults to Fail with cert-manager", func(t *testing.T) {
		out := render(t, "certManager:\n  enabled: true\n")
		if fp := failurePolicy(t, out); fp != "Fail" {
			t.Errorf("failurePolicy = %q, want Fail (guard runs closed when a CA is injected):\n%s", fp, out)
		}
	})

	t.Run("forced to Ignore without cert-manager", func(t *testing.T) {
		// The caBundle is a placeholder here.
		out := render(t, "certManager:\n  enabled: false\nballoonsReady:\n  secure: false\n")
		if fp := failurePolicy(t, out); fp != "Ignore" {
			t.Errorf("failurePolicy = %q, want Ignore (no injected CA — Fail would brick policy edits):\n%s", fp, out)
		}
	})

	t.Run("configured failurePolicy is honored with cert-manager", func(t *testing.T) {
		out := render(t, "certManager:\n  enabled: true\nwebhook:\n  validatingWebhookConfiguration:\n    failurePolicy: Ignore\n")
		if fp := failurePolicy(t, out); fp != "Ignore" {
			t.Errorf("failurePolicy = %q, want the configured Ignore:\n%s", fp, out)
		}
	})
}

// TestWebhookCABundle proves webhook.caBundle reaches both webhook configurations
// without cert-manager and lifts the forced Ignore, while the default keeps the
// placeholder and cert-manager ignores the value.
func TestWebhookCABundle(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")
	bundle := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"))
	byo := []string{"--set", "certManager.enabled=false", "--set", "balloonsReady.secure=false"}

	render := func(t *testing.T, args ...string) (string, error) {
		t.Helper()
		args = append([]string{"template", "t", chartDir, "-f", policyTargetValues,
			"--show-only", "templates/mutatingwebhook.yaml",
			"--show-only", "templates/validatingwebhook.yaml"}, args...)
		out, err := exec.Command(helm, args...).CombinedOutput()
		requireSubcharts(t, string(out))
		return string(out), err
	}
	// caBundles returns the clientConfig.caBundle of each webhook, "" when absent.
	caBundles := func(t *testing.T, rendered string) map[string]string {
		t.Helper()
		got := map[string]string{}
		for _, doc := range strings.Split(rendered, "\n---") {
			var obj struct {
				Kind     string
				Webhooks []struct {
					ClientConfig struct {
						CABundle string `yaml:"caBundle"`
					} `yaml:"clientConfig"`
					FailurePolicy string `yaml:"failurePolicy"`
				}
			}
			if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
				t.Fatalf("parse render: %v\n%s", err, doc)
			}
			if obj.Kind == "" {
				continue
			}
			got[obj.Kind] = obj.Webhooks[0].ClientConfig.CABundle
			got[obj.Kind+"/failurePolicy"] = obj.Webhooks[0].FailurePolicy
		}
		return got
	}
	const mwc, vwc = "MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"

	t.Run("default keeps the placeholder and forced Ignore", func(t *testing.T) {
		out, err := render(t, byo...)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, out)
		}
		got := caBundles(t, out)
		if got[mwc] != "Cg==" || got[vwc] != "Cg==" || got[vwc+"/failurePolicy"] != "Ignore" {
			t.Errorf("got %v, want placeholder Cg== on both and validating Ignore", got)
		}
	})

	t.Run("BYO caBundle reaches both webhooks and lifts forced Ignore", func(t *testing.T) {
		out, err := render(t, append(byo, "--set", "webhook.caBundle="+bundle)...)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, out)
		}
		got := caBundles(t, out)
		if got[mwc] != bundle || got[vwc] != bundle || got[vwc+"/failurePolicy"] != "Fail" {
			t.Errorf("got %v, want the bundle on both and the configured Fail", got)
		}
	})

	// A line-wrapped bundle (base64 without -w0) still decodes, so it must be
	// emitted as one line or both manifests stop being valid YAML.
	t.Run("wrapped caBundle renders single-line", func(t *testing.T) {
		pemB64 := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n", 3)))
		var w strings.Builder
		for s := pemB64; len(s) > 0; {
			n := min(76, len(s))
			w.WriteString(s[:n] + "\n")
			s = s[n:]
		}
		dir := t.TempDir()
		bundleFile := filepath.Join(dir, "ca.b64")
		valuesFile := filepath.Join(dir, "values.yaml")
		if err := os.WriteFile(bundleFile, []byte(w.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		block := "webhook:\n  caBundle: |\n    " + strings.ReplaceAll(strings.TrimSuffix(w.String(), "\n"), "\n", "\n    ") + "\n"
		if err := os.WriteFile(valuesFile, []byte(block), 0o600); err != nil {
			t.Fatal(err)
		}
		for name, args := range map[string][]string{
			"values file": {"-f", valuesFile},
			"--set-file":  {"--set-file", "webhook.caBundle=" + bundleFile},
		} {
			out, err := render(t, append(byo, args...)...)
			if err != nil {
				t.Fatalf("%s: helm template: %v\n%s", name, err, out)
			}
			got := caBundles(t, out)
			if got[mwc] != pemB64 || got[vwc] != pemB64 {
				t.Errorf("%s: got %v, want the unwrapped bundle on both", name, got)
			}
		}
	})

	// --reuse-values from a chart without the key leaves webhook.caBundle nil.
	t.Run("missing caBundle key keeps the placeholder and forced Ignore", func(t *testing.T) {
		out, err := render(t, append(byo, "--set", "webhook.caBundle=null")...)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, out)
		}
		got := caBundles(t, out)
		if got[mwc] != "Cg==" || got[vwc] != "Cg==" || got[vwc+"/failurePolicy"] != "Ignore" {
			t.Errorf("got %v, want placeholder Cg== on both and validating Ignore", got)
		}
	})

	t.Run("whitespace-only caBundle keeps the placeholder and forced Ignore", func(t *testing.T) {
		vf := filepath.Join(t.TempDir(), "ws.yaml")
		if err := os.WriteFile(vf, []byte("webhook:\n  caBundle: \" \\n\\t\"\n"), 0o600); err != nil {
			t.Fatalf("write values: %v", err)
		}
		out, err := render(t, append(byo, "-f", vf)...)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, out)
		}
		got := caBundles(t, out)
		if got[mwc] != "Cg==" || got[vwc] != "Cg==" || got[vwc+"/failurePolicy"] != "Ignore" {
			t.Errorf("got %v, want placeholder Cg== on both and validating Ignore", got)
		}
	})

	t.Run("cert-manager ignores the value", func(t *testing.T) {
		out, err := render(t, "--set", "certManager.enabled=true", "--set", "webhook.caBundle="+bundle)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, out)
		}
		got := caBundles(t, out)
		if got[mwc] != "" || got[vwc] != "" {
			t.Errorf("got %v, want no rendered caBundle (cainjector owns it)", got)
		}
	})

	t.Run("non-PEM value fails the render", func(t *testing.T) {
		out, err := render(t, append(byo, "--set", "webhook.caBundle=Zm9v")...)
		if err == nil || !strings.Contains(out, "webhook.caBundle must be a base64-encoded PEM") {
			t.Errorf("want a render failure naming webhook.caBundle, got err=%v:\n%s", err, out)
		}
	})
}
