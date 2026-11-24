// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package classification

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// node builds a Node; a zero GPU or CPU count omits that resource.
func node(labelSet map[string]string, gpuAllocatable, cpuCapacity int) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: labelSet}}
	if gpuAllocatable > 0 {
		n.Status.Allocatable = corev1.ResourceList{
			corev1.ResourceName(resourceNvidiaGPU): *resource.NewQuantity(int64(gpuAllocatable), resource.DecimalSI),
		}
	}
	if cpuCapacity > 0 {
		n.Status.Capacity = corev1.ResourceList{
			corev1.ResourceCPU: *resource.NewQuantity(int64(cpuCapacity), resource.DecimalSI),
		}
	}
	return n
}

// nodeWithGPUs builds a Node with NVIDIA and Intel-xe GPU quantities (0 = omit).
func nodeWithGPUs(labelSet map[string]string, nvidia, intelXe int) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: labelSet}}
	n.Status.Allocatable = corev1.ResourceList{}
	if nvidia > 0 {
		n.Status.Allocatable[corev1.ResourceName(resourceNvidiaGPU)] = *resource.NewQuantity(int64(nvidia), resource.DecimalSI)
	}
	if intelXe > 0 {
		n.Status.Allocatable[corev1.ResourceName(resourceIntelGPUXe)] = *resource.NewQuantity(int64(intelXe), resource.DecimalSI)
	}
	return n
}

func topo(amx bool, logicalCPUs int) *topology.NodeTopology {
	return &topology.NodeTopology{AMX: amx, LogicalCPUs: logicalCPUs}
}

func mustSelector(t *testing.T, ls *metav1.LabelSelector) labels.Selector {
	t.Helper()
	sel, err := metav1.LabelSelectorAsSelector(ls)
	if err != nil {
		t.Fatalf("bad selector: %v", err)
	}
	return sel
}

func TestClassifyNode_AutoRules(t *testing.T) {
	amxSel := mustSelector(t, &metav1.LabelSelector{MatchLabels: map[string]string{"cpu-inference": "true"}})
	gpuOnlySel := mustSelector(t, &metav1.LabelSelector{MatchLabels: map[string]string{"gpu-only": "true"}})

	tests := []struct {
		name      string
		node      *corev1.Node
		topo      *topology.NodeTopology
		spec      Spec
		wantClass string
		wantRule  string
	}{
		{
			name:      "gpu and amx -> mixed",
			node:      node(nil, 2, 0),
			topo:      topo(true, 128),
			wantClass: ClassMixedCPUAMXGPU,
			wantRule:  "gpu+amx",
		},
		{
			name:      "gpu without amx -> gpu-only",
			node:      node(nil, 1, 0),
			topo:      topo(false, 128),
			wantClass: ClassGPUOnly,
			wantRule:  "gpu-only",
		},
		{
			name:      "intel-xe gpu and amx -> mixed",
			node:      nodeWithGPUs(nil, 0, 1),
			topo:      topo(true, 128),
			wantClass: ClassMixedCPUAMXGPU,
			wantRule:  "gpu+amx",
		},
		{
			name:      "gpu+amx but gpuOnlySelector forces gpu-only",
			node:      node(map[string]string{"gpu-only": "true"}, 1, 0),
			topo:      topo(true, 128),
			spec:      Spec{GPUOnlySelector: gpuOnlySel},
			wantClass: ClassGPUOnly,
			wantRule:  "gpu+gpuOnlySelector",
		},
		{
			name:      "amx no gpu, cpu count above threshold -> cpu-amx",
			node:      node(nil, 0, 0),
			topo:      topo(true, 96),
			spec:      Spec{CPUAMXMinLogicalCPUs: 64},
			wantClass: ClassCPUAMX,
			wantRule:  "amx+cpuCount",
		},
		{
			name:      "amx no gpu, cpu count below threshold -> cpu-only",
			node:      node(nil, 0, 0),
			topo:      topo(true, 32),
			spec:      Spec{CPUAMXMinLogicalCPUs: 64},
			wantClass: ClassCPUOnly,
			wantRule:  "default",
		},
		{
			name:      "amx no gpu, cpu count exactly at threshold -> cpu-amx (boundary)",
			node:      node(nil, 0, 0),
			topo:      topo(true, 64),
			spec:      Spec{CPUAMXMinLogicalCPUs: 64},
			wantClass: ClassCPUAMX,
			wantRule:  "amx+cpuCount",
		},
		{
			name:      "amx no gpu, small node but amxNodeSelector matches -> cpu-amx",
			node:      node(map[string]string{"cpu-inference": "true"}, 0, 0),
			topo:      topo(true, 16),
			spec:      Spec{AMXSelector: amxSel, CPUAMXMinLogicalCPUs: 64},
			wantClass: ClassCPUAMX,
			wantRule:  "amx+amxSelector",
		},
		{
			name:      "no gpu no amx -> cpu-only",
			node:      node(nil, 0, 0),
			topo:      topo(false, 128),
			wantClass: ClassCPUOnly,
			wantRule:  "default",
		},
		{
			name:      "zero threshold normalizes to default 64",
			node:      node(nil, 0, 0),
			topo:      topo(true, 48),
			spec:      Spec{CPUAMXMinLogicalCPUs: 0},
			wantClass: ClassCPUOnly,
			wantRule:  "default",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotClass, reason := ClassifyNode(tc.node, tc.topo, tc.spec)
			if gotClass != tc.wantClass {
				t.Errorf("class = %q, want %q", gotClass, tc.wantClass)
			}
			if r, _ := reason["rule"].(string); r != tc.wantRule {
				t.Errorf("rule = %q, want %q", r, tc.wantRule)
			}
		})
	}
}

