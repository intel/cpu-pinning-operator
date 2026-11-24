// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// wantImageSources are the chart templates that must reference the operator image,
// asserted by name so a missing or extra source fails.
var wantImageSources = []string{
	"cpu-pinning-operator/templates/configmap.yaml",
	"cpu-pinning-operator/templates/deployment.yaml",
	"cpu-pinning-operator/templates/pre-delete-hook.yaml",
}

// TestOperatorImageReference proves the default image reference tracks appVersion and a
// digest wins over any tag, so replicas cannot run different binaries.
func TestOperatorImageReference(t *testing.T) {
	appVersion := chartAppVersion(t)
	if appVersion == "" {
		t.Fatalf("Chart.yaml has no appVersion — the tag default has nothing to follow")
	}

	t.Run("default tag follows the chart appVersion", func(t *testing.T) {
		refs := operatorImageRefs(t, renderWithImageValues(t, ""))
		assertAllRefs(t, refs, "docker.io/intel/cpu-pinning-operator:"+appVersion)
	})

	t.Run("explicit tag is used verbatim", func(t *testing.T) {
		refs := operatorImageRefs(t, renderWithImageValues(t,
			"image:\n  repository: registry.example.com/cpu-pinning-operator\n  tag: v1.2.3\n"))
		assertAllRefs(t, refs, "registry.example.com/cpu-pinning-operator:v1.2.3")
	})

	t.Run("digest joins with @ and needs no tag", func(t *testing.T) {
		refs := operatorImageRefs(t, renderWithImageValues(t,
			"image:\n  repository: registry.example.com/cpu-pinning-operator\n  digest: "+testDigest+"\n"))
		assertAllRefs(t, refs, "registry.example.com/cpu-pinning-operator@"+testDigest)
	})

	// When both are set, the chart emits repository@digest and omits the redundant tag.
	t.Run("digest wins over tag when both are set", func(t *testing.T) {
		rendered := renderWithImageValues(t,
			"image:\n  repository: registry.example.com/cpu-pinning-operator\n  tag: v1.2.3\n  digest: "+testDigest+"\n")
		refs := operatorImageRefs(t, rendered)
		assertAllRefs(t, refs, "registry.example.com/cpu-pinning-operator@"+testDigest)
		for source, got := range refs {
			if strings.Contains(got, "v1.2.3") {
				t.Errorf("%s reference %q still carries the tag; a digest reference must not concatenate both",
					source, got)
			}
		}
	})

	// A digest without its algorithm prefix cannot be pulled; reject it at render time.
	t.Run("digest without an algorithm prefix fails the render", func(t *testing.T) {
		_, stderr, err := renderImageValues(t, "image:\n  digest: 0123456789abcdef\n")
		if err == nil {
			t.Fatalf("render succeeded on a digest with no algorithm prefix; want a failure")
		}
		if !strings.Contains(stderr, "image.digest must carry its algorithm prefix") {
			t.Errorf("render error does not name the bad value: %s", stderr)
		}
	})
}

// testDigest is a syntactically valid digest; nothing pulls it.
const testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// assertAllRefs checks that every template in wantImageSources contributed a reference
// equal to want.
func assertAllRefs(t *testing.T, refs map[string]string, want string) {
	t.Helper()
	for _, source := range wantImageSources {
		got, ok := refs[source]
		if !ok {
			t.Errorf("%s renders no operator image reference; found references in %v",
				source, sortedKeys(refs))
			continue
		}
		if got != want {
			t.Errorf("%s image reference = %q; want %q", source, got, want)
		}
	}
}

// renderWithImageValues renders the whole chart with an optional image block, failing on
// any render error.
func renderWithImageValues(t *testing.T, valuesBody string) string {
	t.Helper()
	out, stderr, err := renderImageValues(t, valuesBody)
	if err != nil {
		requireSubcharts(t, stderr)
		t.Fatalf("helm template failed: %v\n%s", err, stderr)
	}
	return out
}

// renderImageValues renders the chart with a values overlay and returns stdout, stderr
// and the exit error.
func renderImageValues(t *testing.T, valuesBody string) (stdout, stderr string, err error) {
	t.Helper()
	helm := helmBin(t)
	args := []string{"template", "t", chartUnderTest(), "-f", policyTargetValues, "-n", "cpu-pinning-operator-system"}
	if valuesBody != "" {
		vf := filepath.Join(t.TempDir(), "values.yaml")
		if writeErr := os.WriteFile(vf, []byte(valuesBody), 0o600); writeErr != nil {
			t.Fatalf("write values: %v", writeErr)
		}
		args = append(args, "-f", vf)
	}
	cmd := exec.Command(helm, args...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// operatorImageRefs maps each of this chart's templates to the operator image it renders
// (container `image:` and INIT_CONTAINER_IMAGE), skipping subcharts. A template with
// several references must render them alike.
func operatorImageRefs(t *testing.T, rendered string) map[string]string {
	t.Helper()
	refs := map[string]string{}
	source := ""
	for _, line := range strings.Split(rendered, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "# Source: "); ok {
			source = rest
			continue
		}
		if !strings.HasPrefix(source, "cpu-pinning-operator/templates/") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || (key != "image" && key != "INIT_CONTAINER_IMAGE") {
			continue
		}
		ref := strings.Trim(strings.TrimSpace(value), `"`)
		if prev, seen := refs[source]; seen && prev != ref {
			t.Errorf("%s renders two different operator images (%q and %q); every container running the operator binary must ask for one reference",
				source, prev, ref)
		}
		refs[source] = ref
	}
	return refs
}

// chartAppVersion reads appVersion from Chart.yaml.
func chartAppVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(chartUnderTest(), "Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	var chart struct {
		AppVersion string `yaml:"appVersion"`
	}
	if err := yaml.Unmarshal(data, &chart); err != nil {
		t.Fatalf("parse Chart.yaml: %v", err)
	}
	return chart.AppVersion
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
