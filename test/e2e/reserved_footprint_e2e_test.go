// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/intel/cpu-pinning-operator/internal/balloons"
)

// runReservedFootprintPhysicalCoresDividesByThreads proves physical-cores mode reserves the hyperthreads count
// ÷ threadsPerCore (even-rounded): the pool has every sibling excluded, so a logical count would double the footprint.
func runReservedFootprintPhysicalCoresDividesByThreads(t *testing.T, c client.Client, node string) {
	// The mode is detected via the exclude-cpuset, which density mode does not emit.
	if clusterHTSiblingPolicy(t, c) == "reserved" {
		skipNoConfig(t, "cluster htSiblingPolicy=reserved (density mode); the reserved pool is the HT-sibling set, the ÷threadsPerCore footprint conversion is not applied, and the exclude-cpuset mode signal is absent — this test is only valid in exclude mode")
	}

	_, threads := nodeTopology(t, c, node)
	if threads < 2 {
		skipNoCapability(t, "node %s has threads-per-core=%d (not SMT); no ÷threadsPerCore conversion to observe", node, threads)
	}

	origAlloc, origPresent := pfGetCPUAllocation(t, c)
	t.Cleanup(func() {
		pfRestoreCPUAllocation(t, c, origAlloc, origPresent)
	})

	pfSetCPUAllocation(t, c, "physical-cores")
	physCount, physSpread := pfWaitReservedForMode(t, c, node, "physical-cores")
	if physCount < 2 {
		skipNoCapability(t, "physical-cores reserved count is %d on node %s (tiny-node fallback); nothing to divide", physCount, node)
	}
	t.Logf("physical-cores reserved count on %s = %d (threads-per-core=%d)", node, physCount, threads)

	pfSetCPUAllocation(t, c, "hyperthreads")
	htCount, htSpread := pfWaitReservedForMode(t, c, node, "hyperthreads")
	t.Logf("hyperthreads reserved count on %s = %d", node, htCount)

	// A cpuset rounds the resolved size UP to a multiple of the NUMA count, so the rendered htCount only bounds
	// the resolved one; physCount must match what one of the candidates converts to.
	numa := pfNUMANodes(t, c, node)
	wantPhys := pfWantPhysical(htCount, threads, numa, htSpread, physSpread)
	switch pfJudgeFootprint(wantPhys, physCount, htCount) {
	case pfFootprintWrong:
		t.Errorf("physical-cores reserved = %d, want one of %v (hyperthreads %d ÷ threads %d, even-rounded, "+
			"spread over %d NUMA node(s)); the ÷threadsPerCore footprint conversion did not apply",
			physCount, wantPhys, htCount, threads, numa)
	case pfFootprintAmbiguous:
		skipNoCapability(t, "node %s: physical-cores reserved %d equals hyperthreads reserved %d, and the spread over %d "+
			"NUMA node(s) renders that count with or without the ÷threadsPerCore conversion; nothing to observe",
			node, physCount, htCount, numa)
	}

	pfSetCPUAllocation(t, c, "physical-cores")
	restored, _ := pfWaitReservedForMode(t, c, node, "physical-cores")
	if restored != physCount {
		t.Errorf("after flipping back to physical-cores, reserved = %d, want the original %d", restored, physCount)
	}
}

// pfFootprintVerdict is the outcome of pfJudgeFootprint.
type pfFootprintVerdict int

const (
	pfFootprintOK        pfFootprintVerdict = iota // a converted count was observed
	pfFootprintWrong                               // the count no conversion produces
	pfFootprintAmbiguous                           // could be converted or not; inconclusive
)

// pfJudgeFootprint checks physCount against wantPhys. It is ambiguous when physCount also equals htCount,
// since a dropped conversion would render that same count.
func pfJudgeFootprint(wantPhys []int, physCount, htCount int) pfFootprintVerdict {
	if !slices.Contains(wantPhys, physCount) {
		return pfFootprintWrong
	}
	if physCount == htCount {
		return pfFootprintAmbiguous
	}
	return pfFootprintOK
}

