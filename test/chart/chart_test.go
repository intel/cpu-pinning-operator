// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package chart checks the Helm chart's rendered manifests against the operator's Go API
// types, so a misnamed field cannot be silently pruned at admission.
package chart_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
)

// policyTargetValues gives the chart's policy the match and namespaces it refuses to
// render without; every `helm template` here passes it right after the chart path.
const policyTargetValues = "../../docs/examples/values/kserve.yaml"

// missingPrereq skips outside CI, where helm may be absent, and fails in CI.
func missingPrereq(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("CI") != "" {
		t.Fatalf("%s — CI must provide the chart render, see .github/workflows/val-unit-tests.yml", msg)
	}
	t.Skip(msg)
}

func helmBin(t *testing.T) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		missingPrereq(t, "helm binary not on PATH")
	}
	return helm
}

// requireSubcharts fails on helm's "missing in charts/" error everywhere: the archives
// are committed, so it is a tree defect. Other render errors are left to the caller.
func requireSubcharts(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "missing in charts/") || strings.Contains(out, "helm dependency build") {
		t.Fatalf("chart dependencies unresolved — deploy/cpu-pinning-operator/charts/ must hold an archive for every dependency in Chart.yaml: %s",
			strings.TrimSpace(out))
	}
}

// jsonTagsOf maps a struct's exported fields' JSON tags to their types, unwrapping
// pointers. It does not flatten inline fields or descend into map/slice element types.
func jsonTagsOf(t reflect.Type) map[string]reflect.Type {
	if t == nil {
		return nil
	}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			continue
		}
		out[name] = f.Type
	}
	return out
}

// walkYAML returns every field path in a YAML tree with no matching json tag on the Go
// type at that path. Sequences recurse into struct elements; scalars are not checked.
func walkYAML(yamlNode *yaml.Node, goType reflect.Type, path string) []string {
	if yamlNode == nil || goType == nil {
		return nil
	}
	if yamlNode.Kind == yaml.DocumentNode && len(yamlNode.Content) > 0 {
		return walkYAML(yamlNode.Content[0], goType, path)
	}
	for goType.Kind() == reflect.Ptr {
		goType = goType.Elem()
	}

	if yamlNode.Kind == yaml.SequenceNode {
		if goType.Kind() != reflect.Slice && goType.Kind() != reflect.Array {
			return nil
		}
		elem := goType.Elem()
		for elem.Kind() == reflect.Ptr {
			elem = elem.Elem()
		}
		if elem.Kind() != reflect.Struct {
			return nil // e.g. []string — no per-element schema to check.
		}
		var drift []string
		for i, item := range yamlNode.Content {
			drift = append(drift, walkYAML(item, elem, fmt.Sprintf("%s[%d]", path, i))...)
		}
		return drift
	}

	if yamlNode.Kind != yaml.MappingNode {
		return nil // scalar leaf — nothing to check.
	}
	tags := jsonTagsOf(goType)
	if tags == nil {
		return nil
	}
	var drift []string
	for i := 0; i+1 < len(yamlNode.Content); i += 2 {
		key := yamlNode.Content[i].Value
		val := yamlNode.Content[i+1]
		fullPath := path + "." + key
		fieldType, ok := tags[key]
		if !ok {
			drift = append(drift, fullPath)
			continue
		}
		drift = append(drift, walkYAML(val, fieldType, fullPath)...)
	}
	return drift
}

// TestChartValuesMatchBalloonPinningPolicySpec proves every field in values.yaml's
// balloonPinningPolicy block is a real BalloonPinningPolicySpec field.
func TestChartValuesMatchBalloonPinningPolicySpec(t *testing.T) {
	// Relative to test/chart, two directories below the module root.
	valuesPath := filepath.Join("..", "..", "deploy", "cpu-pinning-operator", "values.yaml")
	data, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("read %s: %v", valuesPath, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse %s: %v", valuesPath, err)
	}
	bpp := findChild(&root, "balloonPinningPolicy")
	if bpp == nil {
		t.Fatalf("%s missing top-level `balloonPinningPolicy` block", valuesPath)
	}

	// Chart-only keys that are not part of the CRD spec.
	chartOnly := map[string]bool{"create": true, "name": true}
	spec := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i+1 < len(bpp.Content); i += 2 {
		key := bpp.Content[i].Value
		if chartOnly[key] {
			continue
		}
		spec.Content = append(spec.Content, bpp.Content[i], bpp.Content[i+1])
	}

	drift := walkYAML(spec, reflect.TypeOf(cpopv1alpha1.BalloonPinningPolicySpec{}), "spec")
	if len(drift) > 0 {
		t.Errorf("chart values.yaml has fields NOT in BalloonPinningPolicySpec — the API server will silently strip these at admission:\n  %s",
			strings.Join(drift, "\n  "))
		t.Log("this class of bug means a user setting the value in a values override gets NO effect at runtime.")
	}
}

// TestExamplePoliciesMatchSpec proves the example policies under docs/examples/ use only
// real spec fields.
func TestExamplePoliciesMatchSpec(t *testing.T) {
	examples := filepath.Join("..", "..", "docs", "examples")
	entries, err := os.ReadDir(examples)
	if err != nil {
		missingPrereq(t, "docs/examples/ not accessible: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		checked++
		path := filepath.Join(examples, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var root yaml.Node
			if err := yaml.Unmarshal(data, &root); err != nil {
				t.Fatalf("parse: %v", err)
			}
			spec := findChild(&root, "spec")
			if spec == nil {
				t.Fatalf("%s missing .spec", path)
			}
			drift := walkYAML(spec, reflect.TypeOf(cpopv1alpha1.BalloonPinningPolicySpec{}), "spec")
			if len(drift) > 0 {
				t.Errorf("example policy %s has fields NOT in BalloonPinningPolicySpec:\n  %s",
					path, strings.Join(drift, "\n  "))
			}
		})
	}
	if checked == 0 {
		t.Fatalf("no example policies in %s — the guard would pass vacuously", examples)
	}
}

// findChild returns the value node for key at the top level of a mapping (through a
// document node), or nil.
func findChild(n *yaml.Node, key string) *yaml.Node {
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return findChild(n.Content[0], key)
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
