// Package reasoncoverage implements a static-analysis check closing the
// A1 audit-finding gap in internal/domain/imagedecision's own closed
// Reason vocabulary (see that package's own top doc comment, point 2, for
// the full four-mechanism account this Analyzer is one part of).
//
// Reason is a defined string type, not a sealed sum type, so nothing in
// the Go type system stops a new Reason constant from being declared and
// returned from a real call site while never being added to All() --
// the single slice imagedecision_integration_test.go's own drift check
// diffs against the live Postgres ENUM, and the ONLY thing
// persistImageDecisionBestEffort (internal/app/sessionactor) checks a
// value against before persisting it. Before this Analyzer existed, that
// exact one-sided edit compiled cleanly, passed go vet, passed
// golangci-lint, passed the unit test (which only inspects All()'s own
// length and contents, unchanged by the new constant), and passed the
// drift test (which only compares All() against pg_enum, and All() was
// not touched) -- reaching production having been checked by nothing,
// and failing at runtime with a Postgres "invalid input value for enum"
// error that rolled back the whole persisting transact, silently
// dropping the record.
//
// This Analyzer removes that possibility from the type of mistake it is:
// every Reason-typed constant declared in the imagedecision package's own
// const block must be listed in that package's own All() function, or
// `make lint` fails naming the constant, at the exact point it was
// declared.
package reasoncoverage

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

const doc = `report a Reason constant declared in package imagedecision that All() omits

internal/domain/imagedecision.Reason is a defined string type, not a
sealed sum type: nothing in Go stops a new Reason constant from being
declared, and returned from a real call site, without ever being added to
All() -- the single source both the Postgres ENUM migration and the
live-database drift test are written against. A one-sided edit like that
compiles, vets, and lints clean today, and fails only at runtime, as a
Postgres enum rejection that rolls back the whole persisting transact and
silently drops the record. This check reports the missing constant at
its own declaration, before any of that can happen.`

// Analyzer reports any exported-or-unexported constant of type Reason,
// declared in a package named "imagedecision", that this package's own
// All() function (a same-named, no-receiver func returning a slice whose
// elements are collected from composite-literal identifiers) does not
// list. See allowedMissing below for the one deliberate exception.
var Analyzer = &analysis.Analyzer{
	Name: "reasoncoverage",
	Doc:  doc,
	Run:  run,
}

// allowedMissing is the single, deliberate exception to "every declared
// Reason must appear in All()". ReasonNone (internal/domain/imagedecision/
// reason.go's own const block) exists purely as a typed placeholder for a
// caller position where "there is no reason to report" is itself the
// correct answer, and its own doc comment states plainly it must NEVER be
// persisted -- excluded from All() (and therefore the Postgres enum) on
// purpose, not the omission this Analyzer exists to catch. Widening this
// map is exactly the kind of deliberate act it should stay: name a new
// exception here, with a reason, rather than letting this Analyzer
// silently learn to ignore one some other way.
var allowedMissing = map[string]bool{
	"ReasonNone": true,
}

// reasonTypeName / allFuncName / targetPackageName name the exact shape
// this Analyzer looks for -- see this package's own top doc comment for
// why these three names, specifically, are what closes the gap.
const (
	reasonTypeName    = "Reason"
	allFuncName       = "All"
	targetPackageName = "imagedecision"
)

func run(pass *analysis.Pass) (any, error) {
	if pass.Pkg.Name() != targetPackageName {
		return nil, nil
	}

	// reasonType is this package's own Reason named type, looked up by
	// name rather than assumed -- every constant below is tested against
	// it with types.AssignableTo, not against its own AST-level type
	// syntax (see the loop below for why that distinction is load-bearing).
	reasonObj := pass.Pkg.Scope().Lookup(reasonTypeName)
	if reasonObj == nil {
		return nil, nil
	}
	reasonType := reasonObj.Type()

	declared := map[string]token.Pos{}
	var order []string
	listed := map[string]bool{}

	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						if name.Name == "_" {
							continue
						}
						// types.AssignableTo, not an *ast.Ident match against
						// vs.Type, on purpose: an UNTYPED constant (no "Reason"
						// written at its own declaration, e.g. `ReasonFoo =
						// "foo"`) reports vs.Type == nil and would be invisible
						// to a syntactic check, yet is still implicitly
						// assignable to Reason and returnable from a real call
						// site. pass.TypesInfo.Defs[name].Type() is "untyped
						// string" for that shape, NOT this package's own
						// Reason -- AssignableTo is what recognizes it anyway,
						// exactly as it does the explicitly-typed shape.
						obj := pass.TypesInfo.Defs[name]
						if obj == nil || !types.AssignableTo(obj.Type(), reasonType) {
							continue
						}
						if _, exists := declared[name.Name]; !exists {
							order = append(order, name.Name)
						}
						declared[name.Name] = name.Pos()
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name == allFuncName {
					collectListedNames(d, listed)
				}
			}
		}
	}

	for _, name := range order {
		if allowedMissing[name] {
			continue
		}
		if listed[name] {
			continue
		}
		pass.Reportf(declared[name],
			"%s is declared as a %s constant but is not listed in %s() -- persistImageDecisionBestEffort's own validatedPersistReason (internal/app/sessionactor/imageresolve.go) substitutes any Reason outside %s() with ReasonUnrecognized before the write ever reaches the Postgres enum (migrations/000139_sandboxes_image_decision.up.sql), so a real call site returning this constant silently mis-buckets into 'unrecognized' rather than being rejected at write time; add it to %s(), and to that migration's CREATE TYPE list, or name it in reasoncoverage's own allowedMissing if it must never be persisted (see ReasonNone)",
			name, reasonTypeName, allFuncName, allFuncName, allFuncName)
	}

	return nil, nil
}

// collectListedNames walks fd's body for composite-literal elements that
// are bare identifiers (the []Reason{ReasonFoo, ReasonBar, ...} shape
// All() actually uses) and records each identifier's name in out.
func collectListedNames(fd *ast.FuncDecl, out map[string]bool) {
	if fd.Body == nil {
		return
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, elt := range lit.Elts {
			if ident, ok := elt.(*ast.Ident); ok {
				out[ident.Name] = true
			}
		}
		return true
	})
}
