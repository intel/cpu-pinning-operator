// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestNetworkPolicyRendering proves the default-deny NetworkPolicy renders the documented
// rules for each value combination, with an explicit `egress: []` rather than null.
func TestNetworkPolicyRendering(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	// render runs `helm template` for the networkpolicy template only ("" = chart
	// defaults).
	render := func(t *testing.T, valuesFile string) string {
		t.Helper()
		args := []string{
			"template", "t", chartDir, "-f", policyTargetValues,
			"--show-only", "templates/networkpolicy.yaml",
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

	// writeValues writes a values overlay in the test's temp dir.
	writeValues := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write values: %v", err)
		}
		return p
	}

	// hasNetworkPolicy ignores the source header helm emits even for an empty render.
	hasNetworkPolicy := func(out string) bool {
		return strings.Contains(out, "kind: NetworkPolicy")
	}

	t.Run("present by default", func(t *testing.T) {
		// Chart default is networkPolicy.enabled=true.
		out := render(t, "")
		if !hasNetworkPolicy(out) {
			t.Errorf("NetworkPolicy not rendered with default values")
		}
	})

	t.Run("absent when explicitly disabled", func(t *testing.T) {
		vf := writeValues(t, "networkPolicy:\n  enabled: false\n")
		out := render(t, vf)
		if hasNetworkPolicy(out) {
			t.Errorf("NetworkPolicy rendered while explicitly disabled:\n%s", out)
		}
	})

	t.Run("present when enabled", func(t *testing.T) {
		vf := writeValues(t, "networkPolicy:\n  enabled: true\n")
		out := render(t, vf)
		if !hasNetworkPolicy(out) {
			t.Fatalf("NetworkPolicy not rendered when enabled:\n%s", out)
		}
		np := parseNetworkPolicy(t, out)

		if pts := policyTypes(t, np); !pts["Ingress"] || !pts["Egress"] {
			t.Errorf("expected Ingress+Egress policyTypes for default-deny, got %v", pts)
		}

		if !strings.Contains(out, "port: webhook-server") {
			t.Errorf("expected webhook-server ingress rule:\n%s", out)
		}
	})

	t.Run("metrics ingress present when metrics enabled", func(t *testing.T) {
		// Chart default has metrics.enabled=true.
		vf := writeValues(t, "networkPolicy:\n  enabled: true\n")
		out := render(t, vf)
		if !strings.Contains(out, "port: metrics") {
			t.Errorf("expected metrics ingress rule when metrics.enabled=true:\n%s", out)
		}
	})

	t.Run("metrics ingress absent when metrics disabled", func(t *testing.T) {
		vf := writeValues(t, "networkPolicy:\n  enabled: true\nmetrics:\n  enabled: false\n")
		out := render(t, vf)
		if strings.Contains(out, "port: metrics") {
			t.Errorf("metrics ingress rule rendered while metrics.enabled=false:\n%s", out)
		}
	})

	t.Run("metrics.from peers applied", func(t *testing.T) {
		vf := writeValues(t, `networkPolicy:
  enabled: true
  metrics:
    from:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
`)
		out := render(t, vf)
		if !strings.Contains(out, "kubernetes.io/metadata.name: monitoring") {
			t.Errorf("networkPolicy.metrics.from peers not applied to metrics ingress rule:\n%s", out)
		}
		// The from peers must attach to the metrics rule.
		if idxMetrics, idxFrom := strings.Index(out, "port: metrics"), strings.Index(out, "namespaceSelector"); idxFrom < idxMetrics {
			t.Errorf("metrics.from peers not associated with the metrics ingress rule:\n%s", out)
		}
	})

	t.Run("webhook ingress open to any source by default", func(t *testing.T) {
		// The API server dials from host-network IPs no selector matches, so the default
		// must stay open.
		rule := webhookIngressRule(t, parseNetworkPolicy(t, render(t, "")))
		if findChild(rule, "from") != nil {
			t.Errorf("default webhook ingress rule carries a `from` block, which would " +
				"cut admission off on any cluster whose control-plane IPs it does not name")
		}
	})

	t.Run("webhook.from peers applied", func(t *testing.T) {
		vf := writeValues(t, restrictedWebhookValues)
		out := render(t, vf)
		rule := webhookIngressRule(t, parseNetworkPolicy(t, out))
		// Read off the webhook rule itself, not the metrics rule below it.
		if findChild(rule, "from") == nil {
			t.Fatalf("networkPolicy.webhook.from peers not applied to the webhook ingress rule:\n%s", out)
		}
		for _, allowed := range []string{"10.0.0.1", "10.0.0.14", "172.20.9.3"} {
			if !webhookRuleAdmits(t, rule, allowed) {
				t.Errorf("control-plane address %s is not admitted by the restricted rule, "+
					"so this configuration would break admission rather than narrow it:\n%s", allowed, out)
			}
		}
	})

	// Evaluated against the rendered policy; TestWebhookPortRejectsNonAllowedPod in
	// test/e2e is the live counterpart.
	t.Run("a pod outside the allowed CIDRs cannot reach the webhook port", func(t *testing.T) {
		out := render(t, writeValues(t, restrictedWebhookValues))
		rule := webhookIngressRule(t, parseNetworkPolicy(t, out))
		// Addresses the value deliberately omits.
		for _, blocked := range []string{"10.244.3.17", "10.0.0.17", "192.168.5.4", "172.20.9.4"} {
			if webhookRuleAdmits(t, rule, blocked) {
				t.Errorf("source %s reaches the webhook port under networkPolicy.webhook.from; "+
					"a pod there could post a crafted AdmissionReview straight at the webhook:\n%s", blocked, out)
			}
		}
	})

	t.Run("egress: api-server ports and DNS by default", func(t *testing.T) {
		vf := writeValues(t, "networkPolicy:\n  enabled: true\n")
		out := render(t, vf)

		for _, want := range []string{"port: 443", "port: 6443", "port: 53"} {
			if !strings.Contains(out, want) {
				t.Errorf("expected egress %q with chart defaults:\n%s", want, out)
			}
		}
	})

	t.Run("egress: DNS only when apiServerPorts empty", func(t *testing.T) {
		vf := writeValues(t, "networkPolicy:\n  enabled: true\n  egress:\n    apiServerPorts: []\n    dns: true\n")
		out := render(t, vf)
		if strings.Contains(out, "port: 443") || strings.Contains(out, "port: 6443") {
			t.Errorf("api-server egress rule rendered while apiServerPorts empty:\n%s", out)
		}
		if !strings.Contains(out, "port: 53") {
			t.Errorf("expected DNS egress rule when dns=true:\n%s", out)
		}
	})

	t.Run("egress: only pre-flight loopback when no other egress rules", func(t *testing.T) {
		// Only the balloons-ready loopback rule remains. The reserve authority is off
		// because it adds its own egress rule (asserted in reserve_authority_test.go).
		vf := writeValues(t, "networkPolicy:\n  enabled: true\n  egress:\n    apiServerPorts: []\n    dns: false\n"+
			"webhook:\n  reserveAuthority:\n    enabled: false\n")
		out := render(t, vf)
		np := parseNetworkPolicy(t, out)
		egress := childValue(np, "spec", "egress")
		if egress == nil {
			t.Fatalf("spec.egress missing; expected pre-flight loopback rule:\n%s", out)
		}
		if egress.Kind != yaml.SequenceNode {
			t.Fatalf("spec.egress is not a list (got tag %q):\n%s", egress.Tag, out)
		}
		if len(egress.Content) != 1 {
			t.Errorf("expected 1 egress rule (pre-flight loopback), got %d:\n%s", len(egress.Content), out)
		}
		// Egress must stay declared so the pod is default-deny.
		if !policyTypes(t, np)["Egress"] {
			t.Errorf("Egress policyType dropped alongside empty egress list; pod is no longer default-deny:\n%s", out)
		}
	})

	// Pins the default rule set, so an unconditional rule cannot widen every install
	// unnoticed.
	t.Run("default policy opens exactly the flows the operator needs", func(t *testing.T) {
		np := parseNetworkPolicy(t, render(t, ""))
		for _, want := range []struct {
			dir   string
			rules int
		}{{"ingress", 5}, {"egress", 4}} {
			got := childValue(np, "spec", want.dir)
			if got == nil || got.Kind != yaml.SequenceNode {
				t.Fatalf("spec.%s missing or not a list", want.dir)
			}
			if len(got.Content) != want.rules {
				t.Errorf("default %s has %d rules, want %d — a rule was added or removed; "+
					"ingress: webhook, metrics, health, balloons-ready, reserve; egress: balloons-ready loopback, reserve, api-server, DNS",
					want.dir, len(got.Content), want.rules)
			}
		}
	})
}

