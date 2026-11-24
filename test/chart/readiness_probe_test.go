// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestReadinessProbePath proves readiness uses /readyz and liveness /healthz.
// /readyz gates on cache sync, so a cold follower cannot admit matched pods unmutated
// (failurePolicy Ignore + empty cache).
func TestReadinessProbePath(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	args := []string{"template", "t", chartDir, "-f", policyTargetValues, "--show-only", "templates/deployment.yaml"}
	out, err := exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		requireSubcharts(t, string(out))
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}

	container := managerContainer(t, string(out))

	if got := probePath(t, container, "readinessProbe"); got != "/readyz" {
		t.Errorf("readinessProbe.httpGet.path = %q; want /readyz "+
			"(readiness must gate on informer-cache-sync to avoid the HA follower warm-up race)", got)
	}
	if got := probePath(t, container, "livenessProbe"); got != "/healthz" {
		t.Errorf("livenessProbe.httpGet.path = %q; want /healthz "+
			"(the probes must not be transposed)", got)
	}
}

// managerContainer returns the container named "manager" from a rendered Deployment, by
// name so a sidecar cannot redirect the assertions.
func managerContainer(t *testing.T, rendered string) *yaml.Node {
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
	for _, c := range containers.Content {
		if name := findChild(c, "name"); name != nil && name.Value == "manager" {
			return c
		}
	}
	t.Fatalf("no manager container in rendered Deployment:\n%s", rendered)
	return nil
}

// probePath extracts <probe>.httpGet.path from a container node.
func probePath(t *testing.T, container *yaml.Node, probe string) string {
	t.Helper()
	p := childValue(container, probe, "httpGet", "path")
	if p == nil {
		t.Fatalf("manager container has no %s.httpGet.path", probe)
	}
	return p.Value
}
