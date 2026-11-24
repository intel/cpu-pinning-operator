// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// withPackageTopology sets the per-package NUMA-count label on a node.
func withPackageTopology(n *corev1.Node, v string) *corev1.Node {
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	n.Labels[controller.LabelPackageTopology] = v
	return n
}

func TestVariantSuffix(t *testing.T) {
	cases := map[string]string{
		"balloon-span2":          "",
		"balloon-span2-colocate": "-colocate",
		"balloon-span2-spread":   "-spread",
		"balloon-span1":          "",
		"balloon-span4-spread":   "-spread",
		"balloon-device":         "", // device balloon has no coLocate variant
	}
	for name, want := range cases {
		if got := variantSuffix(name); got != want {
			t.Errorf("variantSuffix(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestVariantChecker(t *testing.T) {
	lbl := func(pkgTopo string) map[string]string {
		m := map[string]string{}
		if pkgTopo != "" {
			m[controller.LabelPackageTopology] = pkgTopo
		}
		return m
	}

	if !variantChecker(2, "")(lbl("")) {
		t.Error(`suffix "" must pass regardless of label`)
	}
	// "1_1" (two packages, one NUMA each) supports -spread, not -colocate.
	if !variantChecker(2, "-spread")(lbl("1_1")) {
		t.Error(`"1_1" should support span2-spread`)
	}
	if variantChecker(2, "-colocate")(lbl("1_1")) {
		t.Error(`"1_1" must NOT support span2-colocate`)
	}

	if !variantChecker(2, "-colocate")(lbl("2_2")) || !variantChecker(2, "-spread")(lbl("2_2")) {
		t.Error(`"2_2" should support both span2 variants`)
	}

	if variantChecker(2, "-spread")(lbl("2")) {
		t.Error(`"2" must NOT support span2-spread`)
	}
	// Missing label: variant unsupported.
	if variantChecker(2, "-spread")(lbl("")) {
		t.Error("missing package-topology label must make a variant unsupported")
	}
	if variantChecker(2, "-spread")(lbl("x_y")) {
		t.Error("garbage package-topology label must make a variant unsupported")
	}
}

// TestAdmitWithHeadroomVariantFiltersSteering: steering requires a node whose package
// topology can emit the requested -spread variant, not just one with CPU room.
func TestAdmitWithHeadroomVariantFiltersSteering(t *testing.T) {
	ctx := context.Background()
	topo := topology.NewCache()
	topo.Set("spreadable", simpleTopo()) // 2 NUMA / 2 cores each, CPUs free
	topo.Set("onepkg", simpleTopo())
	m := newMutatorWithTopo(topo,
		withPackageTopology(withMaxSpan(topoNode("spreadable", 4, 2), 2), "1_1"), // 2 packages → spread OK
		withPackageTopology(withMaxSpan(topoNode("onepkg", 4, 2), 2), "2"),       // 1 package → no spread
		nrtObj("spreadable"), nrtObj("onepkg"),
	)
	m.Reservations = NewReservationStore(0)

	pod := unscheduledPod("p", "2")
	v := m.admitWithHeadroom(ctx, pod, 2, 2, 0, true, "-spread", "physical-cores", packingSpread, nil, reservePolicy{}, logr.Discard())
	if !v.sawLive || !v.fits {
		t.Fatalf("verdict = %+v, want sawLive=true fits=true", v)
	}
	req := affinityRequiredValues(pod)
	if len(req) != 1 || req[0] != "spreadable" {
		t.Errorf("REQUIRED hostnames = %v, want [spreadable] (onepkg can't emit -spread)", req)
	}
}
