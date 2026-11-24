// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestReserveAuthorityRendering proves the reserve authority renders by default only
// where it can work (replicaCount > 1 with leader election), in both the deployment and
// networkpolicy templates, with both NetworkPolicy directions open.
func TestReserveAuthorityRendering(t *testing.T) {
	helm := helmBin(t)
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	render := func(t *testing.T, template, valuesFile string) string {
		t.Helper()
		args := []string{"template", "t", chartDir, "-f", policyTargetValues, "--show-only", "templates/" + template}
		if valuesFile != "" {
			args = append(args, "-f", valuesFile)
		}
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template %s failed: %v\n%s", template, err, out)
		}
		return string(out)
	}

	writeValues := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write values: %v", err)
		}
		return p
	}

	const (
		flag         = "--reserve-authority"
		portArg      = "--reserve-port=9444"
		portName     = "name: reserve"
		authorityOff = "webhook:\n  reserveAuthority:\n    enabled: false\n"
	)

	// argLines trims a render to the manager's args and ports for failure messages.
	argLines := func(out string) string {
		var keep []string
		for _, line := range strings.Split(out, "\n") {
			if t := strings.TrimSpace(line); strings.HasPrefix(t, "- --") ||
				strings.HasPrefix(t, "- containerPort:") || strings.HasPrefix(t, "name: ") {
				keep = append(keep, t)
			}
		}
		return strings.Join(keep, "\n")
	}
	assertAbsent := func(t *testing.T, out, needle, why string) {
		t.Helper()
		if strings.Contains(out, needle) {
			t.Errorf("rendered manifest contains %q — %s\n%s", needle, why, argLines(out))
		}
	}
	assertPresent := func(t *testing.T, out, needle, why string) {
		t.Helper()
		if !strings.Contains(out, needle) {
			t.Errorf("rendered manifest is missing %q — %s\n%s", needle, why, argLines(out))
		}
	}

	// reserveRules returns the rule naming the reserve port per direction. A render with
	// no NetworkPolicy fails, so negative cases cannot pass vacuously.
	reserveRules := func(t *testing.T, out string) map[string]*yaml.Node {
		t.Helper()
		found := map[string]*yaml.Node{}
		if !strings.Contains(out, "kind: NetworkPolicy") {
			t.Fatalf("render has no NetworkPolicy, so a reserve-rule assertion proves nothing:\n%s", out)
		}
		np := parseNetworkPolicy(t, out)
		for _, dir := range []string{"ingress", "egress"} {
			rules := childValue(np, "spec", dir)
			if rules == nil || rules.Kind != yaml.SequenceNode {
				continue
			}
			for _, rule := range rules.Content {
				ports := childValue(rule, "ports")
				if ports == nil {
					continue
				}
				for _, port := range ports.Content {
					if p := childValue(port, "port"); p != nil && p.Value == "reserve" {
						found[dir] = rule
					}
				}
			}
		}
		return found
	}

	// Reads the defaults from values.yaml, so flipping one fails here instead of making
	// the default case vacuous.
	t.Run("the chart's defaults are the ones that render it", func(t *testing.T) {
		vals := parseValues(t)
		// Compared as a number: any count below two leaves no peer.
		got := childValue(vals, "replicaCount")
		if n, err := strconv.Atoi(nodeValue(got)); err != nil || n < 2 {
			t.Errorf("replicaCount = %s; the default install must be multi-replica for the authority to have a peer",
				nodeValue(got))
		}
		if got := childValue(vals, "leaderElection", "enabled"); got == nil || got.Value != "true" {
			t.Errorf("leaderElection.enabled = %v, want true; the authority is elected by the lease", nodeValue(got))
		}
		if got := childValue(vals, "webhook", "reserveAuthority", "enabled"); got == nil || got.Value != "true" {
			t.Errorf("webhook.reserveAuthority.enabled = %s, want true — two replicas with two independent "+
				"reservation stores cannot account the fleet's headroom against one store", nodeValue(got))
		}
		if got := childValue(vals, "certManager", "enabled"); got == nil || got.Value != "true" {
			t.Errorf("certManager.enabled = %s, want true; the authority needs a serving cert carrying "+
				"ca.crt and a DNS SAN, and the chart-managed one is what vouches for that", nodeValue(got))
		}
	})

	t.Run("on by the chart's shipped default", func(t *testing.T) {
		dep := render(t, "deployment.yaml", "")
		assertPresent(t, dep, flag, "a default multi-replica install must account against one store without being asked")
		assertPresent(t, dep, portArg, "the reserve port must be passed alongside the flag")
		assertPresent(t, dep, portName, "the internal reserve listener needs its container port declared")

		// The NetworkPolicy is on by default, so the port must be open both ways.
		rules := reserveRules(t, render(t, "networkpolicy.yaml", ""))
		for _, dir := range []string{"ingress", "egress"} {
			if rules[dir] == nil {
				t.Errorf("no reserve %s rule; a follower cannot transact with the leader under a default-deny policy", dir)
			}
		}
		// Without the peer selector the mTLS port is reachable from every pod.
		if in := rules["ingress"]; in != nil {
			from := childValue(in, "from")
			if from == nil || from.Kind != yaml.SequenceNode || len(from.Content) == 0 {
				t.Error("reserve ingress rule has no `from` peer selector; the reserve port is open to every pod")
			} else if sel := childValue(from.Content[0], "podSelector", "matchLabels", "control-plane"); sel == nil {
				t.Error("reserve ingress peer selector does not match the manager's own labels")
			}
		}
	})

	// Each case withholds one conjunct from an otherwise default install.
	for _, tc := range []struct {
		name, values, why string
	}{
		{
			name:   "single replica pays nothing",
			values: "replicaCount: 1\n",
			why:    "a single-replica install has no peer to forward to",
		},
		{
			name:   "leader election off pays nothing",
			values: "leaderElection:\n  enabled: false\n",
			why:    "the manager exits when --reserve-authority is set without --leader-elect",
		},
		{
			name:   "explicitly disabled pays nothing",
			values: authorityOff,
			why:    "the escape hatch must still turn the listener and the round trip off",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vals := writeValues(t, tc.values)
			dep := render(t, "deployment.yaml", vals)
			assertAbsent(t, dep, flag, tc.why)
			assertAbsent(t, dep, portArg, "the reserve port argument follows the flag")
			assertAbsent(t, dep, portName, "no reserve listener: "+tc.why)

			if rules := reserveRules(t, render(t, "networkpolicy.yaml", vals)); len(rules) != 0 {
				t.Errorf("reserve NetworkPolicy rules rendered with %s: %v — %s", tc.name, keysOf(rules), tc.why)
			}
		})
	}

	// The manager exits if the serving cert lacks ca.crt or a DNS SAN, which a BYO secret
	// need not carry.
	t.Run("an unvouched serving cert renders nothing", func(t *testing.T) {
		vals := writeValues(t, "certManager:\n  enabled: false\nballoonsReady:\n  secure: false\n")
		dep := render(t, "deployment.yaml", vals)
		assertAbsent(t, dep, flag, "the manager cannot verify a peer against a cert the chart did not issue")
		assertAbsent(t, dep, portName, "no reserve listener without a vouched serving cert")

		if rules := reserveRules(t, render(t, "networkpolicy.yaml", vals)); len(rules) != 0 {
			t.Errorf("reserve NetworkPolicy rules rendered without a vouched serving cert: %v", keysOf(rules))
		}
	})

	t.Run("acknowledging an external serving cert renders it again", func(t *testing.T) {
		vals := writeValues(t, "certManager:\n  enabled: false\nballoonsReady:\n  secure: false\n"+
			"webhook:\n  reserveAuthority:\n    allowExternalServingCert: true\n")
		dep := render(t, "deployment.yaml", vals)
		assertPresent(t, dep, flag, "allowExternalServingCert must re-open the gate it is the escape hatch for")
		assertPresent(t, dep, portArg, "the reserve port must be passed alongside the flag")

		rules := reserveRules(t, render(t, "networkpolicy.yaml", vals))
		for _, dir := range []string{"ingress", "egress"} {
			if rules[dir] == nil {
				t.Errorf("no reserve %s rule; the acknowledged path must be reachable too", dir)
			}
		}
	})
}

// parseValues returns the chart's values.yaml as a document node.
func parseValues(t *testing.T) *yaml.Node {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "cpu-pinning-operator", "values.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return &root
}

// nodeValue renders a node for a failure message, naming an absent key.
func nodeValue(n *yaml.Node) string {
	if n == nil {
		return "(absent)"
	}
	return n.Value
}

// keysOf names the directions a reserve rule was found in, for failure messages.
func keysOf(m map[string]*yaml.Node) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
