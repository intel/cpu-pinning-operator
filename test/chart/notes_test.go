// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

// TestNotes proves NOTES.txt shows the no-NetworkPolicy warning and the CRD re-apply
// reminder only where they apply.
// `helm template` never renders NOTES.txt, so it is executed with text/template and a
// stubbed `include`.
func TestNotes(t *testing.T) {
	notesPath := filepath.Join("..", "..", "deploy", "cpu-pinning-operator", "templates", "NOTES.txt")
	notesBody, err := os.ReadFile(notesPath)
	if err != nil {
		t.Fatalf("read NOTES.txt: %v", err)
	}

	render := func(t *testing.T, networkPolicyEnabled, isUpgrade bool) string {
		t.Helper()

		var tmpl *template.Template
		funcs := template.FuncMap{
			"int": func(v interface{}) int {
				switch n := v.(type) {
				case int:
					return n
				default:
					var i int
					fmt.Sscanf(fmt.Sprint(n), "%d", &i)
					return i
				}
			},
			// NOTES.txt only calls `include "cpu-pinning-operator.name" .`.
			"include": func(name string, data interface{}) (string, error) {
				var buf bytes.Buffer
				if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
					return "", err
				}
				return buf.String(), nil
			},
		}

		tmpl = template.New("root").Funcs(funcs)
		if _, err := tmpl.New("cpu-pinning-operator.name").Parse("cpu-pinning-operator"); err != nil {
			t.Fatalf("parse stub helper: %v", err)
		}
		notes, err := tmpl.New("NOTES.txt").Parse(string(notesBody))
		if err != nil {
			t.Fatalf("parse NOTES.txt: %v", err)
		}

		data := map[string]interface{}{
			"Chart": map[string]interface{}{"AppVersion": "v0.0.0-test"},
			"Release": map[string]interface{}{
				"Name": "test", "Namespace": "test-ns", "IsUpgrade": isUpgrade,
			},
			"Values": map[string]interface{}{
				"replicaCount": 2,
				"networkPolicy": map[string]interface{}{
					"enabled": networkPolicyEnabled,
				},
			},
		}
		var out bytes.Buffer
		if err := notes.Execute(&out, data); err != nil {
			t.Fatalf("execute NOTES.txt: %v", err)
		}
		return out.String()
	}

	const warningNeedle = "networkPolicy.enabled is false"

	t.Run("absent when networkPolicy.enabled=true (chart default)", func(t *testing.T) {
		out := render(t, true, false)
		if strings.Contains(out, warningNeedle) {
			t.Errorf("unexpected NetworkPolicy warning with networkPolicy.enabled=true:\n%s", out)
		}
	})

	t.Run("present when networkPolicy.enabled=false", func(t *testing.T) {
		out := render(t, false, false)
		if !strings.Contains(out, warningNeedle) {
			t.Errorf("expected NetworkPolicy warning with networkPolicy.enabled=false:\n%s", out)
		}
	})

	// Helm treats crds/ as install-only, so the reminder shows on upgrade and not on
	// first install.
	const crdNeedle = "kubectl apply -f deploy/cpu-pinning-operator/crds/"

	t.Run("CRD re-apply reminder present on upgrade", func(t *testing.T) {
		out := render(t, true, true)
		if !strings.Contains(out, crdNeedle) {
			t.Errorf("expected the CRD re-apply command on upgrade:\n%s", out)
		}
		if !strings.Contains(out, "upgraded as release") {
			t.Errorf("expected the upgrade wording, not the install wording:\n%s", out)
		}
	})

	t.Run("CRD re-apply reminder absent on install", func(t *testing.T) {
		out := render(t, true, false)
		if strings.Contains(out, crdNeedle) {
			t.Errorf("unexpected CRD re-apply command on a first install:\n%s", out)
		}
		if !strings.Contains(out, "installed as release") {
			t.Errorf("expected the install wording:\n%s", out)
		}
	})
}
