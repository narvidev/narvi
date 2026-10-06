package sessionactor

import (
	"go/ast"
	"go/types"
	"strings"
	"testing"
)

// workflowenginePkgPath is the workflow engine's package, under the module.
const workflowenginePkgPath = "internal/app/workflowengine"

// TestOnTurnCompletedCallersWireTheFreeze pins that the workflow engine's
// automatic advance is read against the autonomy freeze (technical plan
// §40.2) wherever it runs. OnTurnCompleted advances, logged at Error, when
// its Deps carry no Autonomy -- the package's fail-open rule -- so a caller
// that forgot it would advance while frozen. Every call of
// workflowengine.OnTurnCompleted in the production tree is in this package
// and hands it the Deps workflowDeps builds, directly or through a variable
// assigned from it; and every workflowengine.Deps literal in this package
// sets Autonomy, to something other than nil.
func TestOnTurnCompletedCallersWireTheFreeze(t *testing.T) {
	t.Parallel()

	calls, literals := 0, 0
	for _, f := range productionFiles(t) {
		if f.info == nil {
			continue
		}
		inPackage := strings.HasPrefix(f.rel, sessionactorPkgPath+"/") && !strings.Contains(strings.TrimPrefix(f.rel, sessionactorPkgPath+"/"), "/")
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if !f.isWorkflowEngineObject(n.Fun, "OnTurnCompleted") {
					return true
				}
				calls++
				pos := f.fset.Position(n.Pos())
				if !inPackage {
					t.Errorf("%s: workflowengine.OnTurnCompleted called outside %s: wire its Deps.Autonomy (the autonomy freeze, §40.2) there, then extend this test", pos, sessionactorPkgPath)
					return true
				}
				if len(n.Args) < 2 || !f.fromWorkflowDeps(n.Args[1]) {
					t.Errorf("%s: OnTurnCompleted's Deps do not come from a.workflowDeps(tx), which binds the autonomy freeze: an advance from here is never held while autonomy is frozen (§40.2)", pos)
				}
			case *ast.CompositeLit:
				if !inPackage || !f.isWorkflowEngineDeps(n) {
					return true
				}
				literals++
				if !setsNonNil(n, "Autonomy") {
					t.Errorf("%s: a workflowengine.Deps literal without Autonomy: the engine's advance from it is never held while autonomy is frozen (§40.2)", f.fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
	// pushpr.go, timerfired.go, dispatch.go and stop.go each end a turn.
	if calls < 4 {
		t.Fatalf("found %d calls of workflowengine.OnTurnCompleted, want at least 4: the scan no longer sees them", calls)
	}
	if literals < 1 {
		t.Fatalf("found no workflowengine.Deps literal in %s: the scan no longer sees workflowDeps", sessionactorPkgPath)
	}
}

// isWorkflowEngineObject reports whether expr names the workflow engine's
// package-level object name, through the type checker.
func (f productionFile) isWorkflowEngineObject(expr ast.Expr, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	obj := f.info.Uses[sel.Sel]
	return obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == f.module+"/"+workflowenginePkgPath
}

// isWorkflowEngineDeps reports whether lit is a workflowengine.Deps value.
func (f productionFile) isWorkflowEngineDeps(lit *ast.CompositeLit) bool {
	tv, ok := f.info.Types[lit]
	if !ok {
		return false
	}
	named, ok := types.Unalias(tv.Type).(*types.Named)
	return ok && named.Obj().Name() == "Deps" && named.Obj().Pkg() != nil &&
		named.Obj().Pkg().Path() == f.module+"/"+workflowenginePkgPath
}

// fromWorkflowDeps reports whether expr is a call of the Actor's
// workflowDeps, or a variable this file assigns one to.
func (f productionFile) fromWorkflowDeps(expr ast.Expr) bool {
	if f.callsWorkflowDeps(expr) {
		return true
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	obj := f.info.Uses[ident]
	if obj == nil {
		return false
	}
	found := false
	ast.Inspect(f.file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != len(assign.Rhs) {
			return !found
		}
		for i, lhs := range assign.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && (f.info.Defs[id] == obj || f.info.Uses[id] == obj) && f.callsWorkflowDeps(assign.Rhs[i]) {
				found = true
			}
		}
		return !found
	})
	return found
}

// callsWorkflowDeps reports whether expr calls this package's
// (*Actor).workflowDeps.
func (f productionFile) callsWorkflowDeps(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	m, ok := f.method(sel)
	return ok && m.is(sessionactorPkgPath, "Actor") && m.name == "workflowDeps"
}

// setsNonNil reports whether lit sets its field key to something other than
// nil.
func setsNonNil(lit *ast.CompositeLit, key string) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == key {
			v, isIdent := kv.Value.(*ast.Ident)
			return !isIdent || v.Name != "nil"
		}
	}
	return false
}
