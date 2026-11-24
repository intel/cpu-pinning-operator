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

// TestManagerSecurityContextRendering proves the manager's hardened securityContext
// (restricted PSA, OpenShift restricted-v2) and the writable /tmp emptyDir it needs with
// a read-only root filesystem.
func TestManagerSecurityContextRendering(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	render := func(t *testing.T, valuesFile string) string {
		t.Helper()
		args := []string{
			"template", "t", chartDir, "-f", policyTargetValues,
			"--show-only", "templates/deployment.yaml",
		}
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

	// managerContainer returns the `manager` container node from a rendered Deployment.
	managerContainer := func(t *testing.T, rendered string) *yaml.Node {
		t.Helper()
		dep := parseDeployment(t, rendered)
		containers := childValue(dep, "spec", "template", "spec", "containers")
		if containers == nil || containers.Kind != yaml.SequenceNode {
			t.Fatalf("no containers list in rendered Deployment:\n%s", rendered)
		}
		for _, c := range containers.Content {
			if name := findChild(c, "name"); name != nil && name.Value == "manager" {
				return c
			}
		}
		t.Fatalf("no manager container in rendered Deployment:\n%s", rendered)
		return nil
	}

	t.Run("hardened defaults", func(t *testing.T) {
		out := render(t, "")
		mgr := managerContainer(t, out)
		sc := findChild(mgr, "securityContext")
		if sc == nil {
			t.Fatalf("manager container has no securityContext:\n%s", out)
		}

		if v := findChild(sc, "readOnlyRootFilesystem"); v == nil || v.Value != "true" {
			t.Errorf("expected readOnlyRootFilesystem: true, got %v:\n%s", v, out)
		}

		if v := findChild(sc, "allowPrivilegeEscalation"); v == nil || v.Value != "false" {
			t.Errorf("expected allowPrivilegeEscalation: false, got %v:\n%s", v, out)
		}

		if v := findChild(sc, "runAsNonRoot"); v == nil || v.Value != "true" {
			t.Errorf("expected runAsNonRoot: true, got %v:\n%s", v, out)
		}
		// Pinned to the image's gid, above the 10000 scanner floor. OpenShift admits a
		// literal gid, unlike runAsUser, which must stay unset.
		if v := findChild(sc, "runAsGroup"); v == nil || v.Value != "65532" {
			t.Errorf("expected runAsGroup: 65532, got %v:\n%s", v, out)
		}
		if v := findChild(sc, "runAsUser"); v != nil {
			t.Errorf("runAsUser must stay unset so the platform assigns the uid, got %q:\n%s", v.Value, out)
		}

		drop := childValue(sc, "capabilities", "drop")
		if drop == nil || drop.Kind != yaml.SequenceNode || len(drop.Content) != 1 || drop.Content[0].Value != "ALL" {
			t.Errorf("expected capabilities.drop: [ALL], got %v:\n%s", drop, out)
		}

		if v := childValue(sc, "seccompProfile", "type"); v == nil || v.Value != "RuntimeDefault" {
			t.Errorf("expected seccompProfile.type: RuntimeDefault, got %v:\n%s", v, out)
		}
	})

	t.Run("writable tmp so read-only rootfs still starts", func(t *testing.T) {
		out := render(t, "")
		dep := parseDeployment(t, out)

		// Go's os.TempDir() and the default webhook cert dir are under /tmp.
		mgr := managerContainer(t, out)
		mounts := findChild(mgr, "volumeMounts")
		if mounts == nil || mounts.Kind != yaml.SequenceNode {
			t.Fatalf("manager has no volumeMounts sequence (read-only rootfs needs a /tmp mount):\n%s", out)
		}
		var tmpMount *yaml.Node
		for _, m := range mounts.Content {
			if mp := findChild(m, "mountPath"); mp != nil && mp.Value == "/tmp" {
				tmpMount = m
			}
		}
		if tmpMount == nil {
			t.Fatalf("expected a /tmp volumeMount on the manager for read-only rootfs:\n%s", out)
		}
		tmpVol := findChild(tmpMount, "name")
		if tmpVol == nil {
			t.Fatalf("/tmp volumeMount has no name:\n%s", out)
		}

		volumes := childValue(dep, "spec", "template", "spec", "volumes")
		if volumes == nil || volumes.Kind != yaml.SequenceNode {
			t.Fatalf("no volumes list in rendered Deployment:\n%s", out)
		}
		var backed bool
		for _, v := range volumes.Content {
			if n := findChild(v, "name"); n != nil && n.Value == tmpVol.Value {
				if findChild(v, "emptyDir") == nil {
					t.Errorf("/tmp volume %q is not an emptyDir:\n%s", tmpVol.Value, out)
				}
				backed = true
			}
		}
		if !backed {
			t.Errorf("/tmp volumeMount %q has no backing volume:\n%s", tmpVol.Value, out)
		}
	})

	t.Run("overridable from values", func(t *testing.T) {
		vf := writeValues(t, "securityContext:\n  readOnlyRootFilesystem: false\n")
		out := render(t, vf)
		mgr := managerContainer(t, out)
		sc := findChild(mgr, "securityContext")
		if sc == nil {
			t.Fatalf("manager container has no securityContext:\n%s", out)
		}
		if v := findChild(sc, "readOnlyRootFilesystem"); v == nil || v.Value != "false" {
			t.Errorf("securityContext not overridable from values; readOnlyRootFilesystem = %v:\n%s", v, out)
		}
	})
}

// parseDeployment extracts the single Deployment document from a helm render.
func parseDeployment(t *testing.T, rendered string) *yaml.Node {
	t.Helper()
	for _, doc := range strings.Split(rendered, "\n---") {
		if !strings.Contains(doc, "kind: Deployment") {
			continue
		}
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
			t.Fatalf("parse rendered Deployment: %v", err)
		}
		if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
			return n.Content[0]
		}
		return &n
	}
	t.Fatalf("no Deployment document in render:\n%s", rendered)
	return nil
}
