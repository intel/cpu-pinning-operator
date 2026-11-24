// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestPreDeleteHookRendering proves the uninstall hooks render with the right phase,
// annotations, delete-policy and narrow RBAC.
// Pre-delete removes the guard's ValidatingWebhookConfiguration, which must happen before
// Helm deletes the default policy or uninstall exits non-zero. Post-delete strips node
// labels, which must happen after the manager is gone or the reconciler re-labels the
// nodes.
func TestPreDeleteHookRendering(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	render := func(t *testing.T, valuesBody string) string {
		t.Helper()
		args := []string{"template", "t", chartDir, "-f", policyTargetValues, "--show-only", "templates/pre-delete-hook.yaml"}
		if valuesBody != "" {
			vf := filepath.Join(t.TempDir(), "values.yaml")
			if err := os.WriteFile(vf, []byte(valuesBody), 0o600); err != nil {
				t.Fatalf("write values: %v", err)
			}
			args = append(args, "-f", vf)
		}
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			// --show-only on a template that renders nothing errors with "could not find
			// template".
			if strings.Contains(string(out), "could not find template") {
				return ""
			}
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		return string(out)
	}

	t.Run("renders both hook Jobs and their narrow RBAC by default", func(t *testing.T) {
		out := render(t, "")
		if out == "" {
			t.Fatalf("uninstall hooks did not render with chart defaults")
		}
		// Both hook phases must render a full SA + ClusterRole + ClusterRoleBinding
		// + Job. Count the Jobs so we catch a hook silently dropping.
		if got := strings.Count(out, "kind: Job"); got != 2 {
			t.Errorf("expected 2 hook Jobs (pre-delete + post-delete), got %d:\n%s", got, out)
		}
		for _, want := range []string{
			"kind: ServiceAccount",
			"kind: ClusterRole\n",
			"kind: ClusterRoleBinding",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("rendered hooks missing %q:\n%s", want, out)
			}
		}
		// Every hook resource must clean itself up so it does not leak across releases.
		if !strings.Contains(out, `"helm.sh/hook": pre-delete`) {
			t.Errorf("no pre-delete hook annotation rendered:\n%s", out)
		}
		if !strings.Contains(out, `"helm.sh/hook": post-delete`) {
			t.Errorf("no post-delete hook annotation rendered:\n%s", out)
		}
		if !strings.Contains(out, "hook-delete-policy") {
			t.Errorf("hooks have no hook-delete-policy (would leak across releases):\n%s", out)
		}

		if !strings.Contains(out, "- pre-delete") {
			t.Errorf("no hook Job runs the pre-delete subcommand:\n%s", out)
		}
		if !strings.Contains(out, "- post-delete") {
			t.Errorf("no hook Job runs the post-delete subcommand:\n%s", out)
		}

		if !strings.Contains(out, "VALIDATING_WEBHOOK_CONFIG_NAME") {
			t.Errorf("pre-delete Job does not pass VALIDATING_WEBHOOK_CONFIG_NAME:\n%s", out)
		}
	})

	t.Run("webhook-config teardown stays pre-delete, label strip is post-delete", func(t *testing.T) {
		out := render(t, "")
		if out == "" {
			t.Fatalf("uninstall hooks did not render with chart defaults")
		}
		pre, post := splitHookPhases(t, out)

		// The hook deletes by name and never reads it, so there is no get grant.
		if !strings.Contains(pre, "- pre-delete") {
			t.Errorf("pre-delete phase does not run the pre-delete subcommand:\n%s", pre)
		}
		// Assert on the decoded rule: the name also appears in the Job env, and verb
		// lists are unordered.
		rule := findRule(t, pre, "validatingwebhookconfigurations")
		if !sameSet(rule.Verbs, []string{"delete"}) {
			t.Errorf("pre-delete validatingwebhookconfigurations verbs = %v, want exactly [delete]", rule.Verbs)
		}
		// The resourceNames pin and the name handed to the Job must be the same string,
		// or the delete is Forbidden at uninstall.
		configName := jobEnvValue(t, pre, "VALIDATING_WEBHOOK_CONFIG_NAME")
		if !sameSet(rule.ResourceNames, []string{configName}) {
			t.Errorf("pre-delete resourceNames = %v but the Job deletes %q — the delete would be Forbidden",
				rule.ResourceNames, configName)
		}
		// Release-derived, so two releases do not contend for one name.
		if want := "t-cpu-pinning-operator-validating-webhook"; configName != want {
			t.Errorf("guard webhook config name = %q, want the release-derived %q", configName, want)
		}
		// The label strip in pre-delete races the live reconciler.
		if strings.Contains(pre, "- post-delete") || strings.Contains(pre, `resources: ["nodes"]`) {
			t.Errorf("label strip must not run in the pre-delete phase (races the live reconciler):\n%s", pre)
		}

		if !strings.Contains(post, "- post-delete") {
			t.Errorf("post-delete phase does not run the post-delete subcommand:\n%s", post)
		}
		if nodes := findRule(t, post, "nodes"); !sameSet(nodes.Verbs, []string{"list", "patch"}) {
			t.Errorf("post-delete nodes verbs = %v, want exactly [list patch]", nodes.Verbs)
		}
		// Post-delete is too late for the webhook: the policy DELETE already happened.
		if strings.Contains(post, "- pre-delete") || strings.Contains(post, `resources: ["validatingwebhookconfigurations"]`) {
			t.Errorf("webhook-config teardown must not run in the post-delete phase:\n%s", post)
		}
	})

	t.Run("RBAC is scoped to exactly the grants each hook needs", func(t *testing.T) {
		out := render(t, "")
		if out == "" {
			t.Fatalf("uninstall hooks did not render with chart defaults")
		}
		// GC handles the CRs; the hooks only touch labels and the webhook.
		if strings.Contains(out, "balloonspolicies") {
			t.Errorf("hook ClusterRole unexpectedly references balloonspolicies (GC handles those):\n%s", out)
		}
		// Every delete verb must be paired with a resourceNames pin.
		for _, rule := range parseRBACRules(t, out) {
			if slices.Contains(rule.Verbs, "delete") && len(rule.ResourceNames) == 0 {
				t.Errorf("hook ClusterRole grants delete on %v without a resourceNames pin", rule.Resources)
			}
		}
	})

	t.Run("absent when disabled", func(t *testing.T) {
		out := render(t, "preDeleteHook:\n  enabled: false\n")
		if strings.Contains(out, "kind: Job") {
			t.Errorf("uninstall hooks rendered while preDeleteHook.enabled=false:\n%s", out)
		}
	})

	t.Run("absent when rbac.create is false", func(t *testing.T) {
		// Without rbac.create the hooks cannot be bound, so both are gated off.
		out := render(t, "rbac:\n  create: false\n")
		if strings.Contains(out, "kind: Job") {
			t.Errorf("uninstall hooks rendered while rbac.create=false:\n%s", out)
		}
	})
}