// pfGetCPUAllocation returns the singleton's cpuAllocation and whether it is set; skips when the singleton is absent.
func pfGetCPUAllocation(t *testing.T, c client.Client) (value string, present bool) {
	t.Helper()
	bcc, found, err := getBCC(context.Background(), c)
	if err != nil {
		t.Fatalf("get BalloonsClusterConfig/cluster: %v", err)
	}
	if !found {
		skipNoConfig(t, "BalloonsClusterConfig/cluster not present; nothing to flip")
	}
	v, present, _ := unstructured.NestedString(bcc.Object, "spec", "balloon", "cpuAllocation")
	return v, present
}

// pfSetCPUAllocation sets spec.balloon.cpuAllocation, retrying on conflict.
func pfSetCPUAllocation(t *testing.T, c client.Client, value string) {
	t.Helper()
	updateBCC(t, c, func(bcc *unstructured.Unstructured) error {
		return unstructured.SetNestedField(bcc.Object, value, "spec", "balloon", "cpuAllocation")
	})
}

// pfRestoreCPUAllocation restores the captured cpuAllocation, removing it if it was unset.
// It runs from t.Cleanup and reports with t.Errorf, so the callback finishes after
// recording a failure.
func pfRestoreCPUAllocation(t *testing.T, c client.Client, value string, present bool) {
	t.Helper()
	updateBCCReport(t, c, func(bcc *unstructured.Unstructured) error {
		if present {
			return unstructured.SetNestedField(bcc.Object, value, "spec", "balloon", "cpuAllocation")
		}
		unstructured.RemoveNestedField(bcc.Object, "spec", "balloon", "cpuAllocation")
		return nil
	}, func(verb string, err error) { t.Errorf("cleanup: restore cpuAllocation: %s: %v", verb, err) })
}

// pfWaitReservedForMode waits until availableResources reflects mode (exclude-cpuset for physical-cores, empty for
// hyperthreads) and returns the reserved count. Both fields are written in the same CR update.
func pfWaitReservedForMode(t *testing.T, c client.Client, node, mode string) (count int, spread bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	bpKey := client.ObjectKey{Name: "node." + node, Namespace: balloonsNS()} // resolved once, not per poll
	for {
		bp := newUnstructured(bpGVK)
		err := c.Get(context.Background(), bpKey, bp)
		if err == nil {
			avail, _, _ := unstructured.NestedString(bp.Object, "spec", "availableResources", "cpu")
			excluded := strings.HasPrefix(avail, "exclude-cpuset:")
			matched := (mode == "physical-cores" && excluded) ||
				(mode == "hyperthreads" && !excluded)
			if matched {
				return pfReservedCount(t, bp)
			}
		} else if !apierrors.IsNotFound(err) {
			t.Fatalf("get BalloonsPolicy node.%s: %v", node, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for node %s BalloonsPolicy to re-render in %q mode", node, mode)
		}
		time.Sleep(3 * time.Second)
	}
}

// pfReservedCount returns the reserved CPU count from reservedResources.cpu ("cpuset:<set>" or a bare count).
func pfReservedCount(t *testing.T, bp *unstructured.Unstructured) (count int, spread bool) {
	t.Helper()
	cpu, _, _ := unstructured.NestedString(bp.Object, "spec", "reservedResources", "cpu")
	if cpu == "" {
		t.Fatalf("BalloonsPolicy %s has no reservedResources.cpu", bp.GetName())
	}
	if set, ok := strings.CutPrefix(cpu, "cpuset:"); ok {
		return len(parseCPUSet(set)), true
	}
	n, err := strconv.Atoi(strings.TrimSpace(cpu))
	if err != nil {
		t.Fatalf("reservedResources.cpu = %q on %s: not a cpuset or count: %v", cpu, bp.GetName(), err)
	}
	return n, false
}

// pfWantPhysical lists the physical-cores counts a correct operator can render, given the hyperthreads count.
func pfWantPhysical(htCount, threads, numa int, htSpread, physSpread bool) []int {
	resolved := []int{htCount}
	if htSpread {
		resolved = nil
		for r := htCount; r > 0 && r > htCount-numa; r-- {
			if r%2 == 0 && pfApplied(r, numa, true) == htCount {
				resolved = append(resolved, r)
			}
		}
	}
	var want []int
	for _, r := range resolved {
		if w := pfApplied(pfEvenDown(r/threads), numa, physSpread); !slices.Contains(want, w) {
			want = append(want, w)
		}
	}
	return want
}

// pfApplied is the count a resolved size renders as: spread via balloons.ReservedPerNUMA when a cpuset, else bare.
func pfApplied(resolved, numa int, spread bool) int {
	if !spread || numa < 1 {
		return resolved
	}
	return balloons.ReservedPerNUMA(resolved, numa) * numa
}

// pfNUMANodes reads the node's numa-nodes label; 1 when missing, so no spread is modelled.
func pfNUMANodes(t *testing.T, c client.Client, node string) int {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, &n); err != nil {
		t.Fatalf("get node %s: %v", node, err)
	}
	numa, _ := strconv.Atoi(n.Labels[labelNumaNodes])
	return max(numa, 1)
}