func TestClassifyNode_ManualOverride(t *testing.T) {
	label := DefaultOverrideLabel
	base := Spec{OverrideLabel: label, CPUAMXMinLogicalCPUs: 64}

	t.Run("valid override wins over auto class", func(t *testing.T) {
		// A GPU+AMX node would auto-classify mixed, but the operator forces gpu-only.
		n := node(map[string]string{label: ClassGPUOnly}, 1, 0)
		got, reason := ClassifyNode(n, topo(true, 128), base)
		if got != ClassGPUOnly {
			t.Fatalf("class = %q, want %q", got, ClassGPUOnly)
		}
		if r, _ := reason["rule"].(string); r != "manual-override" {
			t.Errorf("rule = %q, want manual-override", r)
		}
	})

	t.Run("override to cpu-amx honored when node has AMX", func(t *testing.T) {
		// Small AMX node that would auto-classify cpu-only, forced to cpu-amx.
		n := node(map[string]string{label: ClassCPUAMX}, 0, 0)
		got, _ := ClassifyNode(n, topo(true, 8), base)
		if got != ClassCPUAMX {
			t.Fatalf("class = %q, want %q", got, ClassCPUAMX)
		}
	})

	t.Run("override to AMX class rejected on non-AMX node, falls back to auto", func(t *testing.T) {
		n := node(map[string]string{label: ClassCPUAMX}, 0, 0)
		got, reason := ClassifyNode(n, topo(false, 128), base)
		if got != ClassCPUOnly {
			t.Fatalf("class = %q, want auto cpu-only", got)
		}
		if rejected, _ := reason["overrideRejected"].(bool); !rejected {
			t.Errorf("expected overrideRejected=true, reason=%v", reason)
		}
	})

	t.Run("override to mixed rejected on GPU node without AMX, falls back to gpu-only", func(t *testing.T) {
		n := node(map[string]string{label: ClassMixedCPUAMXGPU}, 1, 0)
		got, reason := ClassifyNode(n, topo(false, 128), base)
		if got != ClassGPUOnly {
			t.Fatalf("class = %q, want auto gpu-only", got)
		}
		if rejected, _ := reason["overrideRejected"].(bool); !rejected {
			t.Errorf("expected overrideRejected=true")
		}
	})

	t.Run("unknown class rejected, falls back to auto", func(t *testing.T) {
		n := node(map[string]string{label: "nonsense"}, 0, 0)
		got, reason := ClassifyNode(n, topo(false, 128), base)
		if got != ClassCPUOnly {
			t.Fatalf("class = %q, want auto cpu-only", got)
		}
		if rejected, _ := reason["overrideRejected"].(bool); !rejected {
			t.Errorf("expected overrideRejected=true for unknown class")
		}
	})

	t.Run("empty override label means no override honored", func(t *testing.T) {
		// OverrideLabel unset: a node carrying the default label is ignored.
		n := node(map[string]string{label: ClassGPUOnly}, 0, 0)
		got, reason := ClassifyNode(n, topo(false, 128), Spec{})
		if got != ClassCPUOnly {
			t.Fatalf("class = %q, want cpu-only (override not consulted)", got)
		}
		if _, ok := reason["overrideRequested"]; ok {
			t.Errorf("override should not have been consulted")
		}
	})
}

