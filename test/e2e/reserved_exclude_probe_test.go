// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runReservedCpusetCannotOverrideExcludeCpuset probes that a reserved cpuset naming an excluded CPU is rejected by the plugin:
// the exclude-cpuset wins ("contains CPUs not in AllowedResources"). A rejected config is inert, so the probe is safe.
func runReservedCpusetCannotOverrideExcludeCpuset(t *testing.T, c client.Client, node string) {
	if alloc := clusterCPUAllocation(t, c); alloc != "physical-cores" {
		skipNoConfig(t, "cluster cpuAllocation is %q, not physical-cores; no exclude-cpuset to probe", alloc)
	}
	ctx := context.Background()
	key := client.ObjectKey{Name: "node." + node, Namespace: balloonsNS()}

	bp := newUnstructured(bpGVK)
	if err := c.Get(ctx, key, bp); err != nil {
		t.Fatalf("get BalloonsPolicy %s: %v", key.Name, err)
	}
	excl, _, _ := unstructured.NestedString(bp.Object, "spec", "availableResources", "cpu")
	if !strings.HasPrefix(excl, "exclude-cpuset:") {
		skipNoCapability(t, "availableResources.cpu = %q is not an exclude-cpuset; nothing to probe", excl)
	}
	excludedCPUs := parseCPUSet(strings.TrimPrefix(excl, "exclude-cpuset:"))
	if len(excludedCPUs) == 0 {
		skipNoCapability(t, "exclude-cpuset is empty; no excluded CPU to name in the reserved pool")
	}
	victim := -1
	for cpu := range excludedCPUs {
		if victim == -1 || cpu < victim {
			victim = cpu
		}
	}

	// Only a known-good baseline lets a later Failure be attributed to the patch.
	if base, _, _ := unstructured.NestedString(bp.Object, "status", "nodes", node, "status"); base == "Failure" {
		beErr, _, _ := unstructured.NestedString(bp.Object, "status", "nodes", node, "errors")
		skipNoConfig(t, "BalloonsPolicy already in Failure state before the probe (errors=%q); baseline not known-good", beErr)
	}

	// Without a known original value the cleanup could not restore the shared CR.
	origReserved, found, err := unstructured.NestedString(bp.Object, "spec", "reservedResources", "cpu")
	if err != nil || !found {
		skipNoConfig(t, "reservedResources.cpu is absent or not a string (found=%v err=%v); cannot restore it after the probe", found, err)
	}
	t.Logf("node %s: exclude-cpuset=%q, will name excluded CPU %d in reservedResources.cpu (was %q)",
		node, excl, victim, origReserved)

	t.Cleanup(func() {
		patch := fmt.Sprintf(`{"spec":{"reservedResources":{"cpu":%q}}}`, origReserved)
		cur := newUnstructured(bpGVK)
		if err := c.Get(context.Background(), key, cur); err != nil {
			t.Errorf("cleanup: get BalloonsPolicy to restore: %v", err)
			return
		}
		if err := c.Patch(context.Background(), cur, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
			t.Errorf("cleanup: FAILED to restore reservedResources.cpu to %q: %v", origReserved, err)
			return
		}
		// Hand the next test a Ready plugin with a verdict on the restored spec.
		restored := cur.GetGeneration()
		deadline := time.Now().Add(3 * time.Minute)
		ok, _, err := watchBalloonsPolicy(key, "", time.Until(deadline), func(bp *unstructured.Unstructured) bool {
			v := readPluginVerdict(bp, node)
			return v.found && v.generation >= restored
		})
		for ok && err == nil && !pluginReady(t, c, node) && time.Now().Before(deadline) {
			time.Sleep(3 * time.Second)
		}
		if !ok || err != nil || !pluginReady(t, c, node) {
			t.Errorf("cleanup: the plugin on %s was not Ready with a verdict on the restored generation %d within 3m "+
				"(verdict=%v err=%v); later tests that wait on a verdict will fail on this. Plugin: %s",
				node, restored, ok, err, pluginPodState(t, c, node))
		}
	})

	// The NodeReconciler restores its own spec on any Node event, so only a verdict on a
	// generation carrying the probe's cpuset counts, and a restore is answered by re-applying.
	wanted := fmt.Sprintf("cpuset:%d", victim)
	bad := fmt.Sprintf(`{"spec":{"reservedResources":{"cpu":%q}}}`, wanted)
	if err := c.Patch(ctx, bp, client.RawPatch(types.MergePatchType, []byte(bad))); err != nil {
		t.Fatalf("patch reservedResources.cpu to excluded CPU: %v", err)
	}
	pw := newProbeWatch(node, wanted, bp.GetGeneration())
	var verdict, last pluginVerdict
	reverts := 0
	// Watch from the patch response so an early verdict is still delivered.
	decided, gap, err := watchBalloonsPolicy(key, bp.GetResourceVersion(), 90*time.Second, func(cur *unstructured.Unstructured) bool {
		v, ok, reapply := pw.observe(cur)
		last = v
		if ok {
			verdict = v
			return true
		}
		if reapply {
			reverts++
			if err := c.Patch(ctx, cur, client.RawPatch(types.MergePatchType, []byte(bad))); err != nil {
				t.Fatalf("re-apply the probe's reservedResources.cpu after the operator restored it: %v", err)
			}
			pw.applied(cur.GetGeneration())
		}
		return false
	})
	if err != nil {
		t.Fatalf("watch the plugin's verdict: %v", err)
	}
	if !decided {
		skipNoConfig(t, "the plugin never reported on a generation carrying the probe's reservedResources.cpu %q of BalloonsPolicy %s "+
			"within 90s (probe generations %v; last verdict: generation %d status=%q; the operator restored its own spec %d time(s); "+
			"watch history expired and was re-read: %v); "+
			"plugin on %s: %s. With no verdict for the probe's own config, nothing can be concluded about reserved-vs-exclude precedence",
			wanted, key.Name, sortedGens(pw.gens), last.generation, last.status, reverts, gap, node, pluginPodState(t, c, node))
	}
	probed, status, errText := verdict.generation, verdict.status, verdict.errors
	if status != "Failure" {
		t.Fatalf("plugin accepted reservedResources.cpu naming excluded CPU %d at generation %d (status=%q errors=%q); "+
			"the exclude-cpuset should have won — a reserved cpuset must not override it", victim, probed, status, errText)
	}
	// Require the AllowedResources conflict naming the injected CPU, not any unrelated failure.
	if !strings.Contains(errText, "not in AllowedResources") {
		t.Errorf("expected an AllowedResources rejection, got status=%q errors=%q", status, errText)
	}
	if !strings.Contains(errText, fmt.Sprintf("namely %d", victim)) {
		t.Errorf("rejection does not name the injected CPU %d (expected %q in errors), got status=%q errors=%q",
			victim, fmt.Sprintf("namely %d", victim), status, errText)
	}
	t.Logf("confirmed: exclude-cpuset wins — plugin rejected reserved cpuset naming CPU %d: %q", victim, errText)
}

