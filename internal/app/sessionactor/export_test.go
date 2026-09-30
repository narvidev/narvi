package sessionactor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// HasOwnTimerHandlerForTest reports whether handleTimerFired sends a timer
// named name to a handler of its own (timerHandler) rather than to
// handleUnknownTimer. For tests only: the external test package pins
// through it that every declared kind has its own handler.
func HasOwnTimerHandlerForTest(name string) bool {
	_, ok := (&Actor{}).timerHandler(name)
	return ok
}

// DeclaredTimerKindsForTest parses this package's own non-test Go files and
// returns every package-level string constant named Timer* -- the named
// timer kinds (command.go) -- as name -> value. Parsed from source, not
// listed by hand, so a kind added later is found without anyone updating a
// list. For tests only, and shared here so the external unit tests
// (timerwork_test.go) and this package's integration tests read the same
// list.
func DeclaredTimerKindsForTest(t testing.TB) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	kinds := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, ident := range vs.Names {
					if !strings.HasPrefix(ident.Name, "Timer") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", ident.Name, err)
					}
					kinds[ident.Name] = value
				}
			}
		}
	}
	return kinds
}
