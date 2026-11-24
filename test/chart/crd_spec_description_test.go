// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSpecDescriptionsPromiseNoStatusCondition proves no spec-field description in the
// shipped CRDs mentions a status condition; status fields are left alone.
func TestSpecDescriptionsPromiseNoStatusCondition(t *testing.T) {
	crds, err := filepath.Glob("../../deploy/cpu-pinning-operator/crds/*.yaml")
	if err != nil || len(crds) == 0 {
		t.Fatalf("no CRDs found under deploy/cpu-pinning-operator/crds: %v", err)
	}

	for _, path := range crds {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, version := range mapSlice(t, doc, "spec", "versions") {
			schema := dig(version, "schema", "openAPIV3Schema", "properties", "spec")
			if schema == nil {
				t.Fatalf("%s: no spec schema to walk", filepath.Base(path))
			}
			for _, offender := range conditionClaims(schema, "spec") {
				t.Errorf("%s: %s describes a status condition; state only what the controller produces (event, metric, admission outcome)",
					filepath.Base(path), offender)
			}
		}
	}
}

// conditionClaims returns the schema paths whose description mentions a condition.
func conditionClaims(node any, path string) []string {
	var found []string
	switch n := node.(type) {
	case map[string]any:
		if text, ok := n["description"].(string); ok && strings.Contains(strings.ToLower(text), "condition") {
			found = append(found, path)
		}
		for _, key := range []string{"properties", "items", "additionalProperties"} {
			child, ok := n[key]
			if !ok {
				continue
			}
			if key == "properties" {
				props, ok := child.(map[string]any)
				if !ok {
					continue
				}
				for name, prop := range props {
					found = append(found, conditionClaims(prop, path+"."+name)...)
				}
				continue
			}
			found = append(found, conditionClaims(child, path+"[]")...)
		}
	}
	return found
}

// dig walks nested map keys, returning nil when any step is missing.
func dig(node any, keys ...string) any {
	for _, key := range keys {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node, ok = m[key]
		if !ok {
			return nil
		}
	}
	return node
}

// mapSlice reads a list of maps out of nested keys.
func mapSlice(t *testing.T, node any, keys ...string) []any {
	t.Helper()
	list, ok := dig(node, keys...).([]any)
	if !ok {
		t.Fatalf("expected a list at %s", fmt.Sprint(keys))
	}
	return list
}
