// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package webhook

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// These let the e2e suite reuse the webhook's own init container shape and CPU checks;
// the build tag keeps them out of the manager binary.
var (
	InitContainerForE2E             = createInitContainer
	InjectReadinessCAVolumeForE2E   = injectReadinessCAVolume
	HasWholeCoreGuaranteedCPUForE2E = hasWholeCoreGuaranteedCPU
)

// LiveFreeFromNRTForE2E is the per-NUMA free room the capacity check reads from an NRT
// CR, before reservations are subtracted.
func LiveFreeFromNRTForE2E(nrt *unstructured.Unstructured, cpuInfo map[int]topology.CPUInfo, cpuAllocation string, density bool) (map[int]int, bool) {
	cache := topology.NewCache()
	cache.Set(nrt.GetName(), cpuInfo)
	return (&PodMutator{TopoCache: cache}).liveFreeFromNRT(nrt.GetName(), nrt, cpuAllocation, density)
}
