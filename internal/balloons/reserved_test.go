// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package balloons

import (
	"math"
	"strconv"
	"strings"
	"testing"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

func intPtr(n int) *int { return &n }

func TestResolveReservedCPUs(t *testing.T) {
	cases := []struct {
		name         string
		spec         *cpopv1alpha1.ReservedSizingSpec
		physicalCPUs int
		threads      int
		usage        int // measuredUsageCPUs (ignored unless spec.Measured is set)
		physCores    bool
		want         int
	}{
		// --- hyperthreads mode (or non-SMT): count is logical CPUs, unchanged ---
		// Explicit CPU count — fits comfortably on a 64-core node.
		{"explicit cpus:12 on 64c", &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(12)}, 64, 2, 0, false, 12},
		{"explicit cpus:8 on 64c", &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(8)}, 64, 2, 0, false, 8},
		{"explicit cpus:16 on 64c", &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(16)}, 64, 2, 0, false, 16},
		// cpus:20 exceeds 25% of 64 physical cores (16) → clamped to 16.
		{"explicit cpus hits 25% cap", &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(20)}, 64, 2, 0, false, 16},
		// cpus:20 fits on a 96-core node (25% = 24).
		{"explicit cpus:20 on 96c", &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(20)}, 96, 2, 0, false, 20},

		// Percent-based sizing: 25% of 64 = 16 (at the cap).
		{"percent:25 on 64c", &cpopv1alpha1.ReservedSizingSpec{Percent: intPtr(25)}, 64, 2, 0, false, 16},
		// percent below floor is raised to reservedMinCPUs (4).
		{"percent below floor", &cpopv1alpha1.ReservedSizingSpec{Percent: intPtr(1)}, 64, 2, 0, false, 4},
		// percent computes then hits the 25% cap.
		{"percent capped at 25%", &cpopv1alpha1.ReservedSizingSpec{Percent: intPtr(50)}, 64, 2, 0, false, 16},

		// nil spec → defaultReservedCPUs (12).
		{"nil spec → default 12 on 64c", nil, 64, 2, 0, false, 12},
		// Empty spec (both fields nil) → defaultReservedCPUs.
		{"empty spec → default 12", &cpopv1alpha1.ReservedSizingSpec{}, 64, 2, 0, false, 12},

		// Workload floor: on a 32-core node, must leave ≥16 → cpus:20 capped to
		// 32-16=16, then to 25% (8) → 8.
		{"workload floor + 25% cap", &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(20)}, 32, 2, 0, false, 8},

		// Even rounding: cpus:7 → 6.
		{"odd cpus round down", &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(7)}, 64, 2, 0, false, 6},

		// Below minimum is raised to 4 (then even) before caps.
		{"raise to minimum", &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(2)}, 64, 2, 0, false, 4},

		// Tiny node where no safe reservation exists → 0.
		{"tiny node → 0", nil, 16, 2, 0, false, 0},

		// --- physical-cores mode: clamp in logical units, then ÷ threads ---
		// default 12 logical → 6 physical cores on a 2-thread box.
		{"physcores default on 64c", nil, 64, 2, 0, true, 6},
		// explicit cpus:16 → 8.
		{"physcores cpus:16 on 64c", &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(16)}, 64, 2, 0, true, 8},
		// cpus:14 → 7 after ÷2 → round down to even → 6.
		{"physcores odd post-division rounds even", &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(14)}, 64, 2, 0, true, 6},
		// non-SMT (threadsPerCore 1) in physcores mode: no division.
		{"physcores non-SMT unchanged", nil, 64, 1, 0, true, 12},
		// 4-way SMT: cpus:16 → 4.
		{"physcores 4-way SMT", &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(16)}, 64, 4, 0, true, 4},

		// Percent is already physical cores: not divided again (16, not 8).
		{"physcores percent:25 not halved", &cpopv1alpha1.ReservedSizingSpec{Percent: intPtr(25)}, 64, 2, 0, true, 16},
		// percent:10 of 64 = 6 physical cores (already even), unchanged in physcores.
		{"physcores percent:10 unchanged", &cpopv1alpha1.ReservedSizingSpec{Percent: intPtr(10)}, 64, 2, 0, true, 6},

		// High SMT: cpus:4 on 4-way → 1 → even → 0, so the plugin default applies.
		{"physcores 4-way SMT floors to 0", &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(4)}, 64, 4, 0, true, 0},
		// 8-way SMT: cpus:16 → 16/8 = 2, stays 2 (still ≥ a whole even core).
		{"physcores 8-way SMT", &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(16)}, 128, 8, 0, true, 2},

		// --- measured mode: ceil(usage × (1+headroom/100)), raised to Min, then clamped ---
		// Cold start: no usage observed yet → resolves to Min (default 4).
		{"measured cold-start → min", &cpopv1alpha1.ReservedSizingSpec{Measured: &cpopv1alpha1.MeasuredSizingSpec{}}, 64, 2, 0, false, 4},
		// usage 8 + default 50% headroom → ceil(12) = 12.
		{"measured usage:8 +50% → 12", &cpopv1alpha1.ReservedSizingSpec{Measured: &cpopv1alpha1.MeasuredSizingSpec{}}, 64, 2, 8, false, 12},
		// usage 8 + 0% headroom tracks usage exactly → 8.
		{"measured usage:8 +0% → 8", &cpopv1alpha1.ReservedSizingSpec{Measured: &cpopv1alpha1.MeasuredSizingSpec{HeadroomPercent: intPtr(0)}}, 64, 2, 8, false, 8},
		// usage 2 + 50% = ceil(3) = 3, below explicit Min:6 → raised to 6.
		{"measured below min raised to min", &cpopv1alpha1.ReservedSizingSpec{Measured: &cpopv1alpha1.MeasuredSizingSpec{Min: intPtr(6)}}, 64, 2, 2, false, 6},
		// usage 20 + 50% = 30, over 25% of 64 (16) → clamped to 16.
		{"measured hits 25% cap", &cpopv1alpha1.ReservedSizingSpec{Measured: &cpopv1alpha1.MeasuredSizingSpec{}}, 64, 2, 20, false, 16},
		// ceil rounding: usage 5 + 50% = 7.5 → ceil 8.
		{"measured ceil rounding", &cpopv1alpha1.ReservedSizingSpec{Measured: &cpopv1alpha1.MeasuredSizingSpec{}}, 64, 2, 5, false, 8},
		// physical-cores mode: usage 16 + 50% = 24 logical → 25%-of-64 cap = 16 → ÷2 = 8.
		{"measured physcores ÷threads", &cpopv1alpha1.ReservedSizingSpec{Measured: &cpopv1alpha1.MeasuredSizingSpec{}}, 64, 2, 16, true, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveReservedCPUs(tc.spec, tc.physicalCPUs, tc.threads, tc.usage, tc.physCores)
			if got != tc.want {
				t.Errorf("ResolveReservedCPUs(%+v, phys=%d, threads=%d, physCores=%v) = %d, want %d",
					tc.spec, tc.physicalCPUs, tc.threads, tc.physCores, got, tc.want)
			}
			if got%2 != 0 {
				t.Errorf("result %d is not even", got)
			}
			if got > tc.physicalCPUs {
				t.Errorf("result %d exceeds physical cores %d", got, tc.physicalCPUs)
			}
		})
	}
}

