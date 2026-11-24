// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package classification assigns a node one of four hardware classes from its
// GPU presence, AMX support and logical-CPU count.
package classification

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// Node class names. They are stable label values; other tooling may match on them.
const (
	// ClassMixedCPUAMXGPU is a node with both a GPU and AMX support.
	ClassMixedCPUAMXGPU = "mixed-cpu-amx-gpu"
	// ClassCPUAMX is a non-GPU node with AMX support intended for CPU inference.
	ClassCPUAMX = "cpu-amx"
	// ClassGPUOnly is a GPU node without AMX, or one forced there by the GPU-only selector.
	ClassGPUOnly = "gpu-only"
	// ClassCPUOnly is the default class, including AMX nodes too small for cpu-amx.
	ClassCPUOnly = "cpu-only"

	// DefaultCPUAMXMinLogicalCPUs is the logical-CPU count at which a non-GPU AMX node qualifies for cpu-amx.
	DefaultCPUAMXMinLogicalCPUs = 64
)

// DefaultOverrideLabel forces a node's class when ClassificationSpec.OverrideLabel is unset.
const DefaultOverrideLabel = cpopv1alpha1.LabelPrefix + "node-class-override"

// GPU and AMX detection signals; NFD labels are only a fallback.
const (
	resourceNvidiaGPU = "nvidia.com/gpu"
	// Intel discrete GPUs on the Xe driver (Flex / Max / Arc).
	resourceIntelGPUXe = "gpu.intel.com/xe"

	labelNFDPCINvidia     = "feature.node.kubernetes.io/pci-10de.present"
	labelNvidiaGPUPresent = "nvidia.com/gpu.present"
	labelNvidiaGPUCount   = "nvidia.com/gpu.count"

	labelNFDAMXBF16 = "feature.node.kubernetes.io/cpu-cpuid.AMX_BF16"
	labelNFDAMXINT8 = "feature.node.kubernetes.io/cpu-cpuid.AMX_INT8"
	labelNFDAMXTile = "feature.node.kubernetes.io/cpu-cpuid.AMX_TILE"
)

// Spec is api/v1alpha1.ClassificationSpec with selectors pre-parsed once at reconcile time.
type Spec struct {
	// OverrideLabel is the node label that forces a class; empty disables overrides.
	OverrideLabel string
	// AMXSelector marks explicit AMX CPU-inference nodes. nil never matches.
	AMXSelector labels.Selector
	// GPUOnlySelector forces matching GPU nodes into gpu-only. nil never matches.
	GPUOnlySelector labels.Selector
	// CPUAMXMinLogicalCPUs is the cpu-amx threshold; <= 0 means DefaultCPUAMXMinLogicalCPUs.
	CPUAMXMinLogicalCPUs int
}

// ClassifyNode returns a node's class and a reason map of the signals and rule used.
// Priority:
//
//	manual override (validated) → GPU + gpuOnlySelector → GPU + AMX → GPU-only →
//	AMX + amxSelector → AMX + CPU-count → default cpu-only
//
// An override naming an unknown class, or an AMX class on a non-AMX node, is ignored.
func ClassifyNode(node *corev1.Node, topo *topology.NodeTopology, spec Spec) (string, map[string]any) {
	gpuCount := gpuCount(node)
	hasGPU := gpuCount > 0
	amx := hasAMX(node, topo)
	logicalCPUs := logicalCPUCount(node, topo)

	reason := map[string]any{
		"gpu":         hasGPU,
		"gpuCount":    gpuCount,
		"amx":         amx,
		"logicalCPUs": logicalCPUs,
	}

	auto, autoRule := autoClassify(node, spec, hasGPU, amx, logicalCPUs)

	if spec.OverrideLabel != "" {
		if requested, ok := node.Labels[spec.OverrideLabel]; ok && requested != "" {
			reason["overrideRequested"] = requested
			if validOverride(requested, amx) {
				reason["rule"] = "manual-override"
				return requested, reason
			}

			reason["overrideRejected"] = true
		}
	}

	reason["rule"] = autoRule
	return auto, reason
}

// autoClassify runs the automatic (non-override) rules in priority order.
func autoClassify(node *corev1.Node, spec Spec, hasGPU, amx bool, logicalCPUs int) (class, rule string) {
	set := labels.Set(node.Labels)
	if hasGPU {
		if spec.GPUOnlySelector != nil && spec.GPUOnlySelector.Matches(set) {
			return ClassGPUOnly, "gpu+gpuOnlySelector"
		}
		if amx {
			return ClassMixedCPUAMXGPU, "gpu+amx"
		}
		return ClassGPUOnly, "gpu-only"
	}
	if amx {
		if spec.AMXSelector != nil && spec.AMXSelector.Matches(set) {
			return ClassCPUAMX, "amx+amxSelector"
		}
		min := spec.CPUAMXMinLogicalCPUs
		if min <= 0 {
			min = DefaultCPUAMXMinLogicalCPUs
		}
		if logicalCPUs >= min {
			return ClassCPUAMX, "amx+cpuCount"
		}
	}
	return ClassCPUOnly, "default"
}

// validOverride reports whether a manually-requested class is honored: it must
// be a known class, and an AMX class is only honored on a node that supports AMX.
func validOverride(requested string, amx bool) bool {
	if !IsKnownClass(requested) {
		return false
	}
	if IsAMXClass(requested) && !amx {
		return false
	}
	return true
}

// IsKnownClass reports whether s is one of the four defined node classes.
func IsKnownClass(s string) bool {
	switch s {
	case ClassMixedCPUAMXGPU, ClassCPUAMX, ClassGPUOnly, ClassCPUOnly:
		return true
	}
	return false
}

// IsAMXClass reports whether a class requires AMX support.
func IsAMXClass(s string) bool {
	return s == ClassCPUAMX || s == ClassMixedCPUAMXGPU
}

// gpuCount prefers allocatable GPUs, then the NFD count label, then 1 if only a presence label is set.
func gpuCount(node *corev1.Node) int {
	if node == nil {
		return 0
	}
	var alloc int64
	for _, r := range []string{resourceNvidiaGPU, resourceIntelGPUXe} {
		if q, ok := node.Status.Allocatable[corev1.ResourceName(r)]; ok {
			if v := q.Value(); v > 0 {
				alloc += v
			}
		}
	}
	if alloc > 0 {
		return int(alloc)
	}
	if s := node.Labels[labelNvidiaGPUCount]; s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	if node.Labels[labelNvidiaGPUPresent] == "true" || node.Labels[labelNFDPCINvidia] == "true" {
		return 1
	}
	return 0
}

// hasAMX checks the operator's own probe first, then NFD CPUID labels.
func hasAMX(node *corev1.Node, topo *topology.NodeTopology) bool {
	if topo != nil && topo.AMX {
		return true
	}
	if node == nil {
		return false
	}
	for _, l := range []string{labelNFDAMXBF16, labelNFDAMXINT8, labelNFDAMXTile} {
		if node.Labels[l] == "true" {
			return true
		}
	}
	return false
}

// logicalCPUCount prefers the topology probe and falls back to node capacity.
func logicalCPUCount(node *corev1.Node, topo *topology.NodeTopology) int {
	if topo != nil && topo.LogicalCPUs > 0 {
		return topo.LogicalCPUs
	}
	if node != nil {
		if q, ok := node.Status.Capacity[corev1.ResourceCPU]; ok {
			if v := q.Value(); v > 0 {
				return int(v)
			}
		}
	}
	return 0
}
