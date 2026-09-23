package ops

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// RegisteredRoute records where one HTTP route was registered on this
// binary's chi.Router — §10's own sibling of instruments.go's
// RegisteredInstrument, over a different source (this repo's own route
// wiring instead of its OTel instrument registrations), kept for the same
// reason: self-describing even if extracted from the map CheckGuideDrift
// consults it through.
type RegisteredRoute struct {
	Method string
	Path   string
	File   string
	Line   int
}

// chiRouterMethods is the set of chi.Router method names this codebase's
// own established convention uses to register a route handler (confirmed
// against every real router.<Method>(...)/r.<Method>(...) call site in
// the controlplane package at the time this scanner was written — see
// ScanRegisteredRoutes's own doc comment for the "controlplane only"
// scope decision this mirrors ScanRegisteredInstruments's identical
// "instrumentMethods" precedent for). Unlike that map, a call matching one
// of these five names is NOT guaranteed to be a real chi.Router — "Get"/
// "Post"/etc. are common enough method names (an outbound http.Client.Get,
// for instance) that this scanner will also record an unrelated call as a
// "route". That specific trade-off is harmless (an unrelated same-named
// call can only ENLARGE the registered set, never omit a real route — see
// CheckDrift's own identical "a false extra entry never hides a genuine
// one" property in drift.go).
//
// A DIFFERENT, real gap: this scanner recognizes exactly these five
// method names plus Route(...) — nothing else. A route registered any
// other real way — a path held in a `const` rather than a string
// literal; router.Method(...)/.Handle(...)/.HandleFunc(...); a helper
// function outside controlplane/ that takes a chi.Router and registers
// routes on it; or a Mount'd sub-router, whose own unprefixed route name
// can silently collide with, and overwrite, an existing top-level key in
// this scanner's own "found" map, losing the real, Mount-prefixed route
// entirely — is invisible to it while staying perfectly reachable in
// production. An earlier version of this comment claimed the opposite:
// that this scanner "can only enlarge the registered set, never omit a
// real route". That is true of the same-named-method trade-off above and
// false as a claim about this scanner in general — a reviewer proved it
// with all four shapes just listed, each one leaving CheckGuideDrift's
// own completeness check (guidedrift.go) silent about a real,
// undocumented, unexempted route, because that check trusts this
// scanner's own "found" set as ground truth and cannot see anything this
// scanner never reports at all.
// TestScanRegisteredRoutes_MatchesGolden (internal/ops/
// routesgolden_test.go) is what closes that gap: it compares this
// scanner's own output against controlplane/testdata/routes.golden — the
// one artifact in this repo captured directly from the real, live
// chi.Walk (controlplane.App.Routes(), pinned to it by the
// integration-tagged TestBuild_RouteTableMatchesGolden) — in both
// directions, so a route this scanner misses fails that test by name
// the moment someone regenerates the golden from the live router,
// instead of silently staying invisible here too. Scanning is restricted
// to controlplane specifically (the one place chi routes are registered
// in this codebase — confirmed by grepping for chi.NewRouter/chi.Router
// across internal/ and cmd/) to keep the noise minimal.
var chiRouterMethods = map[string]bool{
	"Get":    true,
	"Post":   true,
	"Put":    true,
	"Patch":  true,
	"Delete": true,
}

// ScanRegisteredRoutes walks every .go file under each of roots (skipping
// _test.go files, exactly like ScanRegisteredInstruments's own
// skipFile), parses it via go/parser, and collects one RegisteredRoute
// per real chi route registration — every `<expr>.<Method>("path", ...)`
// call (Method one of chiRouterMethods) plus every nested
// `<expr>.Route("prefix", func(r chi.Router) { ... })` group, joined via
// joinRoutePath so a route registered inside a Route(...) group carries
// its full path, e.g. "GET /api/members" for `router.Route("/api/members",
// func(r chi.Router) { r.Get("/", ...) })`.
//
// This is a plain recursive-descent walk, not a single flat ast.Inspect,
// specifically so a Route(...) group's own inner Get/Post/etc. calls are
// visited with the CORRECT accumulated prefix exactly once — see
// scanRouterNode's own doc comment for why returning false on a matched
// Route(...) call is load-bearing, not incidental.
func ScanRegisteredRoutes(roots ...string) (map[string]RegisteredRoute, error) {
	found := map[string]RegisteredRoute{}
	fset := token.NewFileSet()

	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}

			file, perr := parser.ParseFile(fset, p, nil, 0)
			if perr != nil {
				return fmt.Errorf("ops: parse %s: %w", p, perr)
			}

			scanRouterNode(file, "", found, fset, p)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return found, nil
}

// scanRouterNode walks n (a *ast.File, or a *ast.BlockStmt recursed into
// from a matched Route(...) call below) recording every chiRouterMethods
// call found directly inside it into out, joined with prefix.
//
// On a `<expr>.Route("sub", func(r chi.Router) { ... })` call, this
// recurses into the FuncLit's own body with prefix+"sub" and returns
// false for that CallExpr node — this is the one load-bearing detail: ast.
// Inspect's own contract is that returning false skips every DESCENDANT
// of the current node (including the FuncLit and everything inside it),
// so without it the SAME inner Get/Post/etc. calls would be visited AGAIN
// by the enclosing ast.Inspect's own generic walk, once correctly (via
// this recursive call) and once more with the stale outer prefix —
// silently registering a second, WRONG route for the same handler.
func scanRouterNode(n ast.Node, prefix string, out map[string]RegisteredRoute, fset *token.FileSet, file string) {
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		if sel.Sel.Name == "Route" && len(call.Args) == 2 {
			subPath, ok := stringLiteralValue(call.Args[0])
			if !ok {
				return true
			}
			fn, ok := call.Args[1].(*ast.FuncLit)
			if !ok || fn.Body == nil {
				return true
			}
			scanRouterNode(fn.Body, joinRoutePath(prefix, subPath), out, fset, file)
			return false
		}

		if chiRouterMethods[sel.Sel.Name] && len(call.Args) >= 1 {
			subPath, ok := stringLiteralValue(call.Args[0])
			if !ok {
				return true
			}
			method := strings.ToUpper(sel.Sel.Name)
			fullPath := joinRoutePath(prefix, subPath)
			key := method + " " + fullPath
			pos := fset.Position(call.Pos())
			out[key] = RegisteredRoute{Method: method, Path: fullPath, File: file, Line: pos.Line}
		}

		return true
	})
}

// stringLiteralValue extracts a string literal's real value from e, or
// reports ok=false for anything else (a computed path expression) —
// mirrors instruments.go's identical "a non-literal first argument is
// silently skipped rather than erroring" convention.
func stringLiteralValue(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// joinRoutePath joins prefix and p the same way chi itself composes a
// Route(...) group's own prefix with a handler's sub-path (path.Join,
// which also collapses the sub-path "/" chi handlers commonly register
// their own group's root under — e.g. prefix "/api/members" + sub-path
// "/" becomes exactly "/api/members", never "/api/members/"). A caller
// documenting a route in docs/guides/*.md must use this SAME joined
// shape (no trailing slash) for CheckGuideDrift's own lookup to match.
func joinRoutePath(prefix, p string) string {
	joined := path.Join(prefix, p)
	if joined == "" {
		return "/"
	}
	return joined
}
