package mcp

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/token"
	"go/types"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/tools/go/packages"

	"github.com/narvidev/narvi/contracts"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/platform"
)

// rawRestDef independently re-parses contracts.FS's own rest/v1/
// dtos.schema.json (NOT via schemas.go's own loadedRestDefs) so this
// test's own "verbatim" assertions check schemas.go's output against the
// contract file itself, never against schemas.go's own idea of it.
func rawRestDef(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := contracts.FS.ReadFile("rest/v1/dtos.schema.json")
	if err != nil {
		t.Fatalf("read embedded schema: %v", err)
	}
	var doc struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse embedded schema: %v", err)
	}
	raw, ok := doc.Defs[name]
	if !ok {
		t.Fatalf("no $def named %q", name)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal $def %q: %v", name, err)
	}
	return m
}

// TestToolInputSchemas_ComeFromContracts pins technical plan §43.10: each
// tool's InputSchema is the embedded $defs entry, VERBATIM -- no
// bundling, no rewriting, nothing added or removed.
func TestToolInputSchemas_ComeFromContracts(t *testing.T) {
	for _, spec := range toolSpecs(Twins{}) {
		t.Run(spec.Name, func(t *testing.T) {
			got, err := inputSchema(spec.InputDef)
			if err != nil {
				t.Fatalf("inputSchema(%q) error = %v", spec.InputDef, err)
			}
			want := rawRestDef(t, spec.InputDef)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("inputSchema(%q) = %#v,\nwant (verbatim from contracts) %#v", spec.InputDef, got, want)
			}
		})
	}
}

// TestToolOutputSchemas_ValidateRealBodies pins technical plan §43.10:
// the bundled OutputSchema for each of the three 180 tools is a
// self-contained 2020-12 document that VALIDATES a real encoding of the
// REST DTO it reuses (Session, ListSessionsResponse, ModelCatalog),
// including every transitively-$ref'd $def (Session -> AutomationReposElem;
// ModelCatalog -> ModelCatalogProvider -> ModelCatalogModel ->
// ModelCatalogCost).
func TestToolOutputSchemas_ValidateRealBodies(t *testing.T) {
	sessionBody := `{
		"id": "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a",
		"title": null,
		"status": "created",
		"failureReason": null,
		"archived": false,
		"spawnSource": "web",
		"createdBy": null,
		"createdAt": "2026-01-01T00:00:00Z",
		"updatedAt": "2026-01-01T00:00:00Z",
		"repos": [{"name": "repo", "url": "https://example.com/repo", "branch": null}],
		"sandboxStatus": null,
		"buildModelId": null,
		"buildEffort": null
	}`

	modelCatalogBody := `{
		"providers": [{
			"id": "openai",
			"models": [{
				"id": "gpt-5",
				"name": "GPT-5",
				"contextWindow": 128000,
				"toolCall": true,
				"reasoning": true,
				"variants": ["low", "high"],
				"cost": {"input": 1.5, "output": 3.0, "cacheRead": null, "cacheWrite": null}
			}]
		}]
	}`

	tests := []struct {
		outputDef string
		body      string
	}{
		{"Session", sessionBody},
		{"ListSessionsResponse", `{"sessions": [` + sessionBody + `]}`},
		{"ModelCatalog", modelCatalogBody},
	}

	for _, tt := range tests {
		t.Run(tt.outputDef, func(t *testing.T) {
			bundled, err := bundleOutputSchema(tt.outputDef)
			if err != nil {
				t.Fatalf("bundleOutputSchema(%q) error = %v", tt.outputDef, err)
			}

			bundledJSON, err := json.Marshal(bundled)
			if err != nil {
				t.Fatalf("marshal bundled schema: %v", err)
			}

			c := jsonschema.NewCompiler()
			c.AssertFormat()
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(bundledJSON))
			if err != nil {
				t.Fatalf("decode bundled schema: %v", err)
			}
			const resourceURL = "mem://bundled.json"
			if err := c.AddResource(resourceURL, doc); err != nil {
				t.Fatalf("add bundled schema resource: %v", err)
			}
			sch, err := c.Compile(resourceURL)
			if err != nil {
				t.Fatalf("compile bundled schema: %v", err)
			}

			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(tt.body)))
			if err != nil {
				t.Fatalf("decode instance: %v", err)
			}
			if err := sch.Validate(inst); err != nil {
				t.Errorf("bundled %q schema did not validate a real body: %v\nschema: %s\nbody: %s", tt.outputDef, err, bundledJSON, tt.body)
			}
		})
	}
}