// TestResolveReservedCpusPrecedence proves Cpus wins when both Cpus and Percent are set.
func TestResolveReservedCpusPrecedence(t *testing.T) {
	spec := &cpopv1alpha1.ReservedSizingSpec{Cpus: intPtr(10), Percent: intPtr(50)}
	got := ResolveReservedCPUs(spec, 64, 2, 0, false)
	if got != 10 {
		t.Errorf("Cpus must win over Percent when both set: got %d, want 10", got)
	}
}

// TestReservedPerNUMAMatchesReservedCPUSet proves ReservedPerNUMA equals the
// per-NUMA count of the real reservedCPUSet.
func TestReservedPerNUMAMatchesReservedCPUSet(t *testing.T) {
	cases := []struct {
		name      string
		topo      *topology.NodeTopology
		numaNodes int
		count     int
	}{
		// numaTopo has 4 cores per NUMA node and no hyperthreads.
		{"2 NUMA, count divides evenly", numaTopo(2), 2, 2},
		{"2 NUMA, odd count rounds up", numaTopo(2), 2, 3},
		{"3 NUMA, count rounds up", numaTopo(3), 3, 5},
		{"4 NUMA, count divides evenly", numaTopo(4), 4, 8},
		{"4 NUMA, count rounds up", numaTopo(4), 4, 10},
		// Hyperthreading must not change the count: one thread per core is reserved.
		{"2 NUMA hyperthreaded", wideTopo(), 2, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cset := reservedCPUSet(tc.topo, tc.count)
			if cset == "" {
				t.Fatalf("reservedCPUSet declined for count=%d; this case cannot pin a distribution", tc.count)
			}
			byNUMA := map[int]int{}
			byCore := map[int]int{}
			for _, cpu := range cpuSetIDs(t, cset) {
				info, ok := tc.topo.CPUs[cpu]
				if !ok {
					t.Fatalf("reserved cpuset %q names cpu %d, which is not in the topology", cset, cpu)
				}
				byNUMA[info.NUMA]++
				byCore[info.Core]++
			}
			if len(byNUMA) != tc.numaNodes {
				t.Fatalf("reserved cpuset %q covers %d NUMA nodes, want %d — the pool is not spread evenly",
					cset, len(byNUMA), tc.numaNodes)
			}
			// One thread per core, so the share needs no threads-per-core rescale.
			for core, n := range byCore {
				if n != 1 {
					t.Errorf("reserved cpuset %q names %d CPUs of physical core %d, want 1", cset, n, core)
				}
			}
			want := ReservedPerNUMA(tc.count, tc.numaNodes)
			if want <= 0 {
				t.Fatalf("ReservedPerNUMA reported %d for a pool of %d, so nothing would be subtracted", want, tc.count)
			}
			for numa, got := range byNUMA {
				if got != want {
					t.Errorf("reserved cpuset %q puts %d CPUs on NUMA %d, ReservedPerNUMA says %d",
						cset, got, numa, want)
				}
			}
		})
	}
}

