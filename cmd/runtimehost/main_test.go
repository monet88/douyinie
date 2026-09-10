package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestProductionRuntimeHostDoesNotInjectAudioRoleAnalyzer(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse RuntimeHost main.go: %v", err)
	}

	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == "NewAudioRoleServiceWithAnalyzer" {
			t.Errorf("production RuntimeHost must construct AudioRoleService without a direct analyzer so Router governance remains authoritative")
		}
		return true
	})
}
