package ports

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// This file is this package's own port of internal/domain/review's
// placeholderdrift_internal_test.go idiom (see that file's own top doc
// comment for the full "why a source scan" reasoning): a go/ast walk over
// this repository's own real source, rather than a runtime reflection or
// go/packages type-check, so it needs no new import of anything it
// scans and cannot participate in an import cycle no matter what a
// future package under scan imports.
//
// # What this proves, and why a syntactic scan is enough
//
// KnowledgeRanker's own doc comment (knowledgeranker.go) states the
// property technical plan §34.7 requires: Score's result is exactly
// ([]float64, error), full stop, because that is the only shape with no
// channel through which a candidate could be returned. The Go compiler
// already enforces that this interface's own DECLARED shape and any of
// its ACTUAL implementations agree -- a type whose Score disagrees with
// the interface simply fails to satisfy it, at every call site that
// assigns one to the other. What the compiler cannot catch is the
// composed mistake: a future change that edits KnowledgeRanker's own
// declared Score AND every implementation together, consistently, to
// return ([]knowledge.Candidate, error) instead -- nothing fails to
// compile, because both sides changed in lockstep. This test is a second,
// independent opinion that never reads KnowledgeRanker's own current
// declaration at all: it hard-codes "results are ([]float64, error)"
// itself and checks every real implementation against ITS OWN pinned
// expectation, so a coordinated widening of both sides still fails here
// even though it would build cleanly.
//
// A type is treated as an implementation, for this scan's purposes, if it
// declares BOTH a zero-argument, one-result "Name() string" method and a
// "Score" method with a receiver of the same base type name -- the same
// two methods KnowledgeRanker itself declares, checked by name rather
// than by full type-identity (which would require a type-checked scan,
// not merely a syntactic one) because grepping this repository at the
// time this test was written found no OTHER "Score"/"Name() string" pair
// on any type, on any receiver, anywhere (verified: `grep -rn "Name()
// string" --include="*.go" .` paired against a "Score(" method on the
// same receiver matches only this seam's own two implementations).
func TestKnowledgeRankerImplementationsReturnScoresOnly(t *testing.T) {
	t.Parallel()

	root := repoRootForArchTest(t)
	roots := []string{"internal", "cmd", "controlplane", "extension", "contracts"}

	nameStringReceivers := map[string]bool{}
	type scoreMethod struct {
		receiver string
		file     string
		line     int
		results  []string
	}
	var scoreMethods []scoreMethod

	fset := token.NewFileSet()
	for _, r := range roots {
		dir := filepath.Join(root, r)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
					continue
				}
				recvName := receiverBaseTypeName(fn.Recv.List[0].Type)
				if recvName == "" {
					continue
				}
				switch {
				case fn.Name.Name == "Name" && isNilaryStringMethod(fn.Type):
					nameStringReceivers[recvName] = true
				case fn.Name.Name == "Score":
					pos := fset.Position(fn.Pos())
					scoreMethods = append(scoreMethods, scoreMethod{
						receiver: recvName,
						file:     rel,
						line:     pos.Line,
						results:  resultTypeStrings(fn.Type),
					})
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scanning %s for KnowledgeRanker implementations: %v", dir, err)
		}
	}

	var checked int
	for _, sm := range scoreMethods {
		if !nameStringReceivers[sm.receiver] {
			continue // has a Score method, but no matching Name() string -- not a KnowledgeRanker candidate
		}
		checked++
		want := []string{"[]float64", "error"}
		if !equalStrings(sm.results, want) {
			t.Errorf("%s:%d: type %q implements KnowledgeRanker (has both Name() string and Score(...)) but Score's results are %v, want exactly %v -- "+
				"a KnowledgeRanker must never be able to return a Candidate (technical plan §34.7): if this is a deliberate interface change, "+
				"it belongs in internal/app/ports/knowledgeranker.go's own doc comment and this test's own expectation, as an explicit, reviewed edit",
				sm.file, sm.line, sm.receiver, sm.results, want)
		}
	}

	// A defensive sanity check on the scan itself, mirroring internal/
	// domain/review's own identical "finding zero means the scan itself
	// is broken" precedent: at the time this test was written,
	// knowledge.RecencyRanker and controlplane's capabilitySwitchRanker
	// both implement KnowledgeRanker, so finding fewer than 2 means this
	// scan stopped working, not that the codebase suddenly has fewer
	// implementations.
	if checked < 2 {
		t.Fatalf("scan found only %d type(s) implementing KnowledgeRanker (Name() string + Score(...) on the same receiver) under %v -- "+
			"expected at least 2 (knowledge.RecencyRanker, controlplane.capabilitySwitchRanker); the scan itself is almost certainly broken", checked, roots)
	}
}

// receiverBaseTypeName returns a method receiver's own base type name --
// "Foo" for "(r Foo)", "(r *Foo)", or a generic "(r Foo[T])" -- or "" for
// any shape this scan does not need to understand.
func receiverBaseTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return receiverBaseTypeName(e.X)
	case *ast.IndexExpr:
		return receiverBaseTypeName(e.X)
	case *ast.IndexListExpr:
		return receiverBaseTypeName(e.X)
	default:
		return ""
	}
}

// isNilaryStringMethod reports whether fn takes zero parameters and
// returns exactly one result of type "string" -- KnowledgeRanker.Name's
// own shape.
func isNilaryStringMethod(fn *ast.FuncType) bool {
	if fn.Params != nil && len(fn.Params.List) != 0 {
		return false
	}
	if fn.Results == nil || len(fn.Results.List) != 1 {
		return false
	}
	ident, ok := fn.Results.List[0].Type.(*ast.Ident)
	return ok && ident.Name == "string"
}

// resultTypeStrings renders fn's own result types, in order, as their
// source-text form ("[]float64", "error", ...) -- go/types.ExprString is
// a pure AST-to-string formatter here, not a type-checker: this scan
// never resolves what "error" or "float64" actually refer to, it only
// ever compares their written-out spelling.
func resultTypeStrings(fn *ast.FuncType) []string {
	if fn.Results == nil {
		return nil
	}
	var out []string
	for _, field := range fn.Results.List {
		n := len(field.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			out = append(out, types.ExprString(field.Type))
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// repoRootForArchTest resolves the absolute path to this repository's own
// root regardless of the working directory `go test` happens to run
// from -- runtime.Caller(0) gives this file's own compile-time absolute
// path (.../internal/app/ports/knowledgeranker_archtest_test.go), whose
// containing directory (internal/app/ports) is three levels below the
// repository root. Mirrors internal/ops's own repoRoot(t) helper exactly
// (filepath.Join(dir, "..", "..") for a file two levels down); this
// package sits one level deeper, hence three ".." rather than two.
func repoRootForArchTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repository root for the source scan")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}
