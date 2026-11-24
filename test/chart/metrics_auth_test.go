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

// metricsAuthRender renders one template with the given values body.
func metricsAuthRender(t *testing.T, template, valuesBody string) string {
	t.Helper()
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")
	args := []string{"template", "t", chartDir, "-f", policyTargetValues, "--show-only", template}
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
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// clusterRoleByName returns the rendered ClusterRole or ClusterRoleBinding whose name
// ends in nameSuffix, or nil.
func clusterRoleByName(t *testing.T, rendered, kind, nameSuffix string) *yaml.Node {
	t.Helper()
	for _, d := range strings.Split(rendered, "\n---") {
		if !strings.Contains(d, "kind: "+kind+"\n") {
			continue
		}
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(d), &n); err != nil {
			t.Fatalf("parse rendered %s: %v", kind, err)
		}
		doc := &n
		if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
			doc = n.Content[0]
		}
		name := childValue(doc, "metadata", "name")
		if name != nil && strings.HasSuffix(name.Value, nameSuffix) {
			return doc
		}
	}
	return nil
}

// TestMetricsAuthRBAC proves the manager can create TokenReviews and SubjectAccessReviews
// and scrapers get a metrics-reader grant; either missing fails every scrape.
func TestMetricsAuthRBAC(t *testing.T) {
	t.Run("manager may create the token and access reviews", func(t *testing.T) {
		out := metricsAuthRender(t, "templates/rbac.yaml", "")
		role := clusterRoleByName(t, out, "ClusterRole", "-manager-role")
		if role == nil {
			t.Fatalf("no manager ClusterRole in render:\n%s", out)
		}
		rules := childValue(role, "rules")
		if rules == nil {
			t.Fatal("manager ClusterRole has no rules")
		}
		want := map[string]string{
			"authentication.k8s.io": "tokenreviews",
			"authorization.k8s.io":  "subjectaccessreviews",
		}
		for group, resource := range want {
			found := false
			for _, rule := range rules.Content {
				if !nodeListHas(childValue(rule, "apiGroups"), group) ||
					!nodeListHas(childValue(rule, "resources"), resource) {
					continue
				}
				found = true
				verbs := childValue(rule, "verbs")
				if !nodeListHas(verbs, "create") {
					t.Errorf("%s/%s rule lacks the create verb; the review is the request, so create IS the read", group, resource)
				}
				// Review APIs are create-only.
				if verbs != nil && len(verbs.Content) != 1 {
					t.Errorf("%s/%s grants %d verbs, want create only", group, resource, len(verbs.Content))
				}
			}
			if !found {
				t.Errorf("manager ClusterRole grants nothing on %s/%s; the authenticated metrics endpoint would fail every scrape closed:\n%s",
					group, resource, out)
			}
		}
	})

	t.Run("metrics-reader grants /metrics and nothing else", func(t *testing.T) {
		out := metricsAuthRender(t, "templates/rbac.yaml", "")
		reader := clusterRoleByName(t, out, "ClusterRole", "-metrics-reader")
		if reader == nil {
			t.Fatalf("no metrics-reader ClusterRole rendered; no scraper could be authorized:\n%s", out)
		}
		rules := childValue(reader, "rules")
		if rules == nil || len(rules.Content) != 1 {
			t.Fatalf("metrics-reader must hold exactly one rule; got %v", rules)
		}
		rule := rules.Content[0]
		if !nodeListHas(childValue(rule, "nonResourceURLs"), "/metrics") {
			t.Errorf("metrics-reader does not cover the non-resource URL /metrics, which is the attribute the SubjectAccessReview asks about:\n%s", out)
		}
		if !nodeListHas(childValue(rule, "verbs"), "get") {
			t.Errorf("metrics-reader lacks the get verb (the lowercased HTTP method of a scrape):\n%s", out)
		}
		// A scraper reads metrics, no API object.
		if r := childValue(rule, "resources"); r != nil {
			t.Errorf("metrics-reader grants API resources as well as /metrics; it must grant no object access:\n%s", out)
		}
	})

	// No comma in the subtest name: it becomes the values-file path, and helm splits -f
	// on commas.
	t.Run("no binding without readers and exact subjects with them", func(t *testing.T) {
		out := metricsAuthRender(t, "templates/rbac.yaml", "")
		if b := clusterRoleByName(t, out, "ClusterRoleBinding", "-metrics-reader"); b != nil {
			t.Errorf("metrics-reader ClusterRoleBinding rendered with metrics.readers empty; the chart must not guess who may scrape:\n%s", out)
		}

		out = metricsAuthRender(t, "templates/rbac.yaml",
			"metrics:\n  readers:\n    - name: prometheus\n      namespace: monitoring\n")
		b := clusterRoleByName(t, out, "ClusterRoleBinding", "-metrics-reader")
		if b == nil {
			t.Fatalf("metrics.readers set but no ClusterRoleBinding rendered; Prometheus would get 403:\n%s", out)
		}
		if ref := childValue(b, "roleRef", "name"); ref == nil || !strings.HasSuffix(ref.Value, "-metrics-reader") {
			t.Errorf("binding roleRef = %v, want the metrics-reader ClusterRole:\n%s", ref, out)
		}
		subjects := childValue(b, "subjects")
		if subjects == nil || len(subjects.Content) != 1 {
			t.Fatalf("want exactly the one listed subject; got %v", subjects)
		}
		s := subjects.Content[0]
		if kind := childValue(s, "kind"); kind == nil || kind.Value != "ServiceAccount" {
			t.Errorf("subject kind = %v, want ServiceAccount", kind)
		}
		if name := childValue(s, "name"); name == nil || name.Value != "prometheus" {
			t.Errorf("subject name = %v, want prometheus", name)
		}
		if ns := childValue(s, "namespace"); ns == nil || ns.Value != "monitoring" {
			t.Errorf("subject namespace = %v, want monitoring", ns)
		}
	})

	t.Run("no metrics-reader when the endpoint does not authenticate", func(t *testing.T) {
		// Both opt-outs accept any client, so a reader role would be dead RBAC.
		for _, values := range []string{
			"metrics:\n  requireAuth: false\n",
			"metrics:\n  secure: false\n",
			"metrics:\n  enabled: false\n",
		} {
			out := metricsAuthRender(t, "templates/rbac.yaml", values)
			if r := clusterRoleByName(t, out, "ClusterRole", "-metrics-reader"); r != nil {
				t.Errorf("metrics-reader ClusterRole rendered under %q:\n%s", strings.TrimSpace(values), out)
			}
		}
		// The manager's review grants stay unconditional.
		out := metricsAuthRender(t, "templates/rbac.yaml", "metrics:\n  requireAuth: false\n")
		role := clusterRoleByName(t, out, "ClusterRole", "-manager-role")
		if role == nil {
			t.Fatal("no manager ClusterRole in render")
		}
		if !strings.Contains(out, "tokenreviews") || !strings.Contains(out, "subjectaccessreviews") {
			t.Errorf("review grants dropped with requireAuth=false; re-enabling auth would then fail every scrape closed:\n%s", out)
		}
	})
}

