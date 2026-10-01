package decisioninbox

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"slices"
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

// freshnessSelector reports whether n names ResolveBranchSHA or IsAncestor
// -- one of the two live calls reviewfreshness.ReadLive makes -- through a
// selector, and which. Any selector counts, not only a call: a method value
// (f := sc.ResolveBranchSHA) reads the fact as surely as a call does.
func freshnessSelector(n ast.Node) (string, bool) {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	switch sel.Sel.Name {
	case "ResolveBranchSHA", "IsAncestor":
		return sel.Sel.Name, true
	}
	return "", false
}

// funcName is decl's name, with its receiver's type for a method
// ("SCMCache.ResolveBranchSHA").
func funcName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
	}
	recv := decl.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if ident, ok := recv.(*ast.Ident); ok {
		return ident.Name + "." + decl.Name.Name
	}
	return decl.Name.Name
}

// assertReadsLiveFactsOnlyThroughReviewFreshness fails t unless fn, declared
// in file of this package, calls reviewfreshness.ReadLive exactly once and
// names no ResolveBranchSHA or IsAncestor of its own -- called or taken as a
// method value -- that could drift from it.
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
		if name, ok := freshnessSelector(n); ok {
			t.Errorf("%s names %s itself -- live freshness facts must come from reviewfreshness.ReadLive alone", fn, name)
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
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

// cacheReadMethods are the only functions in this package allowed to name
// ResolveBranchSHA or IsAncestor: the cache's own reads of the port, and
// the view of them the read model hands reviewfreshness.ReadLive
// (SCMCache.FreshnessReads).
var cacheReadMethods = []string{
	"SCMCache.ResolveBranchSHA",
	"SCMCache.IsAncestor",
	"freshnessReads.ResolveBranchSHA",
	"freshnessReads.IsAncestor",
}

// TestDecisionInbox_FreshnessCallsOnlyInItsCache sweeps every non-test file
// of this package: outside the cache's own read methods (cacheReadMethods)
// nothing names ResolveBranchSHA or IsAncestor -- called, or taken as a
// method value -- so no code in the inbox or the merge path, a helper
// beside the cache included, reads a freshness fact anywhere ReadLive does
// not. The sweep is by name and covers this package alone: a call made
// from another package on the inbox's behalf is outside what it sees.
func TestDecisionInbox_FreshnessCallsOnlyInItsCache(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join(packageDir(t), "*.go"))
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}
	fset := token.NewFileSet()
	swept := 0
	exempted := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		swept++
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, d := range parsed.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && slices.Contains(cacheReadMethods, funcName(fn)) {
				exempted[funcName(fn)] = true
				continue
			}
			ast.Inspect(d, func(n ast.Node) bool {
				if name, ok := freshnessSelector(n); ok {
					t.Errorf("%s names %s -- outside the cache's own read methods, live freshness facts come from reviewfreshness.ReadLive alone", fset.Position(n.Pos()), name)
				}
				return true
			})
		}
	}
	if swept < 2 {
		t.Fatalf("swept %d source files, want this package's", swept)
	}
	// The exemption must name methods that exist, or it silently widens to
	// whatever later takes one of those names.
	for _, name := range cacheReadMethods {
		if !exempted[name] {
			t.Errorf("cacheReadMethods names %s, which this package no longer declares", name)
		}
	}
}
