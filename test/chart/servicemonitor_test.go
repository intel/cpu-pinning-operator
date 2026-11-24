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

// TestServiceMonitorTLSRendering proves the ServiceMonitor scrapes over https, verifies
// against the serving cert's CA, and never sets insecureSkipVerify.
func TestServiceMonitorTLSRendering(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	render := func(t *testing.T, template, valuesFile string) string {
		t.Helper()
		args := []string{"template", "t", chartDir, "-f", policyTargetValues, "--show-only", template}
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

	writeValues := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write values: %v", err)
		}
		return p
	}

	t.Run("https scheme with CA-backed tlsConfig by default", func(t *testing.T) {
		// metrics.secure defaults to true; enable the ServiceMonitor to render it.
		vf := writeValues(t, "metrics:\n  serviceMonitor:\n    enabled: true\n")
		out := render(t, "templates/servicemonitor.yaml", vf)
		sm := parseServiceMonitor(t, out)
		ep := firstEndpoint(t, sm)

		if scheme := childValue(ep, "scheme"); scheme == nil || scheme.Value != "https" {
			t.Errorf("endpoint scheme = %v, want https (metrics served over TLS):\n%s", scheme, out)
		}
		tlsCfg := childValue(ep, "tlsConfig")
		if tlsCfg == nil {
			t.Fatalf("endpoint missing tlsConfig; https scrape would have no CA to verify against:\n%s", out)
		}

		caKey := childValue(tlsCfg, "ca", "secret", "key")
		if caKey == nil || caKey.Value != "ca.crt" {
			t.Errorf("tlsConfig.ca.secret.key = %v, want ca.crt:\n%s", caKey, out)
		}
		caSecret := childValue(tlsCfg, "ca", "secret", "name")
		if caSecret == nil || caSecret.Value != "webhook-server-cert" {
			t.Errorf("tlsConfig.ca.secret.name = %v, want the serving-cert secret webhook-server-cert:\n%s", caSecret, out)
		}
		if sn := childValue(tlsCfg, "serverName"); sn == nil || !strings.HasSuffix(sn.Value, "-metrics.default.svc") {
			t.Errorf("tlsConfig.serverName = %v, want the metrics Service DNS name:\n%s", sn, out)
		}
		// Check the parsed field: the template's own comment mentions it.
		if skip := childValue(tlsCfg, "insecureSkipVerify"); skip != nil && skip.Value == "true" {
			t.Errorf("tlsConfig sets insecureSkipVerify: true; TLS verification must stay on:\n%s", out)
		}
	})

	t.Run("plaintext fallback drops scheme and tlsConfig", func(t *testing.T) {
		vf := writeValues(t, "metrics:\n  secure: false\n  serviceMonitor:\n    enabled: true\n")
		out := render(t, "templates/servicemonitor.yaml", vf)
		sm := parseServiceMonitor(t, out)
		ep := firstEndpoint(t, sm)
		if scheme := childValue(ep, "scheme"); scheme != nil {
			t.Errorf("endpoint scheme = %q rendered while metrics.secure=false; expected none (http):\n%s", scheme.Value, out)
		}
		if tlsCfg := childValue(ep, "tlsConfig"); tlsCfg != nil {
			t.Errorf("tlsConfig rendered while metrics.secure=false:\n%s", out)
		}
	})

	t.Run("secure scrape without cert-manager fails the render", func(t *testing.T) {
		// Without cert-manager or allowExternalServingCert the serving-cert secret never
		// exists, so the render must fail rather than emit a scrape that never verifies.
		vf := writeValues(t, "metrics:\n  secure: true\n  serviceMonitor:\n    enabled: true\n    allowExternalServingCert: false\ncertManager:\n  enabled: false\nballoonsReady:\n  secure: false\n")
		args := []string{"template", "t", chartDir, "-f", policyTargetValues, "--show-only", "templates/servicemonitor.yaml", "-f", vf}
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			// The message must name both remedies.
			if !strings.Contains(string(out), "allowExternalServingCert=true") ||
				!strings.Contains(string(out), "certManager.enabled=true") {
				t.Errorf("render failed but not with the expected serving-cert gating message (should name both remedies):\n%s", out)
			}
			return
		}
		t.Errorf("expected a hard fail for secure metrics + certManager.enabled=false, but render succeeded:\n%s", out)
	})

	t.Run("secure scrape without cert-manager renders when an external serving cert is opted in", func(t *testing.T) {
		// allowExternalServingCert=true (BYO secret) must render the secure
		// ServiceMonitor normally.
		vf := writeValues(t, "metrics:\n  secure: true\n  serviceMonitor:\n    enabled: true\n    allowExternalServingCert: true\ncertManager:\n  enabled: false\nballoonsReady:\n  secure: false\n")
		out := render(t, "templates/servicemonitor.yaml", vf)
		sm := parseServiceMonitor(t, out)
		ep := firstEndpoint(t, sm)

		if scheme := childValue(ep, "scheme"); scheme == nil || scheme.Value != "https" {
			t.Errorf("endpoint scheme = %v, want https (secure scrape with external serving cert):\n%s", scheme, out)
		}
		tlsCfg := childValue(ep, "tlsConfig")
		if tlsCfg == nil {
			t.Fatalf("endpoint missing tlsConfig; https scrape would have no CA to verify against:\n%s", out)
		}
		// Same secret name, now provided out-of-band.
		caKey := childValue(tlsCfg, "ca", "secret", "key")
		if caKey == nil || caKey.Value != "ca.crt" {
			t.Errorf("tlsConfig.ca.secret.key = %v, want ca.crt:\n%s", caKey, out)
		}
		caSecret := childValue(tlsCfg, "ca", "secret", "name")
		if caSecret == nil || caSecret.Value != "webhook-server-cert" {
			t.Errorf("tlsConfig.ca.secret.name = %v, want the serving-cert secret webhook-server-cert:\n%s", caSecret, out)
		}

		if skip := childValue(tlsCfg, "insecureSkipVerify"); skip != nil && skip.Value == "true" {
			t.Errorf("tlsConfig sets insecureSkipVerify: true; TLS verification must stay on:\n%s", out)
		}
	})

	t.Run("secure scrape with cert-manager disabled is fine when the ServiceMonitor is off", func(t *testing.T) {
		// The gate only trips when the ServiceMonitor renders.
		vf := writeValues(t, "metrics:\n  secure: true\n  serviceMonitor:\n    enabled: false\ncertManager:\n  enabled: false\nballoonsReady:\n  secure: false\n")
		out := render(t, "templates/servicemonitor.yaml", vf)
		if strings.Contains(out, "kind: ServiceMonitor") {
			t.Errorf("ServiceMonitor rendered while serviceMonitor.enabled=false:\n%s", out)
		}
	})

	t.Run("serving cert covers the metrics Service DNS names", func(t *testing.T) {
		// Metrics are served with the webhook cert, so its SANs must include the metrics
		// Service names.
		out := render(t, "templates/certificate.yaml", "")
		if !strings.Contains(out, "-metrics.default.svc") {
			t.Errorf("serving Certificate does not cover the metrics Service DNS name; https scrape verification would fail:\n%s", out)
		}
	})
}

// parseServiceMonitor extracts the single ServiceMonitor document from a multi-doc
// render.
func parseServiceMonitor(t *testing.T, rendered string) *yaml.Node {
	t.Helper()
	for _, doc := range strings.Split(rendered, "\n---") {
		if !strings.Contains(doc, "kind: ServiceMonitor") {
			continue
		}
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
			t.Fatalf("parse rendered ServiceMonitor: %v", err)
		}
		if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
			return n.Content[0]
		}
		return &n
	}
	t.Fatalf("no ServiceMonitor document in render:\n%s", rendered)
	return nil
}

// firstEndpoint returns the first entry of spec.endpoints.
func firstEndpoint(t *testing.T, sm *yaml.Node) *yaml.Node {
	t.Helper()
	eps := childValue(sm, "spec", "endpoints")
	if eps == nil || eps.Kind != yaml.SequenceNode || len(eps.Content) == 0 {
		t.Fatalf("ServiceMonitor has no spec.endpoints entries")
	}
	return eps.Content[0]
}
