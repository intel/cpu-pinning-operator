// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// renderedDoc is the subset of a rendered manifest the manager-RBAC test reads.
type renderedDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Data    map[string]string `yaml:"data"`
	Rules   []rbacRule        `yaml:"rules"`
	RoleRef struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"roleRef"`
	Subjects []struct {
		Kind      string `yaml:"kind"`
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"subjects"`
}

// renderDocs renders the named chart templates and decodes every document.
func renderDocs(t *testing.T, release string, extra []string, templates ...string) []renderedDoc {
	t.Helper()
	args := []string{"template", release, filepath.Join("..", "..", "deploy", "cpu-pinning-operator"), "-f", policyTargetValues}
	for _, tpl := range templates {
		args = append(args, "--show-only", tpl)
	}
	out, err := exec.Command(helmBin(t), append(args, extra...)...).CombinedOutput()
	if err != nil {
		requireSubcharts(t, string(out))
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	var docs []renderedDoc
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	for {
		var doc renderedDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("decoding rendered YAML: %v\n%s", err, out)
		}
		docs = append(docs, doc)
	}
}

// TestProbePodWritesAreNamespaced proves pod create/delete comes from a Role in the
// release namespace, never the ClusterRole: cluster-wide pod create can run a pod under
// any ServiceAccount.
func TestProbePodWritesAreNamespaced(t *testing.T) {
	const ns = "probe-ns"
	docs := renderDocs(t, "cpu-pinning-operator", []string{"--namespace", ns},
		"templates/rbac.yaml", "templates/deployment.yaml")

	var deployNS, roleName string
	var bound bool
	for _, doc := range docs {
		switch doc.Kind {
		case "Deployment":
			deployNS = doc.Metadata.Namespace
		case "ClusterRole":
			for _, rule := range doc.Rules {
				if slices.Contains(rule.Resources, "pods") &&
					(slices.Contains(rule.Verbs, "create") || slices.Contains(rule.Verbs, "delete")) {
					t.Errorf("ClusterRole %s grants %v on pods cluster-wide", doc.Metadata.Name, rule.Verbs)
				}
			}
		case "Role":
			if doc.Metadata.Namespace != ns {
				t.Errorf("Role %s in namespace %q, want the release namespace %q", doc.Metadata.Name, doc.Metadata.Namespace, ns)
			}
			if len(doc.Rules) != 1 || !sameSet(doc.Rules[0].Resources, []string{"pods"}) ||
				!sameSet(doc.Rules[0].Verbs, []string{"create", "delete"}) || len(doc.Rules[0].ResourceNames) != 0 {
				t.Errorf("Role %s rules = %+v, want exactly pods [create delete]", doc.Metadata.Name, doc.Rules)
			}
			roleName = doc.Metadata.Name
		case "RoleBinding":
			bound = doc.Metadata.Namespace == ns && doc.RoleRef.Kind == "Role" && doc.RoleRef.Name != "" &&
				len(doc.Subjects) == 1 && doc.Subjects[0].Kind == "ServiceAccount" &&
				doc.Subjects[0].Name == "cpu-pinning-operator" && doc.Subjects[0].Namespace == ns
			if bound && doc.RoleRef.Name != roleName {
				t.Errorf("RoleBinding refs Role %q, rendered Role is %q", doc.RoleRef.Name, roleName)
			}
		}
	}
	if roleName == "" || !bound {
		t.Fatalf("no Role/RoleBinding granting the manager ServiceAccount pod create/delete in %q", ns)
	}
	// Probe pods are created in POD_NAMESPACE, the Deployment's namespace.
	if deployNS != ns {
		t.Errorf("Deployment in namespace %q, the probe Role in %q — probe pod creation would be Forbidden", deployNS, ns)
	}
}

// TestManagerWebhookPatchIsPinned proves patch on mutatingwebhookconfigurations is pinned
// to the name in WEBHOOK_CONFIG_NAME while read verbs stay unscoped (list/watch cannot be
// restricted by name).
func TestManagerWebhookPatchIsPinned(t *testing.T) {
	for _, tc := range []struct {
		release string
		args    []string
		want    string
	}{
		{release: "cpu-pinning-operator", want: "cpu-pinning-operator-mutating-webhook"},
		{release: "other", want: "other-cpu-pinning-operator-mutating-webhook"},
		{release: "other", args: []string{"--set", "fullnameOverride=cpo", "--set", "webhook.mutatingWebhookConfiguration.name=mw"}, want: "cpo-mw"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			var webhookName, envName string
			var rules []rbacRule
			for _, doc := range renderDocs(t, tc.release, tc.args,
				"templates/rbac.yaml", "templates/configmap.yaml", "templates/mutatingwebhook.yaml") {
				switch doc.Kind {
				case "MutatingWebhookConfiguration":
					webhookName = doc.Metadata.Name
				case "ConfigMap":
					if v, ok := doc.Data["WEBHOOK_CONFIG_NAME"]; ok {
						envName = v
					}
				case "ClusterRole":
					if strings.HasSuffix(doc.Metadata.Name, "-manager-role") {
						rules = append(rules, doc.Rules...)
					}
				}
			}
			if webhookName != tc.want || envName != tc.want {
				t.Fatalf("rendered webhook %q, WEBHOOK_CONFIG_NAME %q, want both %q", webhookName, envName, tc.want)
			}

			var read, write int
			for _, rule := range rules {
				if !slices.Contains(rule.Resources, "mutatingwebhookconfigurations") {
					continue
				}
				if len(rule.ResourceNames) == 0 {
					read++
					if !sameSet(rule.Verbs, []string{"get", "list", "watch"}) {
						t.Errorf("unscoped mutatingwebhookconfigurations verbs = %v, want exactly [get list watch]", rule.Verbs)
					}
					continue
				}
				write++
				if !sameSet(rule.Verbs, []string{"patch"}) {
					t.Errorf("pinned mutatingwebhookconfigurations verbs = %v, want exactly [patch]", rule.Verbs)
				}
				if !sameSet(rule.ResourceNames, []string{tc.want}) {
					t.Errorf("patch resourceNames = %v, want [%s] — the reconcilers' patch would be Forbidden", rule.ResourceNames, tc.want)
				}
			}
			if read != 1 || write != 1 {
				t.Errorf("want one unscoped read rule and one pinned patch rule on mutatingwebhookconfigurations, got %d and %d", read, write)
			}
		})
	}
}
