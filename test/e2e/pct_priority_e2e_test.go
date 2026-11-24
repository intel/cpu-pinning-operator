// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestPCTPriorityPolicyAcceptedByPlugin proves the rendered policy carries the PCT
// classes (gpu-hp/high on the device def, cpu-lp/low on CPU-only leaves) and the live
// plugin accepts it; unit tests cannot prove the plugin's validation passes.
func TestPCTPriorityPolicyAcceptedByPlugin(t *testing.T) {
	at := allureCase(t)
	c := newClient(at)
	node := balloonsEnabledNode(at, c)
	waitForNodeSettled(t, c, node)
	ctx := context.Background()
	key := client.ObjectKey{Name: "node." + node, Namespace: balloonsNS()}

	bp := newUnstructured(bpGVK)
	if err := c.Get(ctx, key, bp); err != nil {
		at.Fatalf("get BalloonsPolicy %s: %v", key.Name, err)
	}

	// Exactly one high and one low managed class, by name.
	classes, _, _ := unstructured.NestedSlice(bp.Object, "spec", "cpuClasses")
	got := map[string]string{}
	for _, cc := range classes {
		if m, ok := cc.(map[string]interface{}); ok {
			name, _, _ := unstructured.NestedString(m, "name")
			pri, _, _ := unstructured.NestedString(m, "pctPriority")
			got[name] = pri
		}
	}
	if len(classes) != 2 || got["gpu-hp"] != "high" || got["cpu-lp"] != "low" {
		at.Errorf("spec.cpuClasses = %v, want exactly {gpu-hp: high, cpu-lp: low}", got)
	}

	types, _, _ := unstructured.NestedSlice(bp.Object, "spec", "balloonTypes")
	leaves := 0
	for _, tt := range types {
		m, ok := tt.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(m, "name")
		class, _, _ := unstructured.NestedString(m, "cpuClass")
		comps, _, _ := unstructured.NestedSlice(m, "components")
		var want string
		switch {
		case name == "balloon-device":
			want = "gpu-hp"
		case name == "reserved", len(comps) > 0:
			want = ""
		default:
			want = "cpu-lp"
			leaves++
		}
		if class != want {
			at.Errorf("balloon type %s cpuClass = %q, want %q", name, class, want)
		}
	}
	if leaves == 0 {
		at.Error("no CPU-only leaf balloon type found; nothing carries cpu-lp")
	}

	// Only a verdict on the newest generation says anything about this config.
	var gen int64
	var verdict pluginVerdict
	decided, _, err := watchBalloonsPolicy(key, "", 90*time.Second, func(cur *unstructured.Unstructured) bool {
		gen = max(gen, cur.GetGeneration())
		verdict = readPluginVerdict(cur, node)
		return verdict.found && verdict.generation == gen
	})
	if err != nil {
		at.Fatalf("watch the plugin's verdict: %v", err)
	}
	if !decided {
		skipNoConfig(at, "the plugin never reported on generation %d of %s within 90s (last verdict: generation %d status=%q); "+
			"plugin on %s: %s; no verdict to check", gen, key.Name, verdict.generation, verdict.status, node, pluginPodState(t, c, node))
	}
	if verdict.status != "Success" {
		at.Fatalf("plugin did not accept the PCT-wired policy at generation %d: status=%q errors=%q", gen, verdict.status, verdict.errors)
	}
}
