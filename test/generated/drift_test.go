// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package generated checks the Helm chart's RBAC and webhooks against deploy/generated,
// which `make generate` produces from the
// +kubebuilder:rbac / +kubebuilder:webhook markers in internal/controller and
// internal/webhook; the markers are the source of truth.
package generated_test

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoRoot resolves the repository root from this file, independent of CWD.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller for repo-root path")
	}

	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// readYAMLDocs reads a multi-document YAML file and returns the decoded docs.
func readYAMLDocs(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return decodeYAMLDocs(t, path, string(raw))
}

// readChartTemplateDocs decodes a chart template's static YAML, dropping every line with
// "{{", so no helm is needed.
func readChartTemplateDocs(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "{{") {
			continue
		}
		kept = append(kept, line)
	}
	return decodeYAMLDocs(t, path, strings.Join(kept, "\n"))
}

func decodeYAMLDocs(t *testing.T, path, content string) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(content))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("decoding %s: %v", path, err)
		}
		if len(doc) == 0 {
			continue
		}
		docs = append(docs, doc)
	}
	return docs
}

// ruleSet is a ClusterRole's flattened permissions, (apiGroup, resource) -> sorted verbs,
// so rule grouping doesn't matter. A rule with resourceNames gets a "[named]" suffix so a
// pinned grant can't widen unnoticed; the names themselves are templated and not compared.
type ruleSet map[[2]string][]string

func flattenRules(t *testing.T, docs []map[string]any, kind string) ruleSet {
	t.Helper()
	out := ruleSet{}
	found := false
	for _, doc := range docs {
		if s, _ := doc["kind"].(string); s != kind {
			continue
		}
		found = true
		rules, _ := doc["rules"].([]any)
		for _, r := range rules {
			rule, _ := r.(map[string]any)
			groups := stringSlice(rule["apiGroups"])
			resources := stringSlice(rule["resources"])
			verbs := stringSlice(rule["verbs"])
			_, named := rule["resourceNames"]
			for _, g := range groups {
				for _, res := range resources {
					if named {
						res += "[named]"
					}
					key := [2]string{g, res}
					out[key] = mergeSorted(out[key], verbs)
				}
			}
		}
	}
	if !found {
		t.Fatalf("no %s document found", kind)
	}
	return out
}

// webhookShape is the marker-expressible shape of one admission webhook.
type webhookShape struct {
	Path                    string
	SideEffects             string
	AdmissionReviewVersions []string
	Operations              []string
	APIGroups               []string
	APIVersions             []string
	Resources               []string
}

// webhookShapes extracts the shape of every webhook in docs, keyed by name.
func webhookShapes(t *testing.T, docs []map[string]any) map[string]webhookShape {
	t.Helper()
	out := map[string]webhookShape{}
	for _, doc := range docs {
		webhooks, _ := doc["webhooks"].([]any)
		for _, w := range webhooks {
			wh, _ := w.(map[string]any)
			name, _ := wh["name"].(string)
			cc, _ := wh["clientConfig"].(map[string]any)
			svc, _ := cc["service"].(map[string]any)
			path, _ := svc["path"].(string)
			side, _ := wh["sideEffects"].(string)
			shape := webhookShape{
				Path:                    path,
				SideEffects:             side,
				AdmissionReviewVersions: stringSlice(wh["admissionReviewVersions"]),
			}
			// Markers and chart both declare one rule block per webhook.
			rules, _ := wh["rules"].([]any)
			for _, r := range rules {
				rule, _ := r.(map[string]any)
				shape.Operations = mergeSorted(shape.Operations, stringSlice(rule["operations"]))
				shape.APIGroups = mergeSorted(shape.APIGroups, stringSlice(rule["apiGroups"]))
				shape.APIVersions = mergeSorted(shape.APIVersions, stringSlice(rule["apiVersions"]))
				shape.Resources = mergeSorted(shape.Resources, stringSlice(rule["resources"]))
			}
			out[name] = shape
		}
	}
	return out
}