// restrictedWebhookValues pins the webhook port to a control-plane /28 plus one
// off-subnet API-server address, as docs/secure-configuration.md describes.
const restrictedWebhookValues = `networkPolicy:
  enabled: true
  webhook:
    from:
      - ipBlock:
          cidr: 10.0.0.0/28
      - ipBlock:
          cidr: 172.20.9.3/32
`

// webhookIngressRule returns the ingress rule that opens the webhook port.
func webhookIngressRule(t *testing.T, np *yaml.Node) *yaml.Node {
	t.Helper()
	ingress := childValue(np, "spec", "ingress")
	if ingress == nil || ingress.Kind != yaml.SequenceNode {
		t.Fatalf("spec.ingress missing or not a list")
	}
	for _, rule := range ingress.Content {
		ports := findChild(rule, "ports")
		if ports == nil {
			continue
		}
		for _, p := range ports.Content {
			if port := findChild(p, "port"); port != nil && port.Value == "webhook-server" {
				return rule
			}
		}
	}
	t.Fatalf("no ingress rule opens the webhook-server port")
	return nil
}

// webhookRuleAdmits reports whether an ingress rule admits ip: no `from` admits all, an
// ipBlock admits its cidr.
// Any other peer shape, or an `except`, fails the test so negative assertions cannot pass
// vacuously.
func webhookRuleAdmits(t *testing.T, rule *yaml.Node, ip string) bool {
	t.Helper()
	from := findChild(rule, "from")
	if from == nil {
		return true // no peers = every source
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		t.Fatalf("parse test address %q: %v", ip, err)
	}
	for _, peer := range from.Content {
		block := findChild(peer, "ipBlock")
		if block == nil || len(peer.Content) != 2 {
			t.Fatalf("webhook ingress peer is not a plain ipBlock; this evaluator cannot decide it")
		}
		if findChild(block, "except") != nil {
			t.Fatalf("ipBlock carries an `except`, which this evaluator does not implement")
		}
		cidr := findChild(block, "cidr")
		if cidr == nil {
			t.Fatalf("ipBlock peer has no cidr")
		}
		prefix, err := netip.ParsePrefix(cidr.Value)
		if err != nil {
			t.Fatalf("parse rendered cidr %q: %v", cidr.Value, err)
		}
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// parseNetworkPolicy extracts the single NetworkPolicy document from a multi-doc render.
func parseNetworkPolicy(t *testing.T, rendered string) *yaml.Node {
	t.Helper()
	for _, doc := range strings.Split(rendered, "\n---") {
		if !strings.Contains(doc, "kind: NetworkPolicy") {
			continue
		}
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
			t.Fatalf("parse rendered NetworkPolicy: %v", err)
		}
		if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
			return n.Content[0]
		}
		return &n
	}
	t.Fatalf("no NetworkPolicy document in render:\n%s", rendered)
	return nil
}

// policyTypes returns the set of spec.policyTypes entries.
func policyTypes(t *testing.T, np *yaml.Node) map[string]bool {
	t.Helper()
	pts := childValue(np, "spec", "policyTypes")
	if pts == nil || pts.Kind != yaml.SequenceNode {
		return nil
	}
	out := map[string]bool{}
	for _, e := range pts.Content {
		out[e.Value] = true
	}
	return out
}

// childValue walks a mapping down the key path, returning the final value node or nil.
func childValue(n *yaml.Node, keys ...string) *yaml.Node {
	cur := n
	for _, k := range keys {
		if cur == nil {
			return nil
		}
		cur = findChild(cur, k)
	}
	return cur
}