// cpuSetIDs expands a compact cpuset string ("0-1,4-5") into its CPU ids.
func cpuSetIDs(t *testing.T, cset string) []int {
	t.Helper()
	var ids []int
	for _, part := range strings.Split(cset, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		first, err := strconv.Atoi(lo)
		if err != nil {
			t.Fatalf("malformed cpuset %q", cset)
		}
		last := first
		if isRange {
			if last, err = strconv.Atoi(hi); err != nil {
				t.Fatalf("malformed cpuset %q", cset)
			}
		}
		for cpu := first; cpu <= last; cpu++ {
			ids = append(ids, cpu)
		}
	}
	return ids
}

// TestReservedPerNUMA covers absent, nonsensical and overflow-sized pools.
func TestReservedPerNUMA(t *testing.T) {
	cases := []struct {
		name                    string
		reservedCPUs, numaNodes int
		want                    int
	}{
		// No pool or no NUMA count → nothing to subtract.
		{"no pool", 0, 2, 0},
		{"negative pool", -8, 2, 0},
		{"no numa count", 16, 0, 0},
		// Even split, rounded up, matching reservedCPUSet's own footprint.
		{"16 over 2 NUMA", 16, 2, 8},
		{"16 over 4 NUMA", 16, 4, 4},
		{"14 over 4 NUMA rounds up", 14, 4, 4},
		{"1 over 8 NUMA rounds up", 1, 8, 1},
		// Would overflow the (a+b-1) ceil idiom; must not wrap negative.
		{"saturating ceil", math.MaxInt, 2, math.MaxInt/2 + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReservedPerNUMA(tc.reservedCPUs, tc.numaNodes)
			if got != tc.want {
				t.Errorf("ReservedPerNUMA(%d, %d) = %d, want %d",
					tc.reservedCPUs, tc.numaNodes, got, tc.want)
			}
			if got < 0 {
				t.Errorf("ReservedPerNUMA returned %d; a negative share would inflate capacity", got)
			}
		})
	}
}