func TestGPUDetection_Sources(t *testing.T) {
	tests := []struct {
		name  string
		node  *corev1.Node
		want  int
		isGPU bool
	}{
		{"allocatable", node(nil, 4, 0), 4, true},
		{"intel-xe allocatable", nodeWithGPUs(nil, 0, 2), 2, true},
		{"nvidia + intel-xe allocatable sum", nodeWithGPUs(nil, 3, 2), 5, true},
		{"nfd count label", node(map[string]string{labelNvidiaGPUCount: "3"}, 0, 0), 3, true},
		{"nvidia present label (count unknown -> 1)", node(map[string]string{labelNvidiaGPUPresent: "true"}, 0, 0), 1, true},
		{"nfd pci label (count unknown -> 1)", node(map[string]string{labelNFDPCINvidia: "true"}, 0, 0), 1, true},
		{"no gpu", node(nil, 0, 0), 0, false},
		{"allocatable wins over label", node(map[string]string{labelNvidiaGPUCount: "1"}, 8, 0), 8, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gpuCount(tc.node); got != tc.want {
				t.Errorf("gpuCount = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestAMXDetection_ProbeAndNFD(t *testing.T) {
	// Probe result is authoritative.
	if !hasAMX(node(nil, 0, 0), topo(true, 8)) {
		t.Error("expected AMX from probe")
	}
	// NFD fallback when probe reports no AMX (or no topology).
	for _, l := range []string{labelNFDAMXBF16, labelNFDAMXINT8, labelNFDAMXTile} {
		if !hasAMX(node(map[string]string{l: "true"}, 0, 0), topo(false, 8)) {
			t.Errorf("expected AMX from NFD label %s", l)
		}
		if !hasAMX(node(map[string]string{l: "true"}, 0, 0), nil) {
			t.Errorf("expected AMX from NFD label %s with nil topology", l)
		}
	}
	if hasAMX(node(nil, 0, 0), topo(false, 8)) {
		t.Error("expected no AMX")
	}
}

func TestLogicalCPUCount_Fallback(t *testing.T) {
	// Probe count preferred.
	if got := logicalCPUCount(node(nil, 0, 200), topo(true, 128)); got != 128 {
		t.Errorf("logicalCPUCount = %d, want 128 (probe)", got)
	}
	// Fall back to node capacity when no probe reading.
	if got := logicalCPUCount(node(nil, 0, 200), nil); got != 200 {
		t.Errorf("logicalCPUCount = %d, want 200 (capacity)", got)
	}
	// AMX threshold honored from capacity fallback (no probe topology).
	got, reason := ClassifyNode(node(map[string]string{labelNFDAMXTile: "true"}, 0, 200), nil, Spec{CPUAMXMinLogicalCPUs: 64})
	if got != ClassCPUAMX {
		t.Errorf("class = %q, want cpu-amx via capacity fallback (reason=%v)", got, reason)
	}
}

func TestClassifyNode_NilTopology(t *testing.T) {
	// Missing topology must not panic: classification still runs off node signals.
	got, _ := ClassifyNode(node(nil, 2, 0), nil, Spec{})
	if got != ClassGPUOnly {
		t.Errorf("class = %q, want gpu-only", got)
	}
}
