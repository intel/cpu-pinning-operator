// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// fleetWideNodeLists names the functions allowed to list Nodes without the
// provisioned selector, each with why it must see nodes outside the pinning pool.
var fleetWideNodeLists = map[string]string{
	"enqueueAllNodes": "adopts a node that carries no operator label yet",
}

// TestNodeListsCarryTheProvisionedSelector requires every Node list in this
// package's source to pass provisionedNodesOnly() unless its function is in
// fleetWideNodeLists; a behavioural test cannot catch a new unscoped call site.
func TestNodeListsCarryTheProvisionedSelector(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing package sources: %v", err)
	}

	found, inText, exempt := 0, 0, 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file) // #nosec G304 -- file comes from a glob of this package's own directory
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		inText += len(regexp.MustCompile(`\.List\(ctx, &nodes[,)]`).FindAll(source, -1))

		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, source, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		nodeLists := nodeListVars(parsed)

		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "List" || len(call.Args) < 2 || !listsNodeList(call.Args[1], nodeLists) {
					return true
				}
				found++
				if _, ok := fleetWideNodeLists[fn.Name.Name]; ok {
					exempt++
					return true
				}
				if scopedByProvisionedNodes(call.Args[2:]) {
					return true
				}
				t.Errorf("%s: %s lists Nodes fleet-wide; pass provisionedNodesOnly(), or name the function in fleetWideNodeLists with what bounds it",
					fset.Position(call.Pos()), fn.Name.Name)
				return true
			})
		}
	}

	// Self-check: an empty walk would pass vacuously, so compare against a text scan
	// and require every exemption to be reached.
	if inText == 0 || found < inText {
		t.Fatalf("the walk found %d Node list sites and the text holds %d; the walk is scanning the wrong thing", found, inText)
	}
	if exempt != len(fleetWideNodeLists) {
		t.Fatalf("reached %d of %d fleet-wide exemptions; a stale entry in fleetWideNodeLists exempts nothing",
			exempt, len(fleetWideNodeLists))
	}
}

// nodeListVars returns the variables in one file that hold a NodeList.
func nodeListVars(file *ast.File) map[string]bool {
	declared := map[string]bool{}
	note := func(names []ast.Expr, types ...ast.Expr) {
		holds := false
		for _, t := range types {
			holds = holds || isNodeListExpr(t)
		}
		if !holds {
			return
		}
		for _, name := range names {
			if ident, ok := name.(*ast.Ident); ok {
				declared[ident.Name] = true
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch decl := n.(type) {
		case *ast.ValueSpec:
			names := make([]ast.Expr, 0, len(decl.Names))
			for _, name := range decl.Names {
				names = append(names, name)
			}
			note(names, append([]ast.Expr{decl.Type}, decl.Values...)...)
		case *ast.AssignStmt:
			if decl.Tok == token.DEFINE {
				note(decl.Lhs, decl.Rhs...)
			}
		}
		return true
	})
	return declared
}

// isNodeListExpr reports whether an expression names or builds a NodeList.
func isNodeListExpr(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name == "NodeList"
	case *ast.StarExpr:
		return isNodeListExpr(e.X)
	case *ast.UnaryExpr:
		return e.Op == token.AND && isNodeListExpr(e.X)
	case *ast.CompositeLit:
		return isNodeListExpr(e.Type)
	}
	return false
}

// listsNodeList reports whether a List destination argument is a Node list.
func listsNodeList(arg ast.Expr, declared map[string]bool) bool {
	if unary, ok := arg.(*ast.UnaryExpr); ok && unary.Op == token.AND {
		arg = unary.X
	}
	if ident, ok := arg.(*ast.Ident); ok {
		return declared[ident.Name]
	}
	return isNodeListExpr(arg)
}

// scopedByProvisionedNodes reports whether the list options include provisionedNodesOnly().
func scopedByProvisionedNodes(opts []ast.Expr) bool {
	for _, opt := range opts {
		call, ok := opt.(*ast.CallExpr)
		if !ok {
			continue
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "provisionedNodesOnly" {
			return true
		}
	}
	return false
}

// TestProvisionedNodesOnly_AgainstRealAPIServer proves a real apiserver returns
// only the provisioned node; a fake client may apply the selector differently.
func TestProvisionedNodesOnly_AgainstRealAPIServer(t *testing.T) {
	c, stop := startWebhookEnv(t)
	defer stop()

	ctx := context.Background()
	provisioned := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "scope-provisioned",
		Labels: map[string]string{LabelBalloonsEnabled: "true"},
	}}
	fresh := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "scope-fresh"}}
	for _, node := range []*corev1.Node{provisioned, fresh} {
		if err := c.Create(ctx, node); err != nil {
			t.Fatalf("create %s: %v", node.Name, err)
		}
	}

	var all corev1.NodeList
	if err := c.List(ctx, &all); err != nil {
		t.Fatalf("list all nodes: %v", err)
	}
	if len(all.Items) != 2 {
		t.Fatalf("unscoped list returned %d nodes, want 2; the fixture is wrong, not the selector", len(all.Items))
	}

	var scoped corev1.NodeList
	if err := c.List(ctx, &scoped, provisionedNodesOnly()); err != nil {
		t.Fatalf("list provisioned nodes: %v", err)
	}
	if len(scoped.Items) != 1 || scoped.Items[0].Name != provisioned.Name {
		t.Fatalf("scoped list returned %v, want only %q", nodeNames(scoped), provisioned.Name)
	}
}

// TestProvisionedNodesOnly_SelectsTheLabelTheNodeReconcilerWrites fails if the
// selector and the label the reconciler writes drift apart.
func TestProvisionedNodesOnly_SelectsTheLabelTheNodeReconcilerWrites(t *testing.T) {
	opt, ok := provisionedNodesOnly().(client.MatchingLabels)
	if !ok {
		t.Fatalf("provisionedNodesOnly() = %T, want client.MatchingLabels", provisionedNodesOnly())
	}
	if got, want := opt[LabelBalloonsEnabled], "true"; got != want {
		t.Fatalf("selector on %s = %q, want %q", LabelBalloonsEnabled, got, want)
	}
	if len(opt) != 1 {
		t.Fatalf("selector = %v, want exactly the balloons-enabled label", opt)
	}
}

func nodeNames(list corev1.NodeList) []string {
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	return names
}