// TestServiceMonitorBearerToken proves the ServiceMonitor presents a bearer token; the
// endpoint answers 401 without one.
func TestServiceMonitorBearerToken(t *testing.T) {
	const saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

	t.Run("projected ServiceAccount token by default", func(t *testing.T) {
		out := metricsAuthRender(t, "templates/servicemonitor.yaml", "metrics:\n  serviceMonitor:\n    enabled: true\n")
		ep := firstEndpoint(t, parseServiceMonitor(t, out))
		tok := childValue(ep, "bearerTokenFile")
		if tok == nil {
			t.Fatalf("endpoint presents no credential; the authenticated endpoint would answer 401 on every scrape:\n%s", out)
		}
		if tok.Value != saTokenPath {
			t.Errorf("bearerTokenFile = %q, want %q (the only path Prometheus Operator permits in a ServiceMonitor)", tok.Value, saTokenPath)
		}
		// The token is in addition to server verification.
		if childValue(ep, "tlsConfig") == nil {
			t.Errorf("tlsConfig dropped alongside the bearer token; the scrape would stop verifying the server:\n%s", out)
		}
	})

	t.Run("no credential when the endpoint does not authenticate", func(t *testing.T) {
		// requireAuth=false: TLS stays, the token goes.
		out := metricsAuthRender(t, "templates/servicemonitor.yaml",
			"metrics:\n  requireAuth: false\n  serviceMonitor:\n    enabled: true\n")
		ep := firstEndpoint(t, parseServiceMonitor(t, out))
		if tok := childValue(ep, "bearerTokenFile"); tok != nil {
			t.Errorf("bearerTokenFile = %q rendered with metrics.requireAuth=false:\n%s", tok.Value, out)
		}
		if childValue(ep, "scheme") == nil {
			t.Errorf("https scheme dropped with requireAuth=false; the auth opt-out must not take TLS with it:\n%s", out)
		}

		// secure=false: a token on a plaintext scrape would leak the credential.
		out = metricsAuthRender(t, "templates/servicemonitor.yaml",
			"metrics:\n  secure: false\n  serviceMonitor:\n    enabled: true\n")
		ep = firstEndpoint(t, parseServiceMonitor(t, out))
		if tok := childValue(ep, "bearerTokenFile"); tok != nil {
			t.Errorf("bearerTokenFile = %q rendered on a plaintext scrape:\n%s", tok.Value, out)
		}
	})
}

// nodeListHas reports whether a YAML sequence node contains the given scalar.
func nodeListHas(n *yaml.Node, want string) bool {
	if n == nil || n.Kind != yaml.SequenceNode {
		return false
	}
	for _, c := range n.Content {
		if c.Value == want {
			return true
		}
	}
	return false
}