// pfEvenDown rounds n down to even, mirroring ResolveReservedCPUs.
func pfEvenDown(n int) int {
	if n < 0 {
		return 0
	}
	return n - n%2
}

// TestReservedFootprintModelsTheSpread proves pfWantPhysical matches rendered counts and never accepts a dropped conversion. No cluster.
func TestReservedFootprintModelsTheSpread(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		htCount, threads, numa int
		htSpread, physSpread   bool
		accept                 []int
	}{
		// Four NUMA nodes: 12 hyperthreads halve to 6 cores, and the even per-NUMA spread accepts 4 or 8.
		{"4 NUMA", 12, 2, 4, true, true, []int{4, 8}},
		{"2 NUMA", 12, 2, 2, true, true, []int{6}},
		{"1 NUMA", 12, 2, 1, true, true, []int{6}},
		{"bare counts", 12, 2, 4, false, false, []int{6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pfWantPhysical(tc.htCount, tc.threads, tc.numa, tc.htSpread, tc.physSpread)
			slices.Sort(got)
			if !slices.Equal(got, tc.accept) {
				t.Errorf("pfWantPhysical = %v, want %v", got, tc.accept)
			}
			if slices.Contains(got, tc.htCount) {
				t.Errorf("accepts %d, the count a dropped ÷threadsPerCore conversion renders", tc.htCount)
			}
		})
	}

	// A count no conversion produces fails even when the hyperthreads count is a candidate.
	for _, tc := range []struct {
		name     string
		want     []int
		phys, ht int
		verdict  pfFootprintVerdict
	}{
		{"wrong count with an ambiguous hyperthreads count still fails", []int{4, 0}, 8, 4, pfFootprintWrong},
		{"observed count equal to an ambiguous hyperthreads count skips", []int{4, 0}, 4, 4, pfFootprintAmbiguous},
		{"converted count on 4 NUMA nodes passes", []int{4, 8}, 8, 12, pfFootprintOK},
		{"unconverted count fails", []int{4, 8}, 12, 12, pfFootprintWrong},
		{"count outside the candidates fails", []int{4, 8}, 6, 12, pfFootprintWrong},
	} {
		t.Run("judge/"+tc.name, func(t *testing.T) {
			if got := pfJudgeFootprint(tc.want, tc.phys, tc.ht); got != tc.verdict {
				t.Errorf("pfJudgeFootprint(%v, phys=%d, ht=%d) = %d, want %d", tc.want, tc.phys, tc.ht, got, tc.verdict)
			}
		})
	}
}
