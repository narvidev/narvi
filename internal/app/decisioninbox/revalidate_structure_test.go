package decisioninbox

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRevalidateCore_ReadsLiveFactsOnlyThroughReviewFreshness pins the merge
// path's side of row 182's "one live read" (technical plan §43.20,
// §21.1b): revalidateCore establishes the live freshness facts only through
// reviewfreshness.ReadLive -- the function a session's result calls too --
// and makes no base or ancestry call of its own that could drift from it.
func TestRevalidateCore_ReadsLiveFactsOnlyThroughReviewFreshness(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "revalidate.go")
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var core *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "revalidateCore" {
			core = fn
		}
	}
	if core == nil {
		t.Fatalf("%s declares no revalidateCore", path)
	}
	readLive := 0
	ast.Inspect(core.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "ReadLive":
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "reviewfreshness" {
				readLive++
			}
		case "ResolveBranchSHA", "IsAncestor":
			t.Errorf("revalidateCore calls %s itself -- live freshness facts must come from reviewfreshness.ReadLive alone", sel.Sel.Name)
		}
		return true
	})
	if readLive != 1 {
		t.Errorf("revalidateCore calls reviewfreshness.ReadLive %d times, want exactly once", readLive)
	}
}
