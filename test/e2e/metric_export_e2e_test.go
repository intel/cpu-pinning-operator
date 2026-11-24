// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestHTSiblingPolicyMetricExported proves the ht-sibling-policy gauge exports four indicator series with exactly one at 1.
// The gauge appears only after the serving manager resolved the node's topology, hence the publish-window wait.
func TestHTSiblingPolicyMetricExported(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_METRICS") == "" {
		skipNotSelected(at, "set E2E_METRICS=1 to scrape the operator metrics endpoint via the apiserver proxy")
	}
	c := newClient(at)
	node := balloonsEnabledNode(at, c)

	// The gauge is Set per node reconcile; a node mid-reprovision can lack it or show a transitional value.
	waitForNodeSettled(t, c, node)

	// A settled render may come from the previous manager instance, so wait for this one to publish.
	const metric = "cpu_pinning_ht_sibling_policy"
	body := scrapeOperatorMetricsForNode(t, node, metric)

	if !metricPresentForNode(body, metric, node) {
		at.Fatalf("metrics missing %s for node %q", metric, node)
	}

	if v, ok := metricValueForNode(body, metric, node); !ok {
		at.Errorf("could not parse %s sample for node %q", metric, node)
	} else if v != 0 && v != 1 {
		at.Errorf("%s = %v for node %q, want an indicator value (0 or 1)", metric, v, node)
	}

	// The label set is spelled out rather than imported, so a change to it must be made here too.
	wantPolicies := map[string]bool{
		"held-back": true, "reserved-pool": true, "allocatable": true, "no-siblings": true,
	}
	seen := map[string]float64{}
	ones := 0
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metric+"{") || !strings.Contains(line, `node="`+node+`"`) {
			continue
		}
		policy := labelValue(line, "policy")
		if policy == "" {
			at.Errorf("%s series has no policy label for node %q: %q", metric, node, line)
			continue
		}
		if !wantPolicies[policy] {
			at.Errorf("%s exports unexpected policy=%q for node %q, want only "+
				"held-back/reserved-pool/allocatable/no-siblings", metric, policy, node)
			continue
		}
		if _, dup := seen[policy]; dup {
			at.Errorf("%s exports a duplicate policy=%q series for node %q", metric, policy, node)
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			at.Errorf("malformed sample line for %s: %q", metric, line)
			continue
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			at.Errorf("could not parse value in %q: %v", line, err)
			continue
		}
		if v != 0 && v != 1 {
			at.Errorf("%s{policy=%q} = %v for node %q, want an indicator value (0 or 1)", metric, policy, v, node)
			continue
		}
		seen[policy] = v
		if v == 1 {
			ones++
		}
	}
	for policy := range wantPolicies {
		if _, ok := seen[policy]; !ok {
			at.Errorf("%s missing the policy=%q series for node %q, want all of held-back, "+
				"reserved-pool, allocatable and no-siblings", metric, policy, node)
		}
	}
	if ones != 1 {
		at.Errorf("%s has %d series set to 1 for node %q, want exactly one outcome in force", metric, ones, node)
	}
}

// labelValue returns one label's value from an exposition line, or "". It handles no escaped quotes or commas.
func labelValue(line, key string) string {
	open := strings.IndexByte(line, '{')
	shut := strings.IndexByte(line, '}')
	if open < 0 || shut < 0 || shut < open {
		return ""
	}
	for _, pair := range strings.Split(line[open+1:shut], ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		if strings.TrimSpace(kv[0]) == key {
			return strings.Trim(strings.TrimSpace(kv[1]), `"`)
		}
	}
	return ""
}