// rbacRule is a decoded ClusterRole rule; substring matching cannot tell which rule a
// verb belongs to.
type rbacRule struct {
	APIGroups     []string `yaml:"apiGroups"`
	Resources     []string `yaml:"resources"`
	ResourceNames []string `yaml:"resourceNames"`
	Verbs         []string `yaml:"verbs"`
}

// parseRBACRules decodes every ClusterRole rule in a rendered YAML stream.
func parseRBACRules(t *testing.T, stream string) []rbacRule {
	t.Helper()
	var rules []rbacRule
	dec := yaml.NewDecoder(strings.NewReader(stream))
	for {
		var doc struct {
			Kind  string     `yaml:"kind"`
			Rules []rbacRule `yaml:"rules"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return rules
		}
		if err != nil {
			t.Fatalf("decoding rendered YAML: %v\n%s", err, stream)
		}
		if doc.Kind == "ClusterRole" {
			rules = append(rules, doc.Rules...)
		}
	}
}

// findRule returns the one ClusterRole rule granting resource, failing when it is missing
// or duplicated.
func findRule(t *testing.T, stream, resource string) rbacRule {
	t.Helper()
	var found []rbacRule
	for _, rule := range parseRBACRules(t, stream) {
		if slices.Contains(rule.Resources, resource) {
			found = append(found, rule)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one ClusterRole rule granting %q, found %d:\n%s", resource, len(found), stream)
	}
	return found[0]
}

// jobEnvValue returns an env var's value on the Job container, failing when missing or
// set twice with different values.
func jobEnvValue(t *testing.T, stream, name string) string {
	t.Helper()
	var values []string
	dec := yaml.NewDecoder(strings.NewReader(stream))
	for {
		var doc struct {
			Kind string `yaml:"kind"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Env []struct {
								Name  string `yaml:"name"`
								Value string `yaml:"value"`
							} `yaml:"env"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding rendered YAML: %v\n%s", err, stream)
		}
		if doc.Kind != "Job" {
			continue
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			for _, e := range c.Env {
				if e.Name == name {
					values = append(values, e.Value)
				}
			}
		}
	}
	if len(values) == 0 {
		t.Fatalf("no Job container sets env %q:\n%s", name, stream)
	}
	for _, v := range values[1:] {
		if v != values[0] {
			t.Fatalf("env %q set to conflicting values %v", name, values)
		}
	}
	return values[0]
}

// sameSet reports whether two string slices hold the same elements, ignoring order.
func sameSet(got, want []string) bool {
	g, w := slices.Clone(got), slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	return slices.Equal(g, w)
}

// splitHookPhases buckets the rendered documents into pre-delete and post-delete by their
// `helm.sh/hook` annotation, failing on a document naming neither or both. Buckets keep
// their `---` separators.
func splitHookPhases(t *testing.T, out string) (pre, post string) {
	t.Helper()
	// appendDoc normalizes one rendered document to "---\n<body>\n" and appends it.
	appendDoc := func(bucket, doc string) string {
		body := strings.Trim(strings.TrimPrefix(strings.Trim(doc, "\n"), "---"), "\n")
		return bucket + "---\n" + body + "\n"
	}
	for _, doc := range strings.Split(out, "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		hasPre := strings.Contains(doc, `"helm.sh/hook": pre-delete`)
		hasPost := strings.Contains(doc, `"helm.sh/hook": post-delete`)
		switch {
		case hasPre && !hasPost:
			pre = appendDoc(pre, doc)
		case hasPost && !hasPre:
			post = appendDoc(post, doc)
		default:
			t.Fatalf("rendered document is not annotated for exactly one hook phase:\n%s", doc)
		}
	}
	if pre == "" {
		t.Fatalf("no pre-delete hook documents rendered:\n%s", out)
	}
	if post == "" {
		t.Fatalf("no post-delete hook documents rendered:\n%s", out)
	}
	return pre, post
}
