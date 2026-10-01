package decisioninbox

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// packageDir is this package's source directory.
func packageDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(file)
}

// freshnessCall reports whether call is a ResolveBranchSHA or IsAncestor
// call -- one of the two live calls reviewfreshness.ReadLive makes -- and
// which.
func freshnessCall(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	switch sel.Sel.Name {
	case "ResolveBranchSHA", "IsAncestor":
		return sel.Sel.Name, true
	}
	return "", false
}

// assertReadsLiveFactsOnlyThroughReviewFreshness fails t unless fn, declared
// in file of this package, calls reviewfreshness.ReadLive exactly once and
// makes no ResolveBranchSHA or IsAncestor call of its own that could drift
// from it.
func assertReadsLiveFactsOnlyThroughReviewFreshness(t *testing.T, file, fn string) {
	t.Helper()
	path := filepath.Join(packageDir(t), file)
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var decl *ast.FuncDecl
	for _, d := range parsed.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == fn {
			decl = f
		}
	}
	if decl == nil {
		t.Fatalf("%s declares no %s", path, fn)
	}
	readLive := 0
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name, ok := freshnessCall(call); ok {
			t.Errorf("%s calls %s itself -- live freshness facts must come from reviewfreshness.ReadLive alone", fn, name)
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ReadLive" {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "reviewfreshness" {
				readLive++
			}
		}
		return true
	})
	if readLive != 1 {
		t.Errorf("%s calls reviewfreshness.ReadLive %d times, want exactly once", fn, readLive)
	}
}

// TestRevalidateCore_ReadsLiveFactsOnlyThroughReviewFreshness pins the merge
// path's side of row 182's "one live read" (technical plan §43.20,
// §21.1b): revalidateCore establishes the live freshness facts only through
// reviewfreshness.ReadLive -- the function a session's result calls too --
// and makes no base or ancestry call of its own that could drift from it.
func TestRevalidateCore_ReadsLiveFactsOnlyThroughReviewFreshness(t *testing.T) {
	t.Parallel()
	assertReadsLiveFactsOnlyThroughReviewFreshness(t, "revalidate.go", "revalidateCore")
}

// TestComputeRealEligibility_ReadsLiveFactsOnlyThroughReviewFreshness pins
// the decision inbox's side (§21.1b, row 220): the read model establishes
// the live freshness facts only through reviewfreshness.ReadLive, as the
// merge path does, handing it the cache's view of the calls rather than
// making them itself.
func TestComputeRealEligibility_ReadsLiveFactsOnlyThroughReviewFreshness(t *testing.T) {
	t.Parallel()
	assertReadsLiveFactsOnlyThroughReviewFreshness(t, "aggregate.go", "computeRealEligibility")
}

// TestDecisionInbox_FreshnessCallsOnlyInItsCache sweeps every non-test file
// of this package: the only ResolveBranchSHA and IsAncestor calls are
// scmcache.go's -- the cache's own reads of the port, and the view of them
// the read model hands reviewfreshness.ReadLive -- so no code in the inbox
// or the merge path reads a freshness fact anywhere ReadLive does not.
func TestDecisionInbox_FreshnessCallsOnlyInItsCache(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join(packageDir(t), "*.go"))
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}
	fset := token.NewFileSet()
	swept := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		swept++
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if filepath.Base(path) == "scmcache.go" {
			continue
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name, ok := freshnessCall(call); ok {
				t.Errorf("%s calls %s -- outside the cache, live freshness facts come from reviewfreshness.ReadLive alone", fset.Position(call.Pos()), name)
			}
			return true
		})
	}
	if swept < 2 {
		t.Fatalf("swept %d source files, want this package's", swept)
	}
}