// TestNodeMetricsMissingTracksThePublishWindow proves nodeMetricsMissing reports exactly the unpublished series
// across a rollout's expositions. Needs no cluster.
func TestNodeMetricsMissingTracksThePublishWindow(t *testing.T) {
	const (
		node = "node1"
		ht   = "cpu_pinning_ht_sibling_policy"
		frag = "cpu_pinning_cpu_fragmentation"
	)
	// Runtime series exist from the first scrape, so a non-empty body says nothing about the operator's gauges.
	runtime := "go_goroutines 42\nprocess_cpu_seconds_total 1.5\n"
	htSeries := ht + `{node="node1",policy="allocatable"} 0` + "\n" +
		ht + `{node="node1",policy="held-back"} 1` + "\n" +
		ht + `{node="node1",policy="no-siblings"} 0` + "\n" +
		ht + `{node="node1",policy="reserved-pool"} 0` + "\n"
	fragSeries := frag + `{node="node1"} 0` + "\n"

	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"a freshly started replica: no cpu_pinning_ node series at all", runtime, []string{ht, frag}},
		{"mid-window: the NRT-driven gauge up, the node-reconcile one not yet", runtime + fragSeries, []string{ht}},
		{"the settled replica: both published", runtime + fragSeries + htSeries, nil},
		{"another node's series only", runtime + ht + `{node="node2",policy="held-back"} 1` + "\n", []string{ht, frag}},
		// The label match closes the quote, so node10's series must not pass for node1's.
		{"a longer node name sharing the prefix", runtime + fragSeries + ht + `{node="node10",policy="held-back"} 1` + "\n", []string{ht}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nodeMetricsMissing(tc.body, node, ht, frag)
			if len(got) != len(tc.want) {
				t.Fatalf("nodeMetricsMissing = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("nodeMetricsMissing = %v, want %v (order follows the metrics given)", got, tc.want)
					break
				}
			}
		})
	}
}

// TestNodeProvisioningMetricExported proves the provisioning counter is exported as a non-negative counter.
// It increments only on a transition, so absence is a capability skip that E2E_STRICT must not turn red.
func TestNodeProvisioningMetricExported(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_METRICS") == "" {
		skipNotSelected(at, "set E2E_METRICS=1 to scrape the operator metrics endpoint via the apiserver proxy")
	}
	// The series exists only once a node was provisioned.
	c := newClient(at)
	_ = balloonsEnabledNode(at, c)

	body := scrapeOperatorMetrics(t)

	const metric = "cpu_pinning_node_provisioning_total"
	v, present, parsed := anyCounterValue(body, metric)
	if !present {
		skipNoCapability(at, "%s not present — the counter increments only on a provisioning transition; "+
			"a long-running operator with unchanged config re-emits nothing (not a failure)", metric)
	}
	// Present but unparseable is a real regression, not an absence.
	if !parsed {
		at.Errorf("%s is present but its value did not parse as a counter", metric)
	} else if v < 0 {
		at.Errorf("%s = %v, want a non-negative counter", metric, v)
	}
}

// TestCPUFragmentationMetricExported proves the stranded-free-CPU gauge is a non-negative count per provisioned node.
func TestCPUFragmentationMetricExported(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_METRICS") == "" {
		skipNotSelected(at, "set E2E_METRICS=1 to scrape the operator metrics endpoint via the apiserver proxy")
	}
	c := newClient(at)
	node := balloonsEnabledNode(at, c)

	// Wait for the render to settle, then for the serving manager to cache the node's topology.
	waitForNodeSettled(t, c, node)

	const metric = "cpu_pinning_cpu_fragmentation"
	body := scrapeOperatorMetricsForNode(t, node, metric)

	if !metricPresentForNode(body, metric, node) {
		// Absent means the topology is not cached yet: unknown, not a failure.
		skipNoCapability(at, "%s not present for node %q — the gauge is emitted once the node's "+
			"topology is cached (NRT observed); skipping until then", metric, node)
	}
	if v, ok := metricValueForNode(body, metric, node); !ok {
		at.Errorf("could not parse %s sample for node %q", metric, node)
	} else if v < 0 {
		at.Errorf("%s = %v for node %q, want a non-negative stranded-CPU count", metric, v, node)
	}
}
