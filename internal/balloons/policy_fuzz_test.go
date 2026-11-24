// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package balloons

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	nripolicy "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// balloonDefNameRe is the lowercase DNS-safe set balloon def names must stay inside.
var balloonDefNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// spanBalloonNameRe is the set of names the webhook can stamp on a pod.
var spanBalloonNameRe = regexp.MustCompile(`^balloon-span[1248](-colocate|-spread)?$`)

// maxFuzzNUMAs bounds the NUMA-node count: the generator's search is combinatorial.
const maxFuzzNUMAs = 16

// maxFuzzCPUSetWidth bounds the ids one rendered cpuset range is expanded to when
// checking the emitted policy.
const maxFuzzCPUSetWidth = 1 << 16

// FuzzNodePolicyFromProbeOutput fuzzes probe parsing plus policy rendering with
// reserved-sizing knobs; both inputs are untrusted.
func FuzzNodePolicyFromProbeOutput(f *testing.F) {
	f.Add(`NUMA node(s): 2
CPU(s): 128
Thread(s) per core: 2
PKGNODE node=0 pkg=0
PKGNODE node=1 pkg=1
MEMNODE node=0 kb=131072000
MEMNODE node=1 kb=131072000
CPUINFO cpu=0 numa=0 core=0
CPUINFO cpu=64 numa=0 core=0
CPUINFO cpu=1 numa=1 core=1
CPUINFO cpu=65 numa=1 core=1`, 12, 0, 0, true, true)
	f.Add("NUMA node(s): 1\nCPU(s): 8\nPKGNODE node=0 pkg=0\nCPUINFO cpu=0 numa=0 core=0\n", 0, 100, 0, false, false)
	f.Add("NUMA node(s): 4\nCPU(s): 256\nThread(s) per core: 2\n", -1, -1, -1, true, false)
	f.Add("NUMA node(s): 8\nCPU(s): 9223372036854775807\nThread(s) per core: 1\n", 9223372036854775807, 100, 9223372036854775807, true, true)
	f.Add("", 4, 25, 4, false, true)

	f.Fuzz(func(t *testing.T, probe string, reservedCPUs, reservedPercent, measuredUsage int, physicalCores, density bool) {
		topo, err := topology.ParseProbeOutput(probe)
		if err != nil {
			return
		}
		if len(topo.CPUs) > 4096 || numaCount(topo) > maxFuzzNUMAs {
			return
		}

		cpuAllocation := CPUAllocationPhysicalCores
		if !physicalCores {
			cpuAllocation = "hyperthreads"
		}
		htSiblingPolicy := HTSiblingPolicyExclude
		if density {
			htSiblingPolicy = HTSiblingPolicyReserved
		}

		// One sizing source at a time, as the CRD's CEL rule enforces.
		var spec *cpopv1alpha1.ReservedSizingSpec
		switch {
		case reservedCPUs > 0:
			spec = &cpopv1alpha1.ReservedSizingSpec{Cpus: &reservedCPUs}
		case reservedPercent > 0:
			spec = &cpopv1alpha1.ReservedSizingSpec{Percent: &reservedPercent}
		case measuredUsage != 0:
			spec = &cpopv1alpha1.ReservedSizingSpec{Measured: &cpopv1alpha1.MeasuredSizingSpec{}}
		}

		reserved := ResolveReservedCPUs(spec, topo.PhysicalCPUs, topo.ThreadsPerCore, measuredUsage, physicalCores)
		if reserved < 0 {
			t.Fatalf("ResolveReservedCPUs = %d, negative", reserved)
		}
		if topo.PhysicalCPUs > 0 && reserved > topo.PhysicalCPUs {
			t.Fatalf("ResolveReservedCPUs = %d, above the node's %d physical cores", reserved, topo.PhysicalCPUs)
		}

		defs := GenerateBalloonDefs(topo.PkgNode, topo.MaxSpan, cpuAllocation)
		seen := map[string]bool{}
		for _, d := range defs {
			if !balloonDefNameRe.MatchString(d.Name) {
				t.Fatalf("generated balloon name %q is not a DNS-safe token", d.Name)
			}
			if seen[d.Name] {
				// Duplicate def names make the policy ambiguous.
				t.Fatalf("generated balloon %q twice", d.Name)
			}
			if d.MinCpus < 0 || d.MaxCpus < 0 {
				t.Fatalf("balloon %q sized %d..%d", d.Name, d.MinCpus, d.MaxCpus)
			}
			if strings.HasPrefix(d.Name, "balloon-span") && !spanBalloonNameRe.MatchString(d.Name) {
				t.Fatalf("generated span balloon %q is not a name the webhook can stamp", d.Name)
			}
			// Components must precede their composite; checked before marking this def seen.
			for _, c := range d.Components {
				if !seen[c.DefName] {
					t.Fatalf("balloon %q references component %q, which no earlier def defines", d.Name, c.DefName)
				}
			}
			seen[d.Name] = true
		}

		policy := RenderNodePolicy("node", DefaultBalloonsNamespace, topo, defs,
			reserved, cpuAllocation, htSiblingPolicy, nil)
		if policy == nil {
			t.Fatal("RenderNodePolicy returned no policy")
		}

		// The reserved balloon must be pinned to exactly the reserved cpuset.
		amount := string(policy.Spec.Config.ReservedResources[nripolicy.CPU])
		size := reservedAmountSize(t, amount)
		for _, d := range policy.Spec.Config.BalloonDefs {
			if d.Name != "reserved" {
				continue
			}
			if d.MinCpus != d.MaxCpus {
				t.Fatalf("reserved balloon sized %d..%d, not pinned", d.MinCpus, d.MaxCpus)
			}
			if size > 0 && d.MinCpus != size {
				t.Fatalf("reserved balloon pinned at %d but the policy reserves %d CPUs (%q)", d.MinCpus, size, amount)
			}
		}

		// Excluded siblings and the reserved cpuset must not overlap.
		if excl, ok := policy.Spec.Config.AvailableResources[nripolicy.CPU]; ok {
			excluded := strings.TrimPrefix(string(excl), "exclude-cpuset:")
			if cset, isSet := strings.CutPrefix(amount, "cpuset:"); isSet {
				for _, cpu := range parseFuzzCPUSet(cset) {
					for _, ex := range parseFuzzCPUSet(excluded) {
						if cpu == ex {
							t.Fatalf("reserved cpuset %q names CPU %d, excluded from the pool by %q", cset, cpu, excl)
						}
					}
				}
			}
		}
	})
}

