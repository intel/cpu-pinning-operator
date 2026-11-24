// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// nrtPodReaders read one node's topology on a pod's behalf; podBuilders create the
// pods they read about.
var (
	nrtPodReaders = []string{"containerCPUSet", "cpuToCore"}
	podBuilders   = []string{"vllmPod", "ccPod", "chaosDeployment"}
)

// unboundNRTReaders returns the functions in src that build a pod and read a node's
// NRT for it but never touch NodeName or NodeSelector.
func unboundNRTReaders(t *testing.T, fset *token.FileSet, filename string, src any) []string {
	t.Helper()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	var bad []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var reads, builds, binds bool
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok {
					reads = reads || slices.Contains(nrtPodReaders, id.Name)
					builds = builds || slices.Contains(podBuilders, id.Name)
				}
			case *ast.SelectorExpr:
				binds = binds || x.Sel.Name == "NodeName" || x.Sel.Name == "NodeSelector"
			}
			return true
		})
		if reads && builds && !binds {
			bad = append(bad, fset.Position(fn.Pos()).String()+" "+fn.Name.Name)
		}
	}
	return bad
}

// TestPodNRTReadsBindThePod fails on a function that reads one node's NRT for a pod
// it never bound to that node: on a multi-node fleet the pod lands elsewhere.
func TestPodNRTReadsBindThePod(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob the e2e package: %v", err)
	}
	const anchor = "e2e_test.go" // defines containerCPUSet
	requireSourceScan(t, files, slices.Contains(files, anchor), anchor)
	fset := token.NewFileSet()
	var bad []string
	for _, f := range files {
		bad = append(bad, unboundNRTReaders(t, fset, f, nil)...)
	}
	if len(bad) > 0 {
		t.Fatalf("%d function(s) create a pod and read one node's NodeResourceTopology for it without binding it "+
			"to that node: set Spec.NodeName (or a hostname NodeSelector), or read the node from the pod's "+
			"Spec.NodeName once scheduled:\n\t%s", len(bad), strings.Join(bad, "\n\t"))
	}
}

// TestPodNRTReadsBindThePodCatchesTheShape proves the guard reports the unbound shape
// and not the bound one.
func TestPodNRTReadsBindThePodCatchesTheShape(t *testing.T) {
	const unbound = `package e2e
func TestX(t *testing.T) {
	node := balloonsEnabledNode(t, c)
	createAndCleanup(t, c, vllmPod("a", ns, "4", "4", 2))
	_ = containerCPUSet(t, c, node, ns, "a", "vllm")
}`
	const bound = `package e2e
func TestX(t *testing.T) {
	node, _ := widestSpanNode(t, c)
	p := vllmPod("a", ns, "4", "4", 2)
	p.Spec.NodeName = node
	createAndCleanup(t, c, p)
	_ = containerCPUSet(t, c, node, ns, "a", "vllm")
}`
	if got := unboundNRTReaders(t, token.NewFileSet(), "unbound.go", unbound); len(got) != 1 {
		t.Errorf("unbound pod + NRT read: guard reported %v, want exactly TestX", got)
	}
	if got := unboundNRTReaders(t, token.NewFileSet(), "bound.go", bound); len(got) != 0 {
		t.Errorf("bound pod + NRT read: guard reported %v, want nothing", got)
	}
}

// TestSaturationCountsOnlyCapacityDenials proves only the operator's capacity denial,
// not a quota rejection, counts as saturation overflow.
func TestSaturationCountsOnlyCapacityDenials(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errors.New(`admission webhook "pods.cpu-pinning-operator.intel.com" denied the request: ` +
			"cpu-pinning capacity check failed: the requested 4 cores do not fit"), true},
		{errors.New(`pods "e2e-sat-01" is forbidden: exceeded quota: compute, requested: cpu=4`), false},
		{nil, false},
	} {
		if got := satCapacityOverflow(tc.err); got != tc.want {
			t.Errorf("satCapacityOverflow(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
