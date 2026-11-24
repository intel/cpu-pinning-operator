// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package balloons

import (
	"math"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
)

// Reserved CPU pool sizing. Size comes from ReservedSizingSpec, then clamped
// against the node's physical core count.

const (
	// defaultReservedCPUs is used when no ReservedSizingSpec is set.
	defaultReservedCPUs = 12

	// reservedMinCPUs is the floor: fewer cannot host system pods.
	reservedMinCPUs = 4
	// reservedMaxPercent caps the reserved pool at this % of physical cores.
	reservedMaxPercent = 25
	// reservedMinWorkloadCPUs is the minimum that must remain for workloads.
	reservedMinWorkloadCPUs = 16

	// defaultMeasuredHeadroomPercent is the slack above observed usage when
	// spec.reserved.measured sets no HeadroomPercent. Matches the API default.
	defaultMeasuredHeadroomPercent = 50
)

// ResolveReservedCPUs sizes the reserved pool, clamped to 25% of physical cores
// with reservedMinWorkloadCPUs left over. spec.Percent is already physical-core
// relative, so it is not divided by threadsPerCore. 0 means no safe size fits.
func ResolveReservedCPUs(spec *cpopv1alpha1.ReservedSizingSpec, physicalCPUs, threadsPerCore, measuredUsageCPUs int, physicalCoresMode bool) int {
	reserved, logicalBaseline := reservedFromSpec(spec, physicalCPUs, measuredUsageCPUs)

	if reserved < reservedMinCPUs {
		reserved = reservedMinCPUs
	}

	if maxByPercent := physicalCPUs * reservedMaxPercent / 100; reserved > maxByPercent {
		reserved = maxByPercent
	}

	if maxByWorkload := physicalCPUs - reservedMinWorkloadCPUs; reserved > maxByWorkload {
		reserved = maxByWorkload
	}

	// Round down: rounding up can re-breach the caps above.
	reserved -= reserved % 2

	if reserved < 0 {
		reserved = 0
	}

	// Logical baselines must be converted; Percent is already physical cores.
	if physicalCoresMode && logicalBaseline && threadsPerCore > 1 && reserved > 0 {
		reserved /= threadsPerCore
		reserved -= reserved % 2
	}

	return reserved
}

// ReservedPerNUMA returns the reserved pool's per-NUMA share: even (reservedCPUSet
// takes the same count from each node) and rounded up (as reservedCPUSet rounds
// the footprint). Without a reservedCPUSet the split is an assumption.
//
// One thread per core is reserved, so the share needs no threads-per-core rescale.
// In density mode LabelReservedCPUs is the sibling count, not a pool size; callers
// must detect that (see MaxCredibleReservedPerNUMA).
func ReservedPerNUMA(reservedCPUs, numaNodes int) int {
	if reservedCPUs <= 0 || numaNodes <= 0 {
		return 0
	}
	// Ceil division without the (a+b-1) idiom, which could wrap on a hand-edited label.
	perNUMA := reservedCPUs / numaNodes
	if reservedCPUs%numaNodes != 0 {
		perNUMA++
	}
	return perNUMA
}

// MaxCredibleReservedPerNUMA is the largest per-NUMA share a resolved pool can have
// (reservedMaxPercent of physical cores, which ResolveReservedCPUs always clamps
// to), so a larger LabelReservedCPUs value is a density-mode sibling count. A
// sibling count under the bound is indistinguishable.
func MaxCredibleReservedPerNUMA(physicalCPUs, numaNodes int) int {
	return ReservedPerNUMA(maxReservedByPercent(physicalCPUs), numaNodes)
}

// maxReservedByPercent is the reservedMaxPercent ceiling ResolveReservedCPUs clamps
// every resolved pool to, saturating rather than wrapping on an absurd core count.
func maxReservedByPercent(physicalCPUs int) int {
	if physicalCPUs <= 0 {
		return 0
	}
	if physicalCPUs > math.MaxInt/reservedMaxPercent {
		return math.MaxInt
	}
	return physicalCPUs * reservedMaxPercent / 100
}

// reservedFromSpec returns the pre-clamp count and whether it is in logical CPUs
// (true for default/Cpus/Measured) rather than physical cores (Percent).
func reservedFromSpec(spec *cpopv1alpha1.ReservedSizingSpec, physicalCPUs, measuredUsageCPUs int) (reserved int, logicalBaseline bool) {
	if spec == nil {
		return defaultReservedCPUs, true
	}
	if spec.Cpus != nil {
		return *spec.Cpus, true
	}
	if spec.Percent != nil {
		return physicalCPUs * *spec.Percent / 100, false
	}
	if spec.Measured != nil {
		return measuredReserved(spec.Measured, measuredUsageCPUs), true
	}
	return defaultReservedCPUs, true
}

// measuredReserved is ceil(usage * (1 + headroom/100)), raised to Min. Min is
// also the cold-start value when usage is 0.
func measuredReserved(m *cpopv1alpha1.MeasuredSizingSpec, measuredUsageCPUs int) int {
	headroom := defaultMeasuredHeadroomPercent
	if m.HeadroomPercent != nil {
		headroom = *m.HeadroomPercent
	}
	min := reservedMinCPUs
	if m.Min != nil {
		min = *m.Min
	}

	if measuredUsageCPUs < 0 {
		measuredUsageCPUs = 0
	}
	// ceil(usage * (100 + headroom) / 100) with integer math.
	sized := (measuredUsageCPUs*(100+headroom) + 99) / 100
	if sized < min {
		sized = min
	}
	return sized
}
