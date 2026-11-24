// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"strings"
	"testing"
)

// TestBalloonsReadyTLSRendering proves the readiness endpoint's scheme, CA ConfigMap and
// --balloons-ready-secure read one helper and agree.
func TestBalloonsReadyTLSRendering(t *testing.T) {
	const svc = "t-cpu-pinning-operator-balloons-ready.cpu-pinning-operator-system.svc:8082"
	const caCM = "READINESS_CA_CONFIGMAP: t-cpu-pinning-operator-readiness-ca"
	cases := map[string]struct {
		args     []string
		endpoint string
		secure   bool
	}{
		"default: cert-manager, TLS": {endpoint: "https://" + svc, secure: true},
		"external serving cert asserted": {
			args:     []string{"--set", "certManager.enabled=false", "--set", "balloonsReady.allowExternalServingCert=true"},
			endpoint: "https://" + svc, secure: true,
		},
		"plaintext opt-out": {
			args:     []string{"--set", "balloonsReady.secure=false"},
			endpoint: "http://" + svc,
		},
		"plaintext opt-out without cert-manager": {
			args:     []string{"--set", "certManager.enabled=false", "--set", "balloonsReady.secure=false"},
			endpoint: "http://" + svc,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out := renderChart(t, append([]string{
				"--show-only", "templates/configmap.yaml", "--show-only", "templates/deployment.yaml",
			}, tc.args...)...)
			for needle, present := range map[string]bool{
				`BALLOONS_READY_ENDPOINT: "` + tc.endpoint + `"`: true,
				caCM:                                  tc.secure,
				"--balloons-ready-secure=true":        tc.secure,
				"--balloons-ready-secure=false":       !tc.secure,
				"--balloons-ready-bind-address=:8082": true,
			} {
				if strings.Contains(out, needle) != present {
					t.Errorf("rendered %q present=%v, want %v", needle, !present, present)
				}
			}
		})
	}

	t.Run("TLS without a verifiable CA fails the render", func(t *testing.T) {
		out, err := renderValues(t, "--set", "certManager.enabled=false")
		if err == nil || !strings.Contains(out, "balloonsReady.secure=true requires a serving cert") {
			t.Fatalf("render = %v, want the unverifiable-CA failure:\n%s", err, out)
		}
	})
}

// TestServingCertCoversBalloonsReady proves the serving cert covers the Service name the
// init container dials and chains to the chart CA.
func TestServingCertCoversBalloonsReady(t *testing.T) {
	out := renderChart(t, "--show-only", "templates/certificate.yaml")
	for _, want := range []string{
		"- t-cpu-pinning-operator-balloons-ready.cpu-pinning-operator-system.svc\n",
		"- t-cpu-pinning-operator-balloons-ready.cpu-pinning-operator-system.svc.cluster.local\n",
		"isCA: true",
		"duration: 8760h",
		"secretName: t-cpu-pinning-operator-ca\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("certificate.yaml lacks %q:\n%s", want, out)
		}
	}
	caPart, serving, _ := strings.Cut(out, "name: t-cpu-pinning-operator-serving-cert")
	if !strings.Contains(serving, "name: t-cpu-pinning-operator-ca-issuer") {
		t.Errorf("serving cert is not issued by the CA issuer:\n%s", serving)
	}
	// The CA keeps its key so replicas holding either CA cert still verify; the leaf
	// still rotates.
	if !strings.Contains(caPart, "rotationPolicy: Never") {
		t.Errorf("CA Certificate does not keep its key across renewals (rotationPolicy: Never):\n%s", caPart)
	}
	if strings.Contains(serving, "rotationPolicy") {
		t.Errorf("serving Certificate pins a rotationPolicy; its key should rotate on renewal:\n%s", serving)
	}
}