// TestBundleOutputSchema_TransitiveClosure pins the bundler's own
// transitive-closure guarantee explicitly: bundling "ModelCatalog" pulls
// in ModelCatalogProvider, ModelCatalogModel, AND ModelCatalogCost (a
// three-hop chain), not merely its own direct $ref.
func TestBundleOutputSchema_TransitiveClosure(t *testing.T) {
	bundled, err := bundleOutputSchema("ModelCatalog")
	if err != nil {
		t.Fatalf("bundleOutputSchema error = %v", err)
	}
	defs, ok := bundled["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("bundled[$defs] = %#v, want a map", bundled["$defs"])
	}
	for _, want := range []string{"ModelCatalog", "ModelCatalogProvider", "ModelCatalogModel", "ModelCatalogCost"} {
		if _, ok := defs[want]; !ok {
			t.Errorf("bundled $defs missing %q; got keys %v", want, defKeys(defs))
		}
	}
	if len(defs) != 4 {
		t.Errorf("bundled $defs has %d entries (%v), want exactly 4", len(defs), defKeys(defs))
	}
}

func defKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestListModelsOutputSchema_ValidatesRealCatalogBody closes round 2
// review findings N6/N10: TestToolOutputSchemas_ValidateRealBodies above
// validates only a HAND-WRITTEN ModelCatalog body, in which every model
// already carries a non-empty "variants" array -- it cannot notice
// httpapi.GetModelCatalog()'s own nonNilStrings fix (round 1 finding M5)
// ever regressing, since a body that never exercises the empty-variants
// case validates identically whether or not that fix is even present.
// This test instead drives the REAL httpapi.GetModelCatalog() handler --
// the exact function narvi_list_models' own twin invokes -- directly
// (authorize's own check is pure role logic, no store, so no Postgres is
// needed here), and validates the REAL body it writes against the SAME
// bundled schema narvi_list_models advertises as its OutputSchema. The
// real catalog (internal/app/modelcatalog's own compiled-in snapshot)
// has multiple models with zero variants (google's gemini-2.0-flash and
// others), so a regression back to `Variants: m.Variants` (letting a nil
// slice encode as JSON null) fails this test where the hand-written-body
// test above cannot.
func TestListModelsOutputSchema_ValidatesRealCatalogBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	req = req.WithContext(platform.WithUser(req.Context(), testUser))
	rec := httptest.NewRecorder()
	httpapi.GetModelCatalog()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("httpapi.GetModelCatalog(): status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()

	if strings.Contains(string(body), `"variants":null`) {
		t.Errorf("the real model catalog body contains \"variants\":null -- round 1 finding M5's fix (nonNilStrings, httpapi/modelcatalog.go) has regressed:\n%s", body)
	}

	bundled, err := bundleOutputSchema("ModelCatalog")
	if err != nil {
		t.Fatalf("bundleOutputSchema(%q) error = %v", "ModelCatalog", err)
	}
	bundledJSON, err := json.Marshal(bundled)
	if err != nil {
		t.Fatalf("marshal bundled schema: %v", err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(bundledJSON))
	if err != nil {
		t.Fatalf("decode bundled schema: %v", err)
	}
	const resourceURL = "mem://bundled-real-catalog.json"
	if err := c.AddResource(resourceURL, doc); err != nil {
		t.Fatalf("add bundled schema resource: %v", err)
	}
	sch, err := c.Compile(resourceURL)
	if err != nil {
		t.Fatalf("compile bundled schema: %v", err)
	}

	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("decode real catalog body: %v", err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Errorf("the real model catalog body does not validate against narvi_list_models' own advertised outputSchema: %v\nbody: %s", err, body)
	}
}

// TestCompileInputSchemas_RealDefsSucceed and
// TestCompileInputSchemas_UnknownDefFails pin compileInputSchemas' own two
// documented outcomes (schemas.go): every one of toolInputDefs()'s real
// names compiles into a non-nil *jsonschema.Schema, and a name with no
// matching $def in rest/v1/dtos.schema.json returns an error rather than
// panicking or silently omitting an entry -- the exact error NewHandler
// now surfaces as a BOOT failure (round 2 review of PR #324, findings
// N1/N3/N4: a schema defect must fail boot, never a live request).
func TestCompileInputSchemas_RealDefsSucceed(t *testing.T) {
	defs := toolInputDefs()
	schemas, err := compileInputSchemas(defs)
	if err != nil {
		t.Fatalf("compileInputSchemas(%v) error = %v", defs, err)
	}
	if len(schemas) != len(defs) {
		t.Fatalf("len(schemas) = %d, want %d", len(schemas), len(defs))
	}
	for _, name := range defs {
		if schemas[name] == nil {
			t.Errorf("schemas[%q] = nil, want a compiled *jsonschema.Schema", name)
		}
	}
}

func TestCompileInputSchemas_UnknownDefFails(t *testing.T) {
	_, err := compileInputSchemas([]string{"ThisDefDoesNotExist"})
	if err == nil {
		t.Fatal("compileInputSchemas with an unknown $def name: error = nil, want an error")
	}
}

// jsonschemaImportPath is the JSON Schema compiler package's own IMPORT
// PATH -- TestNoSecondJSONSchemaCompiler below resolves every candidate
// call against THIS, via go/types, rather than against the literal
// identifier "jsonschema" a file happens to spell an import with (round 4
// review of PR #324, findings S3/S5: an import aliased to anything else,
// "js" or otherwise, made a prior, identifier-text-only revision of this
// test blind to a second, lazy compile hiding behind it).
const jsonschemaImportPath = "github.com/santhosh-tekuri/jsonschema/v6"

// eagerCompileFile and eagerCompileFunc name compileInputSchemas'
// (schemas.go) own home -- the ONE place this package's production code
// may ever call a Compile or MustCompile method on a *jsonschema.
// Compiler value (see TestNoSecondJSONSchemaCompiler's own doc comment).
const (
	eagerCompileFile = "schemas.go"
	eagerCompileFunc = "compileInputSchemas"
)

// TestNoSecondJSONSchemaCompiler is the STRUCTURAL half of round 3
// review of PR #324, finding R3: TestToolCall_ConcurrentFirstCalls_NoRace
// (toolcall_test.go) only reproduces the lazy-compile crash it exists to
// catch when it runs BEFORE any other test in the same binary has already
// exercised all three tools (its own doc comment says so) -- but
// `go test -race ./...`, the exact command CI runs (Makefile), gives no
// such guarantee: Go does not randomize test order by default, and
// several earlier tests in this package's own file order already call
// tools/call for every tool, warming any package-level cache before the
// concurrency test ever gets to run.
//
// This test does not depend on runtime ordering or on triggering a race
// at all: it type-checks this package's OWN production source (every
// *.go file in its directory, excluding _test.go -- exactly
// tools/lint/narvichecks/mcpimportban's identical "a test constructing X
// is not a production decision point" exemption, via golang.org/x/tools/
// go/packages) and inspects every call expression by what it RESOLVES TO
// (go/types' own Uses map), never by how its identifier is spelled.
//
// Round 4 review, findings S3/S5: a prior revision counted only CallExprs
// of the literal shape `jsonschema.NewCompiler(...)`, matched purely on
// identifier TEXT, and its own doc comment claimed any reintroduced lazy
// compile "necessarily needs a SECOND jsonschema.NewCompiler() call
// site" -- false on two counts, both closed here:
//
//  1. An import aliased to anything other than "jsonschema" (`js
//     "github.com/santhosh-tekuri/jsonschema/v6"`) made every call
//     through it invisible to identifier-text matching. Resolving
//     `pkg.TypesInfo.Uses[ident]` to the actual `*types.Func` and
//     checking ITS OWN package path closes this regardless of alias --
//     and a dot or blank import of this package is refused outright,
//     below, rather than taught its own special-case matching rule.
//  2. f307de9's own lazy shape needed only ONE NewCompiler call site (a
//     package-level `sync.OnceValues`) -- a SECOND, independently
//     compiled schema cache can share that SAME call site through an
//     ordinary helper function, reached from a second, lazy caller
//     the eager path never uses. Counting call SITES textually can never
//     see this: the defect is WHERE `.Compile`/`.MustCompile` is ever
//     invoked on the resulting *jsonschema.Compiler value, not how many
//     places construct one. This test therefore also resolves every
//     Compile/MustCompile call by RECEIVER TYPE (its Named type's own
//     package path and name), and requires every one of them to be
//     lexically inside compileInputSchemas itself -- the one function
//     NewHandler's own boot path relies on to compile eagerly.
//
// compileInputSchemas' result -- an immutable map[string]*jsonschema.
// Schema, built once at NewHandler/boot time (round 2 review findings
// N1/N3/N4) -- remains the ONLY source toolHandler may read a compiled
// schema from.
func TestNoSecondJSONSchemaCompiler(t *testing.T) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax,
		Tests: false, // production files only -- a test's own throwaway compiler is not a production decision point.
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		t.Fatalf("packages.Load(\".\"): %v", err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("packages.Load(\".\") reported %d error(s) loading this package (see stderr above)", n)
	}
	if len(pkgs) != 1 {
		t.Fatalf("packages.Load(\".\") returned %d packages, want exactly 1", len(pkgs))
	}
	pkg := pkgs[0]
	if len(pkg.Syntax) == 0 {
		t.Fatal("packages.Load(\".\") found no source files -- this test must run from its own package directory (go test's own documented working-directory contract)")
	}

	// A dot or blank import of the compiler package is refused outright:
	// a dot import lets NewCompiler be called with no qualifier at all
	// (a bare *ast.Ident, not a *ast.SelectorExpr with something to
	// resolve), and a blank import serves this package no purpose beyond
	// obscuring a real one alongside it -- neither is worth teaching the
	// call-site logic below its own special case for.
	for _, file := range pkg.Syntax {
		filename := pkg.Fset.Position(file.Pos()).Filename
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil || path != jsonschemaImportPath {
				continue
			}
			if imp.Name != nil && (imp.Name.Name == "." || imp.Name.Name == "_") {
				t.Fatalf("%s: %s is imported as %q -- a dot or blank import of the JSON Schema compiler package is never allowed in this package's production code", filepath.Base(filename), jsonschemaImportPath, imp.Name.Name)
			}
		}
	}

	type callSite struct {
		file, fn string
		line     int
	}
	var newCompilerSites []callSite
	var badCompileSites []callSite

	for _, file := range pkg.Syntax {
		base := filepath.Base(pkg.Fset.Position(file.Pos()).Filename)

		// enclosingFunc reports the name of the top-level func
		// declaration lexically containing pos, or "" if pos falls
		// outside every one of them (e.g. inside a package-level var's
		// own func literal -- exactly the shape a package-level
		// `sync.OnceValues(func() {...})` lazy cache takes, which must
		// never count as "inside compileInputSchemas").
		enclosingFunc := func(pos token.Pos) string {
			var name string
			ast.Inspect(file, func(n ast.Node) bool {
				if fd, ok := n.(*ast.FuncDecl); ok && fd.Pos() <= pos && pos <= fd.End() {
					name = fd.Name.Name
				}
				return true
			})
			return name
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var funcIdent *ast.Ident
			switch fn := call.Fun.(type) {
			case *ast.Ident: // a dot-imported or package-local call
				funcIdent = fn
			case *ast.SelectorExpr: // <pkg-or-value>.Name(...)
				funcIdent = fn.Sel
			default:
				return true
			}
			fnObj, ok := pkg.TypesInfo.Uses[funcIdent].(*types.Func)
			if !ok {
				return true
			}
			sig, ok := fnObj.Type().(*types.Signature)
			if !ok {
				return true
			}
			pos := pkg.Fset.Position(call.Pos())

			// A call resolving to the package-level jsonschema.
			// NewCompiler function, whatever local alias it was
			// spelled with.
			if sig.Recv() == nil && fnObj.Pkg() != nil && fnObj.Pkg().Path() == jsonschemaImportPath && fnObj.Name() == "NewCompiler" {
				newCompilerSites = append(newCompilerSites, callSite{file: base, fn: enclosingFunc(call.Pos()), line: pos.Line})
				return true
			}

			// A Compile or MustCompile method call whose RECEIVER
			// resolves to jsonschema.Compiler (by pointer or by
			// value) -- caught regardless of which variable, cache,
			// or helper the value flowed through to get here.
			if sig.Recv() != nil && (fnObj.Name() == "Compile" || fnObj.Name() == "MustCompile") {
				recvType := sig.Recv().Type()
				if ptr, ok := recvType.(*types.Pointer); ok {
					recvType = ptr.Elem()
				}
				named, ok := recvType.(*types.Named)
				if ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == jsonschemaImportPath && named.Obj().Name() == "Compiler" {
					if fn := enclosingFunc(call.Pos()); fn != eagerCompileFunc || base != eagerCompileFile {
						badCompileSites = append(badCompileSites, callSite{file: base, fn: fn, line: pos.Line})
					}
				}
			}
			return true
		})
	}

	if len(newCompilerSites) != 1 {
		t.Fatalf("found %d call site(s) resolving to %s.NewCompiler (%v), want exactly 1 -- every tool's input schema must come from %s's own %s, compiled EAGERLY at NewHandler/boot time (round 2 N1/N3/N4); a second call site is exactly the shape a reintroduced lazy, per-request compile takes (round 3 R3)", len(newCompilerSites), jsonschemaImportPath, newCompilerSites, eagerCompileFile, eagerCompileFunc)
	}
	if site := newCompilerSites[0]; site.file != eagerCompileFile {
		t.Fatalf("the one %s.NewCompiler() call site is in %s:%d, want it in %s", jsonschemaImportPath, site.file, site.line, eagerCompileFile)
	}
	if len(badCompileSites) > 0 {
		t.Fatalf("found Compile/MustCompile call(s) on a %s.Compiler value outside %s's own %s (%v) -- every compile must happen EAGERLY, at boot, in that one function; round 4 review finding S3/S5's own reproduction shows a SECOND, lazily-built shared instance can reuse the ONE NewCompiler call site above and still slip past a call-site count alone", jsonschemaImportPath, eagerCompileFile, eagerCompileFunc, badCompileSites)
	}
}
