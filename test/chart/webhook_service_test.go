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

// TestWebhookServiceSessionAffinity proves sessionAffinity accepts "", None and ClientIP
// and fails the render on anything else.
func TestWebhookServiceSessionAffinity(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	// render runs `helm template` for the webhook-service template only.
	render := func(t *testing.T, valuesFile string) (string, error) {
		t.Helper()
		args := []string{
			"template", "t", chartDir, "-f", policyTargetValues,
			"--show-only", "templates/webhook-service.yaml",
		}
		if valuesFile != "" {
			args = append(args, "-f", valuesFile)
		}
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

	t.Run("omitted by default", func(t *testing.T) {
		// The default "" renders no sessionAffinity field.
		out, err := render(t, "")
		if err != nil {
			t.Fatalf("helm template failed with chart defaults: %v\n%s", err, out)
		}
		if strings.Contains(out, "sessionAffinity:") {
			t.Errorf("sessionAffinity rendered with empty default:\n%s", out)
		}
	})

	t.Run("ClientIP renders", func(t *testing.T) {
		vf := writeValues(t, "webhook:\n  service:\n    sessionAffinity: ClientIP\n")
		out, err := render(t, vf)
		if err != nil {
			t.Fatalf("helm template failed for ClientIP: %v\n%s", err, out)
		}
		if !strings.Contains(out, "sessionAffinity: ClientIP") {
			t.Errorf("expected sessionAffinity: ClientIP in render:\n%s", out)
		}
	})

	t.Run("None renders", func(t *testing.T) {
		vf := writeValues(t, "webhook:\n  service:\n    sessionAffinity: None\n")
		out, err := render(t, vf)
		if err != nil {
			t.Fatalf("helm template failed for None: %v\n%s", err, out)
		}
		if !strings.Contains(out, "sessionAffinity: None") {
			t.Errorf("expected sessionAffinity: None in render:\n%s", out)
		}
	})

	t.Run("invalid value fails fast", func(t *testing.T) {
		vf := writeValues(t, "webhook:\n  service:\n    sessionAffinity: Sticky\n")
		out, err := render(t, vf)
		if err == nil {
			t.Fatalf("expected helm template to fail for unsupported sessionAffinity, got success:\n%s", out)
		}
		if !strings.Contains(out, "sessionAffinity must be") {
			t.Errorf("expected fail-fast message naming sessionAffinity, got:\n%s", out)
		}
	})
}