func stringSlice(v any) []string {
	s, _ := v.([]any)
	out := make([]string, 0, len(s))
	for _, e := range s {
		if str, ok := e.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

// mergeSorted returns the sorted, deduplicated union of a and b.
func mergeSorted(a, b []string) []string {
	seen := map[string]struct{}{}
	for _, s := range append(append([]string{}, a...), b...) {
		seen[s] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// TestChartRBACMatchesGeneratedRole asserts the chart ClusterRole and Role grant exactly
// the permissions the code's
// +kubebuilder:rbac markers declare (via the committed deploy/generated/role.yaml
// baseline), neither more nor less.
func TestChartRBACMatchesGeneratedRole(t *testing.T) {
	for _, kind := range []string{"ClusterRole", "Role"} {
		t.Run(kind, func(t *testing.T) { compareRBAC(t, kind) })
	}
}

func compareRBAC(t *testing.T, kind string) {
	root := repoRoot(t)
	generated := flattenRules(t,
		readYAMLDocs(t, filepath.Join(root, "deploy", "generated", "role.yaml")),
		kind)
	chart := flattenRules(t,
		readChartTemplateDocs(t, filepath.Join(root, "deploy", "cpu-pinning-operator", "templates", "rbac.yaml")),
		kind)

	for key, chartVerbs := range chart {
		genVerbs, ok := generated[key]
		if !ok {
			t.Errorf("chart RBAC grants %q/%q (verbs %v) but no +kubebuilder:rbac marker declares it — remove the chart rule or add the marker",
				key[0], key[1], chartVerbs)
			continue
		}
		if strings.Join(chartVerbs, ",") != strings.Join(genVerbs, ",") {
			t.Errorf("verb-set drift for %q/%q: chart has %v, generated marker has %v",
				key[0], key[1], chartVerbs, genVerbs)
		}
	}
	// Declared by markers but not granted: the running operator would lack it.
	for key, genVerbs := range generated {
		if _, ok := chart[key]; !ok {
			t.Errorf("+kubebuilder:rbac marker declares %q/%q (verbs %v) but the chart RBAC does not grant it — add the rule to templates/rbac.yaml (run `make generate` and mirror deploy/generated/role.yaml)",
				key[0], key[1], genVerbs)
		}
	}
}

// TestChartWebhooksMatchGenerated asserts each chart webhook's marker-expressible shape
// matches deploy/generated/manifests.yaml; failurePolicy, timeoutSeconds and the
// runtime-managed namespaceSelector are not compared.
func TestChartWebhooksMatchGenerated(t *testing.T) {
	root := repoRoot(t)
	generated := webhookShapes(t,
		readYAMLDocs(t, filepath.Join(root, "deploy", "generated", "manifests.yaml")))

	chart := map[string]webhookShape{}
	for name, shape := range webhookShapes(t,
		readChartTemplateDocs(t, filepath.Join(root, "deploy", "cpu-pinning-operator", "templates", "mutatingwebhook.yaml"))) {
		chart[name] = shape
	}
	for name, shape := range webhookShapes(t,
		readChartTemplateDocs(t, filepath.Join(root, "deploy", "cpu-pinning-operator", "templates", "validatingwebhook.yaml"))) {
		chart[name] = shape
	}

	if len(generated) == 0 {
		t.Fatal("no webhooks found in the generated baseline")
	}
	for name, gen := range generated {
		got, ok := chart[name]
		if !ok {
			t.Errorf("+kubebuilder:webhook marker declares webhook %q but no chart template defines it", name)
			continue
		}
		if got.Path != gen.Path {
			t.Errorf("webhook %q path drift: chart %q, generated %q", name, got.Path, gen.Path)
		}
		if got.SideEffects != gen.SideEffects {
			t.Errorf("webhook %q sideEffects drift: chart %q, generated %q", name, got.SideEffects, gen.SideEffects)
		}
		assertSliceEq(t, name, "admissionReviewVersions", got.AdmissionReviewVersions, gen.AdmissionReviewVersions)
		assertSliceEq(t, name, "operations", got.Operations, gen.Operations)
		assertSliceEq(t, name, "apiGroups", got.APIGroups, gen.APIGroups)
		assertSliceEq(t, name, "apiVersions", got.APIVersions, gen.APIVersions)
		assertSliceEq(t, name, "resources", got.Resources, gen.Resources)
	}
}

func assertSliceEq(t *testing.T, webhook, field string, chart, generated []string) {
	t.Helper()
	if strings.Join(chart, ",") != strings.Join(generated, ",") {
		t.Errorf("webhook %q %s drift: chart %v, generated %v", webhook, field, chart, generated)
	}
}