func numaCount(topo *topology.NodeTopology) int {
	ids := map[int]bool{}
	for _, nodes := range topo.PkgNode {
		for _, n := range nodes {
			ids[n] = true
		}
	}
	for _, info := range topo.CPUs {
		ids[info.NUMA] = true
	}
	return len(ids)
}

// reservedAmountSize returns the CPU count a rendered reservedResources amount
// names: the size of a "cpuset:" list, or the bare count.
func reservedAmountSize(t *testing.T, amount string) int {
	t.Helper()
	if cset, ok := strings.CutPrefix(amount, "cpuset:"); ok {
		return len(parseFuzzCPUSet(cset))
	}
	n, err := strconv.Atoi(amount)
	if err != nil {
		t.Fatalf("reservedResources.cpu = %q, neither a cpuset nor a count", amount)
	}
	return n
}

// parseFuzzCPUSet expands a rendered cpuset into ids, skipping implausibly wide ranges.
func parseFuzzCPUSet(cset string) []int {
	var out []int
	if cset == "" {
		return nil
	}
	for _, part := range strings.Split(cset, ",") {
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			l, err1 := strconv.Atoi(lo)
			h, err2 := strconv.Atoi(hi)
			if err1 != nil || err2 != nil || h < l || h-l >= maxFuzzCPUSetWidth {
				continue
			}
			for c := l; c <= h; c++ {
				out = append(out, c)
			}
			continue
		}
		if c, err := strconv.Atoi(part); err == nil {
			out = append(out, c)
		}
	}
	return out
}

