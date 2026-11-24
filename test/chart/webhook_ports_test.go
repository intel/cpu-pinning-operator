// Copyright (C) 2024-2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWebhookPortsRenderConsistently proves non-default webhook.port and
// webhook.service.port reach every place that must agree: the manager's
// --webhook-port, the webhook-server containerPort, the Service port, and both
// webhook clients' clientConfig.service.port.
func TestWebhookPortsRenderConsistently(t *testing.T) {
	args := []string{
		"template", "t", filepath.Join("..", "..", "deploy", "cpu-pinning-operator"), "-f", policyTargetValues,
		"--set", "webhook.port=9555", "--set", "webhook.service.port=8443",
		"--show-only", "templates/deployment.yaml",
		"--show-only", "templates/webhook-service.yaml",
		"--show-only", "templates/mutatingwebhook.yaml",
		"--show-only", "templates/validatingwebhook.yaml",
	}
	out, err := exec.Command(helmBin(t), args...).CombinedOutput()
	if err != nil {
		requireSubcharts(t, string(out))
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}

	type port struct {
		Name          string `yaml:"name"`
		Port          int    `yaml:"port"`
		TargetPort    string `yaml:"targetPort"`
		ContainerPort int    `yaml:"containerPort"`
	}
	type doc struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Ports    []port `yaml:"ports"`
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string   `yaml:"name"`
						Args  []string `yaml:"args"`
						Ports []port   `yaml:"ports"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
		Webhooks []struct {
			Name         string `yaml:"name"`
			ClientConfig struct {
				Service struct {
					Port int `yaml:"port"`
				} `yaml:"service"`
			} `yaml:"clientConfig"`
		} `yaml:"webhooks"`
	}

	seen := map[string]bool{}
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	for {
		var d doc
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decoding rendered YAML: %v\n%s", err, out)
		}
		switch d.Kind {
		case "Deployment":
			for _, c := range d.Spec.Template.Spec.Containers {
				if c.Name != "manager" {
					continue
				}
				seen["args"] = slices.Contains(c.Args, "--webhook-port=9555")
				for _, p := range c.Ports {
					if p.Name == "webhook-server" {
						seen["containerPort"] = p.ContainerPort == 9555
					}
				}
			}
		case "Service":
			for _, p := range d.Spec.Ports {
				if p.Name == "webhook-server" {
					seen["service"] = p.Port == 8443 && p.TargetPort == "webhook-server"
				}
			}
		case "MutatingWebhookConfiguration", "ValidatingWebhookConfiguration":
			for _, w := range d.Webhooks {
				if w.ClientConfig.Service.Port != 8443 {
					t.Errorf("%s %s clientConfig.service.port = %d, want 8443", d.Kind, w.Name, w.ClientConfig.Service.Port)
				}
				seen[d.Kind] = true
			}
		}
	}
	for _, k := range []string{"args", "containerPort", "service", "MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"} {
		if !seen[k] {
			t.Errorf("%s does not carry the configured webhook port\n%s", k, out)
		}
	}
}