// probeWatch tracks the generations carrying the probe's cpuset; only a verdict on one of them counts.
type probeWatch struct {
	node, wanted string
	gens         map[int64]bool
	newest       int64
}

func newProbeWatch(node, wanted string, patched int64) *probeWatch {
	return &probeWatch{node: node, wanted: wanted, gens: map[int64]bool{patched: true}, newest: patched}
}

// observe reads one revision. reapply means the operator restored its spec and the caller must re-apply and call applied.
func (p *probeWatch) observe(cur *unstructured.Unstructured) (v pluginVerdict, decided, reapply bool) {
	gen := cur.GetGeneration()
	live, _, _ := unstructured.NestedString(cur.Object, "spec", "reservedResources", "cpu")
	if live == p.wanted {
		p.gens[gen] = true
	}
	v = readPluginVerdict(cur, p.node)
	if v.found && p.gens[v.generation] {
		return v, true, false
	}
	reapply = live != p.wanted && gen > p.newest
	p.newest = max(p.newest, gen)
	return v, false, reapply
}

// applied records the generation a re-apply produced.
func (p *probeWatch) applied(gen int64) {
	p.gens[gen] = true
	p.newest = max(p.newest, gen)
}

// TestProbeWatchJudgesOnlyTheProbesContent proves a verdict counts only for a probe generation when the operator restores in between. No cluster.
func TestProbeWatchJudgesOnlyTheProbesContent(t *testing.T) {
	const node, wanted, orig = "n1", "cpuset:64", "cpuset:0-3"
	rev := func(gen int64, cpu string, statusGen int64, status string) *unstructured.Unstructured {
		u := newUnstructured(bpGVK)
		u.SetGeneration(gen)
		_ = unstructured.SetNestedField(u.Object, cpu, "spec", "reservedResources", "cpu")
		_ = unstructured.SetNestedField(u.Object, map[string]any{"generation": statusGen, "status": status}, "status", "nodes", node)
		return u
	}
	pw := newProbeWatch(node, wanted, 5)
	steps := []struct {
		name             string
		rev              *unstructured.Unstructured
		decided, reapply bool
	}{
		{"probe patched, old verdict", rev(5, wanted, 4, "Success"), false, false},
		{"operator restores", rev(6, orig, 4, "Success"), false, true},
		{"stale event of the restore", rev(6, orig, 4, "Success"), false, false},
		{"re-applied, verdict on the restore", rev(7, wanted, 6, "Success"), false, false},
		{"late verdict on the first probe generation", rev(7, wanted, 5, "Failure"), true, false},
	}
	for _, s := range steps {
		_, decided, reapply := pw.observe(s.rev)
		if decided != s.decided || reapply != s.reapply {
			t.Fatalf("%s: decided=%v reapply=%v, want %v/%v", s.name, decided, reapply, s.decided, s.reapply)
		}
		if reapply {
			pw.applied(7)
		}
	}
}
