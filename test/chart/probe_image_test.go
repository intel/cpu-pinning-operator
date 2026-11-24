// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestProbeImageFollowsOperatorImage proves PROBE_IMAGE equals the rendered manager image
// under any repository or tag override, so air-gapped installs need only one mirrored
// image.
func TestProbeImageFollowsOperatorImage(t *testing.T) {
	helm := helmBin(t)
	const chartDir = "../../deploy/cpu-pinning-operator"
	const releaseNS = "cpu-pinning-operator-system"

	render := func(t *testing.T, template string, extra ...string) string {
		t.Helper()
		args := []string{"template", "t", chartDir, "-f", policyTargetValues, "-n", releaseNS, "--show-only", template}
		args = append(args, extra...)
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		return string(out)
	}

	// value reads an unquoted "key: value" line out of a render.
	value := func(t *testing.T, out, key string) string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, key+":") {
				return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, key+":")), `"`)
			}
		}
		t.Fatalf("%s not found in render:\n%s", key, out)
		return ""
	}

	for name, override := range map[string][]string{
		"chart defaults":      nil,
		"repository override": {"--set", "image.repository=registry.example.com/team/cpu-pinning-operator"},
		"tag override":        {"--set", "image.tag=1.2.3"},
		"repository plus tag": {"--set", "image.repository=registry.example.com/team/cpu-pinning-operator", "--set", "image.tag=1.2.3"},
		"digest":              {"--set", "image.digest=sha256:" + strings.Repeat("a", 64)},
		"digest wins over tag": {
			"--set", "image.tag=1.2.3",
			"--set", "image.digest=sha256:" + strings.Repeat("b", 64),
		},
	} {
		t.Run(name, func(t *testing.T) {
			managerImage := value(t, render(t, "templates/deployment.yaml", override...), "image")
			probeImage := value(t, render(t, "templates/configmap.yaml", override...), "PROBE_IMAGE")

			if probeImage != managerImage {
				t.Errorf("PROBE_IMAGE = %q, want the manager image %q", probeImage, managerImage)
			}
			// The probe is the operator binary; a digest must reach it whole.
			if !strings.Contains(probeImage, "cpu-pinning-operator") {
				t.Errorf("PROBE_IMAGE = %q, which does not name the operator image", probeImage)
			}
		})
	}

	t.Run("explicit probe image override wins", func(t *testing.T) {
		const mirror = "registry.internal.example/mirror/cpu-pinning-operator@sha256:abc123"
		got := value(t, render(t, "templates/configmap.yaml", "--set", "probe.image="+mirror), "PROBE_IMAGE")
		if got != mirror {
			t.Errorf("PROBE_IMAGE = %q, want the explicit override %q", got, mirror)
		}
	})
}

// TestProbeImagePullSecretsWired proves the probe pod gets the manager's image pull
// secrets, or it sits in ImagePullBackOff.
func TestProbeImagePullSecretsWired(t *testing.T) {
	helm := helmBin(t)
	const chartDir = "../../deploy/cpu-pinning-operator"

	render := func(t *testing.T, extra ...string) string {
		t.Helper()
		args := []string{
			"template", "t", chartDir, "-f", policyTargetValues,
			"-n", "cpu-pinning-operator-system",
			"--show-only", "templates/configmap.yaml",
		}
		args = append(args, extra...)
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		return string(out)
	}

	t.Run("no secrets configured renders no key", func(t *testing.T) {
		if out := render(t); strings.Contains(out, "PROBE_IMAGE_PULL_SECRETS") {
			t.Errorf("PROBE_IMAGE_PULL_SECRETS rendered with no imagePullSecrets set:\n%s", out)
		}
	})

	t.Run("configured secrets reach the manager", func(t *testing.T) {
		out := render(t, "--set", "imagePullSecrets[0].name=registry-creds")
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "PROBE_IMAGE_PULL_SECRETS:") {
				continue
			}
			raw := strings.TrimSpace(strings.TrimPrefix(line, "PROBE_IMAGE_PULL_SECRETS:"))
			unquoted, err := strconv.Unquote(raw)
			if err != nil {
				t.Fatalf("PROBE_IMAGE_PULL_SECRETS is not a quoted scalar (%s): %v", raw, err)
			}

			var refs []corev1.LocalObjectReference
			if err := json.Unmarshal([]byte(unquoted), &refs); err != nil {
				t.Fatalf("PROBE_IMAGE_PULL_SECRETS does not decode as []LocalObjectReference (%s): %v", unquoted, err)
			}
			if len(refs) != 1 || refs[0].Name != "registry-creds" {
				t.Errorf("PROBE_IMAGE_PULL_SECRETS = %+v, want the one configured secret", refs)
			}
			return
		}
		t.Fatalf("PROBE_IMAGE_PULL_SECRETS not rendered with imagePullSecrets set:\n%s", out)
	})
}

// TestProbeResourcesWired proves the probe's requests and limits reach the manager as a
// decodable ResourceRequirements; without requests a LimitRange or quota rejects the
// probe pod.
func TestProbeResourcesWired(t *testing.T) {
	helm := helmBin(t)
	const chartDir = "../../deploy/cpu-pinning-operator"

	probeResources := func(t *testing.T, extra ...string) corev1.ResourceRequirements {
		t.Helper()
		args := []string{
			"template", "t", chartDir, "-f", policyTargetValues,
			"-n", "cpu-pinning-operator-system",
			"--show-only", "templates/configmap.yaml",
		}
		args = append(args, extra...)
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "PROBE_RESOURCES:") {
				continue
			}
			raw := strings.TrimSpace(strings.TrimPrefix(line, "PROBE_RESOURCES:"))

			unquoted, err := strconv.Unquote(raw)
			if err != nil {
				t.Fatalf("PROBE_RESOURCES is not a quoted scalar (%s): %v", raw, err)
			}
			// The manager decodes it the same way; an undecodable value falls back to
			// built-in defaults.
			var res corev1.ResourceRequirements
			if err := json.Unmarshal([]byte(unquoted), &res); err != nil {
				t.Fatalf("PROBE_RESOURCES does not decode as ResourceRequirements (%s): %v", unquoted, err)
			}
			return res
		}
		t.Fatalf("PROBE_RESOURCES not rendered:\n%s", out)
		return corev1.ResourceRequirements{}
	}

	t.Run("defaults carry a cpu and memory request and limit", func(t *testing.T) {
		res := probeResources(t)
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			if q, ok := res.Requests[name]; !ok || q.IsZero() {
				t.Errorf("no %s request in the rendered probe resources (%+v)", name, res)
			}
			if q, ok := res.Limits[name]; !ok || q.IsZero() {
				t.Errorf("no %s limit in the rendered probe resources (%+v)", name, res)
			}
		}
	})

	t.Run("override reaches the manager", func(t *testing.T) {
		res := probeResources(t, "--set", "probe.resources.requests.cpu=250m")
		if got := res.Requests[corev1.ResourceCPU]; got.String() != "250m" {
			t.Errorf("cpu request = %s, want the overridden 250m", got.String())
		}
	})
}
