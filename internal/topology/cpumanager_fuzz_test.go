// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

// FuzzParseCPUManagerPolicy proves, for any document: no panic; a non-empty policy
// is the top-level cpuManagerPolicy string, or "none" defaulted only for a
// KubeletConfiguration; only exact "static"/"none" become verdicts.
func FuzzParseCPUManagerPolicy(f *testing.F) {
	f.Add(staticKubeletConfig)
	f.Add("cpuManagerPolicy: none\n")
	f.Add(`{"cpuManagerPolicy":"static","kubeReserved":{"cpu":"100m"}}`)
	// Nested one level down: must not be read as the cluster's policy.
	f.Add("kubeletconfig:\n  cpuManagerPolicy: static\n")
	// Anchor and alias: the policy is only reachable through the alias.
	f.Add("a: &p static\ncpuManagerPolicy: *p\n")
	f.Add("a: &p\n  cpuManagerPolicy: static\nb: *p\n")
	// Scalars that are not strings: a kubelet would reject these too.
	f.Add("cpuManagerPolicy: true\n")
	f.Add("cpuManagerPolicy: 5\n")
	f.Add("cpuManagerPolicy: null\n")
	f.Add("cpuManagerPolicy:\n")
	f.Add("cpuManagerPolicy: \"\"\n")
	// Repeated key: whatever wins, it must be what the document says wins.
	f.Add("cpuManagerPolicy: none\ncpuManagerPolicy: static\n")
	// A top-level sequence, a bare scalar and two documents: none is a
	// KubeletConfiguration.
	f.Add("- cpuManagerPolicy: static\n")
	f.Add("static\n")
	f.Add("cpuManagerPolicy: static\n---\ncpuManagerPolicy: none\n")
	f.Add("\tnot: [a, document")
	f.Add("")
	// A policy nobody recognises, and a mis-cased one: parsed, but not a verdict.
	f.Add("cpuManagerPolicy: future-policy\n")
	f.Add("cpuManagerPolicy: Static\n")
	// The optional field omitted: the API default only for the declared kind, and
	// only when the kind matches exactly.
	f.Add(policylessKubeletConfig)
	f.Add("kind: KubeletConfiguration\n")
	f.Add("kind: kubeletconfiguration\n")
	f.Add("kind: KubeletConfiguration\ncpuManagerPolicy: \"\"\n")
	f.Add("kind: KubeletConfiguration\ncpuManagerPolicy:\n  none: true\n")
	f.Add("kind:\n  KubeletConfiguration: {}\n")

	f.Fuzz(func(t *testing.T, doc string) {
		policy := ParseCPUManagerPolicy([]byte(doc))

		// Only an exact "static" or "none" becomes a verdict.
		switch verdict := ClassifyCPUManagerPolicy(policy); verdict {
		case CPUManagerConflict:
			if policy != CPUManagerPolicyStatic {
				t.Fatalf("policy %q classified as %q, want a verdict only for %q", policy, verdict, CPUManagerPolicyStatic)
			}
		case CPUManagerCompatible:
			if policy != CPUManagerPolicyNone {
				t.Fatalf("policy %q classified as %q, want a verdict only for %q", policy, verdict, CPUManagerPolicyNone)
			}
		case CPUManagerUnknown:
		default:
			t.Fatalf("policy %q classified as %q, want one of the three verdicts", policy, verdict)
		}

		if policy == "" {
			return
		}
		// The policy must be the top-level key's own string value.
		var top map[string]any
		if err := yaml.Unmarshal([]byte(doc), &top); err != nil {
			t.Fatalf("ParseCPUManagerPolicy = %q from a document that is not a mapping (%v)", policy, err)
		}
		raw, ok := top["cpuManagerPolicy"]
		if !ok {
			// Only the exact kind gets the default.
			kind, _ := top["kind"].(string)
			if policy == CPUManagerPolicyNone && kind == kubeletConfigurationKind {
				return
			}
			t.Fatalf("ParseCPUManagerPolicy = %q from a document with no top-level cpuManagerPolicy (keys %v)", policy, keysOf(top))
		}
		// Not coerced, nested, or matched case-insensitively.
		if s, isString := raw.(string); !isString || s != policy {
			t.Fatalf("ParseCPUManagerPolicy = %q, but the document's top-level cpuManagerPolicy is %#v", policy, raw)
		}
	})
}

// keysOf lists a document's top-level keys for a failure message.
func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
