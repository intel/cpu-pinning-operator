// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNodeListsGoThroughScopedReader requires every Node list in the mutator's source to
// use nodeReader(). A List on the manager's client starts an unscoped Node informer on
// every replica, lifting the provisioned-nodes-only memory bound. Pod and NRT reads are fine.
func TestNodeListsGoThroughScopedReader(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing package sources: %v", err)
	}

	found, inText := 0, 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file) // #nosec G304 -- file comes from a glob of this package's own directory
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		inText += len(regexp.MustCompile(`\.List\(ctx, &nodes\)`).FindAll(source, -1))

		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, source, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}

		nodeLists := nodeListVars(parsed)
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "List" || len(call.Args) < 2 || !listsNodes(call.Args[1], nodeLists) {
				return true
			}
			found++
			reader := exprText(sel.X)
			if strings.HasSuffix(reader, "nodeReader()") {
				return true
			}
			t.Errorf("%s: %s.List lists Nodes through the unscoped client; use m.nodeReader().List instead",
				fset.Position(call.Pos()), reader)
			return true
		})
	}

	// Guard against a walk that matches nothing: it must find at least the text count.
	if inText == 0 || found < inText {
		t.Fatalf("the walk found %d Node list sites and the text holds %d; the walk is scanning the wrong thing",
			found, inText)
	}
}

// nodeListVars returns the variables that hold a NodeList in one file.
func nodeListVars(file *ast.File) map[string]bool {
	declared := map[string]bool{}
	note := func(names []ast.Expr, types ...ast.Expr) {
		holds := false
		for _, t := range types {
			holds = holds || isNodeList(t)
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

// isNodeList reports whether expr names or builds a NodeList, directly or via pointer.
func isNodeList(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name == "NodeList"
	case *ast.StarExpr:
		return isNodeList(e.X)
	case *ast.UnaryExpr:
		return e.Op == token.AND && isNodeList(e.X)
	case *ast.CompositeLit:
		return isNodeList(e.Type)
	}
	return false
}

// listsNodes reports whether a List destination argument is a Node list.
func listsNodes(arg ast.Expr, declared map[string]bool) bool {
	if unary, ok := arg.(*ast.UnaryExpr); ok && unary.Op == token.AND {
		arg = unary.X
	}
	if ident, ok := arg.(*ast.Ident); ok {
		return declared[ident.Name]
	}
	return isNodeList(arg)
}

// exprText renders a call receiver for failure messages.
func exprText(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprText(e.X) + "." + e.Sel.Name
	case *ast.CallExpr:
		return exprText(e.Fun) + "()"
	}
	return "the receiver"
}
