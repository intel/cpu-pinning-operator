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

// TestDeploymentMetricsArgs proves the metrics args track metrics.enabled: disabled
// passes --metrics-bind-address=0 and no --metrics-secure, so no unintended plaintext
// endpoint listens.
func TestDeploymentMetricsArgs(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	renderArgs := func(t *testing.T, valuesBody string) []string {
		t.Helper()
		vf := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(vf, []byte(valuesBody), 0o600); err != nil {
			t.Fatalf("write values: %v", err)
		}
		args := []string{"template", "t", chartDir, "-f", policyTargetValues, "--show-only", "templates/deployment.yaml", "-f", vf}
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		return managerArgs(t, string(out))
	}

	t.Run("enabled passes bind-address and metrics-secure", func(t *testing.T) {
		args := renderArgs(t, "metrics:\n  enabled: true\n  port: 8080\n  secure: true\n")
		if !hasArg(args, "--metrics-bind-address=:8080") {
			t.Errorf("want --metrics-bind-address=:8080 when metrics.enabled=true; got %v", args)
		}
		if !hasArg(args, "--metrics-secure=true") {
			t.Errorf("want --metrics-secure=true when metrics.enabled=true; got %v", args)
		}
		if hasArg(args, "--metrics-bind-address=0") {
			t.Errorf("must not disable the metrics server when metrics.enabled=true; got %v", args)
		}
		// Passed explicitly so the chart's value is the value the manager runs with.
		if !hasArg(args, "--metrics-require-auth=true") {
			t.Errorf("want --metrics-require-auth=true when metrics.enabled=true; got %v", args)
		}
	})

	t.Run("enabled honors metrics.requireAuth=false", func(t *testing.T) {
		// The opt-out reaches the manager as an explicit false (it defaults to true) and
		// keeps TLS.
		args := renderArgs(t, "metrics:\n  enabled: true\n  port: 8080\n  secure: true\n  requireAuth: false\n")
		if !hasArg(args, "--metrics-require-auth=false") {
			t.Errorf("want --metrics-require-auth=false to be passed explicitly; got %v", args)
		}
		if !hasArg(args, "--metrics-secure=true") {
			t.Errorf("metrics.requireAuth=false must not turn off TLS; want --metrics-secure=true, got %v", args)
		}
	})

	t.Run("enabled honors metrics.secure=false without disabling the server", func(t *testing.T) {
		args := renderArgs(t, "metrics:\n  enabled: true\n  port: 8080\n  secure: false\n")
		if !hasArg(args, "--metrics-bind-address=:8080") {
			t.Errorf("want --metrics-bind-address=:8080; got %v", args)
		}
		// Passed explicitly: the binary defaults it to true.
		if !hasArg(args, "--metrics-secure=false") {
			t.Errorf("want --metrics-secure=false to be passed explicitly; got %v", args)
		}
	})

	t.Run("disabled sets bind-address=0 and omits metrics-secure", func(t *testing.T) {
		args := renderArgs(t, "metrics:\n  enabled: false\n  port: 8080\n  secure: true\n")
		if !hasArg(args, "--metrics-bind-address=0") {
			t.Errorf("want --metrics-bind-address=0 (server disabled) when metrics.enabled=false; got %v", args)
		}
		for _, a := range args {
			if strings.HasPrefix(a, "--metrics-secure") {
				t.Errorf("must NOT pass --metrics-secure when the metrics server is off; got %q in %v", a, args)
			}
			if strings.HasPrefix(a, "--metrics-require-auth") {
				t.Errorf("must NOT pass --metrics-require-auth when the metrics server is off; got %q in %v", a, args)
			}
			if a == "--metrics-bind-address=:8080" {
				t.Errorf("must NOT pass a real bind address when metrics.enabled=false; got %v", args)
			}
		}
	})
}

// TestDeploymentDriftEvictionArgs proves the drift-eviction args track the driftEviction
// values: empty or 0 omits the flag, a set value renders exactly.
func TestDeploymentDriftEvictionArgs(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	renderArgs := func(t *testing.T, valuesBody string) []string {
		t.Helper()
		vf := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(vf, []byte(valuesBody), 0o600); err != nil {
			t.Fatalf("write values: %v", err)
		}
		args := []string{"template", "t", chartDir, "-f", policyTargetValues, "--show-only", "templates/deployment.yaml", "-f", vf}
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		return managerArgs(t, string(out))
	}

	hasPrefix := func(args []string, prefix string) bool {
		for _, a := range args {
			if strings.HasPrefix(a, prefix) {
				return true
			}
		}
		return false
	}

	t.Run("defaults omit both flags", func(t *testing.T) {
		args := renderArgs(t, "driftEviction:\n  interval: \"\"\n  burst: 0\n")
		if hasPrefix(args, "--drift-eviction-interval") {
			t.Errorf("must NOT render --drift-eviction-interval when interval is empty; got %v", args)
		}
		if hasPrefix(args, "--drift-eviction-burst") {
			t.Errorf("must NOT render --drift-eviction-burst when burst is 0; got %v", args)
		}
	})

	t.Run("configured values render the exact flags", func(t *testing.T) {
		args := renderArgs(t, "driftEviction:\n  interval: \"2s\"\n  burst: 5\n")
		if !hasArg(args, "--drift-eviction-interval=2s") {
			t.Errorf("want --drift-eviction-interval=2s; got %v", args)
		}
		if !hasArg(args, "--drift-eviction-burst=5") {
			t.Errorf("want --drift-eviction-burst=5; got %v", args)
		}
	})

	t.Run("interval only renders only the interval flag", func(t *testing.T) {
		args := renderArgs(t, "driftEviction:\n  interval: \"1m\"\n  burst: 0\n")
		if !hasArg(args, "--drift-eviction-interval=1m") {
			t.Errorf("want --drift-eviction-interval=1m; got %v", args)
		}
		if hasPrefix(args, "--drift-eviction-burst") {
			t.Errorf("must NOT render --drift-eviction-burst when burst is 0; got %v", args)
		}
	})
}

// managerArgs extracts the manager container's args from a rendered Deployment.
func managerArgs(t *testing.T, rendered string) []string {
	t.Helper()
	var doc *yaml.Node
	for _, d := range strings.Split(rendered, "\n---") {
		if !strings.Contains(d, "kind: Deployment") {
			continue
		}
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(d), &n); err != nil {
			t.Fatalf("parse rendered Deployment: %v", err)
		}
		if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
			doc = n.Content[0]
		} else {
			doc = &n
		}
		break
	}
	if doc == nil {
		t.Fatalf("no Deployment document in render:\n%s", rendered)
	}
	containers := childValue(doc, "spec", "template", "spec", "containers")
	if containers == nil || containers.Kind != yaml.SequenceNode || len(containers.Content) == 0 {
		t.Fatalf("Deployment has no containers")
	}
	argsNode := childValue(containers.Content[0], "args")
	if argsNode == nil || argsNode.Kind != yaml.SequenceNode {
		t.Fatalf("manager container has no args sequence")
	}
	var out []string
	for _, a := range argsNode.Content {
		out = append(out, a.Value)
	}
	return out
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