// FuzzPackageTopologyLabel proves any label string parses safely and round-trips label-safe.
func FuzzPackageTopologyLabel(f *testing.F) {
	f.Add("1_1", 2, "")
	f.Add("2_2", 4, "-colocate")
	f.Add("8", 8, "-spread")
	f.Add("0_1", 1, "x")
	f.Add("_", 3, "")
	f.Add("99999999999999999999_1", 8, "")
	f.Add("9223372036854775807_9223372036854775807", 8, "")
	f.Add("16_16_16", 8, "")
	f.Add("9", 8, "")
	f.Add("8_8", 8, "-colocate")
	// Spans {1,2,8} have a hole at 4, exercising the no-gap check.
	f.Add("2_3_3", 8, "")
	f.Add("", 0, "")

	f.Fuzz(func(t *testing.T, label string, span int, suffix string) {
		counts := ParsePackageTopology(label)
		for _, c := range counts {
			if c < 1 {
				t.Fatalf("ParsePackageTopology(%q) = %v, carries a non-positive count", label, counts)
			}
			// The span-8 search is C(perPackage, 8).
			if c > maxLabelNUMAsPerPackage {
				t.Fatalf("ParsePackageTopology(%q) = %v, carries %d NUMA nodes on one package, past the %d bound",
					label, counts, c, maxLabelNUMAsPerPackage)
			}
		}
		for i := 1; i < len(counts); i++ {
			if counts[i] < counts[i-1] {
				t.Fatalf("ParsePackageTopology(%q) = %v, not sorted", label, counts)
			}
		}

		rendered := FormatPackageTopology(counts)
		for _, r := range rendered {
			if (r < '0' || r > '9') && r != '_' {
				t.Fatalf("FormatPackageTopology(%v) = %q, not a valid label value", counts, rendered)
			}
		}
		if again := ParsePackageTopology(rendered); !equalCounts(again, counts) {
			t.Fatalf("round trip of %q changed the shape: %v -> %q -> %v", label, counts, rendered, again)
		}

		// A parsed shape stays inside the parser's bounds.
		if totalNUMAs(counts) > maxLabelNUMAs {
			t.Fatalf("ParsePackageTopology(%q) = %v, past the %d NUMA bound", label, counts, maxLabelNUMAs)
		}

		// The advertised max-span and every smaller span must be generated.
		if len(counts) > 0 {
			genCap := topology.MaxSpanForNUMA(totalNUMAs(counts))
			defs := GenerateBalloonDefs(syntheticPkgNode(counts), genCap, defaultCPUAllocationForGen)
			advertisable := MaxAdvertisableSpan(defs)
			for _, s := range generatedSpans {
				if s > advertisable {
					continue
				}
				if findDef(defs, spanBalloonPrefix+strconv.Itoa(s)) == nil {
					t.Fatalf("shape %v advertises span %d but emits no balloon-span%d", counts, advertisable, s)
				}
			}
		}

		// Variant candidacy must only ever claim a balloon the generator emits.
		if VariantEmittable(counts, span, suffix) {
			switch span {
			case 1, 2, 4, 8:
			default:
				t.Fatalf("VariantEmittable(%v, %d, %q) claims an unsupported span", counts, span, suffix)
			}
			switch suffix {
			case "", "-colocate", "-spread":
			default:
				t.Fatalf("VariantEmittable(%v, %d, %q) claims an unknown variant suffix", counts, span, suffix)
			}
		}
	})
}

// totalNUMAs sums per-package NUMA counts, saturating instead of wrapping.
func totalNUMAs(counts []int) int {
	total := 0
	for _, c := range counts {
		if c <= 0 || total > math.MaxInt-c {
			return math.MaxInt
		}
		total += c
	}
	return total
}

func equalCounts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
